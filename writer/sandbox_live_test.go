package writer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/joho/godotenv"
	ynabapi "pkg.venceslau.dev/ynab"

	"github.com/deasa/YNAB_AutoCategorizer/store"
	"github.com/deasa/YNAB_AutoCategorizer/sync"
	"github.com/deasa/YNAB_AutoCategorizer/ynab"
)

// sandboxAccount is the account the sandbox test finds or creates in the
// sandbox plan. YNAB's API can't delete accounts, so it is reused across runs.
const sandboxAccount = "finbot sandbox test"

// sandboxMaxRequests bounds one run's requests to YNAB.
const sandboxMaxRequests = 40

// sandbox holds the clients for the sandbox plan. Every request from either
// client goes through guard.
type sandbox struct {
	token, planID, liveID string
	guard                 *guardTransport
	client                *ynab.Client  // the product client the writer uses
	fx                    *ynabapi.Plan // fixture setup and read-back only
}

// newSandbox checks the plan ids before it builds anything that can send a
// request: YNAB_SANDBOX_PLAN_ID and YNAB_BUDGET_ID (the live plan, needed
// only so the guard can refuse it) must both be set and differ.
func newSandbox(getenv func(string) string, base http.RoundTripper) (*sandbox, error) {
	sb, live, token := getenv("YNAB_SANDBOX_PLAN_ID"), getenv("YNAB_BUDGET_ID"), getenv("YNAB_ACCESS_TOKEN")
	switch {
	case sb == "":
		return nil, errors.New("YNAB_SANDBOX_PLAN_ID is not set")
	case live == "":
		return nil, errors.New("YNAB_BUDGET_ID is not set (the guard needs it to refuse the live plan)")
	case sb == live:
		return nil, errors.New("YNAB_SANDBOX_PLAN_ID equals YNAB_BUDGET_ID: refusing to write to the live plan")
	case token == "":
		return nil, errors.New("YNAB_ACCESS_TOKEN is not set")
	}
	g := &guardTransport{base: base, sandbox: sb, live: live}
	return &sandbox{
		token: token, planID: sb, liveID: live, guard: g,
		client: ynab.NewClient(token, sb, log.New(io.Discard, "", 0), ynab.WithBaseTransport(g)),
		fx:     ynabapi.New(token, ynabapi.WithHTTPClient(&http.Client{Transport: g})).Plan(ynabapi.PlanID(sb)),
	}, nil
}

// guardTransport refuses any request that names the live plan or isn't under
// the sandbox plan, caps the request count, and records each PATCH body.
type guardTransport struct {
	base          http.RoundTripper
	sandbox, live string

	mu      gosync.Mutex
	n       int
	methods map[string]int
	patches [][]byte
}

func (g *guardTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.Contains(r.URL.String(), g.live) {
		return nil, errors.New("sandbox guard: request names the live plan")
	}
	prefix := "/v1/plans/" + g.sandbox
	if p := r.URL.Path; p != prefix && !strings.HasPrefix(p, prefix+"/") {
		return nil, errors.New("sandbox guard: request is not for the sandbox plan")
	}
	g.mu.Lock()
	g.n++
	if g.n > sandboxMaxRequests {
		g.mu.Unlock()
		return nil, fmt.Errorf("sandbox guard: more than %d requests", sandboxMaxRequests)
	}
	if g.methods == nil {
		g.methods = map[string]int{}
	}
	g.methods[r.Method]++
	if r.Method == http.MethodPatch && r.Body != nil {
		b, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			g.mu.Unlock()
			return nil, err
		}
		g.patches = append(g.patches, b)
		r.Body = io.NopCloser(bytes.NewReader(b))
	}
	g.mu.Unlock()
	return g.base.RoundTrip(r)
}

var uuidRE = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// scrub removes the token, both plan ids and any UUID from s.
func (s *sandbox) scrub(v any) string {
	out := fmt.Sprint(v)
	for _, secret := range []string{s.token, s.planID, s.liveID} {
		out = strings.ReplaceAll(out, secret, "<redacted>")
	}
	return uuidRE.ReplaceAllString(out, "<id>")
}

type countingTransport struct{ n int }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.n++
	return nil, errors.New("fake transport: no network in this test")
}

