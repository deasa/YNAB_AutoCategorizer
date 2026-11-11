package jev

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/classify"
)

const (
	testKey    = "sk-test-SECRETKEY-123"
	secretMemo = "memo-text-do-not-leak"
	secretPay  = "Payee-do-not-leak"
	policyText = "Everyday spending: dining out, restaurants, groceries, Amazon orders and general shopping."
)

var cands = []classify.Candidate{
	{Name: "Discretionary", Group: "Everyday", Description: policyText},
	{Name: "Fuel", Group: "Auto", Description: "Gas for the cars"},
	{Name: "Mortgage", Group: "Fixed"},
}

func txns(n int) []classify.Txn {
	out := make([]classify.Txn, n)
	for i := range out {
		out[i] = classify.Txn{
			ID: fmt.Sprintf("id-%d", i), Payee: fmt.Sprintf("%s %d", secretPay, i), Memo: secretMemo,
			Account: "Checking", Date: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), AmountMilli: -12340,
			PayeeHistory: []string{"Fuel", "Discretionary"},
		}
	}
	return out
}

// fake is a Jev stand-in. answer builds the response for each question id; status
// lists status codes for successive calls (200 once exhausted).
type fake struct {
	t      *testing.T
	calls  atomic.Int32
	status []int
	model  string
	answer func(id string) any
	last   request
	auth   string
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := int(f.calls.Add(1)) - 1
	f.auth = r.Header.Get("Authorization")
	raw, _ := io.ReadAll(r.Body)
	f.last = request{} // Unmarshal into a used map would merge stale keys
	if err := json.Unmarshal(raw, &f.last); err != nil {
		f.t.Errorf("request body: %v", err)
	}
	if n < len(f.status) && f.status[n] != 200 {
		w.WriteHeader(f.status[n])
		// An error body echoing the request, as a hostile or verbose server might.
		w.Write(raw)
		return
	}
	model := f.model
	if model == "" {
		model = PinnedModel
	}
	ans := map[string]any{}
	for id := range f.last.Questions {
		if a := f.answer(id); a != nil {
			ans[id] = a
		}
	}
	json.NewEncoder(w).Encode(map[string]any{"model": model, "answers": ans, "usage": map[string]int{"input_tokens": 1}})
}

func fuelAnswer(string) any {
	return map[string]any{"type": "choice", "choice": "Fuel", "confidence": 0.7,
		"probabilities": map[string]float64{"Fuel": 0.7, "Discretionary": 0.2, "Mortgage": 0.1}}
}

func newClient(t *testing.T, f *fake, opts ...Option) *Client {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	base := []Option{WithEndpoint(srv.URL), WithHTTP(srv.Client()), WithRetryDelay(0), WithTimeout(2 * time.Second)}
	c, err := New(testKey, append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClassify_RequestShape(t *testing.T) {
	f := &fake{answer: fuelAnswer}
	c := newClient(t, f)
	preds, err := c.Classify(context.Background(), txns(2), cands)
	if err != nil {
		t.Fatal(err)
	}
	r := f.last
	if r.Model != PinnedModel {
		t.Errorf("model = %q, want %q", r.Model, PinnedModel)
	}
	if f.auth != "Bearer "+testKey {
		t.Errorf("auth header not set")
	}
	if len(r.Questions) != 2 || len(r.State.Transactions) != 2 {
		t.Fatalf("questions %d, transactions %d", len(r.Questions), len(r.State.Transactions))
	}
	s := r.State.Transactions["t01"]
	if got := strings.Join(s.PayeePreviousCategories, ","); got != "Fuel,Discretionary" {
		t.Errorf("history = %q", got)
	}
	if s.Memo != secretMemo || s.Payee != secretPay+" 0" || s.Account != "Checking" {
		t.Errorf("state = %+v", s)
	}
	if s.Amount != "$12.34 outflow (money spent)" || s.Date != "2026-11-09 (Monday)" {
		t.Errorf("amount %q date %q", s.Amount, s.Date)
	}
	q := r.Questions["t01"]
	if q.Type != "choice" || !strings.Contains(q.Instructions, "`transactions.t01`") {
		t.Errorf("question = %+v", q)
	}
	if strings.Contains(q.Instructions, secretPay) || strings.Contains(q.Instructions, secretMemo) {
		t.Error("payee or memo leaked into the instructions")
	}
	if got := q.Criteria["Discretionary"]; got != "Group: Everyday. "+policyText {
		t.Errorf("policy criterion = %q", got)
	}
	if got := q.Criteria["Mortgage"]; got != "Group: Fixed." {
		t.Errorf("empty-description criterion = %q", got)
	}
	for _, p := range preds {
		if p.Model != PinnedModel || len(p.Probs) != 3 || strings.Join(p.Top, ",") != "Fuel,Discretionary,Mortgage" {
			t.Errorf("prediction = %+v", p)
		}
	}
}

func TestClassify_OmitsEmptyMemoAndHistory(t *testing.T) {
	f := &fake{answer: fuelAnswer}
	c := newClient(t, f)
	tx := txns(1)
	tx[0].Memo, tx[0].PayeeHistory = "", nil
	if _, err := c.Classify(context.Background(), tx, cands); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(f.last.State)
	if strings.Contains(string(raw), "memo") || strings.Contains(string(raw), "payee_previous_categories") {
		t.Errorf("state has empty fields: %s", raw)
	}
}

func TestClassify_BatchesInOrder(t *testing.T) {
	// Each transaction's top pick encodes its position, so order is checkable.
	names := []string{"Discretionary", "Fuel", "Mortgage"}
	f := &fake{}
	f.answer = func(id string) any {
		var i int
		fmt.Sscanf(id, "t%d", &i)
		pick := names[(int(f.calls.Load()-1)*10+i-1)%3] // calls is already counted for this request
		probs := map[string]float64{"Discretionary": 0.1, "Fuel": 0.1, "Mortgage": 0.1}
		probs[pick] = 0.8
		return map[string]any{"type": "choice", "choice": pick, "probabilities": probs}
	}
	c := newClient(t, f, WithBatchSize(10))
	preds, err := c.Classify(context.Background(), txns(23), cands)
	if err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", f.calls.Load())
	}
	if len(preds) != 23 {
		t.Fatalf("preds = %d", len(preds))
	}
	for i, p := range preds {
		if p.Top[0] != names[i%3] {
			t.Errorf("pred %d top = %s, want %s", i, p.Top[0], names[i%3])
		}
	}
}

func TestClassify_RetryOnce(t *testing.T) {
	f := &fake{status: []int{503}, answer: fuelAnswer}
	c := newClient(t, f)
	if _, err := c.Classify(context.Background(), txns(1), cands); err != nil {
		t.Fatalf("a single 503 should be retried: %v", err)
	}
	if f.calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", f.calls.Load())
	}
}

