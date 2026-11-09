package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	finauth "github.com/deasa/YNAB_AutoCategorizer/auth"
	"github.com/deasa/YNAB_AutoCategorizer/store"
)

const decisionsTool = "get_category_decisions"

func fptr(f float64) *float64 { return &f }

func modelDecision(plan, txn, category string, at time.Time) store.Decision {
	return store.Decision{
		PlanID: plan, TxnID: txn, CategoryID: ptr("id-" + category), CategoryName: ptr(category),
		Confidence: fptr(0.8), RunnerUp: ptr("Dining"), Method: store.MethodModel, Model: "model-a",
		PolicyHash: "policy", CandidatesHash: "candidates", Mode: store.ModeDryRun, Outcome: store.OutcomeWouldApply,
		Features: json.RawMessage(`{"payee":"secret-feature"}`), CreatedAt: at,
	}
}

func insertDecisions(t *testing.T, o Options, decs ...store.Decision) {
	t.Helper()
	for _, d := range decs {
		if _, err := o.DB.InsertDecision(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
}

func decisionsSession(t *testing.T, o Options) *sdkmcp.ClientSession {
	t.Helper()
	if err := o.DB.SetSyncState(context.Background(), store.SyncState{PlanID: planID, ServerKnowledge: 1, SyncedAt: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHandler(o))
	t.Cleanup(srv.Close)
	return connect(t, srv.URL, "good")
}

func decisionCategories(ds []decisionEntry) []string {
	out := []string{}
	for _, d := range ds {
		out = append(out, d.Category)
	}
	return out
}

// The last two share a timestamp, so insertion order must break the tie.
func TestCategoryDecisions_NewestFirstWithSuperseded(t *testing.T) {
	o, _ := testOptions(t)
	first := modelDecision(planID, "t1", "Groceries", now.Add(-2*time.Hour))
	second := modelDecision(planID, "t1", "Dining", now.Add(-time.Hour))
	third := modelDecision(planID, "t1", "Fun", now.Add(-time.Hour))
	third.Mode, third.Outcome, third.Model = store.ModeAuto, store.OutcomeApplied, "model-b"
	third.SecondOpinion = json.RawMessage(`{"model":"reviewer","category":"Fun","agrees":true}`)
	insertDecisions(t, o, first, second, third, modelDecision(planID, "t2", "Elsewhere", now))

	r := call[decisionsData](t, decisionsSession(t, o), decisionsTool, map[string]any{"transaction_id": "t1"})
	if r.Data.TransactionID != "t1" {
		t.Errorf("transaction_id = %q", r.Data.TransactionID)
	}
	if got, want := decisionCategories(r.Data.Decisions), []string{"Fun", "Dining", "Groceries"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	top := r.Data.Decisions[0]
	if top.Category != "Fun" || top.CategoryID != "id-Fun" || *top.Confidence != 0.8 || top.RunnerUp != "Dining" ||
		top.Method != store.MethodModel || top.Model != "model-b" || top.Mode != store.ModeAuto ||
		top.Outcome != store.OutcomeApplied || top.CreatedAt != now.Add(-time.Hour).Format(time.RFC3339) {
		t.Errorf("newest decision: %+v", top)
	}
	want := map[string]any{"model": "reviewer", "category": "Fun", "agrees": true}
	if !reflect.DeepEqual(top.SecondOpinion, any(want)) {
		t.Errorf("second_opinion = %#v", top.SecondOpinion)
	}
	if r.Data.Decisions[1].SecondOpinion != nil {
		t.Errorf("second_opinion should be absent: %#v", r.Data.Decisions[1].SecondOpinion)
	}
}

// A skip has no category, confidence or runner-up, and a rule has no model.
// Those stay absent, and a real confidence of zero stays present.
func TestCategoryDecisions_SkipAndZeroConfidence(t *testing.T) {
	o, _ := testOptions(t)
	zero := modelDecision(planID, "t1", "Groceries", now.Add(-time.Hour))
	zero.Confidence = fptr(0)
	skip := store.Decision{PlanID: planID, TxnID: "t1", Method: store.MethodRule, Mode: store.ModeDryRun,
		Outcome: store.OutcomeSkipped, Features: json.RawMessage(`{}`), CreatedAt: now}
	insertDecisions(t, o, zero, skip)

	res, err := decisionsSession(t, o).CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name: decisionsTool, Arguments: map[string]any{"transaction_id": "t1"}})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	r := decode[decisionsData](t, res)
	if len(r.Data.Decisions) != 2 {
		t.Fatalf("decisions: %+v", r.Data.Decisions)
	}
	s := r.Data.Decisions[0]
	if s.Category != "" || s.CategoryID != "" || s.Confidence != nil || s.RunnerUp != "" || s.Model != "" || s.Outcome != store.OutcomeSkipped {
		t.Errorf("skip: %+v", s)
	}
	if c := r.Data.Decisions[1].Confidence; c == nil || *c != 0 {
		t.Errorf("zero confidence lost: %v", c)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	for _, leak := range []string{"secret-feature", "policy", "candidates"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("response carries %q: %s", leak, raw)
		}
	}
}

func TestCategoryDecisions_OnlyActivePlan(t *testing.T) {
	o, _ := testOptions(t)
	insertDecisions(t, o,
		modelDecision(planID, "shared", "Mine", now),
		modelDecision("other-plan", "shared", "Theirs", now.Add(time.Minute)),
		modelDecision("other-plan", "only-other", "Theirs", now),
	)
	s := decisionsSession(t, o)
	r := call[decisionsData](t, s, decisionsTool, map[string]any{"transaction_id": "shared"})
	if got, want := decisionCategories(r.Data.Decisions), []string{"Mine"}; !slices.Equal(got, want) {
		t.Errorf("shared id: %v, want %v", got, want)
	}
	r = call[decisionsData](t, s, decisionsTool, map[string]any{"transaction_id": "only-other"})
	if len(r.Data.Decisions) != 0 {
		t.Errorf("another plan's decisions appeared: %+v", r.Data.Decisions)
	}
}

func TestCategoryDecisions_UndecidedIsEmptyWithFreshness(t *testing.T) {
	o, _ := testOptions(t)
	res, err := decisionsSession(t, o).CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name: decisionsTool, Arguments: map[string]any{"transaction_id": "never-decided"}})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(raw), `"decisions":[]`) {
		t.Errorf("want an empty list, not null: %s", raw)
	}
	r := decode[decisionsData](t, res)
	if r.DataFreshness.LastSyncedAt != now.Add(-time.Minute).Format(time.RFC3339) || r.DataFreshness.Stale {
		t.Errorf("freshness: %+v", r.DataFreshness)
	}
}

