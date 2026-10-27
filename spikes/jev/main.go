// Command jevspike is a throwaway evaluation of TypeSafe's Jev model as a
// transaction classifier (task M0-1). It reads labeled history from the live
// YNAB plan (GET only), asks Jev to pick each transaction's category, and
// prints aggregate accuracy, calibration, latency and cost. It never prints
// the keys, the plan id or raw responses. Fetched data and Jev answers are
// cached outside the repo (-cache) so reruns don't spend calls.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	jevURL      = "https://api.typesafe.ai/v1/systemone"
	jevModel    = "jev-1.14.0"
	pricePerTok = 0.042 / 1e6 // USD per input token (models page)
	maxJevCalls = 300         // brief: Brendan's credits
	maxYNABGets = 30
	sampleSize  = 100
	batchSize   = 10
	histN       = 5
)

// ---- YNAB (read-only) ----

// getOnly rejects anything but GET before it reaches the network.
type getOnly struct{ n int }

func (g *getOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodGet {
		return nil, fmt.Errorf("spike is read-only: refused %s", r.Method)
	}
	if g.n >= maxYNABGets {
		return nil, fmt.Errorf("YNAB GET budget (%d) exhausted", maxYNABGets)
	}
	g.n++
	return http.DefaultTransport.RoundTrip(r)
}

type category struct {
	ID, Name, Group, Note string
}

type txn struct {
	ID, Date, Payee, Memo, Account, CategoryID string
	Amount                                     int64
}

type ynabData struct {
	Categories []category // candidates
	SkippedCat map[string]int
	History    []txn // every approved, categorized, non-transfer, non-split txn
	Gets       int
}

func ynabGet(c *http.Client, path string, out any) error {
	req, _ := http.NewRequest(http.MethodGet, "https://api.ynab.com/v1/plans/"+os.Getenv("YNAB_BUDGET_ID")+path, nil)
	req.Header.Set("Authorization", "Bearer "+os.Getenv("YNAB_ACCESS_TOKEN"))
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("GET {plan}%s: transport error", path) // err may carry the plan id
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET {plan}%s: status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func fetchYNAB() (*ynabData, error) {
	g := &getOnly{}
	c := &http.Client{Transport: g, Timeout: 60 * time.Second}

	var cats struct {
		Data struct {
			CategoryGroups []struct {
				Name            string
				Hidden, Deleted bool
				Categories      []struct {
					ID, Name        string
					Note            *string
					Hidden, Deleted bool
				}
			} `json:"category_groups"`
		}
	}
	if err := ynabGet(c, "/categories", &cats); err != nil {
		return nil, err
	}
	d := &ynabData{SkippedCat: map[string]int{}}
	for _, grp := range cats.Data.CategoryGroups {
		for _, ct := range grp.Categories {
			low := strings.ToLower(ct.Name)
			switch {
			case ct.Deleted || grp.Deleted:
				d.SkippedCat["deleted"]++
			case ct.Hidden || grp.Hidden:
				d.SkippedCat["hidden"]++
			case grp.Name == "Internal Master Category":
				d.SkippedCat["internal (Ready to Assign etc.)"]++
			case grp.Name == "Credit Card Payments":
				d.SkippedCat["credit card payment"]++
			case strings.Contains(low, "birthday") || strings.Contains(low, "gift"):
				d.SkippedCat["birthday/gift"]++
			default:
				note := ""
				if ct.Note != nil {
					note = strings.TrimSpace(*ct.Note)
				}
				d.Categories = append(d.Categories, category{ct.ID, ct.Name, grp.Name, note})
			}
		}
	}

	var txs struct {
		Data struct {
			Transactions []struct {
				ID, Date          string
				Amount            int64
				Memo              *string
				PayeeName         *string `json:"payee_name"`
				AccountName       string  `json:"account_name"`
				CategoryID        *string `json:"category_id"`
				TransferAccountID *string `json:"transfer_account_id"`
				Approved, Deleted bool
				Subtransactions   []json.RawMessage
			}
		}
	}
	if err := ynabGet(c, "/transactions", &txs); err != nil {
		return nil, err
	}
	for _, t := range txs.Data.Transactions {
		if t.Deleted || !t.Approved || t.CategoryID == nil || t.TransferAccountID != nil || len(t.Subtransactions) > 0 {
			continue
		}
		d.History = append(d.History, txn{t.ID, t.Date, deref(t.PayeeName), deref(t.Memo), t.AccountName, *t.CategoryID, t.Amount})
	}
	d.Gets = g.n
	return d, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ---- Jev ----

type choiceAnswer struct {
	Choice        string
	Confidence    float64
	Probabilities map[string]float64
}

type jevResponse struct {
	Model   string
	Answers map[string]choiceAnswer
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	}
}

type jevClient struct {
	http      *http.Client
	callsPath string // persisted call counter, so the cap holds across runs
	calls     int
}

func (j *jevClient) ask(ctx context.Context, state any, questions map[string]any) (*jevResponse, time.Duration, error) {
	body, _ := json.Marshal(map[string]any{"model": jevModel, "state": state, "questions": questions})
	for attempt := 0; ; attempt++ {
		if j.calls >= maxJevCalls {
			return nil, 0, fmt.Errorf("jev call budget (%d) exhausted", maxJevCalls)
		}
		j.calls++
		os.WriteFile(j.callsPath, []byte(strconv.Itoa(j.calls)), 0o600)

		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, jevURL, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+os.Getenv("JEV_API_KEY"))
		req.Header.Set("Content-Type", "application/json")
		start := time.Now()
		resp, err := j.http.Do(req)
		if err != nil {
			return nil, 0, err
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		lat := time.Since(start)
		if resp.StatusCode == 429 && attempt < 3 {
			wait, _ := strconv.Atoi(resp.Header.Get("retry-after"))
			time.Sleep(time.Duration(max(wait, 2)) * time.Second)
			continue
		}
		if resp.StatusCode != 200 {
			return nil, lat, fmt.Errorf("jev status %d: %.300s", resp.StatusCode, raw)
		}
		var out jevResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, lat, err
		}
		return &out, lat, nil
	}
}

