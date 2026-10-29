package ynab

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ynabapi "pkg.venceslau.dev/ynab"
)

const (
	testPlanID    = "11111111-2222-3333-4444-555555555555"
	testAccountID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	testToken     = "tok-SECRET-1234567890"
)

// exportBody is a minimal plan export carrying the live plan's zone-less
// goal_snoozed_at (spike F1) in both places it appears.
var exportBody = fmt.Sprintf(`{"data":{"server_knowledge":42,"plan":{
 "id":%[1]q,"name":"P","last_modified_on":"2026-10-29T01:02:03+00:00",
 "first_month":"2020-01-01","last_month":"2026-10-01",
 "accounts":[{"id":%[2]q,"name":"Checking","type":"checking","on_budget":true,"closed":false,"balance":0,"cleared_balance":0,"uncleared_balance":0,"transfer_payee_id":null,"deleted":false}],
 "categories":[{"id":"c1","category_group_id":"g1","name":"Groceries","hidden":false,"budgeted":0,"activity":0,"balance":0,"goal_snoozed_at":"2025-08-28T13:55:15.492","deleted":false}],
 "months":[{"month":"2026-10-01","income":0,"budgeted":0,"activity":0,"to_be_budgeted":0,"deleted":false,
   "categories":[{"id":"c1","category_group_id":"g1","name":"Groceries","hidden":false,"budgeted":0,"activity":0,"balance":0,"goal_snoozed_at":"2025-08-28T13:55:15.492","deleted":false}]}],
 "transactions":[{"id":"t1","date":"2026-10-28","amount":-5000,"cleared":"cleared","approved":false,"account_id":%[2]q,"deleted":true}]
}}}`, testPlanID, testAccountID)

func newTestClient(t *testing.T, h http.HandlerFunc, gov *Governor) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewClient(testToken, testPlanID, log.New(io.Discard, "", 0),
		WithBaseURL(srv.URL+"/v1"), WithGovernor(gov))
}

func fullGovernor() *Governor { return NewGovernor(DefaultHourlyLimit, DefaultWriteReserve, nil) }

func TestClient_ExportDecodesZonelessTimestamp(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Errorf("auth header not sent")
		}
		io.WriteString(w, exportBody)
	}, fullGovernor())

	detail, sk, err := c.Export(context.Background())
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if sk != 42 || len(detail.Categories) != 1 || detail.Categories[0].GoalSnoozedAt == nil {
		t.Fatalf("unexpected decode: sk=%d cats=%d", sk, len(detail.Categories))
	}
	want := time.Date(2025, 8, 28, 13, 55, 15, 492e6, time.UTC)
	if got := *detail.Categories[0].GoalSnoozedAt; !got.Equal(want) {
		t.Errorf("goal_snoozed_at = %v, want %v", got, want)
	}
	if !detail.Transactions[0].Deleted {
		t.Error("tombstone lost")
	}
}

func TestClient_DeltaSendsCursorAndAdvances(t *testing.T) {
	var queries []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query().Get("last_knowledge_of_server"))
		io.WriteString(w, exportBody)
	}, fullGovernor())

	st := &SyncState{}
	if _, err := c.Delta(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Delta(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if st.Plan != 42 || len(queries) != 2 || queries[0] != "" || queries[1] != "42" {
		t.Errorf("st.Plan=%d queries=%q", st.Plan, queries)
	}
}

func TestClient_PlansListsAll(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/plans") {
			t.Errorf("path %s", r.URL.Path)
		}
		io.WriteString(w, `{"data":{"plans":[{"id":"p1","name":"Live","last_modified_on":"2026-10-29T01:02:03Z","first_month":"2020-01-01","last_month":"2026-10-01"},{"id":"p2","name":"Archived 2019","last_modified_on":"2019-12-31T01:02:03Z","first_month":"2019-01-01","last_month":"2019-12-01"}],"default_plan":null}}`)
	}, fullGovernor())

	plans, err := c.Plans(context.Background())
	if err != nil || len(plans) != 2 || plans[1].Name != "Archived 2019" {
		t.Fatalf("plans=%v err=%v", plans, err)
	}
}

