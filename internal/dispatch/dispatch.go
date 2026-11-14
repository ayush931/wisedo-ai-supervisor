// Package dispatch delivers committed receipts to durable Temporal workflows.
package dispatch

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/approvals"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/cases"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ingestion"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/google/uuid"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

type Reference struct{ Tenant, Command string }

func Probe(ctx workflow.Context, ref Reference) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 30 * time.Second, RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, MaximumInterval: time.Minute}})
	var status string
	err := workflow.ExecuteActivity(ctx, "ProcessFoundationProbe", ref).Get(ctx, &status)
	return status, err
}

type Activities struct {
	Store     *store.Store
	Ingestion *ingestion.Service
	Cases     *cases.Service
	Allow     map[string]bool
	// Agent is the WIS-275 runtime; nil when SUPERVISOR_AGENT_PROVIDER is unset.
	Agent *agent.Runtime
	// Vision is the OCR runtime (WIS-625); nil without a configured vision model.
	Vision             *agent.Runtime
	draftConfiguration func(context.Context, agent.ConfigurationDraftRef) (agent.ConfigurationDraftResult, error)
	chatAnswer         func(context.Context, agent.ChatAnswerRef) (agent.ChatAnswerResult, error)
}

// InvestigateRef names one case generation for the InvestigateCase activity.
type InvestigateRef struct {
	Tenant, Site, Case string
	Revision           int64
}

// InvestigateCase runs or resumes one bounded investigation and records its outcome. A terminal
// run replays its stored result, so a Temporal retry never repeats provider calls or authorizes anything.
func (a *Activities) InvestigateCase(ctx context.Context, ref InvestigateRef) (agent.Result, error) {
	if a.Agent == nil {
		return agent.Result{}, temporal.NewNonRetryableApplicationError("agent runtime disabled", "disabled", nil)
	}
	if err := a.scope(ctx, ref.Tenant); err != nil {
		return agent.Result{}, err
	}
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				activity.RecordHeartbeat(heartbeatCtx)
			}
		}
	}()
	defer func() { stopHeartbeat(); <-done }()
	res, err := a.Agent.Investigate(ctx, agent.InvestigateRef{TenantID: ref.Tenant, SiteID: ref.Site, CaseID: ref.Case}, ref.Revision)
	switch {
	case errors.Is(err, agent.ErrInvalidRequest), errors.Is(err, agent.ErrCaseNotFound):
		return agent.Result{}, temporal.NewNonRetryableApplicationError("investigation unavailable", "scope", nil)
	case errors.Is(err, agent.ErrBudgetExhausted), errors.Is(err, agent.ErrContextOverflow):
		return agent.Result{}, temporal.NewNonRetryableApplicationError("investigation budget exhausted", "budget", nil)
	case errors.Is(err, agent.ErrRunCanceled):
		return agent.Result{}, temporal.NewNonRetryableApplicationError("investigation canceled", "canceled", nil)
	case errors.Is(err, agent.ErrQueueFull):
		return agent.Result{}, temporal.NewApplicationError("agent queue full", "queue_full")
	case errors.Is(err, agent.ErrSiteBusy), errors.Is(err, agent.ErrWorkerBusy):
		// Not retried here: the case workflow waits on a durable timer and tries again (WIS-273).
		return agent.Result{}, temporal.NewNonRetryableApplicationError("investigation deferred", admissionBusy, nil)
	}
	var toolErr *agent.ToolError
	if errors.As(err, &toolErr) && toolErr.Fatal {
		return agent.Result{}, temporal.NewNonRetryableApplicationError("agent tool rejected", "tool_rejected", nil)
	}
	var providerErr *agent.ProviderError
	if errors.As(err, &providerErr) && providerErr.Code == agent.CodeModelNotPinned {
		return agent.Result{}, temporal.NewNonRetryableApplicationError("agent model not pinned", "model_not_pinned", nil)
	}
	return res, err
}