// ---- question variants ----

// discretionaryPolicy is the household rule the current app's prompt already
// encodes (CLAUDE.md): everyday spending goes to the catch-all.
const discretionaryPolicy = "Everyday spending: dining out, restaurants, groceries, Amazon orders and general shopping."

// criteria maps each candidate name to its group and note; with policy, the
// catch-all gets the household rule as its note.
func criteria(cats []category, policy bool) map[string]string {
	m := map[string]string{}
	for _, c := range cats {
		desc := "Group: " + c.Group + "."
		if policy && c.Name == "Discretionary" {
			desc += " " + discretionaryPolicy
		} else if c.Note != "" {
			desc += " Note: " + c.Note
		}
		m[c.Name] = desc
	}
	return m
}

func money(milli int64) string {
	dir := "outflow (money spent)"
	if milli > 0 {
		dir = "inflow (money received)"
	}
	return fmt.Sprintf("$%.2f %s", math.Abs(float64(milli))/1000, dir)
}

func txnState(t txn, prev []string) map[string]any {
	s := map[string]any{"payee": t.Payee, "amount": money(t.Amount), "account": t.Account}
	if d, err := time.Parse("2006-01-02", t.Date); err == nil {
		s["date"] = t.Date + " (" + d.Weekday().String() + ")"
	}
	if t.Memo != "" {
		s["memo"] = t.Memo
	}
	if len(prev) > 0 { // omitted rather than empty: an empty list reads as a signal
		s["payee_previous_categories"] = prev
	}
	return s
}

// result is one transaction's answer under one variant.
type result struct {
	Idx     int // into sample
	Pick    string
	PTop    float64
	Conf    float64
	Top3    []string
	Req     int     // request number within the variant
	Latency float64 // seconds, of the whole request
	Tokens  int     // input tokens of the whole request
	Share   int     // transactions in the request
}

