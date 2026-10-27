// Command ynabspike is a throwaway, read-only probe of the live YNAB plan
// (task M0-5). It issues GET requests only — the transport refuses any other
// method — and prints aggregate findings. It never prints the token, the plan
// id, amounts, or raw responses.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"pkg.venceslau.dev/ynab"
)

// getOnly records every request and rejects anything but GET before it
// reaches the network.
type getOnly struct {
	planID string
	log    []reqLog
	last   []byte
	naive  map[string]int // JSON keys whose timestamps lacked a zone
}

// naiveTS matches a timestamp value with no zone designator.
var naiveTS = regexp.MustCompile(`"(\w+)":"(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?)"`)

type reqLog struct {
	path      string
	status    int
	bytes     int
	dur       time.Duration
	rateLimit string
}

func (g *getOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodGet {
		return nil, fmt.Errorf("spike is read-only: refused %s", r.Method)
	}
	start := time.Now()
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	// v1.6.1 decodes timestamps as RFC 3339; YNAB sends some without a
	// zone. Record which keys, and assume UTC so decoding can proceed.
	body = naiveTS.ReplaceAllFunc(body, func(m []byte) []byte {
		g.naive[string(naiveTS.FindSubmatch(m)[1])]++
		return append(append([]byte{}, m[:len(m)-1]...), `Z"`...)
	})
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	g.last = body
	path := strings.ReplaceAll(r.URL.Path, g.planID, "{plan}")
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	g.log = append(g.log, reqLog{path, resp.StatusCode, len(body), time.Since(start), resp.Header.Get("X-Rate-Limit")})
	return resp, nil
}

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
		if i := strings.Index(v, " #"); i >= 0 { // inline comment, as godotenv strips
			v = v[:i]
		}
		os.Setenv(k, strings.Trim(strings.TrimSpace(v), `"'`))
	}
}

func die(step string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", step, err)
	os.Exit(1)
}

