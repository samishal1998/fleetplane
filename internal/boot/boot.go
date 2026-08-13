// Package boot wires configuration, storage and the HTTP listeners into a
// running control plane, and owns the startup/shutdown ordering (07 §9):
// readiness flips only after storage is open and migrated; shutdown drains
// HTTP within the configured grace before closing the store.
package boot

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/samimishal/fleetplane/internal/api"
	"github.com/samimishal/fleetplane/internal/app"
	"github.com/samimishal/fleetplane/internal/config"
	"github.com/samimishal/fleetplane/internal/lease"
	"github.com/samimishal/fleetplane/internal/operations"
	"github.com/samimishal/fleetplane/internal/reconcile"
	"github.com/samimishal/fleetplane/internal/scheduler"
	"github.com/samimishal/fleetplane/internal/storage"
	"github.com/samimishal/fleetplane/internal/storage/sqlite"
	"github.com/samimishal/fleetplane/pkg/kinds/compute"
	"github.com/samimishal/fleetplane/pkg/sdk"
	"github.com/samimishal/fleetplane/pkg/sdk/secretref"
)

type App struct {
	cfg   *config.Config
	log   *slog.Logger
	db    storage.Store
	ready atomic.Bool

	providers  *app.Providers
	engine     *operations.Engine
	reconciler *reconcile.Reconciler
	sched      *scheduler.Scheduler
	leases     *lease.Manager
	health     *app.HealthTracker
	service    *app.Service
	api        *api.Server

	mainLn net.Listener
	opsLn  net.Listener
}

// New wires storage (including migrations), provider instances, the
// operation engine and the app service. Readiness stays false until Serve
// has verified the store AND journal recovery completed (plan R11).
func New(ctx context.Context, cfg *config.Config, log *slog.Logger) (*App, error) {
	db, err := sqlite.OpenStore(ctx, cfg.Storage.Path)
	if err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}
	clock := sdk.Real{}
	ownerID, err := app.EnsureOwnerID(ctx, db, clock.Now().UnixMilli())
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("instance identity: %w", err)
	}

	var specs []app.ProviderSpec
	for name, p := range cfg.Providers {
		settings, err := json.Marshal(p.Settings)
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("providers.%s: %w", name, err)
		}
		specs = append(specs, app.ProviderSpec{Name: name, Driver: p.Driver, Settings: settings})
	}
	providers, err := app.BuildProviders(ctx, db, specs, ownerID, secretref.NewDefault(), log, clock.Now().UnixMilli())
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	engine := operations.New(db, providers, clock, log, operations.Config{
		PollInterval: cfg.Engine.PollInterval.Std(),
		VerifyWindow: cfg.Engine.VerifyWindow.Std(),
	}, operations.Hooks{})

	classes := app.Classes{}
	for name, cls := range cfg.Classes {
		specJSON, err := json.Marshal(cls.Spec)
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("classes.%s: %w", name, err)
		}
		rc := reconcile.Class{Kind: cls.Kind, Provider: cls.Provider, Spec: specJSON}
		if cls.Reclaim != nil {
			rc.Reclaim = &reconcile.ReclaimPolicy{IdleAfter: compute.Duration(cls.Reclaim.IdleAfter.Std())}
		}
		classes[name] = rc
	}
	reconciler := reconcile.New(db, providers, engine, classes, clock, log, ownerID, reconcile.Config{
		Interval:             cfg.Reconcile.Interval.Std(),
		MaxMutationsPerCycle: cfg.Reconcile.MaxMutationsPerCycle,
	})
	sched := scheduler.New(db, providers, engine, classes, clock, log, ownerID)
	leases := lease.New(db, clock, log, reconciler)
	engine.OnTerminal = func(op *storage.Operation) {
		reconciler.HandleOpTerminal(op)
		sched.HandleOpTerminal(op)
	}

	service := app.NewService(db, providers, engine, clock, log, ownerID)
	service.AttachScheduling(sched, leases)
	service.AttachReconciler(reconciler)

	auth, err := buildAuth(cfg)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if !auth.Enabled() {
		log.Warn("NO API TOKENS CONFIGURED — the API is open; use auth.tokens in production (07 §2)")
	}
	health := app.NewHealthTracker(providers, clock, app.HealthConfig{})

	a := &App{
		cfg: cfg, log: log.With("owner_id", ownerID), db: db,
		providers: providers, engine: engine, reconciler: reconciler,
		sched: sched, leases: leases, service: service, health: health,
		api: api.New(service, auth, health, log),
	}
	return a, nil
}

func buildAuth(cfg *config.Config) (*api.TokenAuthenticator, error) {
	var records []api.TokenRecord
	for i, tok := range cfg.Auth.Tokens {
		perms, err := api.NewPermSet(tok.Permissions)
		if err != nil {
			return nil, fmt.Errorf("auth.tokens[%d]: %w", i, err)
		}
		digest, err := hex.DecodeString(tok.SHA256)
		if err != nil || len(digest) != 32 {
			return nil, fmt.Errorf("auth.tokens[%d].sha256: not 32 hex-encoded bytes", i)
		}
		rec := api.TokenRecord{ID: tok.ID, Name: tok.Name, Perms: perms}
		copy(rec.SHA256[:], digest)
		records = append(records, rec)
	}
	return api.NewTokenAuthenticator(records), nil
}