// agentRuntime builds the WIS-275 runtime from configuration, or nil when it is disabled (an empty
// provider is the rollback). An unset fixture script retains the empty fixture behavior.
func agentRuntime(cfg config.Config, db *store.Store) (*agent.Runtime, error) {
	a := cfg.Agent
	var p agent.Provider
	switch a.Provider {
	case "":
		return nil, nil
	case config.AgentProviderFixture:
		if a.FixtureScript == "" {
			p = agent.NewFixtureProvider()
		} else {
			var err error
			p, err = agent.LoadFixtureProvider(a.FixtureScript)
			if err != nil {
				return nil, err
			}
		}
	case config.AgentProviderLocal:
		m, err := agent.NewLocalModel(a, slog.Default(), time.Now)
		if err != nil {
			return nil, err
		}
		probeCtx, cancel := context.WithTimeout(context.Background(), a.Timeout)
		_ = m.Probe(probeCtx) // An unavailable capacity probe keeps the default profile.
		cancel()
		p = m
	default:
		return nil, errors.New("invalid agent provider")
	}
	limits := agent.DefaultLimits()
	limits.CallTimeout, limits.MaxConcurrent, limits.MaxQueued = a.Timeout, a.MaxConcurrent, a.MaxQueued
	s := agent.NewStore(db.DB)
	return &agent.Runtime{Store: s, Provider: p, Tools: &agent.Tools{Store: s}, Slots: agent.NewSlots(a.MaxConcurrent, a.MaxQueued), Limits: limits,
		Approvals: approvals.NewService(db.DB), Cases: cases.NewService(db.DB), Model: a.Model, ModelRevision: a.ModelRevision,
		Options: agent.LoaderOptions{ModeEnabled: a.ModeEnabled}, Logger: slog.Default()}, nil
}

func (a *Activities) scope(ctx context.Context, tenant string) error {
	id, err := uuid.Parse(tenant)
	if err != nil {
		return temporal.NewNonRetryableApplicationError("invalid scope", "scope", nil)
	}
	tenant = id.String()
	if len(a.Allow) > 0 && !a.Allow[tenant] {
		return temporal.NewNonRetryableApplicationError("unprovisioned scope", "scope", nil)
	}
	active, err := a.Store.TenantActive(ctx, tenant)
	if err != nil {
		return err
	}
	if !active {
		return temporal.NewNonRetryableApplicationError("unprovisioned scope", "scope", nil)
	}
	return nil
}