func TestClassify_NoRetryOn4xx(t *testing.T) {
	f := &fake{status: []int{400}, answer: fuelAnswer}
	c := newClient(t, f)
	if _, err := c.Classify(context.Background(), txns(1), cands); err == nil {
		t.Fatal("want error")
	}
	if f.calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", f.calls.Load())
	}
}

func TestClassify_TimeoutRetriedThenFails(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.ReadAll(r.Body)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release) // runs first, so Close isn't left waiting on the handler
	c, _ := New(testKey, WithEndpoint(srv.URL), WithHTTP(srv.Client()), WithRetryDelay(0),
		WithTimeout(50*time.Millisecond))
	_, err := c.Classify(context.Background(), txns(1), cands)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
}

// errorCases are the responses that must fail the batch.
func errorCases() map[string]*fake {
	return map[string]*fake{
		"500 after retry": {status: []int{500, 500}, answer: fuelAnswer},
		"wrong model":     {model: "jev-1.15.0", answer: fuelAnswer},
		"missing id": {answer: func(id string) any {
			if id == "t02" {
				return nil
			}
			return fuelAnswer(id)
		}},
		"unknown category choice": {answer: func(string) any {
			return map[string]any{"type": "choice", "choice": "Groceries",
				"probabilities": map[string]float64{"Groceries": 0.9, "Fuel": 0.1}}
		}},
		"unknown category probability": {answer: func(string) any {
			return map[string]any{"type": "choice", "choice": "Fuel",
				"probabilities": map[string]float64{"Fuel": 0.6, "Groceries": 0.4}}
		}},
		"no probabilities": {answer: func(string) any {
			return map[string]any{"type": "choice", "choice": "Fuel"}
		}},
	}
}

func TestClassify_Errors(t *testing.T) {
	for name, f := range errorCases() {
		t.Run(name, func(t *testing.T) {
			c := newClient(t, f)
			preds, err := c.Classify(context.Background(), txns(3), cands)
			if err == nil {
				t.Fatalf("want error, got %d predictions", len(preds))
			}
			if preds != nil {
				t.Errorf("predictions returned alongside error")
			}
			if name == "500 after retry" && f.calls.Load() != 2 {
				t.Errorf("calls = %d, want 2 (one retry)", f.calls.Load())
			}
		})
	}
}

func TestClassify_ErrorsCarryNoSecrets(t *testing.T) {
	cases := errorCases()
	cases["400 echoing body"] = &fake{status: []int{400}, answer: fuelAnswer}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			c := newClient(t, f)
			_, err := c.Classify(context.Background(), txns(3), cands)
			if err == nil {
				t.Fatal("want error")
			}
			for _, s := range []string{testKey, secretMemo, secretPay, "id-0", "id-1"} {
				if strings.Contains(err.Error(), s) {
					t.Errorf("error %q contains %q", err, s)
				}
			}
		})
	}
	// Transport failure: unreachable endpoint whose URL is plain.
	c, _ := New(testKey, WithEndpoint("http://127.0.0.1:1/v1"), WithRetryDelay(0))
	_, err := c.Classify(context.Background(), txns(1), cands)
	if err == nil || strings.Contains(err.Error(), testKey) || strings.Contains(err.Error(), secretMemo) {
		t.Errorf("transport err = %v", err)
	}
}

func TestClassify_BadInputs(t *testing.T) {
	c, _ := New(testKey)
	if _, err := New(" "); err == nil {
		t.Error("empty key accepted")
	}
	if _, err := c.Classify(context.Background(), txns(1), nil); err == nil {
		t.Error("no candidates accepted")
	}
	dup := append([]classify.Candidate{}, cands...)
	dup = append(dup, classify.Candidate{Name: "Fuel"})
	if _, err := c.Classify(context.Background(), txns(1), dup); err == nil {
		t.Error("duplicate candidate accepted")
	}
	if p, err := c.Classify(context.Background(), nil, cands); err != nil || p != nil {
		t.Errorf("empty batch: %v %v", p, err)
	}
}

func TestClassify_ContextCanceled(t *testing.T) {
	f := &fake{answer: fuelAnswer}
	c := newClient(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Classify(ctx, txns(1), cands); err == nil {
		t.Fatal("want error")
	}
}