// Listen binds the main and ops listeners; addresses are readable via
// MainAddr/OpsAddr afterwards (tests bind :0).
func (a *App) Listen() error {
	ln, err := net.Listen("tcp", a.cfg.Server.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", a.cfg.Server.Addr, err)
	}
	a.mainLn = ln
	ops, err := net.Listen("tcp", a.cfg.Server.OpsAddr)
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("listen %s: %w", a.cfg.Server.OpsAddr, err)
	}
	a.opsLn = ops
	return nil
}

func (a *App) MainAddr() net.Addr { return a.mainLn.Addr() }
func (a *App) OpsAddr() net.Addr  { return a.opsLn.Addr() }

// Serve runs until ctx is cancelled, then shuts down gracefully.
func (a *App) Serve(ctx context.Context) error {
	if a.mainLn == nil || a.opsLn == nil {
		return errors.New("boot: Serve called before Listen")
	}

	health := a.healthHandler()
	mainMux := http.NewServeMux()
	mainMux.Handle("/health/", health)
	mainMux.Handle("/v1/", a.mutationGate(a.api.Handler()))
	opsMux := http.NewServeMux()
	opsMux.Handle("/health/", health)

	mainSrv := &http.Server{Handler: mainMux, BaseContext: func(net.Listener) context.Context { return ctx }}
	opsSrv := &http.Server{Handler: opsMux, BaseContext: func(net.Listener) context.Context { return ctx }}

	errc := make(chan error, 2)
	go func() { errc <- mainSrv.Serve(a.mainLn) }()
	go func() { errc <- opsSrv.Serve(a.opsLn) }()

	engineCtx, stopEngine := context.WithCancel(ctx)
	defer stopEngine()
	go a.engine.Run(engineCtx)
	go a.reconciler.Run(engineCtx)
	go a.sweepLoop(engineCtx)
	go a.health.Run(engineCtx)

	// Readiness = storage ping ∧ journal recovery complete (plan R11).
	// Provider health never affects readiness (07 §8).
	if err := a.db.Ping(ctx); err != nil {
		a.log.Error("storage unavailable at boot", "error", err)
	} else if err := a.engine.Resume(ctx); err != nil {
		a.log.Error("journal recovery failed; mutations stay gated", "error", err)
	} else {
		// Acquisition crash-resume (plan R6) runs with journal recovery.
		if err := a.sched.Resume(ctx, a.pendingTimeoutMillis()); err != nil {
			a.log.Error("acquisition resume", "error", err)
		}
		a.reconciler.KickAll()
		a.ready.Store(true)
		a.log.Info("fleetplane ready",
			"addr", a.mainLn.Addr().String(), "opsAddr", a.opsLn.Addr().String(),
			"db", a.cfg.Storage.Path, "providers", a.providers.Names())
	}

	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = a.close()
			return fmt.Errorf("http server: %w", err)
		}
	}

	// Graceful shutdown (07 §9): stop accepting, drain within grace, then
	// close storage. Provider-side operations are never assumed stopped —
	// the journal resumes them on next boot.
	a.ready.Store(false)
	grace := a.cfg.Server.ShutdownGrace.Std()
	shCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	a.log.Info("shutting down", "grace", grace.String())
	_ = mainSrv.Shutdown(shCtx)
	_ = opsSrv.Shutdown(shCtx)
	return a.close()
}

func (a *App) close() error {
	if a.providers != nil {
		a.providers.Close()
	}
	if err := a.db.Close(); err != nil {
		a.log.Error("closing storage", "error", err)
		return err
	}
	return nil
}

// mutationGate 503s mutating requests until recovery completes and during
// shutdown drain (plan R11; 07 §9). Reads always pass.
func (a *App) mutationGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.ready.Load() && r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Retry-After", "2")
			writeJSON(w, http.StatusServiceUnavailable,
				map[string]any{"error": map[string]any{"code": "unready", "message": "recovery in progress or shutting down", "retryable": true}})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Service exposes the app facade (used by tests and later the CLI serve path).
func (a *App) Service() *app.Service { return a.service }

func (a *App) pendingTimeoutMillis() int64 {
	t := a.cfg.Acquire.PendingTimeout.Std()
	if t <= 0 {
		t = 15 * time.Minute
	}
	return t.Milliseconds()
}

// sweepLoop runs the periodic level triggers that have no push signal:
// lease TTL expiry and acquisition resume/expiry (plan R6).
func (a *App) sweepLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
		if !a.ready.Load() {
			continue
		}
		if _, err := a.leases.SweepExpired(ctx); err != nil && ctx.Err() == nil {
			a.log.Error("lease sweep", "error", err)
		}
		if err := a.sched.Resume(ctx, a.pendingTimeoutMillis()); err != nil && ctx.Err() == nil {
			a.log.Error("acquisition sweep", "error", err)
		}
	}
}

func (a *App) healthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		// Readiness = storage reachable (+ journal recovery once the
		// operation engine exists — R11). Provider health never affects
		// readiness (07 §8).
		if !a.ready.Load() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unready"})
			return
		}
		if err := a.db.Ping(r.Context()); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unready", "reason": "storage"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Run is the `fleetplane serve` entry point.
func Run(ctx context.Context, cfgPath string, log *slog.Logger) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	app, err := New(ctx, cfg, log)
	if err != nil {
		return err
	}
	if err := app.Listen(); err != nil {
		_ = app.db.Close()
		return err
	}
	return app.Serve(ctx)
}