func TestClient_RateLimitedIsTyped(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600") // beyond the library's retry cap: surfaced, not slept
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"id":"429","name":"too_many_requests","detail":"Too many requests"}}`)
	}, fullGovernor())

	_, _, err := c.Export(context.Background())
	var rl *RateLimitError
	if !errors.As(err, &rl) || !errors.Is(err, ErrRateLimited) || !errors.Is(err, ynabapi.ErrRateLimited) {
		t.Fatalf("got %T %v, want *RateLimitError", err, err)
	}
	if rl.RetryAfter != time.Hour {
		t.Errorf("RetryAfter = %v", rl.RetryAfter)
	}
}

func TestClient_ErrorsAreRedacted(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		// Echo every id and the token back, as a worst case.
		fmt.Fprintf(w, `{"error":{"id":"404.2","name":"resource_not_found","detail":"plan %s account %s token %s"}}`,
			testPlanID, testAccountID, testToken)
	}, fullGovernor())

	_, _, err := c.Export(context.Background())
	if err == nil {
		t.Fatal("want error")
	}
	assertRedacted(t, err)
	if !errors.Is(err, ynabapi.ErrNotFound) {
		t.Errorf("sentinel lost through redaction: %v", err)
	}
	var apiErr *ynabapi.Error
	if errors.As(err, &apiErr) {
		t.Error("unredacted library error reachable via errors.As")
	}
	if !strings.Contains(err.Error(), "{plan}") || !strings.Contains(err.Error(), "{token}") {
		t.Errorf("placeholders missing: %v", err)
	}

	// Decode failures name the request path, which holds the plan id (F2).
	c = newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":{"plan":{"last_modified_on":"not a time"}}}`)
	}, fullGovernor())
	_, _, err = c.Export(context.Background())
	if err == nil {
		t.Fatal("want decode error")
	}
	assertRedacted(t, err)
}

func assertRedacted(t *testing.T, err error) {
	t.Helper()
	for _, secret := range []string{testPlanID, testAccountID, testToken} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks %q: %v", secret, err)
		}
	}
}

func TestClient_GovernorCountsRetries(t *testing.T) {
	calls := 0
	gov := NewGovernor(10, 0, (&fakeClock{t: time.Unix(0, 0)}).Now)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error":{"id":"503","name":"service_unavailable","detail":"x"}}`)
			return
		}
		io.WriteString(w, exportBody)
	}, gov)

	if _, _, err := c.Export(context.Background()); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if calls != 3 {
		t.Fatalf("server saw %d attempts, want 3", calls)
	}
	if r, _ := gov.Remaining(); r != 7 {
		t.Errorf("remaining reads = %d, want 7 (every attempt counted)", r)
	}
}

func TestClient_ExhaustedBudgetFailsFast(t *testing.T) {
	calls := 0
	gov := NewGovernor(1, 1, (&fakeClock{t: time.Unix(0, 0)}).Now) // the only token is the write reserve
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { calls++ }, gov)

	start := time.Now()
	_, err := c.Delta(context.Background(), &SyncState{})
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("got %v, want ErrBudgetExhausted", err)
	}
	if calls != 0 || time.Since(start) > 100*time.Millisecond {
		t.Errorf("calls=%d after %v; want an immediate refusal", calls, time.Since(start))
	}
}

// When a retry finds the budget exhausted, the call ends there: no further
// backoff sleep and attempt. The backoff is pinned to 200ms, so one sleep
// (after the 503, before the refused retry) takes ~200ms; the old transport
// refusal was retried and slept a second time (~400ms).
func TestClient_NoBackoffAfterBudgetExhausted(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":{"id":"503","name":"service_unavailable","detail":"x"}}`)
	}))
	t.Cleanup(srv.Close)
	gov := NewGovernor(1, 0, (&fakeClock{t: time.Unix(0, 0)}).Now) // one token, no refill
	const backoff = 200 * time.Millisecond
	c := NewClient(testToken, testPlanID, log.New(io.Discard, "", 0), WithBaseURL(srv.URL+"/v1"),
		WithGovernor(gov), func(o *options) {
			o.retry = &ynabapi.RetryPolicy{MaxAttempts: 3, MinBackoff: backoff, MaxBackoff: backoff}
		})

	start := time.Now()
	_, _, err := c.Export(context.Background())
	el := time.Since(start)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("got %v, want ErrBudgetExhausted", err)
	}
	if calls != 1 || el >= 2*backoff {
		t.Errorf("calls=%d after %v; want 1 call and a single %v backoff", calls, el, backoff)
	}
	if strings.Contains(err.Error(), testPlanID) {
		t.Errorf("error leaks plan id: %v", err)
	}
}

func TestClient_ImportUsesWriteReserve(t *testing.T) {
	var method, path string
	gov := NewGovernor(1, 1, (&fakeClock{t: time.Unix(0, 0)}).Now) // reads refused, one write left
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"data":{"transaction_ids":["a","b"]}}`)
	}, gov)

	ids, err := c.ImportTransactions(context.Background())
	if err != nil {
		t.Fatalf("ImportTransactions: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("got %d ids, want 2", len(ids))
	}
	if want := "/v1/plans/" + testPlanID + "/transactions/import"; method != http.MethodPost || path != want {
		t.Errorf("sent %s %s, want POST %s", method, path, want)
	}
}
