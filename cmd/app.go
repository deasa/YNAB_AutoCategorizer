// Package cmd implements the finbot subcommands: `serve` (the daemon),
// `sync` (one-shot), `health` (the container healthcheck), `backup` and
// `restore` (off-site copies of the database), `revoke` (MCP sign-out) and `tag-account` (local tax-bucket tags). Nothing it logs or prints carries a plan, account or
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
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/config"
	"github.com/deasa/YNAB_AutoCategorizer/store"
	finsync "github.com/deasa/YNAB_AutoCategorizer/sync"
	"github.com/deasa/YNAB_AutoCategorizer/writer"
	"github.com/deasa/YNAB_AutoCategorizer/ynab"
)

const usage = `usage: finbot <command> [flags]

commands:
  serve             run the daemon: delta sync every SYNC_INTERVAL, /healthz on HTTP_ADDR
  sync              run one delta sync of the active plan and exit
  sync --archived   scan every plan for Discretionary (persisted), then sync each plan that has it
  health            GET /healthz on the local server (HTTP_ADDR); exit 0 on 200, else 1
  backup            store one encrypted backup off-site now
  backup --list     list stored backups (id, time, size)
  restore --key <path> [--id <id>] --out <path>
                    download and decrypt a backup (newest by default) to a new file, then verify it
  revoke <email | u_hash | --all>
                    sign a user (or everyone) out of MCP at once, with no restart
  tag-account [<account name> <bucket>]
                    tag an account of the active plan with a tax bucket (taxable, traditional,
                    roth, hsa, cash, none); with no arguments, list every account's tag
  calibrate [--limit N] [--batch N] [--out path]
                    measure rules + Jev on labelled history, fit τ and write calibration.json
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
	case "health":
		if len(args) > 1 {
			fmt.Fprintf(stderr, "finbot health: unexpected arguments\n\n%s", usage)
			return 2
		}
		return probe(os.Getenv("HTTP_ADDR"), stderr)
	case "backup":
		return backupCmd(args[1:], stdout, stderr)
	case "restore":
		return restoreCmd(args[1:], stdout, stderr)
	case "revoke":
		return revokeCmd(args[1:], stdout, stderr)
	case "tag-account":
		return tagAccountCmd(args[1:], stdout, stderr)
	case "calibrate":
		return calibrateCmd(args[1:], stdout, stderr)
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

	slackURL string // overrides the Slack API base URL (tests)

	// Set by startSlack when Socket Mode is on; nil otherwise.
	socket  socketClient
	alerter alerter

	// Categorization (serve). writer and gate are shared by the pipeline and
	// Slack's answers; cat is set by serve; claude only when configured and
	// CLASSIFY_MODE isn't off.
	writer *writer.Writer
	gate   *writer.Gate
	cat    *categorizer
	claude claudeRunner

	// syncMu serializes syncs: the writer's Resync (from a Slack answer) can
	// run alongside the scheduler.
	syncMu sync.Mutex
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
		logger.Error("open store", "err", scrub(err.Error(), secrets(cfg)...))
		return 1
	}
	defer db.Close()

	gov := ynab.NewGovernor(ynab.DefaultHourlyLimit, ynab.DefaultWriteReserve, nil)
	ynabLog := slog.NewLogLogger(logger.Handler(), slog.LevelInfo)
	clients := func(planID string) finsync.PlanAPI {
		return newPlanClient(cfg.YNABAccessToken, planID, ynabLog, gov)
	}
	a := &app{
		cfg:     cfg,
		log:     logger,
		db:      db,
		clients: clients,
		gov:     gov,
		ynabLog: ynabLog,
		syncer:  finsync.New(db, clients, ynabLog).DetectLocks(cfg.YNABBudgetID),

		slackURL: slackBaseURL,
	}
	if err := run(ctx, a, fs); err != nil {
		logger.Error("finbot "+name+" failed", "err", scrub(err.Error(), secrets(cfg)...))
		return 1
	}
	return 0
}

// slackBaseURL overrides the status message's Slack API base URL (tests).
var slackBaseURL string

// newPlanClient builds a plan's YNAB client (tests replace it, so a serve test
// never reaches YNAB).
var newPlanClient = func(token, planID string, l *log.Logger, gov *ynab.Governor) finsync.PlanAPI {
	return ynab.NewClient(token, planID, l, ynab.WithGovernor(gov))
}

// errSkipped marks a tick the rate budget skipped: not a failure, but not a
// fresh sync either.
var errSkipped = errors.New("sync skipped: rate budget exhausted")

// syncPlan runs one tick for planID and logs it.
func (a *app) syncPlan(ctx context.Context, planID, label string) (finsync.Result, error) {
	a.syncMu.Lock()
	res, err := a.syncer.SyncPlan(ctx, planID)
	a.syncMu.Unlock()
	if err != nil {
		a.log.Error("sync tick failed", "plan", label, "err", a.redact(err.Error()))
		return res, err
	}
	links := make([]map[string]any, 0, len(res.LinkEvents))
	for _, e := range res.LinkEvents {
		links = append(links, map[string]any{"account": e.AccountName, "in_error": e.InError})
	}
	a.log.Info("sync tick", "plan", label, "kind", kind(res), "skipped", res.Skipped,
		"rows", res.Rows, "orphans", res.Orphans, "link_events", links, "human_locks", lockCounts(res.Locks))
	if res.Skipped {
		return res, errSkipped
	}
	return res, nil
}

// lockCounts renders a tick's human-edit detection for the summary line: counts
// only, never ids.
func lockCounts(l finsync.LockCounts) map[string]any {
	return map[string]any{"backfill": l.Backfill, "judged": l.Judged, "ynab_edit": l.Edit,
		"first_touch": l.FirstTouch, "skipped_intent": l.SkippedIntent}
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
	pinger := &Pinger{URL: cfg.HealthcheckPingURL, Log: a.log}
	a.log.Info("healthcheck ping", "enabled", pinger.URL != "")
	sched := &Scheduler{
		Sync: a.syncJob(func(ctx context.Context) (finsync.Result, error) {
			return a.syncPlan(ctx, cfg.YNABBudgetID, "active")
		}, pinger),
		SyncEvery: cfg.SyncInterval,
		Health:    health,
	}
	if a.claude != nil {
		sched.Probe, sched.ProbeEvery = a.probeClaude, probeEvery
		a.log.Info("claude probe enabled", "every", probeEvery.String())
	}
	if cfg.ImportEnabled {
		sched.ImportEvery = cfg.ImportInterval
		sched.Import = a.importJob(time.Now)
		sched.ImportDelay = func() time.Duration { return a.importDelay(context.Background(), time.Now()) }
		a.log.Info("import enabled", "interval", cfg.ImportInterval.String())
	} else {
		a.log.Info("import disabled")
	}
	a.addBackup(sched)
	a.addStatus(sched)
	a.addPrune(sched)
	return sched, health
}

// importJob is serve's bank-import job. Each attempt's outcome is stored in
// import_state (failure text redacted), even when shutdown cut the POST short,
// so the next start counts from it. A tick skipped for want of rate budget
// made no POST and records nothing.
func (a *app) importJob(now func() time.Time) func(context.Context) error {
	planID := a.cfg.YNABBudgetID
	return func(ctx context.Context) error {
		at := now()
		res, err := a.syncer.TriggerImport(ctx, planID)
		var errMsg *string
		if err != nil {
			msg := a.redact(err.Error())
			errMsg = &msg
			a.log.Error("import tick failed", "err", msg)
		} else {
			a.log.Info("import tick", "imported", res.Imported, "skipped", res.Skipped)
			if res.Skipped {
				return nil // no POST was made (no rate budget): nothing to record
			}
		}
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), importRecordTimeout)
		defer cancel()
		if e := a.db.RecordImport(rctx, planID, at, errMsg); e != nil {
			a.log.Error("import record failed", "err", a.redact(e.Error()))
		}
		return err
	}
}

// importRecordTimeout bounds the import_state write, which outlives ctx.
const importRecordTimeout = 5 * time.Second

// importDelay is the time from now until the first import: the last attempt
// plus IMPORT_INTERVAL, so a restart (or a crash loop) never imports more than
// once per interval. Overdue is zero; no prior attempt waits a full interval,
// as before. A read failure also waits a full interval.
func (a *app) importDelay(ctx context.Context, now time.Time) time.Duration {
	every := a.cfg.ImportInterval
	st, ok, err := a.db.GetImportState(ctx, a.cfg.YNABBudgetID)
	if err != nil {
		a.log.Error("import state read failed", "err", a.redact(err.Error()))
		return every
	}
	if !ok {
		return every
	}
	return firstImportDelay(st.LastAttemptAt, now, every)
}

// firstImportDelay is last+every-now, at most every (a clock that went
// backwards can't push the import out further) and at least zero.
func firstImportDelay(last, now time.Time, every time.Duration) time.Duration {
	return max(0, min(every, last.Add(every).Sub(now)))
}

// syncJob is serve's sync job over sync: the ping, then, after a successful
// sync only (not a failed or budget-skipped one), the alerts and the
// categorization pass.
func (a *app) syncJob(sync func(context.Context) (finsync.Result, error), pinger *Pinger) func(context.Context) error {
	return func(ctx context.Context) error {
		start := time.Now()
		res, err := sync(ctx)
		if ctx.Err() == nil {
			pinger.After(ctx, err)
		}
		if err == nil && ctx.Err() == nil {
			a.feedAlerts(ctx, res)
			if a.cat != nil {
				a.cat.After(ctx, start)
			}
		}
		return err
	}
}

func (a *app) serve(ctx context.Context) error {
	cfg := a.cfg
	cat, err := a.newCategorizer()
	if err != nil {
		return err
	}
	a.cat = cat
	sched, health := a.newScheduler()

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", health)
	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	servers := []*http.Server{srv}

	schedCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Bind MCP before any job runs, so a port conflict fails serve at once.
	mcpSrv, mcpLn, mcpDone, err := a.startMCP(schedCtx)
	if err != nil {
		ln.Close()
		return err
	}
	// Slack comes up before the first sync, so its alerts can post.
	slackDone, err := a.startSlack(schedCtx)
	if err != nil {
		cancel()
		ln.Close()
		if mcpLn != nil {
			mcpLn.Close()
		}
		<-mcpDone
		return err
	}
	srvErr := make(chan error, 2)
	go func() { srvErr <- srv.Serve(ln) }()
	if mcpSrv != nil {
		servers = append(servers, mcpSrv)
		go func() { srvErr <- mcpSrv.Serve(mcpLn) }()
	}
	a.log.Info("serving", "addr", ln.Addr().String(), "sync_interval", cfg.SyncInterval.String())

	done := make(chan struct{})
	go func() { sched.Run(schedCtx); close(done) }()

	select {
	case <-ctx.Done():
		a.log.Info("shutting down")
	case err = <-srvErr:
		a.log.Error("http server stopped", "err", err.Error())
	}
	cancel()

	// Every wait below shares one deadline, so a job that ignores its ctx
	// can't hold the process.
	deadline, stopWait := context.WithTimeout(context.Background(), shutdownTimeout)
	defer stopWait()
	var hung []string
	for _, w := range []struct {
		name string
		done <-chan struct{}
	}{{"scheduler", done}, {"mcp", mcpDone}, {"slack", slackDone}} {
		select {
		case <-w.done:
			continue // done already: never call it hung, even past the deadline
		default:
		}
		select {
		case <-w.done:
		case <-deadline.Done():
			hung = append(hung, w.name)
		}
	}
	shutdown(deadline, servers)
	if len(hung) > 0 {
		a.log.Error("shutdown deadline passed; exiting anyway", "waiting_on", hung)
		return fmt.Errorf("shutdown deadline passed waiting on %s", strings.Join(hung, ", "))
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	a.log.Info("stopped")
	return nil
}

// shutdownTimeout bounds the whole of stopping: the running job, the Slack
// client and the listeners.
var shutdownTimeout = 5 * time.Second

// shutdown drains the servers until ctx ends, then closes whatever is left. An
// open MCP stream never finishes by itself, so reaching the close is normal,
// not a failure. Every handler's ctx has ended by the time this returns, and
// the DB closes after.
func shutdown(ctx context.Context, servers []*http.Server) {
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Go(func() {
			if s.Shutdown(ctx) != nil {
				s.Close()
			}
		})
	}
	wg.Wait()
}

// redact scrubs ids, the tokens and the secret URLs from s before it is logged
// or printed.
func (a *app) redact(s string) string {
	return scrub(s, secrets(a.cfg)...)
}

// secrets lists every configured value that must not reach a log.
func secrets(cfg *config.Config) []string {
	return append([]string{
		cfg.YNABAccessToken,
		cfg.HealthcheckPingURL, strings.TrimRight(cfg.HealthcheckPingURL, "/"),
		cfg.SlackBotToken, cfg.SlackAppToken,
		cfg.Backup.TursoToken, cfg.Backup.TursoURL,
		cfg.MCP.GoogleClientSecret,
		cfg.Classify.JevAPIKey, cfg.Classify.ClaudeToken,
	}, append(cfg.MCP.Emails(), cfg.SlackIDs()...)...)
}