func (a *Activities) Process(ctx context.Context, ref Reference) (string, error) {
	if _, e := uuid.Parse(ref.Tenant); e != nil {
		return "", temporal.NewNonRetryableApplicationError("invalid scope", "scope", nil)
	}
	if _, e := uuid.Parse(ref.Command); e != nil {
		return "", temporal.NewNonRetryableApplicationError("invalid reference", "scope", nil)
	}
	if err := a.scope(ctx, ref.Tenant); err != nil {
		return "", err
	}
	status, err := a.Store.ProcessProbe(ctx, ref.Tenant, ref.Command)
	if errors.Is(err, store.ErrDenied) || errors.Is(err, store.ErrNotFound) {
		return "", temporal.NewNonRetryableApplicationError("command unavailable", "scope", nil)
	}
	return status, err
}
func Connect(c config.Config) (client.Client, error) {
	options := client.Options{HostPort: c.TemporalAddress, Namespace: c.TemporalNamespace}
	if c.TemporalTLS {
		options.ConnectionOptions.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	if c.TemporalAPIKey != "" {
		options.Credentials = client.NewAPIKeyStaticCredentials(c.TemporalAPIKey)
	}
	return client.Dial(options)
}
func Relay(ctx context.Context, db *store.Store, c client.Client, cfg config.Config, tenants []string) (int, error) {
	n := 0
	for _, tenant := range tenants {
		events, err := db.Pending(ctx, tenant)
		if err != nil {
			return n, err
		}
		for _, e := range events {
			_, err = c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "foundation-go:" + tenant + ":" + e.CommandID, TaskQueue: cfg.TemporalQueue, WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE, WorkflowExecutionErrorWhenAlreadyStarted: true}, Probe, Reference{tenant, e.CommandID})
			var duplicate *serviceerror.WorkflowExecutionAlreadyStarted
			if err != nil && !errors.As(err, &duplicate) {
				return n, err
			}
			if err = db.MarkDelivered(ctx, tenant, e.ID); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

// workerStopTimeout lets an in-flight model call (agent CallTimeout, 30 s) finish on a normal stop
// before its activity is canceled; a canceled investigation resumes on retry either way. The
// container stop grace period must exceed it.
const workerStopTimeout = 30 * time.Second

// Worker is the Temporal worker plus whether its built agent runtime investigates cases; the
// tick's detected-case sweep uses the same answer as the case workflow (WIS-636).
type Worker struct {
	worker.Worker
	AgentEnabled bool
}

func NewWorker(c client.Client, db *store.Store, cfg config.Config) (*Worker, error) {
	allow := map[string]bool{}
	for _, t := range cfg.Tenants {
		if id, err := uuid.Parse(t); err == nil {
			allow[id.String()] = true
		}
	}
	a := &Activities{Store: db, Ingestion: ingestion.New(db.DB), Cases: cases.NewService(db.DB), Allow: allow}
	w := worker.New(c, cfg.TemporalQueue, worker.Options{WorkerStopTimeout: workerStopTimeout})
	w.RegisterWorkflow(Probe)
	w.RegisterWorkflow(SourceProgress)
	w.RegisterWorkflow(SupervisorCaseWorkflow)
	w.RegisterWorkflow(InterviewTurnWorkflow)
	w.RegisterWorkflow(ConfigurationDraftWorkflow)
	w.RegisterWorkflow(ChatAnswerWorkflow)
	w.RegisterWorkflow(ScanWorkflow)
	w.RegisterActivityWithOptions(a.Process, activity.RegisterOptions{Name: "ProcessFoundationProbe"})
	w.RegisterActivityWithOptions(a.FoldSource, activity.RegisterOptions{Name: "FoldOperationalSource"})
	w.RegisterActivityWithOptions(a.ReadCaseStatus, activity.RegisterOptions{Name: "ReadCaseStatus"})
	w.RegisterActivityWithOptions(a.RecordCaseEvent, activity.RegisterOptions{Name: "RecordCaseEvent"})
	w.RegisterActivityWithOptions(a.EscalateCaseOnDeadline, activity.RegisterOptions{Name: "EscalateCaseOnDeadline"})
	w.RegisterActivityWithOptions(a.ReconcileCaseEvents, activity.RegisterOptions{Name: "ReconcileCaseEvents"})
	w.RegisterActivityWithOptions(a.FinalizeInterviewTurnFailure, activity.RegisterOptions{Name: "FinalizeInterviewTurnFailure"})
	w.RegisterActivityWithOptions(a.FinalizeConfigurationDraftFailure, activity.RegisterOptions{Name: "FinalizeConfigurationDraftFailure"})
	w.RegisterActivityWithOptions(a.FinalizeChatAnswerFailure, activity.RegisterOptions{Name: "FinalizeChatAnswerFailure"})
	w.RegisterActivityWithOptions(a.FinalizeScanFailure, activity.RegisterOptions{Name: "FinalizeScanFailure"})
	// A vision model that cannot be built leaves scans unread: their jobs time out and are
	// quarantined, never guessed (fail closed).
	if vision, err := visionRuntime(cfg, db); err != nil {
		log.Print("vision model unavailable; RunScan not backed by a model")
	} else {
		a.Vision = vision
	}
	w.RegisterActivityWithOptions(a.RunScan, activity.RegisterOptions{Name: "RunScan"})
	registered, err := registerAgent(w, a, cfg, db)
	if err != nil {
		return nil, err
	}
	if !registered {
		w.RegisterActivityWithOptions(a.RunInterviewTurn, activity.RegisterOptions{Name: "RunInterviewTurn"})
		w.RegisterActivityWithOptions(a.RunConfigurationDraft, activity.RegisterOptions{Name: "RunConfigurationDraft"})
		w.RegisterActivityWithOptions(a.RunChatAnswer, activity.RegisterOptions{Name: "RunChatAnswer"})
	}
	return &Worker{Worker: w, AgentEnabled: a.agentEnabled()}, nil
}

// registerAgent registers InvestigateCase only when the agent runtime is configured. A runtime
// that cannot be built leaves it unregistered (fail closed) and says so. A configured fixture
// script that does not load is an operator error, so the worker refuses to start instead.
func registerAgent(w worker.ActivityRegistry, a *Activities, cfg config.Config, db *store.Store) (bool, error) {
	rt, err := agentRuntime(cfg, db)
	if err != nil && cfg.Agent.FixtureScript != "" {
		return false, fmt.Errorf("agent runtime: %w", err)
	}
	if err != nil {
		log.Print("agent runtime unavailable; InvestigateCase not registered")
		return false, nil
	}
	if rt == nil {
		return false, nil
	}
	a.Agent = rt
	w.RegisterActivityWithOptions(a.InvestigateCase, activity.RegisterOptions{Name: "InvestigateCase"})
	w.RegisterActivityWithOptions(a.RunInterviewTurn, activity.RegisterOptions{Name: "RunInterviewTurn"})
	w.RegisterActivityWithOptions(a.RunConfigurationDraft, activity.RegisterOptions{Name: "RunConfigurationDraft"})
	w.RegisterActivityWithOptions(a.RunChatAnswer, activity.RegisterOptions{Name: "RunChatAnswer"})
	return true, nil
}

func filterTenants(active, allow []string) []string {
	if len(allow) == 0 {
		return active
	}
	allowed := make(map[string]bool, len(allow))
	for _, tenant := range allow {
		if id, err := uuid.Parse(tenant); err == nil {
			allowed[id.String()] = true
		}
	}
	var selected []string
	for _, tenant := range active {
		if allowed[tenant] {
			selected = append(selected, tenant)
		}
	}
	return selected
}

// Tenants resolves active worker scopes on every tick; the environment list only narrows them.
func Tenants(ctx context.Context, db *store.Store, allow []string) ([]string, error) {
	active, err := db.ActiveTenants(ctx)
	if err != nil {
		return nil, err
	}
	return filterTenants(active, allow), nil
}

var tenantSetLog = struct {
	sync.Mutex
	active, inactive []string
}{}

func logTenantSet(active, allow []string) {
	missing := filterInactive(active, allow)
	tenantSetLog.Lock()
	defer tenantSetLog.Unlock()
	if !slices.Equal(tenantSetLog.active, active) {
		added, removed := tenantDifference(active, tenantSetLog.active), tenantDifference(tenantSetLog.active, active)
		log.Printf("tenant set changed: added=%v removed=%v", added, removed)
		tenantSetLog.active = slices.Clone(active)
	}
	if !slices.Equal(tenantSetLog.inactive, missing) {
		if len(missing) > 0 {
			log.Printf("allowlisted tenants not active: %v", missing)
		}
		tenantSetLog.inactive = slices.Clone(missing)
	}
}

func filterInactive(active, allow []string) []string {
	set := make(map[string]bool, len(active))
	for _, tenant := range active {
		set[tenant] = true
	}
	var missing []string
	for _, tenant := range allow {
		if id, err := uuid.Parse(tenant); err == nil && !set[id.String()] {
			missing = append(missing, id.String())
		}
	}
	sort.Strings(missing)
	return slices.Compact(missing)
}

func tenantDifference(left, right []string) []string {
	set := make(map[string]bool, len(right))
	for _, tenant := range right {
		set[tenant] = true
	}
	var difference []string
	for _, tenant := range left {
		if !set[tenant] {
			difference = append(difference, tenant)
		}
	}
	return difference
}

// Tick resolves tenants once and skips all work if discovery fails.
// agentEnabled is whether the built runtime investigates cases (WIS-636).
func Tick(ctx context.Context, db *store.Store, c client.Client, cfg config.Config, agentEnabled bool) (int, error) {
	tenants, err := Tenants(ctx, db, cfg.Tenants)
	if err != nil {
		log.Print("tenant discovery unavailable; nothing relayed this tick")
		return 0, err
	}
	logTenantSet(tenants, cfg.Tenants)
	var delivered int
	var failures []error
	if n, e := Relay(ctx, db, c, cfg, tenants); e != nil {
		delivered += n
		failures = append(failures, e)
	} else {
		delivered += n
	}
	if n, e := RelayOperational(ctx, db, c, cfg, tenants); e != nil {
		delivered += n
		failures = append(failures, e)
	} else {
		delivered += n
	}
	// Case evaluation runs on its own loop (Evaluator), never here: its cost depends on each
	// tenant's contexts, and an approved action must not wait for it (WIS-718).
	// Before the relay, so a case that starts investigating is signalled in the same tick (WIS-636).
	if n, e := StartDetectedCases(ctx, db, agentEnabled, tenants); e != nil {
		delivered += n
		failures = append(failures, e)
	} else {
		delivered += n
	}
	// Before the case relay, so a committed execution's signal is delivered in the same tick.
	if n, e := ExecuteApproved(ctx, db, cfg, tenants); e != nil {
		delivered += n
		failures = append(failures, e)
	} else {
		delivered += n
	}
	if n, e := ObserveOutcomes(ctx, db, tenants); e != nil {
		delivered += n
		failures = append(failures, e)
	} else {
		delivered += n
	}
	if n, e := RelayCases(ctx, db, c, cfg, tenants); e != nil {
		delivered += n
		failures = append(failures, e)
	} else {
		delivered += n
	}
	if n, e := RelayInterviewTurns(ctx, db.DB, c, cfg, tenants); e != nil {
		delivered += n
		failures = append(failures, e)
	} else {
		delivered += n
	}
	if n, e := RelayConfigurationDrafts(ctx, db.DB, c, cfg, tenants); e != nil {
		delivered += n
		failures = append(failures, e)
	} else {
		delivered += n
	}
	if n, e := RelayScans(ctx, db.DB, c, cfg, tenants); e != nil {
		delivered += n
		failures = append(failures, e)
	} else {
		delivered += n
	}
	if n, e := RelayChatAnswers(ctx, db.DB, c, cfg, tenants); e != nil {
		delivered += n
		failures = append(failures, e)
	} else {
		delivered += n
	}
	return delivered, errors.Join(failures...)
}
