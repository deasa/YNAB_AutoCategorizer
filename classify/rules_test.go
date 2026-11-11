package classify

import (
	"reflect"
	"testing"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

var ruleCands = []Candidate{{Name: "Groceries"}, {Name: "Discretionary"}, {Name: "Home - Electric"}}

func labels(payee string, cats ...string) []store.LabeledRow {
	var out []store.LabeledRow
	for i, c := range cats {
		// Newest first: the first label gets the latest date.
		out = append(out, store.LabeledRow{Date: time.Date(2026, 10, 30-i, 0, 0, 0, 0, time.UTC).Format(time.DateOnly),
			Payee: store.NormalizePayee(payee), Category: c})
	}
	return out
}

func TestRules_UnanimousAtMinimumDecides(t *testing.T) {
	r := NewRules(labels("Smith's Market", "Groceries", "groceries", "Groceries"), ruleCands)
	got, ok := r.Decide("SMITHS MARKET")
	if !ok || got != "Groceries" {
		t.Fatalf("Decide = %q, %v; want Groceries, true", got, ok)
	}
}

func TestRules_BelowMinimumDoesNotDecide(t *testing.T) {
	r := NewRules(labels("Smith's Market", "Groceries", "Groceries"), ruleCands)
	if got, ok := r.Decide("Smith's Market"); ok {
		t.Fatalf("Decide = %q with %d labels; want no rule", got, RuleMinExamples-1)
	}
}

func TestRules_OneDissentStops(t *testing.T) {
	r := NewRules(labels("Costco", "Groceries", "Groceries", "Discretionary", "Groceries", "Groceries"), ruleCands)
	if got, ok := r.Decide("Costco"); ok {
		t.Fatalf("Decide = %q; want no rule after a dissent", got)
	}
}

func TestRules_OutOfListCategoryStops(t *testing.T) {
	r := NewRules(labels("Delta", "Fun - Next Trip", "Fun - Next Trip", "Fun - Next Trip"), ruleCands)
	if got, ok := r.Decide("Delta"); ok {
		t.Fatalf("Decide = %q; want no rule for a non-candidate", got)
	}
	// An out-of-list label is still a dissent.
	r = NewRules(labels("Delta", "Discretionary", "Discretionary", "Discretionary", "Fun - Next Trip"), ruleCands)
	if got, ok := r.Decide("Delta"); ok {
		t.Fatalf("Decide = %q; want no rule with an out-of-list dissent", got)
	}
}

func TestRules_UnknownPayee(t *testing.T) {
	r := NewRules(nil, ruleCands)
	if _, ok := r.Decide("Anyone"); ok {
		t.Fatal("rule with no history")
	}
	if h := r.PayeeHistory("Anyone", HistoryLen); h != nil {
		t.Fatalf("PayeeHistory = %v, want nil", h)
	}
}

func TestPayeeHistory_FiltersNonCandidatesNewestFirst(t *testing.T) {
	hist := labels("Amazon", "Discretionary", "Holiday - Christmas", "groceries", "Subs - Amazon Prime",
		"Discretionary", "Home - Electric", "Groceries", "Discretionary")
	// Shuffle input order: PayeeHistory orders by date, not input order.
	hist[0], hist[5] = hist[5], hist[0]
	r := NewRules(hist, ruleCands)
	got := r.PayeeHistory("AMAZON", HistoryLen)
	want := []string{"Discretionary", "Groceries", "Discretionary", "Home - Electric", "Groceries"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PayeeHistory = %v, want %v", got, want)
	}
	if got := r.PayeeHistory("Amazon", 2); !reflect.DeepEqual(got, want[:2]) {
		t.Fatalf("PayeeHistory n=2 = %v, want %v", got, want[:2])
	}
}

func TestNewTxn(t *testing.T) {
	r := NewRules(labels("Rocky Mountain Power", "Home - Electric", "Home - Electric"), ruleCands)
	d := store.TransactionDetail{
		Transaction: store.Transaction{ID: "t9", Date: "2026-11-02", Amount: -88_120, Memo: ptr("Oct bill")},
		AccountName: ptr("Joint Checking"),
		PayeeName:   ptr("Rocky Mountain Power"),
	}
	got := r.NewTxn(d)
	want := Txn{ID: "t9", Payee: "Rocky Mountain Power", Memo: "Oct bill", Account: "Joint Checking",
		Date: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), AmountMilli: -88_120,
		PayeeHistory: []string{"Home - Electric", "Home - Electric"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NewTxn = %+v\nwant %+v", got, want)
	}
}
