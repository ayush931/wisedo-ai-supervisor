package dispatch

import (
	"context"
	"errors"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ingestion"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const (
	progressSignal        = "outbox"
	progressSignalsPerRun = 500
	progressFoldLimit     = 500
)

// SourceProgress is one long-running workflow per operational source. Each
// signal folds the durable outbox into completeness/liveness state; duplicate
// signals are drained. Ordering and idempotency live in the database cursor,
// so the workflow only needs signal, activity and continue-as-new. Bounded
// activity retries make a stuck fold fail the run instead of accumulating
// relay signals in history; the next relay tick restarts it from the cursor.
func SourceProgress(ctx workflow.Context, ref ingestion.SourceRef) error {
	actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute, RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, MaximumInterval: time.Minute, MaximumAttempts: 10}})
	ch := workflow.GetSignalChannel(ctx, progressSignal)
	for i := 0; i < progressSignalsPerRun; i++ {
		ch.Receive(ctx, nil)
		for ch.ReceiveAsync(nil) {
		}
		if err := workflow.ExecuteActivity(actx, "FoldOperationalSource", ref).Get(ctx, nil); err != nil {
			return err
		}
	}
	return workflow.NewContinueAsNewError(ctx, SourceProgress, ref)
}

// FoldSource drains the outbox for one source until the cursor is current.
func (a *Activities) FoldSource(ctx context.Context, ref ingestion.SourceRef) error {
	for _, id := range []string{ref.TenantID, ref.SiteID, ref.SourceID} {
		if _, e := uuid.Parse(id); e != nil {
			return temporal.NewNonRetryableApplicationError("invalid scope", "scope", nil)
		}
	}
	if err := a.scope(ctx, ref.TenantID); err != nil {
		return err
	}
	for {
		n, err := a.Ingestion.Fold(ctx, ref, progressFoldLimit)
		if errors.Is(err, ingestion.ErrNotFound) || errors.Is(err, ingestion.ErrInvalid) {
			return temporal.NewNonRetryableApplicationError("source unavailable", "scope", nil)
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
}

func progressWorkflowID(ref ingestion.SourceRef) string {
	return "operational-progress:" + ref.TenantID + ":" + ref.SiteID + ":" + ref.SourceID
}

// RelayOperational signals the per-source progress workflow for every source
// whose accepted cursor is ahead of its processed cursor. It marks nothing:
// lag is recomputed from durable cursors each tick, so a lost signal is
// simply re-sent and duplicates are drained in the workflow.
func RelayOperational(ctx context.Context, db *store.Store, c client.Client, cfg config.Config, tenants []string) (int, error) {
	service := ingestion.New(db.DB)
	n := 0
	for _, tenant := range tenants {
		refs, err := service.Lagging(ctx, tenant)
		if err != nil {
			return n, err
		}
		for _, ref := range refs {
			if _, err = c.SignalWithStartWorkflow(ctx, progressWorkflowID(ref), progressSignal, nil, client.StartWorkflowOptions{ID: progressWorkflowID(ref), TaskQueue: cfg.TemporalQueue}, SourceProgress, ref); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}
