package cmd

import (
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/config"
	"github.com/deasa/YNAB_AutoCategorizer/store"
	finsync "github.com/deasa/YNAB_AutoCategorizer/sync"
	"github.com/deasa/YNAB_AutoCategorizer/ynab"
)

const importPlanID = "0f8fad5b-d9cb-469f-a165-70867728950e"

// importPlan is a PlanAPI whose import answers err, or one id.
type importPlan struct {
	fakePlan
	err   error
	posts *atomic.Int32
}

func (p importPlan) ImportTransactions(context.Context) ([]string, error) {
	p.posts.Add(1)
	if p.err != nil {
		return nil, p.err
	}
	return []string{"t1"}, nil
}

func importApp(t *testing.T, plan *importPlan) *app {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var syncs atomic.Int32
	plan.fakePlan = fakePlan{&syncs}
	if plan.posts == nil {
		plan.posts = &atomic.Int32{}
	}
	return &app{
		cfg: &config.Config{YNABAccessToken: testYNAB, YNABBudgetID: importPlanID, ImportEnabled: true,
			ImportInterval: importEvery, SyncInterval: syncEvery},
		log: slog.New(slog.NewJSONHandler(io.Discard, nil)), db: db,
		syncer: finsync.New(db, func(string) finsync.PlanAPI { return *plan }, log.New(io.Discard, "", 0)),
	}
}

func TestImportJob_PersistsOutcomes(t *testing.T) {
	ctx := context.Background()
	plan := &importPlan{}
	a := importApp(t, plan)
	at := time.Date(2026, 11, 1, 16, 0, 0, 0, time.UTC)
	job := a.importJob(func() time.Time { return at })

	if err := job(ctx); err != nil {
		t.Fatal(err)
	}
	st, ok, err := a.db.GetImportState(ctx, importPlanID)
	if err != nil || !ok {
		t.Fatalf("state after success: ok=%v err=%v", ok, err)
	}
	if !st.LastAttemptAt.Equal(at) || st.LastOKAt == nil || !st.LastOKAt.Equal(at) || st.LastError != nil {
		t.Fatalf("after success: %+v", st)
	}

	// A failure keeps the last success and stores redacted text.
	plan.err = errors.New("POST /plans/" + importPlanID + "/transactions/import: Bearer " + testYNAB + ": 500")
	failAt := at.Add(importEvery)
	at = failAt
	if err := job(ctx); err == nil {
		t.Fatal("job hid the import error")
	}
	st, _, err = a.db.GetImportState(ctx, importPlanID)
	if err != nil {
		t.Fatal(err)
	}
	if !st.LastAttemptAt.Equal(failAt) || st.LastOKAt == nil || !st.LastOKAt.Equal(failAt.Add(-importEvery)) {
		t.Fatalf("after failure: %+v", st)
	}
	if st.LastError == nil || !strings.Contains(*st.LastError, "500") {
		t.Fatalf("failure text not stored: %+v", st.LastError)
	}
	if strings.Contains(*st.LastError, testYNAB) || strings.Contains(*st.LastError, importPlanID) {
		t.Fatalf("failure text not redacted: %q", *st.LastError)
	}

	// Shutdown cutting the POST short is still an attempt.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	plan.err = context.Canceled
	at = failAt.Add(time.Minute)
	_ = job(cctx)
	if st, _, _ = a.db.GetImportState(ctx, importPlanID); !st.LastAttemptAt.Equal(at) {
		t.Fatalf("cancelled attempt not recorded: %+v", st)
	}
}

func TestImportJob_RateSkipRecordsNothing(t *testing.T) {
	ctx := context.Background()
	plan := &importPlan{err: ynab.ErrBudgetExhausted}
	a := importApp(t, plan)
	if err := a.importJob(time.Now)(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := a.db.GetImportState(ctx, importPlanID); ok || err != nil {
		t.Fatalf("a skipped tick was recorded: ok=%v err=%v", ok, err)
	}
}

func TestFirstImportDelay(t *testing.T) {
	now := time.Date(2026, 11, 1, 16, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		last time.Time
		want time.Duration
	}{
		{"mid-interval", now.Add(-2 * time.Hour), 4 * time.Hour},
		{"just now", now, importEvery},
		{"exactly due", now.Add(-importEvery), 0},
		{"overdue", now.Add(-3 * importEvery), 0},
		{"clock went back", now.Add(time.Hour), importEvery},
	} {
		if got := firstImportDelay(tc.last, now, importEvery); got != tc.want {
			t.Errorf("%s: delay = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestImportDelay_FromStore(t *testing.T) {
	ctx := context.Background()
	a := importApp(t, &importPlan{})
	now := time.Date(2026, 11, 1, 16, 0, 0, 0, time.UTC)
	if got := a.importDelay(ctx, now); got != importEvery {
		t.Errorf("no prior state: delay = %v, want %v (today's behaviour)", got, importEvery)
	}
	// A failed attempt counts too, so a failing import doesn't refire on restart.
	msg := "boom"
	if err := a.db.RecordImport(ctx, importPlanID, now.Add(-time.Hour), &msg); err != nil {
		t.Fatal(err)
	}
	if got := a.importDelay(ctx, now); got != 5*time.Hour {
		t.Errorf("after a failure an hour ago: delay = %v, want 5h", got)
	}
}

func TestNewScheduler_ImportDisabledHasNoImport(t *testing.T) {
	a := importApp(t, &importPlan{})
	a.cfg.ImportEnabled = false
	sched, _ := a.newScheduler()
	if sched.Import != nil || sched.ImportDelay != nil {
		t.Fatal("import job wired while IMPORT_ENABLED is false")
	}
}
