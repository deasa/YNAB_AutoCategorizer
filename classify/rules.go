package classify

import (
	"sort"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

const (
	// RuleMinExamples is how many human labels a payee needs before a
	// unanimous rule decides it.
	RuleMinExamples = 3
	// HistoryLen is how many of a payee's labels the model sees (the M0 spike
	// measured last 5).
	HistoryLen = 5
)

// Rules are the payee rules learned from human labels (§3.2 step 2), and the
// payee history the model sees.
type Rules struct {
	byPayee map[string][]string // normalized payee → labels, newest first
	targets map[string]string   // folded candidate name → candidate name
}

// NewRules indexes history (store.LabeledHistory) by normalized payee. Only
// cands can be decided or shown as history.
func NewRules(history []store.LabeledRow, cands []Candidate) *Rules {
	rows := append([]store.LabeledRow(nil), history...)
	// LabeledHistory is already newest first; keep its order on ties.
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Date > rows[j].Date })
	r := &Rules{byPayee: map[string][]string{}, targets: map[string]string{}}
	for _, c := range cands {
		r.targets[fold(c.Name)] = c.Name
	}
	for _, row := range rows {
		p := store.NormalizePayee(row.Payee)
		if p == "" {
			continue
		}
		r.byPayee[p] = append(r.byPayee[p], row.Category)
	}
	return r
}

// Decide returns the category for payee when every one of its labels, at least
// RuleMinExamples of them, names the same category and that category is a
// candidate. One dissenting label, even one outside the candidates, stops it.
func (r *Rules) Decide(payee string) (string, bool) {
	labels := r.byPayee[store.NormalizePayee(payee)]
	if len(labels) < RuleMinExamples {
		return "", false
	}
	first := fold(labels[0])
	for _, l := range labels[1:] {
		if fold(l) != first {
			return "", false
		}
	}
	name, ok := r.targets[first]
	return name, ok
}

// PayeeHistory returns payee's labels that are candidates, newest first, at
// most n of them; nil if there are none.
func (r *Rules) PayeeHistory(payee string, n int) []string {
	var out []string
	for _, l := range r.byPayee[store.NormalizePayee(payee)] {
		if len(out) >= n {
			break
		}
		if name, ok := r.targets[fold(l)]; ok {
			out = append(out, name)
		}
	}
	return out
}

// PayeeName is the name a transaction is known by: its payee, or the imported
// name if it has none, as store.LabeledHistory reads it.
func PayeeName(d store.TransactionDetail) string {
	if d.PayeeName != nil && *d.PayeeName != "" {
		return *d.PayeeName
	}
	if d.ImportPayeeName != nil {
		return *d.ImportPayeeName
	}
	return ""
}

// NewTxn builds the model input for a mirrored transaction, with its payee's
// last HistoryLen candidate labels. A malformed date is left zero.
func (r *Rules) NewTxn(d store.TransactionDetail) Txn {
	t := Txn{ID: d.ID, Payee: PayeeName(d), AmountMilli: d.Amount}
	if d.Memo != nil {
		t.Memo = *d.Memo
	}
	if d.AccountName != nil {
		t.Account = *d.AccountName
	}
	if date, err := time.Parse(time.DateOnly, d.Date); err == nil {
		t.Date = date
	}
	t.PayeeHistory = r.PayeeHistory(t.Payee, HistoryLen)
	return t
}