func TestCategoryDecisions_MalformedInput(t *testing.T) {
	o, logs := testOptions(t)
	s := decisionsSession(t, o)
	for name, id := range map[string]string{
		"empty":      "",
		"blank":      "   ",
		"too long":   strings.Repeat("a", maxTransactionIDLen+1),
		"whitespace": "abc def",
		"control":    "abc\x00def",
	} {
		msg := callErr(t, s, decisionsTool, map[string]any{"transaction_id": id})
		if msg != errBadTransactionID {
			t.Errorf("%s: %q", name, msg)
		}
	}
	// A longest-allowed id is well formed, so it's simply undecided.
	r := call[decisionsData](t, s, decisionsTool, map[string]any{"transaction_id": strings.Repeat("a", maxTransactionIDLen)})
	if len(r.Data.Decisions) != 0 {
		t.Errorf("max length id: %+v", r.Data.Decisions)
	}
	if !strings.Contains(logs.String(), "mcp tool refused input") {
		t.Errorf("logs: %s", logs)
	}
}

// A missing argument never reaches the tool: the SDK's schema check refuses it.
func TestCategoryDecisions_MissingArgument(t *testing.T) {
	o, _ := testOptions(t)
	if msg := callErr(t, decisionsSession(t, o), decisionsTool, map[string]any{}); !strings.Contains(msg, "transaction_id") {
		t.Errorf("%q", msg)
	}
}

func TestCategoryDecisions_NeedsValidToken(t *testing.T) {
	o, _ := testOptions(t)
	insertDecisions(t, o, modelDecision(planID, "t1", "Groceries", now))
	srv := httptest.NewServer(NewHandler(o))
	defer srv.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + decisionsTool + `","arguments":{"transaction_id":"t1"}}}`
	for _, c := range []struct {
		token string
		want  int
	}{{"", 401}, {"bad", 401}, {"noscope", 403}} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("token %q: status %d, want %d", c.token, resp.StatusCode, c.want)
		}
		if strings.Contains(string(got), "Groceries") {
			t.Errorf("token %q: body carries a decision: %s", c.token, got)
		}
	}

	if names := toolNames(t, connect(t, srv.URL, "good")); !slices.Contains(names, decisionsTool) {
		t.Errorf("a valid token doesn't list the tool: %v", names)
	}
	for _, d := range registry {
		if d.name == decisionsTool && d.scope != finauth.ScopeMCP {
			t.Errorf("scope = %q", d.scope)
		}
	}
}
