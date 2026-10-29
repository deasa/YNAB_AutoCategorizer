package ynab

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"testing"

	"github.com/joho/godotenv"
)

// TestLiveSmoke runs one full export and one delta against the live plan in
// .env: two GETs, nothing else. Skipped unless YNAB_LIVE_SMOKE=1.
//
//	YNAB_LIVE_SMOKE=1 go test ./ynab -run TestLiveSmoke -v
func TestLiveSmoke(t *testing.T) {
	if os.Getenv("YNAB_LIVE_SMOKE") != "1" {
		t.Skip("set YNAB_LIVE_SMOKE=1 to run against the live plan")
	}
	_ = godotenv.Load("../.env")
	token, planID := os.Getenv("YNAB_ACCESS_TOKEN"), os.Getenv("YNAB_BUDGET_ID")
	if token == "" || planID == "" {
		t.Fatal("YNAB_ACCESS_TOKEN and YNAB_BUDGET_ID required")
	}

	gets := 0
	readOnly := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			return nil, fmt.Errorf("smoke test is read-only: refused %s", r.Method)
		}
		gets++
		if gets > 5 {
			return nil, fmt.Errorf("smoke test GET budget (5) exceeded")
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	c := NewClient(token, planID, log.New(io.Discard, "", 0), WithBaseTransport(readOnly))
	red := redactor{planID: planID, token: token}
	logf := func(format string, args ...any) { t.Log(red.redact(fmt.Sprintf(format, args...))) }
	ctx := context.Background()

	st := &SyncState{}
	full, err := c.Delta(ctx, st)
	if err != nil {
		t.Fatalf("full export: %v", err)
	}
	snoozed := 0
	for _, cat := range full.Categories {
		if cat.GoalSnoozedAt != nil {
			snoozed++
		}
	}
	logf("full export: plan %s, %d accounts, %d categories (%d with goal_snoozed_at), %d months, %d transactions, %d payees",
		full.ID, len(full.Accounts), len(full.Categories), snoozed, len(full.Months), len(full.Transactions), len(full.Payees))
	cursor := st.Plan

	delta, err := c.Delta(ctx, st)
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	logf("delta from cursor: %d accounts, %d categories, %d transactions; cursor advanced %d -> %d",
		len(delta.Accounts), len(delta.Categories), len(delta.Transactions), cursor, st.Plan)

	reads, writes := c.Governor().Remaining()
	logf("live GETs sent: %d; governor remaining: %d reads, %d writes", gets, reads, writes)
	if gets != 2 {
		t.Errorf("sent %d GETs, want 2", gets)
	}
}
