// Package cmd implements the finbot subcommands: `serve` (the daemon) and
// `sync` (one-shot). Nothing it logs or prints carries a plan, account or
// transaction id, or the token.
package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/config"
	"github.com/deasa/YNAB_AutoCategorizer/store"
	finsync "github.com/deasa/YNAB_AutoCategorizer/sync"
	"github.com/deasa/YNAB_AutoCategorizer/ynab"
)

const usage = `usage: finbot <command> [flags]

commands:
  serve             run the daemon: delta sync every SYNC_INTERVAL, /healthz on HTTP_ADDR
  sync              run one delta sync of the active plan and exit
  sync --archived   scan every plan for Discretionary (persisted), then sync each plan that has it
`

// Main runs finbot with args (without the program name) and returns the
// process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return withApp(args[1:], "serve", stderr, func(ctx context.Context, a *app, fs *flag.FlagSet) error {
			return a.serve(ctx)
		})
	case "sync":
		var archived bool
		return withApp(args[1:], "sync", stderr, func(ctx context.Context, a *app, fs *flag.FlagSet) error {
			if archived {
				return a.syncArchived(ctx, stdout)
			}
			return a.syncOnce(ctx, stdout)
		}, func(fs *flag.FlagSet) {
			fs.BoolVar(&archived, "archived", false, "scan every plan for Discretionary, then sync each plan that has it")
		})
	default:
		fmt.Fprintf(stderr, "finbot: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// app holds what every subcommand shares.
type app struct {
	cfg     *config.Config
	log     *slog.Logger
	db      *store.DB
	syncer  *finsync.Syncer
	clients finsync.ClientFunc
	gov     *ynab.Governor
	ynabLog *log.Logger
}

func withApp(args []string, name string, stderr io.Writer, run func(context.Context, *app, *flag.FlagSet) error, flags ...func(*flag.FlagSet)) int {
	fs := flag.NewFlagSet("finbot "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	for _, f := range flags {
		f(fs)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "finbot %s: unexpected arguments\n\n%s", name, usage)
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "finbot: %v\n", err)
		return 1
	}
	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		logger.Error("open store", "err", scrub(err.Error(), cfg.YNABAccessToken))
		return 1
	}
	defer db.Close()

	gov := ynab.NewGovernor(ynab.DefaultHourlyLimit, ynab.DefaultWriteReserve, nil)
	ynabLog := slog.NewLogLogger(logger.Handler(), slog.LevelInfo)
	clients := func(planID string) finsync.PlanAPI {
		return ynab.NewClient(cfg.YNABAccessToken, planID, ynabLog, ynab.WithGovernor(gov))
	}
	a := &app{
		cfg:     cfg,
		log:     logger,
		db:      db,
		clients: clients,
		gov:     gov,
		ynabLog: ynabLog,
		syncer:  finsync.New(db, clients, ynabLog),
	}
	if err := run(ctx, a, fs); err != nil {
		logger.Error("finbot "+name+" failed", "err", scrub(err.Error(), cfg.YNABAccessToken))
		return 1
	}
	return 0
}

// errSkipped marks a tick the rate budget skipped: not a failure, but not a
// fresh sync either.
var errSkipped = errors.New("sync skipped: rate budget exhausted")

// syncPlan runs one tick for planID and logs it.
func (a *app) syncPlan(ctx context.Context, planID, label string) (finsync.Result, error) {
	res, err := a.syncer.SyncPlan(ctx, planID)
	if err != nil {
		a.log.Error("sync tick failed", "plan", label, "err", a.redact(err.Error()))
		return res, err
	}
	links := make([]map[string]any, 0, len(res.LinkEvents))
	for _, e := range res.LinkEvents {
		links = append(links, map[string]any{"account": e.AccountName, "in_error": e.InError})
	}
	a.log.Info("sync tick", "plan", label, "kind", kind(res), "skipped", res.Skipped,
		"rows", res.Rows, "orphans", res.Orphans, "link_events", links)
	if res.Skipped {
		return res, errSkipped
	}
	return res, nil
}

func kind(res finsync.Result) string {
	if res.Initial {
		return "initial"
	}
	return "delta"
}

func total(rows map[string]int) int {
	n := 0
	for _, v := range rows {
		n += v
	}
	return n
}

