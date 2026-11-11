package ynab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
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

// PatchTransactions sends one bulk PATCH holding only the fields each change
// sets, spends the write reserve, and returns YNAB's per-transaction answer.
func TestClient_PatchTransactionsSendsOnlyChangedFields(t *testing.T) {
	var method, path string
	var body map[string][]map[string]json.RawMessage
	gov := NewGovernor(1, 1, (&fakeClock{t: time.Unix(0, 0)}).Now) // reads refused, one write left
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		io.WriteString(w, `{"data":{"transaction_ids":["t1"],"server_knowledge":7,"transactions":[
 {"id":"t1","date":"2026-10-28","amount":-5000,"cleared":"cleared","approved":true,"account_id":"a1","category_id":"c1","deleted":false}]}}`)
	}, gov)

	cat, yes := "c1", true
	got, err := c.PatchTransactions(context.Background(), []TransactionChange{
		{ID: "t1", CategoryID: &cat, Approved: &yes},
		{ID: "t2", Approved: &yes},
	})
	if err != nil {
		t.Fatalf("PatchTransactions: %v", err)
	}
	if want := "/v1/plans/" + testPlanID + "/transactions"; method != http.MethodPatch || path != want {
		t.Errorf("sent %s %s, want PATCH %s", method, path, want)
	}
	txns := body["transactions"]
	if len(body) != 1 || len(txns) != 2 {
		t.Fatalf("body = %v, want only transactions with 2 entries", body)
	}
	wantKeys := [][]string{{"approved", "category_id", "id"}, {"approved", "id"}}
	for i, tx := range txns {
		var keys []string
		for k := range tx {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, wantKeys[i]) {
			t.Errorf("patch %d sends %v, want %v", i, keys, wantKeys[i])
		}
	}
	if len(got) != 1 || got[0].ID != "t1" || got[0].CategoryID == nil || *got[0].CategoryID != "c1" || !got[0].Approved {
		t.Errorf("got %+v", got)
	}
}

func TestClient_PatchTransactionsRejectsEmpty(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("request sent") }, fullGovernor())
	if _, err := c.PatchTransactions(context.Background(), nil); err == nil {
		t.Error("empty batch: want error")
	}
	if _, err := c.PatchTransactions(context.Background(), []TransactionChange{{ID: "t1"}}); err == nil {
		t.Error("no field set: want error")
	}
}

