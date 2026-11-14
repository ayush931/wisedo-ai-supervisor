// Supervisor runs an independent API, durable worker, or explicit schema migration.
package main

import (
	"log/slog"

	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/agent"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/config"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/dispatch"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/httpapi"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/identity"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/migrate"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/onboarding"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/ratelimit"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/retrieval"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/store"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	// The PDF child (WIS-624) runs before any configuration: it gets a cleared environment.
	onboarding.RunPDFChildIfRequested()
	if err := run(); err != nil {
		if len(os.Args) > 1 && os.Args[1] == "migrate" {
			log.Printf("supervisor migration failed: %s", safeMigrationError(err))
		} else if len(os.Args) > 1 && os.Args[1] == "provision" {
			log.Printf("supervisor provision failed: %s", safeProvisionError(err))
		} else {
			log.Print("supervisor terminated: required dependency or configuration failed")
		}
		os.Exit(1)
	}
}

// safeMigrationError provides migration diagnostics without logging a DSN or
// arbitrary driver text. SQL messages here originate only from fixed schema DDL.
func safeMigrationError(err error) string {
	var dsnErr *config.AdminDSNError
	if errors.As(err, &dsnErr) {
		return dsnErr.Error()
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fmt.Sprintf("PostgreSQL SQLSTATE %s: %s", pgErr.Code, pgErr.Message)
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return fmt.Sprintf("database network failure: %s %s", netErr.Op, netErr.Net)
	}
	return fmt.Sprintf("migration failure (%T)", err)
}
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	mode := "api"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	if mode == "migrate" {
		dsn := os.Getenv("SUPERVISOR_MIGRATION_URL")
		if err := config.ValidateAdminDSN("SUPERVISOR_MIGRATION_URL", dsn); err != nil {
			return err
		}
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return err
		}
		defer db.Close()
		return migrate.Apply(ctx, db)
	}
	if mode == "bootstrap-invite" {
		return bootstrapInvite(ctx)
	}
	if mode == "provision" {
		if len(os.Args) > 2 && os.Args[2] == "deactivate" {
			return provisionDeactivate(ctx, os.Args[3:])
		}
		return provision(ctx, os.Args[2:])
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := store.New(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	if err = db.VerifyRole(ctx); err != nil {
		return err
	}
	// WIS-623: the API (which creates interview and chat runs) and the worker install the same
	// hosted route; a configured route that cannot be built (no price, no key) stops the boot.
	if cfg.Agent.Hosted.Provider != "" {
		route, err := agent.NewHostedRoute(cfg.Agent.Hosted, cfg.Agent.Timeout, slog.Default(), time.Now)
		if err != nil {
			return err
		}
		agent.SetHostedRoute(route)
	}
	// WIS-627: semantic retrieval; the API ranks questions with it, the worker embeds spans.
	if cfg.Embeddings.Enabled {
		c, err := retrieval.New(cfg.Embeddings)
		if err != nil {
			return err
		}
		retrieval.SetDefault(c)
	}
	switch mode {
	case "api":
		auth, err := identity.New(ctx, identity.Config{Issuer: cfg.Issuer, ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURI: cfg.Origin + "/auth/callback", SecureCookies: cfg.SecureCookies(), Sessions: cfg.Sessions, Limit: authRouteLimit(cfg)}, db.DB, &http.Client{Timeout: 10 * time.Second})
		if err != nil {
			return err
		}
		handler, drain := httpapi.NewWithShutdown(cfg, db, auth)
		srv := &http.Server{Addr: cfg.Address, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			<-ctx.Done()
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdown)
			// Then give accepted OEE runs their own budget to finish. Drain cancels any still
			// running when it ends and waits briefly while they record a terminal outcome, so
			// the database (closed when run returns) outlives every execution.
			draining, stopDraining := context.WithTimeout(context.Background(), 10*time.Second)
			defer stopDraining()
			if err := drain(draining); err != nil {
				log.Print("supervisor API: OEE executions were cancelled at shutdown")
			}
		}()
		log.Print("supervisor API listening")
		err = srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			// ListenAndServe returns as soon as Shutdown starts; wait for the drain to end.
			<-stopped
			return nil
		}
		return err
	case "worker":
		c, err := dispatch.Connect(cfg)
		if err != nil {
			return err
		}
		defer c.Close()
		w, err := dispatch.NewWorker(c, db, cfg)
		if err != nil {
			return err
		}
		if err = w.Start(); err != nil {
			return err
		}
		defer w.Stop()
		// WIS-627: span indexing waits on a possibly slow (CPU) embedding endpoint, so it runs on
		// its own loop, one pass at a time; the dispatch tick (approvals, relay) never waits for it.
		indexed := make(chan struct{})
		go func() {
			defer close(indexed)
			every(ctx, time.Second, func(ctx context.Context) {
				tenants, err := dispatch.Tenants(ctx, db, cfg.Tenants)
				if err != nil {
					return // the dispatch tick logs tenant discovery failures
				}
				if _, err := dispatch.EmbedSpans(ctx, db, tenants); err != nil {
					log.Print("span indexing incomplete; retried next pass")
				}
			})
		}()
		defer func() { <-indexed }()
		// WIS-718: case evaluation costs what each tenant's contexts cost, so it also runs on its
		// own loop; one tenant's slow evaluation never delays another tenant's approved action.
		evaluated := make(chan struct{})
		go func() {
			defer close(evaluated)
			evaluator := dispatch.NewEvaluator(db)
			every(ctx, time.Second, func(ctx context.Context) {
				if _, err := evaluator.Pass(ctx, cfg.Tenants); err != nil {
					log.Print("case evaluation incomplete; retried next pass")
				}
			})
		}()
		defer func() { <-evaluated }()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				if _, err = dispatch.Tick(ctx, db, c, cfg, w.AgentEnabled); err != nil {
					log.Print("worker tick incomplete; durable work retained for retry")
				}
			}
		}
	default:
		return errors.New("unknown mode")
	}
}

// every calls fn once per interval until ctx ends, one call at a time: a slow call delays the
// next one instead of overlapping it.
func every(ctx context.Context, interval time.Duration, fn func(context.Context)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// A tick and a cancel can be ready together, and select picks either at random, so
			// a stopped worker could start more passes; check again before each one.
			if !runPass(ctx, fn) {
				return
			}
		}
	}
}

// runPass runs one pass unless the worker was already stopped, and reports whether it ran.
func runPass(ctx context.Context, fn func(context.Context)) bool {
	if ctx.Err() != nil {
		return false
	}
	fn(ctx)
	return true
}

// authRouteLimit limits the unauthenticated /auth/login and /auth/callback routes per client
// (WIS-699): a burst of 60 requests, refilled at one a second, for at most 10 000 clients per
// process. A full browser e2e run signs in about 50 times, twice per sign-in, well inside it.
func authRouteLimit(cfg config.Config) func(http.Handler) http.Handler {
	limiter := ratelimit.New(1, 60, 10000, time.Now)
	if cfg.Environment == "production" && len(cfg.TrustedProxies) == 0 {
		// Behind a proxy every user would share the proxy's one bucket, so one client could deny
		// sign-in to all. Direct exposure is fine; a proxy needs SUPERVISOR_TRUSTED_PROXY_CIDRS.
		slog.Warn("auth route rate limit keys the connection address: set SUPERVISOR_TRUSTED_PROXY_CIDRS when a proxy terminates client connections, and never include client ranges")
	}
	key := ratelimit.ClientKey(cfg.TrustedProxies)
	return func(next http.Handler) http.Handler { return limiter.Wrap(key, next) }
}
