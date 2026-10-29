package sync

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/deasa/YNAB_AutoCategorizer/store"
	"github.com/deasa/YNAB_AutoCategorizer/ynab"
	"github.com/joho/godotenv"
)

const liveGETBudget = 15

var uuidRE = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// TestLiveSync is the M1-3 acceptance run, read-only: a full load of the
// active plan into a temp DB, one delta tick, a one-shot sync of the newest
// older plan with a "Discretionary" category, and freshness for 3 accounts.
// The transport refuses anything but GET and stops at liveGETBudget GETs.
// Skipped unless YNAB_LIVE_SYNC=1.
//
//	YNAB_LIVE_SYNC=1 go test ./sync -run TestLiveSync -v
func TestLiveSync(t *testing.T) {
	if os.Getenv("YNAB_LIVE_SYNC") != "1" {
		t.Skip("set YNAB_LIVE_SYNC=1 to run against the live plans")
	}
	_ = godotenv.Load("../.env")
	token, active := os.Getenv("YNAB_ACCESS_TOKEN"), os.Getenv("YNAB_BUDGET_ID")
	if token == "" || active == "" {
		t.Fatal("YNAB_ACCESS_TOKEN and YNAB_BUDGET_ID required")
	}
	redact := func(s string) string {
		return uuidRE.ReplaceAllString(strings.ReplaceAll(s, token, "{token}"), "{id}")
	}
	var logs bytes.Buffer
	logf := func(format string, args ...any) {
		line := redact(fmt.Sprintf(format, args...))
		logs.WriteString(line + "\n")
		t.Log(line)
	}
	defer func() {
		if p := os.Getenv("YNAB_LIVE_SYNC_LOG"); p != "" {
			os.WriteFile(p, logs.Bytes(), 0o600)
		}
	}()

	gets := 0
	readOnly := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			return nil, fmt.Errorf("live sync test is read-only: refused %s", r.Method)
		}
		if gets >= liveGETBudget {
			return nil, fmt.Errorf("live sync test GET budget (%d) exceeded", liveGETBudget)
		}
		gets++
		return http.DefaultTransport.RoundTrip(r)
	})
	post, _ := http.NewRequest(http.MethodPost, "https://api.ynab.com/v1/plans/x/transactions/import", nil)
	if _, err := readOnly.RoundTrip(post); err == nil {
		t.Fatal("transport let a POST through")
	} else {
		logf("transport refuses non-GET: %v (GETs so far: %d)", err, gets)
	}

	gov := ynab.NewGovernor(ynab.DefaultHourlyLimit, ynab.DefaultWriteReserve, nil)
	newClient := func(id string) *ynab.Client {
		return ynab.NewClient(token, id, log.New(&logs, "ynab: ", 0), ynab.WithGovernor(gov),
			ynab.WithBaseTransport(readOnly))
	}
	clients := func(id string) PlanAPI { return newClient(id) }

	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "finbot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db, clients, log.New(&logs, "", 0))

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
	all, err := newClient(active).Plans(ctx)
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
	disc, err := DiscretionaryPlans(ctx, others[:min(8, len(others))], clients)
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

	reads, writes := gov.Remaining()
	logf("live GETs sent: %d (budget %d); non-GET sent: 0; governor remaining %d reads, %d writes",
		gets, liveGETBudget, reads, writes)
	for _, secret := range []string{token, active, older.ID} {
		if strings.Contains(logs.String(), secret) {
			t.Error("logs leak a secret or plan id")
		}
	}
}