// keysOf returns the sorted JSON keys of the first element of the array at
// path (e.g. "data.plan.accounts") in body.
func keysOf(body []byte, path ...string) []string {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return nil
	}
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return nil
	}
	obj, _ := arr[0].(map[string]any)
	var ks []string
	for k := range obj {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func main() {
	loadEnv("../../.env")
	token, planID := os.Getenv("YNAB_ACCESS_TOKEN"), os.Getenv("YNAB_BUDGET_ID")
	if token == "" || planID == "" {
		die("env", fmt.Errorf("YNAB_ACCESS_TOKEN and YNAB_BUDGET_ID required"))
	}
	tr := &getOnly{planID: planID, naive: map[string]int{}}
	c := ynab.New(token, ynab.WithHTTPClient(&http.Client{Transport: tr}), ynab.WithRetryDisabled())
	plan := c.Plan(ynab.PlanID(planID))
	ctx := context.Background()

	// 1. Full plan fetch.
	st := &ynab.SyncState{}
	start := time.Now()
	full, err := plan.Delta(ctx, st)
	if err != nil {
		die("full export", err)
	}
	fullBody := tr.last
	fmt.Printf("== Full plan export: %v, %d bytes (decoded), server_knowledge captured\n", time.Since(start).Round(time.Millisecond), len(fullBody))
	countDel := func(n int, del func(int) bool) (live, dead int) {
		for i := range n {
			if del(i) {
				dead++
			} else {
				live++
			}
		}
		return
	}
	pr := func(name string, n int, del func(int) bool) {
		l, d := countDel(n, del)
		fmt.Printf("   %-26s %6d (deleted=%d)\n", name, l+d, d)
	}
	pr("accounts", len(full.Accounts), func(i int) bool { return full.Accounts[i].Deleted })
	pr("category_groups", len(full.CategoryGroups), func(i int) bool { return full.CategoryGroups[i].Deleted })
	pr("categories", len(full.Categories), func(i int) bool { return full.Categories[i].Deleted })
	pr("payees", len(full.Payees), func(i int) bool { return full.Payees[i].Deleted })
	pr("transactions", len(full.Transactions), func(i int) bool { return full.Transactions[i].Deleted })
	pr("subtransactions", len(full.Subtransactions), func(i int) bool { return full.Subtransactions[i].Deleted })
	pr("months", len(full.Months), func(i int) bool { return full.Months[i].Deleted })
	pr("scheduled_transactions", len(full.ScheduledTransactions), func(i int) bool { return full.ScheduledTransactions[i].Deleted })
	fmt.Println("   raw account keys (plan export):", keysOf(fullBody, "data", "plan", "accounts"))
	fmt.Println("   raw transaction keys (plan export):", keysOf(fullBody, "data", "plan", "transactions"))
	fmt.Println("   raw subtransaction keys (plan export):", keysOf(fullBody, "data", "plan", "subtransactions"))

	// 2. Delta fetches: at the cursor (expected empty), then from older
	// cursors to observe a non-empty delta and tombstones without any write.
	sk := st.Plan
	for _, back := range []int64{0, 25, 500, int64(sk) - 1} {
		start = time.Now()
		d, _, err := plan.Export(ctx, ynab.Since(sk-ynab.ServerKnowledge(back)))
		if err != nil {
			die("delta", err)
		}
		_, td := countDel(len(d.Transactions), func(i int) bool { return d.Transactions[i].Deleted })
		_, sd := countDel(len(d.Subtransactions), func(i int) bool { return d.Subtransactions[i].Deleted })
		fmt.Printf("== Delta since sk-%d: %v, %d bytes; accts=%d cats=%d payees=%d txns=%d (deleted=%d) subs=%d (deleted=%d) months=%d sched=%d\n",
			back, time.Since(start).Round(time.Millisecond), len(tr.last), len(d.Accounts), len(d.Categories), len(d.Payees),
			len(d.Transactions), td, len(d.Subtransactions), sd, len(d.Months), len(d.ScheduledTransactions))
		if td > 0 {
			for _, t := range d.Transactions {
				if t.Deleted {
					fmt.Printf("   tombstone sample: id-set=%v date-set=%v approved=%v category-set=%v\n",
						t.ID != "", !t.Date.IsZero(), t.Approved, t.CategoryID != nil)
					break
				}
			}
		}
	}

	// Lookups.
	catName := map[string]string{}
	internalCat := map[string]bool{}
	groupInternal := map[string]bool{}
	for _, g := range full.CategoryGroups {
		groupInternal[g.ID] = g.Internal
	}
	for _, cat := range full.Categories {
		catName[cat.ID] = cat.Name
		internalCat[cat.ID] = groupInternal[cat.CategoryGroupID]
	}
	payeeName := map[string]string{}
	for _, p := range full.Payees {
		payeeName[p.ID] = p.Name
	}

	// 3. Subtransactions in the plan export.
	subsByParent := map[string][]ynab.SubtransactionBase{}
	for _, s := range full.Subtransactions {
		if !s.Deleted {
			subsByParent[s.TransactionID] = append(subsByParent[s.TransactionID], s)
		}
	}
	parentCats := map[string]int{}
	sumMatch, sumMismatch, orphan, subCat, subPayee, subTransfer, subMemo := 0, 0, 0, 0, 0, 0, 0
	txByID := map[string]ynab.TransactionBase{}
	for _, t := range full.Transactions {
		txByID[t.ID] = t
	}
	for pid, subs := range subsByParent {
		p, ok := txByID[pid]
		if !ok {
			orphan++
			continue
		}
		parentCats[fmt.Sprintf("set=%v name=%q", p.CategoryID != nil, catName[deref(p.CategoryID)])]++
		var sum ynab.Milliunits
		for _, s := range subs {
			sum += s.Amount
			if s.CategoryID != nil {
				subCat++
			}
			if s.PayeeID != nil {
				subPayee++
			}
			if s.TransferAccountID != nil {
				subTransfer++
			}
			if s.Memo != nil {
				subMemo++
			}
		}
		if sum == p.Amount {
			sumMatch++
		} else {
			sumMismatch++
		}
	}
	fmt.Printf("== Splits (plan export): parents=%d orphanSubs=%d sum(children)==parent: %d match / %d mismatch\n",
		len(subsByParent), orphan, sumMatch, sumMismatch)
	fmt.Printf("   parent category: %v\n", parentCats)
	fmt.Printf("   children with category=%d payee=%d transfer=%d memo=%d (of %d)\n",
		subCat, subPayee, subTransfer, subMemo, len(full.Subtransactions))

	for pid := range subsByParent {
		t, _, err := plan.Transactions.Get(ctx, pid)
		if err != nil {
			die("get split parent", err)
		}
		withCat := 0
		for _, s := range t.Subtransactions {
			if s.CategoryName != nil {
				withCat++
			}
		}
		fmt.Printf("   GET transactions/{id} on a split parent: category_id set=%v category_name=%q nested subtransactions=%d (with category_name=%d)\n",
			t.CategoryID != nil, deref(t.CategoryName), len(t.Subtransactions), withCat)
		break
	}

	// 4. Transactions endpoint (nested subtransactions), last 120 days.
	since := ynab.Today().AddDays(-120)
	txns, _, err := plan.Transactions.List(ctx, ynab.TransactionFilter{SinceDate: since})
	if err != nil {
		die("transactions list", err)
	}
	nested, splitNames := 0, map[string]int{}
	for _, t := range txns {
		if len(t.Subtransactions) > 0 {
			nested++
			splitNames[deref(t.CategoryName)]++
		}
	}
	fmt.Printf("== Transactions endpoint since %s: %d txns, %d bytes, %d with nested subtransactions, parent category_name=%v\n",
		since, len(txns), len(tr.last), nested, splitNames)
	fmt.Println("   raw transaction keys (transactions endpoint):", keysOf(tr.last, "data", "transactions"))

	// 5. Accounts endpoint: link fields.
	accts, _, err := plan.Accounts.List(ctx)
	if err != nil {
		die("accounts list", err)
	}
	fmt.Println("== Accounts endpoint raw keys:", keysOf(tr.last, "data", "accounts"))
	linked, inErr, reconciled, open := 0, 0, 0, 0
	for _, a := range accts {
		if a.Deleted || a.Closed {
			continue
		}
		open++
		if a.DirectImportLinked {
			linked++
		}
		if a.DirectImportInError {
			inErr++
		}
		if a.LastReconciledAt != nil {
			reconciled++
		}
	}
	fmt.Printf("   open accounts=%d direct_import_linked=%d direct_import_in_error=%d last_reconciled_at set=%d\n",
		open, linked, inErr, reconciled)
	// Latest import date per linked account, derived from transactions.
	lastImport := map[string]ynab.Date{}
	for _, t := range full.Transactions {
		if t.Deleted || t.ImportID == nil {
			continue
		}
		if d, ok := lastImport[t.AccountID]; !ok || t.Date.After(d) {
			lastImport[t.AccountID] = t.Date
		}
	}
	for _, a := range accts {
		if a.Deleted || a.Closed || !a.DirectImportLinked {
			continue
		}
		age := "none"
		if d, ok := lastImport[a.ID]; ok {
			age = fmt.Sprintf("%dd ago", int(time.Since(d.Time()).Hours()/24))
		}
		fmt.Printf("   linked %-10s in_error=%-5v newest imported txn date: %s\n", a.Type, a.DirectImportInError, age)
	}

	// 6. §3.1 payee memory: imported txns in the last 60 days.
	cutoff := ynab.Today().AddDays(-60)
	type key struct{ payee, cat string }
	approvedBefore := map[key]bool{}
	payeeHist := map[string]bool{}
	for _, t := range full.Transactions {
		if !t.Deleted && t.Approved && t.CategoryID != nil && t.PayeeID != nil {
			approvedBefore[key{*t.PayeeID, *t.CategoryID}] = true
			payeeHist[*t.PayeeID] = true
		}
	}
	var imported, approved, unapprovedCat, unapprovedUncat, unapprovedInternal, transfers, splits, matched, memoryHit, newPayee int
	var examples, uncatExamples []string
	acctType := map[string]string{}
	for _, a := range full.Accounts {
		acctType[a.ID] = string(a.Type)
	}
	internalBy, uncatByAcct := map[string]int{}, map[string]int{}
	for _, t := range full.Transactions {
		if t.Deleted || t.ImportID == nil || t.Date.Before(cutoff) {
			continue
		}
		imported++
		if t.MatchedTransactionID != nil {
			matched++
		}
		switch {
		case t.TransferAccountID != nil:
			transfers++
		case len(subsByParent[t.ID]) > 0:
			splits++
		case t.Approved:
			approved++
		case t.CategoryID != nil && !internalCat[*t.CategoryID]:
			unapprovedCat++
			if t.PayeeID != nil && approvedBefore[key{*t.PayeeID, *t.CategoryID}] {
				memoryHit++
			}
			if len(examples) < 6 {
				examples = append(examples, fmt.Sprintf("%s → %s", payeeName[deref(t.PayeeID)], catName[*t.CategoryID]))
			}
		default:
			if t.CategoryID != nil {
				unapprovedInternal++ // e.g. Inflow: Ready to Assign / Uncategorized
				internalBy[fmt.Sprintf("%s inflow=%v", catName[*t.CategoryID], t.Amount > 0)]++
			} else {
				unapprovedUncat++
			}
			uncatByAcct[acctType[t.AccountID]]++
			if t.PayeeID == nil || !payeeHist[*t.PayeeID] {
				newPayee++
			}
			if len(uncatExamples) < 6 {
				uncatExamples = append(uncatExamples, payeeName[deref(t.PayeeID)])
			}
		}
	}
	fmt.Printf("== Imported txns since %s: %d (matched to manual=%d)\n", cutoff, imported, matched)
	fmt.Printf("   transfers=%d splits=%d approved=%d\n", transfers, splits, approved)
	fmt.Printf("   unapproved WITH category=%d (same payee+category previously approved: %d)\n", unapprovedCat, memoryHit)
	fmt.Printf("   unapproved, category null=%d, internal category (RTA/Uncategorized)=%d (payee never approved before: %d)\n",
		unapprovedUncat, unapprovedInternal, newPayee)
	fmt.Printf("   internal-category breakdown=%v; uncategorized+internal by account type=%v\n", internalBy, uncatByAcct)
	fmt.Printf("   examples categorized: %q\n", examples)
	fmt.Printf("   examples uncategorized: %q\n", uncatExamples)

	// Request summary.
	fmt.Printf("== Zone-less timestamps rewritten (key: count): %v\n", tr.naive)
	fmt.Printf("== Requests: %d (all GET)\n", len(tr.log))
	for _, l := range tr.log {
		fmt.Printf("   %d %7d B %8v rate=%q %s\n", l.status, l.bytes, l.dur.Round(time.Millisecond), l.rateLimit, l.path)
	}
}