// TestSandboxGuard checks, with a fake transport, that a bad plan setup fails
// before any request and that the guard refuses requests outside the sandbox.
func TestSandboxGuard(t *testing.T) {
	const live, sb = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for name, vars := range map[string]map[string]string{
		"sandbox equals live": {"YNAB_SANDBOX_PLAN_ID": live, "YNAB_BUDGET_ID": live, "YNAB_ACCESS_TOKEN": "tok"},
		"sandbox unset":       {"YNAB_BUDGET_ID": live, "YNAB_ACCESS_TOKEN": "tok"},
		"live unset":          {"YNAB_SANDBOX_PLAN_ID": sb, "YNAB_ACCESS_TOKEN": "tok"},
	} {
		t.Run(name, func(t *testing.T) {
			ft := &countingTransport{}
			s, err := newSandbox(env(vars), ft)
			if err == nil || s != nil {
				t.Fatalf("newSandbox = %v, %v; want an error", s, err)
			}
			if ft.n != 0 {
				t.Errorf("%d requests sent; want 0", ft.n)
			}
		})
	}

	t.Run("guard refuses other plans", func(t *testing.T) {
		ft := &countingTransport{}
		s, err := newSandbox(env(map[string]string{"YNAB_SANDBOX_PLAN_ID": sb, "YNAB_BUDGET_ID": live, "YNAB_ACCESS_TOKEN": "tok"}), ft)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		for _, u := range []string{
			"https://api.ynab.com/v1/plans/" + live + "/transactions",
			"https://api.ynab.com/v1/plans/" + sb + "/transactions?x=" + live,
			"https://api.ynab.com/v1/plans/last-used/transactions",
			"https://api.ynab.com/v1/plans",
		} {
			req, _ := http.NewRequestWithContext(ctx, http.MethodPatch, u, nil)
			if _, err := s.guard.RoundTrip(req); err == nil || !strings.HasPrefix(err.Error(), "sandbox guard:") {
				t.Errorf("guard let a request through: %v", err)
			}
		}
		if ft.n != 0 {
			t.Errorf("%d requests reached the transport; want 0", ft.n)
		}
		// The product client too: a PATCH it builds goes only to the sandbox.
		_, _ = s.client.PatchTransactions(ctx, []ynab.TransactionChange{{ID: "t1", CategoryID: ptr("c1")}})
		if ft.n != 1 {
			t.Errorf("sandbox PATCH reached the transport %d times; want 1", ft.n)
		}
	})
}

