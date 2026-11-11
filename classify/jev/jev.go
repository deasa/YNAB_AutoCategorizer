// Package jev is a classify.Model backed by TypeSafe's Jev (architecture §3.3,
// docs/build/spikes/jev.md). The model id is pinned: a response from any other
// version fails the batch, because τ is calibrated per version.
//
// Payee, memo, account and history travel only as values in the request's
// state; the question wording and criteria are fixed text plus the candidate
// list. Errors never carry the API key, payee, memo or a transaction id.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/classify"
)

const (
	// PinnedModel is the only Jev version this client accepts. Never jev-latest.
	PinnedModel = "jev-1.14.0"
	// DefaultEndpoint is Jev's System One endpoint.
	DefaultEndpoint = "https://api.typesafe.ai/v1/systemone"
	// DefaultBatchSize is how many transactions go in one request (spike: 10).
	DefaultBatchSize = 10
	// DefaultTimeout bounds one HTTP attempt.
	DefaultTimeout = 30 * time.Second
	// DefaultRetryDelay is the pause before the single retry.
	DefaultRetryDelay = time.Second

	maxBody = 4 << 20 // response cap
)

// Doer sends one HTTP request; *http.Client satisfies it.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client classifies transactions with Jev. Build it with New.
type Client struct {
	key        string
	http       Doer
	endpoint   string
	batchSize  int
	timeout    time.Duration
	retryDelay time.Duration
}

var _ classify.Model = (*Client)(nil)

// Option configures a Client.
type Option func(*Client)

// WithHTTP replaces the HTTP client (tests pass a fake or an httptest client).
func WithHTTP(d Doer) Option { return func(c *Client) { c.http = d } }

// WithEndpoint replaces the API URL.
func WithEndpoint(u string) Option { return func(c *Client) { c.endpoint = u } }

// WithBatchSize sets transactions per request (values below 1 are ignored).
func WithBatchSize(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.batchSize = n
		}
	}
}

// WithTimeout sets the per-attempt timeout.
func WithTimeout(d time.Duration) Option { return func(c *Client) { c.timeout = d } }

// WithRetryDelay sets the pause before the retry.
func WithRetryDelay(d time.Duration) Option { return func(c *Client) { c.retryDelay = d } }

// New returns a Client using apiKey. An empty key is an error.
func New(apiKey string, opts ...Option) (*Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("jev: empty API key")
	}
	c := &Client{
		key:        apiKey,
		http:       &http.Client{},
		endpoint:   DefaultEndpoint,
		batchSize:  DefaultBatchSize,
		timeout:    DefaultTimeout,
		retryDelay: DefaultRetryDelay,
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// Classify returns one Prediction per txn, in order. Any failed or malformed
// batch fails the whole call: the caller retries next sync (§3.2 step 5).
func (c *Client) Classify(ctx context.Context, txns []classify.Txn, cands []classify.Candidate) ([]classify.Prediction, error) {
	if len(txns) == 0 {
		return nil, nil
	}
	crit, err := criteria(cands)
	if err != nil {
		return nil, err
	}
	out := make([]classify.Prediction, 0, len(txns))
	for start := 0; start < len(txns); start += c.batchSize {
		end := min(start+c.batchSize, len(txns))
		preds, err := c.batch(ctx, txns[start:end], crit)
		if err != nil {
			return nil, fmt.Errorf("jev: batch at %d: %w", start, err)
		}
		out = append(out, preds...)
	}
	return out, nil
}

// ---- request ----

const instructions = "Which budget category does the transaction in `transactions.%s` belong to?"

type question struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

type request struct {
	Model     string              `json:"model"`
	State     state               `json:"state"`
	Questions map[string]question `json:"questions"`
}

type state struct {
	Transactions map[string]txnState `json:"transactions"`
}

type txnState struct {
	Payee                   string   `json:"payee"`
	Amount                  string   `json:"amount"`
	Date                    string   `json:"date,omitempty"`
	Account                 string   `json:"account"`
	Memo                    string   `json:"memo,omitempty"`                      // omitted when empty
	PayeePreviousCategories []string `json:"payee_previous_categories,omitempty"` // omitted when none: an empty list reads as a signal
}

// criteria maps each candidate name to "Group: <group>. <description>".
func criteria(cands []classify.Candidate) (map[string]string, error) {
	if len(cands) == 0 {
		return nil, errors.New("jev: no candidates")
	}
	m := make(map[string]string, len(cands))
	for _, c := range cands {
		if c.Name == "" {
			return nil, errors.New("jev: candidate with empty name")
		}
		if _, dup := m[c.Name]; dup {
			return nil, fmt.Errorf("jev: duplicate candidate %q", c.Name)
		}
		d := "Group: " + c.Group + "."
		if s := strings.TrimSpace(c.Description); s != "" {
			d += " " + s
		}
		m[c.Name] = d
	}
	return m, nil
}

func qid(i int) string { return fmt.Sprintf("t%02d", i+1) }

func buildRequest(txns []classify.Txn, crit map[string]string) request {
	r := request{
		Model:     PinnedModel,
		State:     state{Transactions: make(map[string]txnState, len(txns))},
		Questions: make(map[string]question, len(txns)),
	}
	for i, t := range txns {
		id := qid(i)
		s := txnState{Payee: t.Payee, Amount: money(t.AmountMilli), Account: t.Account, Memo: t.Memo}
		if !t.Date.IsZero() {
			s.Date = t.Date.Format("2006-01-02") + " (" + t.Date.Weekday().String() + ")"
		}
		if len(t.PayeeHistory) > 0 {
			s.PayeePreviousCategories = t.PayeeHistory
		}
		r.State.Transactions[id] = s
		r.Questions[id] = question{Type: "choice", Instructions: fmt.Sprintf(instructions, id), Criteria: crit}
	}
	return r
}

func money(milli int64) string {
	dir := "outflow (money spent)"
	if milli > 0 {
		dir = "inflow (money received)"
	}
	return fmt.Sprintf("$%.2f %s", math.Abs(float64(milli))/1000, dir)
}

// ---- response ----

type answer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
}