type variant struct {
	name    string
	batch   int
	history bool
	policy  bool
}

var variants = []variant{
	{"single", 1, false, false},
	{"batch10", batchSize, false, false},
	{"batch10+history", batchSize, true, false},
	{"single+history", 1, true, false},
	{"batch10+history+policy", batchSize, true, true},
}

func run(ctx context.Context, j *jevClient, v variant, sample []txn, prev [][]string, cats []category) ([]result, error) {
	crit := criteria(cats, v.policy)
	var out []result
	for start, req := 0, 0; start < len(sample); start, req = start+v.batch, req+1 {
		end := min(start+v.batch, len(sample))
		var state any
		qs := map[string]any{}
		ids := map[string]int{}
		if v.batch == 1 {
			var p []string
			if v.history {
				p = prev[start]
			}
			state = txnState(sample[start], p)
			qs["category"] = map[string]any{"type": "choice",
				"instructions": "Which budget category does this transaction belong to?", "criteria": crit}
			ids["category"] = start
		} else {
			txs := map[string]any{}
			for i := start; i < end; i++ {
				id := fmt.Sprintf("t%02d", i-start+1)
				var p []string
				if v.history {
					p = prev[i]
				}
				txs[id] = txnState(sample[i], p)
				qs[id] = map[string]any{"type": "choice",
					"instructions": "Which budget category does the transaction in `transactions." + id + "` belong to?", "criteria": crit}
				ids[id] = i
			}
			state = map[string]any{"transactions": txs}
		}
		resp, lat, err := j.ask(ctx, state, qs)
		if err != nil {
			return out, fmt.Errorf("%s request %d: %w", v.name, req, err)
		}
		for q, i := range ids {
			a, ok := resp.Answers[q]
			if !ok {
				return out, fmt.Errorf("%s request %d: missing answer %s", v.name, req, q)
			}
			out = append(out, result{i, a.Choice, a.Probabilities[a.Choice], a.Confidence, top3(a.Probabilities),
				req, lat.Seconds(), resp.Usage.InputTokens, end - start})
		}
		fmt.Fprintf(os.Stderr, "\r%s: %d/%d (model %s)", v.name, end, len(sample), resp.Model)
	}
	fmt.Fprintln(os.Stderr)
	sort.Slice(out, func(a, b int) bool { return out[a].Idx < out[b].Idx })
	return out, nil
}

func top3(p map[string]float64) []string {
	ks := make([]string, 0, len(p))
	for k := range p {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(a, b int) bool { return p[ks[a]] > p[ks[b]] })
	return ks[:min(3, len(ks))]
}

// ---- sampling ----

// pickSample draws sampleSize labeled txns from the ~6 months ending at the
// latest approved txn (approval stopped in 2026-04, so "now" would be empty) whose
// category is a candidate, plus each one's payee history (the last histN
// labels for that payee dated strictly before it).
func pickSample(d *ynabData, offset int) ([]txn, [][]string, map[string]int) {
	isCand := map[string]bool{}
	name := map[string]string{}
	for _, c := range d.Categories {
		isCand[c.ID] = true
		name[c.ID] = c.Name
	}
	latest := ""
	for _, t := range d.History {
		latest = max(latest, t.Date)
	}
	end, _ := time.Parse("2006-01-02", latest)
	cutoff := end.AddDate(0, -6, 0).Format("2006-01-02")
	stats := map[string]int{"window: " + cutoff + ".." + latest: 0}
	var pool []txn
	for _, t := range d.History {
		if t.Date < cutoff {
			continue
		}
		stats["eligible (6mo, approved, categorized, not transfer/split)"]++
		if !isCand[t.CategoryID] {
			stats["dropped: category not a candidate"]++
			continue
		}
		pool = append(pool, t)
	}
	sort.Slice(pool, func(a, b int) bool { return pool[a].ID < pool[b].ID })
	rand.New(rand.NewSource(1)).Shuffle(len(pool), func(a, b int) { pool[a], pool[b] = pool[b], pool[a] })
	sample := pool[min(offset, len(pool)):min(offset+sampleSize, len(pool))]
	stats["pool"] = len(pool)

	hist := append([]txn(nil), d.History...)
	sort.Slice(hist, func(a, b int) bool { return hist[a].Date > hist[b].Date })
	prev := make([][]string, len(sample))
	for i, s := range sample {
		for _, h := range hist {
			if h.Date < s.Date && h.Payee == s.Payee && name[h.CategoryID] != "" {
				prev[i] = append(prev[i], name[h.CategoryID])
				if len(prev[i]) == histN {
					break
				}
			}
		}
		if len(prev[i]) > 0 {
			stats["sample txns with payee history"]++
		}
	}
	return sample, prev, stats
}