// TestSandboxWrite proves the writer's real PATCH against the finbot-sandbox
// plan: it applies a category and reads it back, checks write_log's
// before_json and after_json, and drops a row a person edited between the
// decision and the write. Skipped unless YNAB_SANDBOX_WRITE=1.
//
//	YNAB_SANDBOX_WRITE=1 go test ./writer -run TestSandbox -v
func TestSandboxWrite(t *testing.T) {
	if os.Getenv("YNAB_SANDBOX_WRITE") != "1" {
		t.Skip("set YNAB_SANDBOX_WRITE=1 to write to the sandbox plan")
	}
	_ = godotenv.Load("../.env")
	s, err := newSandbox(os.Getenv, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	fatal := func(what string, err error) {
		t.Helper()
		t.Fatalf("%s: %s", what, s.scrub(err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Fixtures: an account, two categories, two uncategorized, unapproved
	// transactions.
	accountID := s.findOrCreateAccount(ctx, t)
	catA, catB := s.pickCategories(ctx, t)
	txnApply := s.createTxn(ctx, t, accountID)
	txnEdited := s.createTxn(ctx, t, accountID)

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "finbot.db"))
	if err != nil {
		fatal("open store", err)
	}
	t.Cleanup(func() { db.Close() })
	syncer := sync.New(db, func(planID string) sync.PlanAPI {
		if planID != s.planID {
			panic("sandbox test: sync asked for a plan other than the sandbox")
		}
		return s.client
	}, log.New(io.Discard, "", 0))
	resync := func(ctx context.Context) error { _, err := syncer.SyncPlan(ctx, s.planID); return err }

	// The decision sees both rows uncategorized.
	if err := resync(ctx); err != nil {
		fatal("initial sync", err)
	}
	for _, id := range []string{txnApply, txnEdited} {
		row, ok, err := db.TransactionByID(ctx, s.planID, id)
		if err != nil || !ok || row.CategoryID != nil || row.Approved {
			t.Fatalf("mirror before the decision: ok=%t categorized=%t approved=%t err=%s", ok, row.CategoryID != nil, row.Approved, s.scrub(err))
		}
	}

	// A person categorizes txnEdited in YNAB after the decision, before the write.
	if _, _, err := s.fx.Transactions.Update(ctx, txnEdited, ynabapi.TransactionUpdate{CategoryID: ynabapi.Set(catB)}); err != nil {
		fatal("human edit", err)
	}

	gate := NewGate()
	gate.Set(ModeAuto)
	w := New(Config{PlanID: s.planID, AutoSince: "2000-01-01", Gate: gate, API: s.client, DB: db, Resync: resync})
	out, err := w.Apply(ctx, Batch{PlanID: s.planID, Writes: []Write{
		{TxnID: txnApply, CategoryID: catA, Approve: true},
		{TxnID: txnEdited, CategoryID: catA, Approve: true},
	}}, store.ActorSystem, store.ViaScheduler)
	if err != nil {
		fatal("apply", err)
	}
	if len(out) != 2 {
		t.Fatalf("apply returned %d outcomes; want 2", len(out))
	}
	t.Logf("apply: row 1 %s %q, row 2 %s %q", out[0].Status, out[0].Reason, out[1].Status, out[1].Reason)
	if out[0].Status != StatusDone || out[0].WriteID == 0 {
		t.Errorf("applied row: status %s (%q), write id set %t; want done with a write id", out[0].Status, out[0].Reason, out[0].WriteID != 0)
	}
	if out[1].Status != StatusSkipped || out[1].Reason != ReasonCategoryChanged || out[1].WriteID != 0 {
		t.Errorf("human-edited row: %s %q, write id set %t; want skipped %q with no write id",
			out[1].Status, out[1].Reason, out[1].WriteID != 0, ReasonCategoryChanged)
	}

	// The wire shape: one PATCH carrying only the applied row's id,
	// category_id and approved.
	s.guard.mu.Lock()
	patches := s.guard.patches
	s.guard.mu.Unlock()
	if len(patches) != 1 { // the human edit is a PUT
		t.Fatalf("saw %d PATCH requests; want 1 (the writer's)", len(patches))
	}
	var body struct {
		Transactions []map[string]any `json:"transactions"`
	}
	if err := json.Unmarshal(patches[0], &body); err != nil {
		fatal("decode writer PATCH body", err)
	}
	if len(body.Transactions) != 1 {
		t.Fatalf("writer PATCH carried %d transactions; want 1", len(body.Transactions))
	}
	var keys []string
	for k := range body.Transactions[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "approved,category_id,id" {
		t.Errorf("writer PATCH fields = %v; want [approved category_id id]", keys)
	}
	if tx := body.Transactions[0]; tx["id"] != txnApply || tx["category_id"] != catA || tx["approved"] != true {
		t.Error("writer PATCH body differs from the write")
	}

	// Read back from YNAB.
	got, _, err := s.fx.Transactions.Get(ctx, txnApply)
	if err != nil {
		fatal("read back applied row", err)
	}
	if got.CategoryID == nil || *got.CategoryID != catA || !got.Approved {
		t.Errorf("YNAB after the write: category matches %t, approved %t; want the applied category, approved",
			got.CategoryID != nil && *got.CategoryID == catA, got.Approved)
	}
	got, _, err = s.fx.Transactions.Get(ctx, txnEdited)
	if err != nil {
		fatal("read back edited row", err)
	}
	if got.CategoryID == nil || *got.CategoryID != catB {
		t.Error("YNAB lost the human edit: the edited row no longer has the person's category")
	}

	// write_log: before_json and after_json for the applied row, nothing for
	// the dropped one, no intent left open.
	e, ok, err := db.LastDoneWrite(ctx, s.planID, txnApply)
	if err != nil || !ok {
		t.Fatalf("write_log for the applied row: ok=%t err=%s", ok, s.scrub(err))
	}
	assertJSON(t, s, "before_json", e.BeforeJSON, map[string]any{"category_id": nil, "approved": false})
	assertJSON(t, s, "after_json", e.AfterJSON, map[string]any{"category_id": catA, "approved": true})
	if e.Endpoint != Endpoint || e.Tier != Tier || e.RequestID == nil || e.CategoryID == nil || *e.CategoryID != catA {
		t.Error("write_log row: endpoint, tier, request id or category id not as written")
	}
	if _, ok, err := db.LastDoneWrite(ctx, s.planID, txnEdited); err != nil || ok {
		t.Errorf("write_log for the dropped row: ok=%t err=%s; want none", ok, s.scrub(err))
	}
	if open, err := db.UnfinishedWrites(ctx); err != nil || len(open) != 0 {
		t.Errorf("unfinished writes: %d, err=%s; want 0", len(open), s.scrub(err))
	}

	// The next sync brings YNAB's echo into the mirror.
	if err := resync(ctx); err != nil {
		fatal("sync after the write", err)
	}
	row, ok, err := db.TransactionByID(ctx, s.planID, txnApply)
	if err != nil || !ok || row.CategoryID == nil || *row.CategoryID != catA || !row.Approved {
		t.Errorf("mirror after the write: ok=%t category matches %t approved %t err=%s",
			ok, row.CategoryID != nil && *row.CategoryID == catA, row.Approved, s.scrub(err))
	}

	s.guard.mu.Lock()
	t.Logf("requests: %d total, by method %v", s.guard.n, s.guard.methods)
	s.guard.mu.Unlock()
}

func assertJSON(t *testing.T, s *sandbox, what string, raw json.RawMessage, want map[string]any) {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Errorf("%s: %s", what, s.scrub(err))
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %s; want %s", what, s.scrub(string(raw)), s.scrub(mustJSON(want)))
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func (s *sandbox) findOrCreateAccount(ctx context.Context, t *testing.T) string {
	t.Helper()
	accts, _, err := s.fx.Accounts.List(ctx)
	if err != nil {
		t.Fatalf("list accounts: %s", s.scrub(err))
	}
	for _, a := range accts {
		if a.Name == sandboxAccount && !a.Deleted && !a.Closed {
			t.Logf("fixture: reusing account %q", sandboxAccount)
			return a.ID
		}
	}
	a, err := s.fx.Accounts.Create(ctx, ynabapi.AccountSpec{Name: sandboxAccount, Type: ynabapi.AccountSpecTypeChecking})
	if err != nil {
		t.Fatalf("create account: %s", s.scrub(err))
	}
	t.Logf("fixture: created account %q", sandboxAccount)
	return a.ID
}

// pickCategories returns two ordinary categories: not internal, hidden,
// deleted or a credit card payment category.
func (s *sandbox) pickCategories(ctx context.Context, t *testing.T) (string, string) {
	t.Helper()
	groups, _, err := s.fx.Categories.List(ctx)
	if err != nil {
		t.Fatalf("list categories: %s", s.scrub(err))
	}
	var ids, names []string
	for _, g := range groups {
		if g.Hidden || g.Deleted || g.Name == "Internal Master Category" || g.Name == "Credit Card Payments" {
			continue
		}
		for _, c := range g.Categories {
			if c.Hidden || c.Deleted || c.Internal {
				continue
			}
			ids, names = append(ids, c.ID), append(names, c.Name)
		}
	}
	if len(ids) < 2 {
		t.Fatalf("sandbox plan has %d usable categories; want at least 2", len(ids))
	}
	t.Logf("fixture: categories %q (applied) and %q (human edit)", names[0], names[1])
	return ids[0], ids[1]
}

// createTxn adds an uncategorized, unapproved outflow dated today and deletes
// it when the test ends.
func (s *sandbox) createTxn(ctx context.Context, t *testing.T, accountID string) string {
	t.Helper()
	txn, _, err := s.fx.Transactions.Create(ctx, ynabapi.TransactionSpec{
		AccountID: accountID,
		Date:      ynabapi.Today(),
		Amount:    -1230,
		// A fresh payee each time: YNAB fills in the category it last saw for a known payee.
		PayeeName: ynabapi.Set("finbot sandbox test " + time.Now().UTC().Format("20060102T150405.000000")),
		Memo:      ynabapi.Set("finbot M3-8 sandbox test; safe to delete"),
		Approved:  ynabapi.Set(false),
	})
	if err != nil {
		t.Fatalf("create transaction: %s", s.scrub(err))
	}
	id := txn.ID
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, _, err := s.fx.Transactions.Delete(ctx, id); err != nil {
			t.Errorf("cleanup: delete test transaction: %s", s.scrub(err))
		}
	})
	return id
}
