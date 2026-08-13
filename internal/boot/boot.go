// Package boot wires configuration, storage and the HTTP listeners into a
// running control plane, and owns the startup/shutdown ordering (07 §9):
// readiness flips only after storage is open and migrated; shutdown drains
// HTTP within the configured grace before closing the store.
package boot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"

	"github.com/samimishal/fleetplane/internal/config"
	"github.com/samimishal/fleetplane/internal/storage/sqlite"
)

type App struct {
	cfg   *config.Config
	log   *slog.Logger
	db    *sqlite.DB
	ready atomic.Bool

	mainLn net.Listener
	opsLn  net.Listener
}

// New loads storage (including migrations). Readiness stays false until
// Serve has verified the store.
func New(ctx context.Context, cfg *config.Config, log *slog.Logger) (*App, error) {
	db, err := sqlite.Open(ctx, cfg.Storage.Path)
	if err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}
	return &App{cfg: cfg, log: log, db: db}, nil
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
	opsMux := http.NewServeMux()
	opsMux.Handle("/health/", health)

	mainSrv := &http.Server{Handler: mainMux, BaseContext: func(net.Listener) context.Context { return ctx }}
	opsSrv := &http.Server{Handler: opsMux, BaseContext: func(net.Listener) context.Context { return ctx }}

	errc := make(chan error, 2)
	go func() { errc <- mainSrv.Serve(a.mainLn) }()
	go func() { errc <- opsSrv.Serve(a.opsLn) }()

	if err := a.db.Ping(ctx); err != nil {
		a.log.Error("storage unavailable at boot", "error", err)
	} else {
		a.ready.Store(true)
		a.log.Info("fleetplane ready",
			"addr", a.mainLn.Addr().String(), "opsAddr", a.opsLn.Addr().String(),
			"db", a.cfg.Storage.Path)
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
	if err := a.db.Close(); err != nil {
		a.log.Error("closing storage", "error", err)
		return err
	}
	return nil
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
