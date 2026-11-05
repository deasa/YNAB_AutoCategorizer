package cmd

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/auth"
	"github.com/deasa/YNAB_AutoCategorizer/config"
	"github.com/deasa/YNAB_AutoCategorizer/mcp"
)

const (
	pruneEvery   = 24 * time.Hour
	pruneTimeout = time.Minute
)

// idpRetry is how long to wait before retrying Google's OIDC discovery.
var idpRetry = 30 * time.Second

// newIDP builds the upstream login (tests replace it, so they never reach Google).
var newIDP = func(ctx context.Context, m config.MCP) (auth.IdentityProvider, error) {
	return auth.NewGoogleProvider(ctx, m.GoogleClientID, m.GoogleClientSecret, m.PublicURL+"/oauth/google/callback")
}

// addPrune deletes expired OAuth rows, as a scheduler job, when MCP is on. A
// failure is logged and goes no further: it can't fail a sync.
func (a *app) addPrune(s *Scheduler) {
	if len(a.cfg.MCP.Missing()) > 0 {
		return
	}
	storage := auth.New(a.db)
	s.PruneEvery = pruneEvery
	s.Prune = func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, pruneTimeout)
		defer cancel()
		n, err := storage.Prune(ctx)
		if err != nil {
			a.log.Error("oauth prune failed", "err", a.redact(err.Error()))
			return err
		}
		a.log.Info("oauth prune", "removed", n)
		return nil
	}
}

// startMCP binds the MCP listener when MCP is configured; otherwise everything
// it returns but the closed channel is nil. Only the MCP routes exist on this
// port, because the tunnel forwards all of it to the public internet: /healthz
// is on its own listener.
//
// Google's discovery runs at startup and can fail for reasons that have nothing
// to do with finbot, and the mirror must keep syncing through that. So the routes
// answer 503 until it succeeds, retried in the background until ctx ends. done
// closes when that background work has stopped.
func (a *app) startMCP(ctx context.Context) (*http.Server, net.Listener, <-chan struct{}, error) {
	done := make(chan struct{})
	cfg := a.cfg.MCP
	if missing := cfg.Missing(); len(missing) > 0 {
		close(done)
		a.log.Info("mcp disabled", "missing", missing)
		return nil, nil, done, nil
	}
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return nil, nil, nil, err
	}
	routes := &swappable{}
	mux := http.NewServeMux()
	for _, p := range []string{mcp.Path, "/oauth/", "/.well-known/"} {
		mux.Handle(p, mcp.CORS(routes))
	}
	srv := &http.Server{
		Handler:           mcp.LogRequests(a.log, mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(a.log.Handler(), slog.LevelWarn),
	}
	go func() {
		defer close(done)
		h, err := a.buildMCP(ctx)
		if err != nil {
			// Not retried: the config or the database is wrong, not the network.
			// The routes stay 503; the sync jobs carry on.
			if ctx.Err() == nil {
				a.log.Error("mcp failed to start", "err", a.redact(err.Error()))
			}
			return
		}
		if h != nil {
			routes.h.Store(&h)
			a.log.Info("mcp serving", "addr", ln.Addr().String(), "allowed_users", len(auth.ParseAllowlist(cfg.AllowedEmails)))
		}
	}()
	return srv, ln, done, nil
}

// buildMCP returns the handler for the MCP routes, or nil if ctx ended first.
func (a *app) buildMCP(ctx context.Context) (http.Handler, error) {
	cfg := a.cfg.MCP
	var idp auth.IdentityProvider
	for {
		var err error
		if idp, err = newIDP(ctx, cfg); err == nil {
			break
		}
		a.log.Warn("mcp: google discovery failed; will retry", "err", a.redact(err.Error()))
		select {
		case <-ctx.Done():
			return nil, nil
		case <-time.After(idpRetry):
		}
	}
	as, err := auth.NewServer(ctx, auth.Config{
		Issuer:    cfg.PublicURL,
		MCPPath:   mcp.Path,
		Storage:   auth.New(a.db),
		IDP:       idp,
		Allowlist: auth.ParseAllowlist(cfg.AllowedEmails),
		Logger:    a.log,
	})
	if err != nil {
		return nil, err
	}
	endpoint := mcp.NewHandler(mcp.Options{
		Verify:              as.VerifyToken,
		ResourceMetadataURL: as.ResourceMetadataURL(),
		Version:             Version,
		DB:                  a.db,
		PlanID:              a.cfg.YNABBudgetID,
		StaleAfter:          2 * a.cfg.SyncInterval,
		Log:                 a.log,
		Redact:              a.redact,
	})
	mux := http.NewServeMux()
	mux.Handle(mcp.Path, endpoint)
	mux.Handle("/oauth/", as)
	mux.Handle("/.well-known/", as)
	return mux, nil
}

// swappable answers 503 until it has a handler.
type swappable struct{ h atomic.Pointer[http.Handler] }

func (s *swappable) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := s.h.Load()
	if h == nil {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "finbot is starting", http.StatusServiceUnavailable)
		return
	}
	(*h).ServeHTTP(w, r)
}