// ---- report ----

func report(v variant, res []result, sample []txn, prev [][]string, cats []category, examples bool) {
	name := map[string]string{}
	for _, c := range cats {
		name[c.ID] = c.Name
	}
	correct := func(r result) bool { return r.Pick == name[sample[r.Idx].CategoryID] }
	n := len(res)
	var hit, hit3 int
	for _, r := range res {
		if correct(r) {
			hit++
		}
		for _, k := range r.Top3 {
			if k == name[sample[r.Idx].CategoryID] {
				hit3++
			}
		}
	}
	fmt.Printf("\n## %s (n=%d)\n\ntop-1 accuracy: %.1f%%   top-3: %.1f%%\n", v.name, n, pct(hit, n), pct(hit3, n))
	var wh, wn, oh, on int // with / without payee history
	for _, r := range res {
		if len(prev[r.Idx]) > 0 {
			wn++
			if correct(r) {
				wh++
			}
		} else {
			on++
			if correct(r) {
				oh++
			}
		}
	}
	fmt.Printf("top-1 on txns whose payee has history: %s (%d/%d); without: %s (%d/%d)\n", pctOrDash(wh, wn), wh, wn, pctOrDash(oh, on), oh, on)

	for _, sig := range []struct {
		label string
		f     func(result) float64
	}{{"p_top (probability of the chosen category)", func(r result) float64 { return r.PTop }},
		{"confidence field", func(r result) float64 { return r.Conf }}} {
		fmt.Printf("\nPrecision/coverage by %s:\n\n| τ | coverage | precision | auto-applied | wrong |\n|---|---|---|---|---|\n", sig.label)
		for _, tau := range []float64{0, 0.3, 0.5, 0.6, 0.7, 0.8, 0.85, 0.9, 0.95, 0.99} {
			c, h := 0, 0
			for _, r := range res {
				if sig.f(r) >= tau {
					c++
					if correct(r) {
						h++
					}
				}
			}
			fmt.Printf("| %.2f | %.0f%% | %s | %d | %d |\n", tau, pct(c, n), pctOrDash(h, c), c, c-h)
		}
		// Lowest τ (max coverage) whose applied set reaches 85% precision.
		vals := make([]float64, n)
		for i, r := range res {
			vals[i] = sig.f(r)
		}
		sort.Float64s(vals)
		for _, tau := range vals {
			c, h := 0, 0
			for _, r := range res {
				if sig.f(r) >= tau {
					c++
					if correct(r) {
						h++
					}
				}
			}
			if pct(h, c) >= 85 {
				fmt.Printf("\nLowest τ reaching ≥85%% precision: %.3f → coverage %.0f%%, precision %.1f%% (%d/%d)\n", tau, pct(c, n), pct(h, c), h, c)
				break
			}
		}
	}

	// Hybrid: §3.2 step 2 (unanimous payee history) first, Jev at τ for the rest.
	fmt.Printf("\nHybrid (unanimous payee-history rule, else Jev p_top ≥ τ):\n\n| τ | coverage | precision |\n|---|---|---|\n")
	for _, tau := range []float64{0.5, 0.7, 0.8, 0.9, 0.95} {
		c, h := 0, 0
		for _, r := range res {
			truth := name[sample[r.Idx].CategoryID]
			if p := prev[r.Idx]; len(p) > 0 && allSame(p) {
				c++
				if p[0] == truth {
					h++
				}
			} else if r.PTop >= tau {
				c++
				if correct(r) {
					h++
				}
			}
		}
		fmt.Printf("| %.2f | %.0f%% | %s |\n", tau, pct(c, n), pctOrDash(h, c))
	}

	fmt.Printf("\nCalibration (p_top bucket → observed accuracy):\n\n| bucket | n | mean p_top | observed |\n|---|---|---|---|\n")
	edges := []float64{0, 0.5, 0.7, 0.8, 0.9, 0.95, 1.0001}
	for i := 0; i+1 < len(edges); i++ {
		c, h, sum := 0, 0, 0.0
		for _, r := range res {
			if r.PTop >= edges[i] && r.PTop < edges[i+1] {
				c++
				sum += r.PTop
				if correct(r) {
					h++
				}
			}
		}
		mean := "—"
		if c > 0 {
			mean = fmt.Sprintf("%.2f", sum/float64(c))
		}
		fmt.Printf("| [%.2f, %.2f) | %d | %s | %s |\n", edges[i], math.Min(edges[i+1], 1), c, mean, pctOrDash(h, c))
	}

	// Per-request latency and tokens.
	reqs := map[int]result{}
	for _, r := range res {
		reqs[r.Req] = r
	}
	var lats []float64
	tok := 0
	for _, r := range reqs {
		lats = append(lats, r.Latency)
		tok += r.Tokens
	}
	sort.Float64s(lats)
	cost := float64(tok) * pricePerTok
	fmt.Printf("\nRequests: %d   latency p50 %.2fs  p95 %.2fs  max %.2fs\n", len(reqs), quant(lats, .5), quant(lats, .95), lats[len(lats)-1])
	fmt.Printf("Input tokens: %d total, %.0f per txn → $%.4f per 1k txns (at $0.042/Mtok)\n", tok, float64(tok)/float64(n), cost/float64(n)*1000)

	if examples {
		for _, r := range res {
			if !correct(r) {
				fmt.Fprintf(os.Stderr, "MISS %-30.30s human=%-25.25s jev=%-25.25s p=%.2f\n", sample[r.Idx].Payee, name[sample[r.Idx].CategoryID], r.Pick, r.PTop)
			}
		}
	}
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

func pctOrDash(a, b int) string {
	if b == 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f%%", pct(a, b))
}

func quant(s []float64, q float64) float64 {
	return s[int(math.Ceil(q*float64(len(s))))-1]
}

// ---- main ----

func loadEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(s.Text()), "=")
		if !ok || strings.HasPrefix(k, "#") || os.Getenv(k) != "" {
			continue
		}
		if i := strings.Index(v, " #"); i >= 0 {
			v = v[:i]
		}
		os.Setenv(k, strings.Trim(strings.TrimSpace(v), `"'`))
	}
}