type response struct {
	Model   string            `json:"model"`
	Answers map[string]answer `json:"answers"`
}

func (c *Client) batch(ctx context.Context, txns []classify.Txn, crit map[string]string) ([]classify.Prediction, error) {
	body, err := json.Marshal(buildRequest(txns, crit))
	if err != nil {
		return nil, errors.New("encode request failed")
	}
	raw, err := c.post(ctx, body)
	if err != nil {
		return nil, err
	}
	var resp response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, errors.New("malformed response")
	}
	return predictions(resp, len(txns), crit)
}

// predictions validates resp against the batch and turns it into Predictions.
func predictions(resp response, n int, crit map[string]string) ([]classify.Prediction, error) {
	if resp.Model != PinnedModel {
		return nil, fmt.Errorf("response model %q, want %q", clip(resp.Model, 40), PinnedModel)
	}
	if len(resp.Answers) != n {
		return nil, fmt.Errorf("got %d answers for %d transactions", len(resp.Answers), n)
	}
	out := make([]classify.Prediction, n)
	for i := range n {
		a, ok := resp.Answers[qid(i)]
		if !ok {
			return nil, fmt.Errorf("no answer for transaction %d of the batch", i+1)
		}
		if a.Type != "" && a.Type != "choice" {
			return nil, fmt.Errorf("transaction %d: answer type is not choice", i+1)
		}
		if _, ok := crit[a.Choice]; !ok {
			return nil, fmt.Errorf("transaction %d: choice is not a candidate", i+1)
		}
		if len(a.Probabilities) == 0 {
			return nil, fmt.Errorf("transaction %d: no probabilities", i+1)
		}
		probs := make(map[string]float64, len(crit))
		for name := range crit {
			probs[name] = 0
		}
		sum := 0.0
		for name, p := range a.Probabilities {
			if _, ok := crit[name]; !ok {
				return nil, fmt.Errorf("transaction %d: probability for a category that is not a candidate", i+1)
			}
			if math.IsNaN(p) || p < 0 || p > 1 {
				return nil, fmt.Errorf("transaction %d: probability out of range", i+1)
			}
			probs[name] = p
			sum += p
		}
		if sum < 0.9 || sum > 1.1 {
			return nil, fmt.Errorf("transaction %d: probabilities sum to %.3f", i+1, sum)
		}
		out[i] = classify.Prediction{Probs: probs, Top: ranked(probs), Model: resp.Model}
	}
	return out, nil
}

// ranked orders names by probability, highest first; ties by name.
func ranked(p map[string]float64) []string {
	names := make([]string, 0, len(p))
	for n := range p {
		names = append(names, n)
	}
	sort.Slice(names, func(a, b int) bool {
		if p[names[a]] != p[names[b]] {
			return p[names[a]] > p[names[b]]
		}
		return names[a] < names[b]
	})
	return names
}

// ---- transport ----

// errRetry marks an attempt worth one retry (5xx or timeout).
type errRetry struct{ error }

// post sends body, retrying once on 5xx or a per-attempt timeout.
func (c *Client) post(ctx context.Context, body []byte) ([]byte, error) {
	raw, err := c.attempt(ctx, body)
	var r errRetry
	if err == nil || !errors.As(err, &r) {
		return raw, err
	}
	if c.retryDelay > 0 {
		t := time.NewTimer(c.retryDelay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
	raw, err = c.attempt(ctx, body)
	if err != nil {
		return nil, fmt.Errorf("after retry: %w", err)
	}
	return raw, nil
}

func (c *Client) attempt(parent context.Context, body []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("build request failed")
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.transportErr(parent, ctx, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, c.transportErr(parent, ctx, err)
	}
	// The body is never quoted: an error echo could carry payee or memo text.
	switch {
	case resp.StatusCode >= 500:
		return nil, errRetry{fmt.Errorf("status %d", resp.StatusCode)}
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	case len(raw) > maxBody:
		return nil, errors.New("response too large")
	}
	return raw, nil
}

// transportErr reports a failed attempt without the URL's query, headers or body.
func (c *Client) transportErr(parent, ctx context.Context, err error) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	if ctx.Err() != nil || isTimeout(err) {
		return errRetry{errors.New("request timed out")}
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Errorf("request failed: %s", c.redact(err.Error()))
}

func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

func (c *Client) redact(s string) string { return strings.ReplaceAll(s, c.key, "[redacted]") }

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
