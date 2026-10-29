package sync

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/deasa/YNAB_AutoCategorizer/store"
	"github.com/deasa/YNAB_AutoCategorizer/ynab"
	"github.com/joho/godotenv"
)

// live is the read-only setup shared by the live tests: a transport that
// refuses anything but GET and stops at a GET budget, a shared governor, a
// temp store, and a log buffer every line is redacted into.
type live struct {
	t       *testing.T
	token   string
	active  string
	gov     *ynab.Governor
	gets    int
	budget  int
	logs    bytes.Buffer
	db      *store.DB
	clients ClientFunc
}

func newLive(t *testing.T, getBudget int) *live {
	t.Helper()
	if os.Getenv("YNAB_LIVE_SYNC") != "1" {
		t.Skip("set YNAB_LIVE_SYNC=1 to run against the live plans")
	}
	_ = godotenv.Load("../.env")
	l := &live{t: t, token: os.Getenv("YNAB_ACCESS_TOKEN"), active: os.Getenv("YNAB_BUDGET_ID"), budget: getBudget}
	if l.token == "" || l.active == "" {
		t.Fatal("YNAB_ACCESS_TOKEN and YNAB_BUDGET_ID required")
	}
	t.Cleanup(func() {
		if p := os.Getenv("YNAB_LIVE_SYNC_LOG"); p != "" {
			os.WriteFile(p, l.logs.Bytes(), 0o600)
		}
	})

	readOnly := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			return nil, fmt.Errorf("live sync test is read-only: refused %s", r.Method)
		}
		if l.gets >= l.budget {
			return nil, fmt.Errorf("live sync test GET budget (%d) exceeded", l.budget)
		}
		l.gets++
		return http.DefaultTransport.RoundTrip(r)
	})
	post, _ := http.NewRequest(http.MethodPost, "https://api.ynab.com/v1/plans/x/transactions/import", nil)
	if _, err := readOnly.RoundTrip(post); err == nil {
		t.Fatal("transport let a POST through")
	} else {
		l.logf("transport refuses non-GET: %v (GETs so far: %d)", err, l.gets)
	}

	l.gov = ynab.NewGovernor(ynab.DefaultHourlyLimit, ynab.DefaultWriteReserve, nil)
	l.clients = func(id string) PlanAPI {
		return ynab.NewClient(l.token, id, log.New(&l.logs, "ynab: ", 0), ynab.WithGovernor(l.gov),
			ynab.WithBaseTransport(readOnly))
	}
	var err error
	if l.db, err = store.Open(context.Background(), filepath.Join(t.TempDir(), "finbot.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.db.Close() })
	return l
}

func (l *live) redact(s string) string {
	return uuidRE.ReplaceAllString(strings.ReplaceAll(s, l.token, "{token}"), "{id}")
}

func (l *live) logf(format string, args ...any) {
	line := l.redact(fmt.Sprintf(format, args...))
	l.logs.WriteString(line + "\n")
	l.t.Log(line)
}

func (l *live) planClient(id string) *ynab.Client { return l.clients(id).(*ynab.Client) }

// finish logs the request totals and fails if the log holds the token, any
// of ids, or any UUID at all.
func (l *live) finish(ids ...string) {
	reads, writes := l.gov.Remaining()
	l.logf("live GETs sent: %d (budget %d); non-GET sent: 0; governor remaining %d reads, %d writes",
		l.gets, l.budget, reads, writes)
	for _, secret := range append([]string{l.token, l.active}, ids...) {
		if strings.Contains(l.logs.String(), secret) {
			l.t.Error("logs leak a secret or plan id")
		}
	}
	if n := len(uuidRE.FindAllString(l.logs.String(), -1)); n != 0 {
		l.t.Errorf("logs hold %d UUIDs", n)
	}
}

// TestLiveScan is the M1-4a acceptance run, read-only: the archived-plan
// scan over every plan the token reaches, persisted to a temp DB.
//
//	YNAB_LIVE_SYNC=1 go test ./sync -run TestLiveScan -v
func TestLiveScan(t *testing.T) {
	l := newLive(t, 60)
	ctx := context.Background()
	all, err := l.planClient(l.active).Plans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all)+1 > l.budget {
		t.Fatalf("%d plans exceed the GET budget %d", len(all), l.budget)
	}
	disc, err := ScanDiscretionary(ctx, fakeLister(all), l.clients, l.db)
	if err != nil {
		l.logf("scan error: %v", err)
	}
	rows, rerr := l.db.PlanScans(ctx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	failed := 0
	for _, r := range rows {
		if r.ScanError != "" {
			failed++
			l.logf("plan scan failed: %s", r.ScanError)
		}
	}
	l.logf("scanned %d of %d, %d with Discretionary, %d failed; %d rows persisted",
		len(rows)-failed, len(all), len(disc), failed, len(rows))
	if len(rows) != len(all) || failed != 0 || err != nil {
		t.Errorf("scan incomplete: %d rows for %d plans, %d failed", len(rows), len(all), failed)
	}
	ids := make([]string, len(all))
	for i, p := range all {
		ids[i] = p.ID
	}
	l.finish(ids...)
}

// TestLiveSync is the M1-3 acceptance run, read-only: a full load of the
// active plan into a temp DB, one delta tick, a one-shot sync of the newest
// older plan with a "Discretionary" category, and freshness for 3 accounts.
//
//	YNAB_LIVE_SYNC=1 go test ./sync -run TestLiveSync -v
func TestLiveSync(t *testing.T) {
	l := newLive(t, 15)
	active, db, clients, logf := l.active, l.db, l.clients, l.logf
	ctx := context.Background()
	s := New(db, clients, log.New(&l.logs, "", 0))

	counts := func(planID string) string {
		var parts []string
		for _, table := range []string{"accounts", "category_groups", "categories", "payees", "transactions",
			"subtransactions", "months", "month_categories", "scheduled_transactions", "scheduled_subtransactions"} {
			var n, del int
			db.Reader().QueryRow("SELECT COUNT(*), COALESCE(SUM(deleted), 0) FROM "+table+" WHERE plan_id = ?",
				planID).Scan(&n, &del)
			parts = append(parts, fmt.Sprintf("%s=%d(%d deleted)", table, n, del))
		}
		return strings.Join(parts, " ")
	}

	// 1. Active plan: full load, then one delta tick.
	res, err := s.SyncPlan(ctx, active)
	if err != nil || !res.Initial {
		t.Fatalf("initial load: %+v, %v", res, err)
	}
	logf("active plan full load: rows %s", counts(active))
	res, err = s.SyncPlan(ctx, active)
	if err != nil || res.Initial || res.Skipped {
		t.Fatalf("delta tick: %+v, %v", res, err)
	}
	logf("active plan delta tick: upserted %v, %d link events", res.Rows, len(res.LinkEvents))
	logf("active plan after delta: rows %s", counts(active))

	// 2. One older plan with a "Discretionary" category: scan at most 8
	// others, newest first, so the run stays inside the GET budget.
	all, err := l.planClient(active).Plans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var others fakeLister
	for _, p := range all {
		if p.ID != active {
			others = append(others, p)
		}
	}
	sort.Slice(others, func(i, j int) bool { return others[i].LastModifiedOn.After(others[j].LastModifiedOn) })
	logf("plans reachable: %d (%d besides the active one; scanning %d)", len(all), len(others), min(8, len(others)))
	disc, err := ScanDiscretionary(ctx, others[:min(8, len(others))], clients, db)
	if err != nil {
		t.Fatal(err)
	}
	logf("older plans with a Discretionary category: %d", len(disc))
	if len(disc) == 0 {
		t.Fatal("no older Discretionary plan found")
	}
	older := disc[0]
	if res, err = s.SyncPlan(ctx, older.ID); err != nil || !res.Initial {
		t.Fatalf("older plan one-shot: %+v, %v", res, err)
	}
	logf("older plan (last modified %s) one-shot: rows %s", older.LastModifiedOn.Format("2006-01-02"), counts(older.ID))

	// 3. Freshness for 3 linked accounts, names redacted.
	fr, err := s.Freshness(ctx, active)
	if err != nil {
		t.Fatal(err)
	}
	shown := 0
	for _, f := range fr {
		if !f.Linked || f.Closed || shown == 3 {
			continue
		}
		shown++
		seen := "never"
		if f.LastImportSeenAt != nil {
			seen = f.LastImportSeenAt.Format("2006-01-02T15:04Z07:00")
		}
		newest := f.NewestImportDate
		if newest == "" {
			newest = "none"
		}
		logf("freshness account#%d: in_error=%v newest_import_date=%s import_first_seen=%s synced_at=%s",
			shown, f.InError, newest, seen, f.SyncedAt.Format("2006-01-02T15:04Z07:00"))
	}

	l.finish(older.ID)
}