func cached[T any](path string, fill func() (T, error)) (T, bool, error) {
	var v T
	if b, err := os.ReadFile(path); err == nil {
		return v, true, json.Unmarshal(b, &v)
	}
	v, err := fill()
	if err != nil {
		return v, false, err
	}
	b, _ := json.Marshal(v)
	return v, false, os.WriteFile(path, b, 0o600)
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func main() {
	cache := flag.String("cache", filepath.Join(os.TempDir(), "jevspike"), "cache dir for fetched data and answers (keep outside the repo)")
	only := flag.String("variants", "single,batch10,batch10+history,single+history,batch10+history+policy", "comma-separated variants to run")
	examples := flag.Bool("examples", false, "print misses to stderr (local eyes only; don't commit)")
	probe := flag.Int("probe", 0, "if >0, ask the single variant about this many txns (uncached) and exit")
	offset := flag.Int("offset", 0, "skip this many shuffled pool txns (100 = disjoint holdout)")
	flag.Parse()
	loadEnv("../../.env")
	for _, k := range []string{"YNAB_ACCESS_TOKEN", "YNAB_BUDGET_ID", "JEV_API_KEY"} {
		if os.Getenv(k) == "" {
			die(fmt.Errorf("%s not set", k))
		}
	}
	if err := os.MkdirAll(*cache, 0o700); err != nil {
		die(err)
	}

	d, hit, err := cached(filepath.Join(*cache, "ynab.json"), fetchYNAB)
	if err != nil {
		die(err)
	}
	gets := d.Gets
	if hit {
		gets = 0
	}
	sample, prev, stats := pickSample(d, *offset)
	if len(sample) < sampleSize {
		die(fmt.Errorf("only %d eligible transactions", len(sample)))
	}

	fmt.Printf("# Jev spike run (%s, model %s)\n\n", time.Now().Format("2006-01-02"), jevModel)
	fmt.Printf("YNAB GETs this run: %d (cached: %v)\n", gets, hit)
	fmt.Printf("Candidate categories: %d   excluded: %v\n", len(d.Categories), d.SkippedCat)
	fmt.Printf("Sampling (offset %d): %v\n", *offset, stats)

	rulesBaseline(sample, prev, d.Categories)

	j := &jevClient{http: &http.Client{Timeout: 120 * time.Second}, callsPath: filepath.Join(*cache, "jev_calls")}
	if b, err := os.ReadFile(j.callsPath); err == nil {
		j.calls, _ = strconv.Atoi(string(b))
	}
	startCalls := j.calls
	ctx := context.Background()
	if *probe > 0 {
		res, err := run(ctx, j, variants[0], sample[:*probe], prev, d.Categories)
		if err != nil {
			die(err)
		}
		report(variants[0], res, sample, prev, d.Categories, true)
		fmt.Printf("\nJev calls this run: %d   cumulative: %d / %d\n", j.calls-startCalls, j.calls, maxJevCalls)
		return
	}
	for _, v := range variants {
		if !strings.Contains(","+*only+",", ","+v.name+",") {
			continue
		}
		res, _, err := cached(filepath.Join(*cache, fmt.Sprintf("jev_%s_off%d.json", v.name, *offset)), func() ([]result, error) {
			return run(ctx, j, v, sample, prev, d.Categories)
		})
		if err != nil {
			die(err)
		}
		report(v, res, sample, prev, d.Categories, *examples)
	}
	fmt.Printf("\nJev calls this run: %d   cumulative: %d / %d\n", j.calls-startCalls, j.calls, maxJevCalls)
}

// rulesBaseline scores §3.2 step 2 alone: predict the payee's most frequent
// previous label (ties → most recent), abstaining when there is no history.
func rulesBaseline(sample []txn, prev [][]string, cats []category) {
	name := map[string]string{}
	for _, c := range cats {
		name[c.ID] = c.Name
	}
	c, h, unan, unanHit := 0, 0, 0, 0
	for i, p := range prev {
		if len(p) == 0 {
			continue
		}
		count := map[string]int{}
		best := p[0]
		for _, l := range p {
			count[l]++
			if count[l] > count[best] {
				best = l
			}
		}
		c++
		ok := best == name[sample[i].CategoryID]
		if ok {
			h++
		}
		if count[best] == len(p) {
			unan++
			if ok {
				unanHit++
			}
		}
	}
	fmt.Printf("\nRules-only baseline (payee's majority previous label, last %d): coverage %.0f%%, precision %s (%d/%d); "+
		"unanimous history only: coverage %.0f%%, precision %s (%d/%d)\n",
		histN, pct(c, len(sample)), pctOrDash(h, c), h, c, pct(unan, len(sample)), pctOrDash(unanHit, unan), unanHit, unan)
}

func allSame(p []string) bool {
	for _, l := range p {
		if l != p[0] {
			return false
		}
	}
	return true
}