func summary(res finsync.Result) string {
	if res.Skipped {
		return "skipped (rate budget exhausted)"
	}
	return fmt.Sprintf("%s, %d rows %v, %d orphans, %d link events",
		kind(res), total(res.Rows), res.Rows, res.Orphans, len(res.LinkEvents))
}

func (a *app) syncOnce(ctx context.Context, stdout io.Writer) error {
	res, err := a.syncPlan(ctx, a.cfg.YNABBudgetID, "active")
	if err != nil && !errors.Is(err, errSkipped) {
		return err
	}
	fmt.Fprintf(stdout, "sync: active plan: %s\n", summary(res))
	return nil
}

func (a *app) syncArchived(ctx context.Context, stdout io.Writer) error {
	// The lister shares the syncer's governor, so the run has one rate budget.
	lister := ynab.NewClient(a.cfg.YNABAccessToken, a.cfg.YNABBudgetID, a.ynabLog, ynab.WithGovernor(a.gov))
	plans, scanErr := finsync.ScanDiscretionary(ctx, lister, a.clients, a.db)
	var se *finsync.ScanError
	switch {
	case errors.As(scanErr, &se):
		fmt.Fprintf(stdout, "scan: %d plans scanned, %d with Discretionary, %d failed\n", se.Scanned, len(plans), len(se.Failed))
		for _, f := range se.Failed {
			fmt.Fprintf(stdout, "scan: failed: %s: %s\n", f.Name, a.redact(f.Err))
		}
	case scanErr != nil:
		return scanErr
	default:
		scans, err := a.db.PlanScans(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "scan: %d plans scanned, %d with Discretionary, 0 failed\n", len(scans), len(plans))
	}

	var failed int
	for _, p := range plans {
		res, err := a.syncPlan(ctx, p.ID, p.Name)
		if err != nil && !errors.Is(err, errSkipped) {
			failed++
			fmt.Fprintf(stdout, "sync: %s: failed: %s\n", p.Name, a.redact(err.Error()))
			continue
		}
		fmt.Fprintf(stdout, "sync: %s: %s\n", p.Name, summary(res))
	}
	if failed > 0 || scanErr != nil {
		msg := fmt.Sprintf("archived sync: %d plan syncs failed", failed)
		if scanErr != nil {
			msg += "; scan had failures"
		}
		return errors.New(msg)
	}
	return nil
}

// newScheduler builds serve's scheduler and its health tracker. The import
// job exists only when IMPORT_ENABLED is set; otherwise Import stays nil and
// the scheduler never creates an import ticker.
func (a *app) newScheduler() (*Scheduler, *Health) {
	cfg := a.cfg
	health := &Health{StaleAfter: 2 * cfg.SyncInterval, Token: cfg.YNABAccessToken}
	sched := &Scheduler{
		Sync: func(ctx context.Context) error {
			_, err := a.syncPlan(ctx, cfg.YNABBudgetID, "active")
			return err
		},
		SyncEvery: cfg.SyncInterval,
		Health:    health,
	}
	if cfg.ImportEnabled {
		sched.ImportEvery = cfg.ImportInterval
		sched.Import = func(ctx context.Context) error {
			res, err := a.syncer.TriggerImport(ctx, cfg.YNABBudgetID)
			if err != nil {
				a.log.Error("import tick failed", "err", a.redact(err.Error()))
				return err
			}
			a.log.Info("import tick", "imported", res.Imported, "skipped", res.Skipped)
			return nil
		}
		a.log.Info("import enabled", "interval", cfg.ImportInterval.String())
	} else {
		a.log.Info("import disabled")
	}
	return sched, health
}

func (a *app) serve(ctx context.Context) error {
	cfg := a.cfg
	sched, health := a.newScheduler()

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", health)
	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Serve(ln) }()
	a.log.Info("serving", "addr", ln.Addr().String(), "sync_interval", cfg.SyncInterval.String())

	schedCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { sched.Run(schedCtx); close(done) }()

	select {
	case <-ctx.Done():
		a.log.Info("shutting down")
	case err = <-srvErr:
		a.log.Error("http server stopped", "err", err.Error())
	}
	cancel()
	<-done

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	if e := srv.Shutdown(shutCtx); e != nil && err == nil {
		err = e
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	a.log.Info("stopped")
	return nil
}

// redact scrubs ids and the token from s before it is logged or printed.
func (a *app) redact(s string) string { return scrub(s, a.cfg.YNABAccessToken) }