// Every client method on every way a call can fail: the error must carry
// neither the plan id nor a transaction id. Each failure puts both ids where
// that path would pick them up, so a pass means they were scrubbed.
func TestClient_NoErrorPathLeaksIDs(t *testing.T) {
	const testTxnID = "99999999-8888-7777-6666-000000000001"
	echo := fmt.Sprintf("plan %s txn %s", testPlanID, testTxnID)

	respond := func(status int, body string) http.RoundTripper {
		return roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Retry-After": {"3600"}, "Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    r,
			}, nil
		})
	}
	apiErr := func(status int, id, name string) http.RoundTripper {
		return respond(status, fmt.Sprintf(`{"error":{"id":%q,"name":%q,"detail":%q}}`, id, name, echo))
	}
	exhausted := func(t *testing.T) *Governor {
		gov := NewGovernor(1, 0, (&fakeClock{t: time.Unix(0, 0)}).Now)
		if err := gov.Take(false); err != nil {
			t.Fatal(err)
		}
		return gov
	}

	failures := []struct {
		name  string
		base  http.RoundTripper
		gov   func(*testing.T) *Governor
		check func(*testing.T, error) // proves this path is the one that ran
	}{
		{name: "budget refused", gov: exhausted, check: func(t *testing.T, err error) {
			if !errors.Is(err, ErrBudgetExhausted) {
				t.Errorf("got %v, want ErrBudgetExhausted", err)
			}
		}},
		{name: "not found", base: apiErr(http.StatusNotFound, "404.2", "resource_not_found"), check: func(t *testing.T, err error) {
			if !errors.Is(err, ynabapi.ErrNotFound) {
				t.Errorf("got %v, want ErrNotFound", err)
			}
			if !strings.Contains(err.Error(), "{plan}") || !strings.Contains(err.Error(), "{id}") {
				t.Errorf("echoed ids were not scrubbed in place: %v", err)
			}
		}},
		{name: "server error", base: apiErr(http.StatusInternalServerError, "500", "internal_server_error")},
		{name: "rate limited", base: apiErr(http.StatusTooManyRequests, "429", "too_many_requests"), check: func(t *testing.T, err error) {
			var rl *RateLimitError
			if !errors.As(err, &rl) {
				t.Errorf("got %T %v, want *RateLimitError", err, err)
			}
		}},
		{name: "undecodable body", base: respond(http.StatusOK, "not json: "+echo)},
		{name: "transport failure", base: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("connection reset: " + echo)
		})},
	}
	calls := []struct {
		name string
		call func(*Client) error
	}{
		{"Export", func(c *Client) error { _, _, err := c.Export(context.Background()); return err }},
		{"Delta", func(c *Client) error { _, err := c.Delta(context.Background(), &SyncState{}); return err }},
		{"Plans", func(c *Client) error { _, err := c.Plans(context.Background()); return err }},
		{"GetUncategorizedTransactions", func(c *Client) error { _, err := c.GetUncategorizedTransactions(); return err }},
		{"GetCategories", func(c *Client) error { _, err := c.GetCategories(context.Background()); return err }},
		{"ImportTransactions", func(c *Client) error { _, err := c.ImportTransactions(context.Background()); return err }},
		{"PatchTransactions", func(c *Client) error {
			cat := testTxnID
			_, err := c.PatchTransactions(context.Background(), []TransactionChange{{ID: testTxnID, CategoryID: &cat}})
			return err
		}},
	}

	for _, f := range failures {
		for _, m := range calls {
			t.Run(f.name+"/"+m.name, func(t *testing.T) {
				gov := fullGovernor()
				if f.gov != nil {
					gov = f.gov(t)
				}
				base := f.base
				if base == nil {
					base = roundTripperFunc(func(*http.Request) (*http.Response, error) {
						t.Error("request sent past an exhausted budget")
						return nil, errors.New("unreachable")
					})
				}
				c := NewClient(testToken, testPlanID, log.New(io.Discard, "", 0), WithBaseTransport(base),
					WithGovernor(gov), func(o *options) { o.retry = &ynabapi.RetryPolicy{MaxAttempts: 1} })

				err := m.call(c)
				if err == nil {
					t.Fatal("want error")
				}
				for _, id := range []string{testPlanID, testTxnID} {
					if strings.Contains(err.Error(), id) {
						t.Errorf("error leaks %q: %v", id, err)
					}
				}
				if f.check != nil {
					f.check(t, err)
				}
			})
		}
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNotApplied(t *testing.T) {
	cases := []struct {
		status int
		id     string
		want   bool
	}{
		{http.StatusBadRequest, "400", true},
		{http.StatusNotFound, "404.2", true},
		{http.StatusConflict, "409", true},
		{http.StatusTooManyRequests, "429", true},
		{http.StatusInternalServerError, "500", false},
		{http.StatusServiceUnavailable, "503", false},
	}
	for _, tc := range cases {
		c := NewClient(testToken, testPlanID, log.New(io.Discard, "", 0), WithGovernor(fullGovernor()),
			WithBaseTransport(roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Retry-After": {"3600"}},
					Body: io.NopCloser(strings.NewReader(`{"error":{"id":"` + tc.id + `","name":"x","detail":"x"}}`)), Request: r}, nil
			})), func(o *options) { o.retry = &ynabapi.RetryPolicy{MaxAttempts: 1} })
		yes := true
		_, err := c.PatchTransactions(context.Background(), []TransactionChange{{ID: "t1", Approved: &yes}})
		if err == nil || NotApplied(err) != tc.want {
			t.Errorf("%d: err %v, NotApplied = %v, want %v", tc.status, err, NotApplied(err), tc.want)
		}
	}
	if !NotApplied(fmt.Errorf("x: %w", ErrBudgetExhausted)) {
		t.Error("budget exhausted: want NotApplied")
	}
	if NotApplied(errors.New("connection reset")) {
		t.Error("transport error: want unknown")
	}
}
