package alerts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

// anomalyHarness is a harness whose Alerter can run Anomalies, with a sync
// clock for first_seen_at.
type anomalyHarness struct {
	*harness
	synced time.Time
	n      int
}

func newAnomalyHarness(t *testing.T) *anomalyHarness {
	t.Helper()
	h := newHarness(t)
	h.a = New(Config{Store: h.db, Poster: h.poster, PlanID: plan, Mirror: h.db, Now: func() time.Time { return h.now },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	return &anomalyHarness{harness: h, synced: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
}

// charge is an imported outflow of dollars to payee on date.
func (h *anomalyHarness) charge(payee, date string, dollars float64) store.Transaction {
	h.n++
	id := fmt.Sprintf("txn-%d", h.n)
	imp := fmt.Sprintf("YNAB:%d", h.n)
	return store.Transaction{ID: id, Date: date, Amount: -int64(dollars*1000 + 0.5), Cleared: "cleared",
		AccountID: "acc-1", ImportID: &imp, ImportPayeeName: &payee}
}

// sync stores rows as one tick's new rows, a minute after the last.
func (h *anomalyHarness) sync(t *testing.T, rows ...store.Transaction) {
	t.Helper()
	h.synced = h.synced.Add(time.Minute)
	if err := h.db.Update(context.Background(), func(tx *store.Tx) error {
		return tx.UpsertTransactions(plan, rows, h.synced)
	}); err != nil {
		t.Fatal(err)
	}
}

func (h *anomalyHarness) run(t *testing.T) {
	t.Helper()
	if err := h.a.Anomalies(context.Background()); err != nil {
		t.Fatalf("Anomalies: %v", err)
	}
}

// kinds returns the stored anomaly alert kind per transaction id.
func (h *anomalyHarness) kinds(t *testing.T) map[string]string {
	t.Helper()
	all, err := h.db.Alerts(context.Background(), plan, time.Time{}, h.now.AddDate(1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, a := range all {
		out[a.DedupeKey] = a.Kind
	}
	return out
}

// monthly returns n charges of dollars to payee, a month apart, ending Oct 1.
func (h *anomalyHarness) monthly(payee string, n int, dollars ...float64) []store.Transaction {
	var rows []store.Transaction
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).AddDate(0, -(n - 1), 0)
	for i := range n {
		rows = append(rows, h.charge(payee, start.AddDate(0, i, 0).Format(time.DateOnly), dollars[i%len(dollars)]))
	}
	return rows
}

type ruleCase struct {
	name    string
	history func(h *anomalyHarness) []store.Transaction
	txn     func(h *anomalyHarness) store.Transaction
	want    string // the alert kind, or "" for none
}

func runRuleCases(t *testing.T, cases []ruleCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAnomalyHarness(t)
			h.sync(t, tc.history(h)...)
			h.run(t) // baseline
			h.wantPosts(t, 0)
			txn := tc.txn(h)
			h.sync(t, txn)
			h.run(t)
			got := h.kinds(t)[txn.ID]
			if got != tc.want {
				t.Fatalf("alert = %q, want %q (posts %q)", got, tc.want, h.poster.posts)
			}
			if tc.want != "" {
				h.wantPosts(t, 1)
				p := h.poster.posts[0]
				if !strings.Contains(p, "Netflix") && !strings.Contains(p, "Costco") && !strings.Contains(p, "Amazon") {
					t.Fatalf("post %q doesn't name the payee", p)
				}
				if strings.Contains(p, "txn-") || strings.Contains(p, "YNAB:") || strings.Contains(p, "acc-") {
					t.Fatalf("post %q holds an id", p)
				}
			}
		})
	}
}

func TestAnomalies_Duplicate(t *testing.T) {
	prior := func(date string) func(h *anomalyHarness) []store.Transaction {
		return func(h *anomalyHarness) []store.Transaction {
			return []store.Transaction{h.charge("Costco", date, 84.12)}
		}
	}
	runRuleCases(t, []ruleCase{
		{"fires 3 days apart", prior("2026-11-01"),
			func(h *anomalyHarness) store.Transaction { return h.charge("Costco", "2026-11-04", 84.12) }, KindDuplicate},
		{"fires dated before its partner", prior("2026-11-04"),
			func(h *anomalyHarness) store.Transaction { return h.charge("Costco", "2026-11-02", 84.12) }, KindDuplicate},
		{"4 days apart is outside 72h", prior("2026-11-01"),
			func(h *anomalyHarness) store.Transaction { return h.charge("Costco", "2026-11-05", 84.12) }, ""},
		{"a cent different", prior("2026-11-01"),
			func(h *anomalyHarness) store.Transaction { return h.charge("Costco", "2026-11-02", 84.13) }, ""},
		{"same import id", prior("2026-11-01"),
			func(h *anomalyHarness) store.Transaction {
				c := h.charge("Costco", "2026-11-02", 84.12)
				imp := "YNAB:1"
				c.ImportID = &imp
				return c
			}, ""},
		{"no history", func(*anomalyHarness) []store.Transaction { return nil },
			func(h *anomalyHarness) store.Transaction { return h.charge("Costco", "2026-11-02", 84.12) }, ""},
		{"manual entry has no import id", prior("2026-11-01"),
			func(h *anomalyHarness) store.Transaction {
				c := h.charge("Costco", "2026-11-02", 84.12)
				c.ImportID = nil
				return c
			}, ""},
	})
}

func TestAnomalies_PriceChange(t *testing.T) {
	bill := func(n int, dollars ...float64) func(h *anomalyHarness) []store.Transaction {
		return func(h *anomalyHarness) []store.Transaction { return h.monthly("Netflix", n, dollars...) }
	}
	next := func(dollars float64) func(h *anomalyHarness) store.Transaction {
		return func(h *anomalyHarness) store.Transaction { return h.charge("Netflix", "2026-11-01", dollars) }
	}
	runRuleCases(t, []ruleCase{
		{"fires up 12.5%", bill(3, 15.99), next(17.99), KindPriceChange},
		{"fires down 20%", bill(4, 20), next(16), KindPriceChange},
		{"fires just over 10%", bill(3, 20), next(22.01), KindPriceChange},
		{"exactly 10% is not more than 10%", bill(3, 20), next(22.00), ""},
		{"just under 10%", bill(3, 20), next(21.99), ""},
		{"only 2 prior charges", bill(2, 15.99), next(17.99), ""},
		{"varying amounts are not a recurring bill", bill(6, 14, 22, 17), next(25), ""},
		{"one odd charge in a long window still recurring", bill(6, 15.99, 15.99, 15.99, 7.50, 15.99, 15.99), next(17.99),
			KindPriceChange},
		{"a big jump is a price change, not also a large charge", bill(6, 15.99), next(99), KindPriceChange},
	})
}

func TestAnomalies_LargeCharge(t *testing.T) {
	hist := func(dollars ...float64) func(h *anomalyHarness) []store.Transaction {
		return func(h *anomalyHarness) []store.Transaction { return h.monthly("Amazon", len(dollars), dollars...) }
	}
	next := func(dollars float64) func(h *anomalyHarness) store.Transaction {
		return func(h *anomalyHarness) store.Transaction { return h.charge("Amazon", "2026-11-01", dollars) }
	}
	// Median 20, MAD 2: 3× the median ($60) governs.
	steady := hist(20, 22, 18, 25, 20)
	// Median 27.50, MAD 7.50: the MAD bar (27.50 + 6×1.4826×7.50 = $94.22)
	// is above 3× the median ($82.50).
	wide := hist(10, 30, 30, 200, 25, 15)
	// Median 5: the $25 floor ($30) is above 3× the median ($15).
	small := hist(5, 5, 6, 4, 5)
	runRuleCases(t, []ruleCase{
		{"fires over 3x median", steady, next(60.01), KindLargeCharge},
		{"exactly 3x median", steady, next(60.00), ""},
		{"only 4 prior charges", hist(20, 22, 18, 25), next(500), ""},
		{"wide history fires over the MAD bar", wide, next(94.30), KindLargeCharge},
		{"wide history, over 3x median but under the MAD bar", wide, next(94.00), ""},
		{"small payee fires over the $25 floor", small, next(30.01), KindLargeCharge},
		{"small payee, over 3x median but under the floor", small, next(29.99), ""},
	})
}

func TestAnomalies_RefundTransferDeletedNeverFire(t *testing.T) {
	h := newAnomalyHarness(t)
	h.sync(t, h.monthly("Amazon", 6, 20, 22, 18)...) // median $20, not a recurring bill
	h.run(t)

	refund := h.charge("Amazon", "2026-11-02", 0)
	refund.Amount = 900_000 // +$900 back
	transfer := h.charge("Amazon", "2026-11-02", 900)
	other := "acc-2"
	transfer.TransferAccountID = &other
	deleted := h.charge("Amazon", "2026-11-02", 900)
	deleted.Deleted = true
	// A refund equal to an earlier charge is not its duplicate either.
	dupRefund := h.charge("Amazon", "2026-11-02", 0)
	dupRefund.Amount = 20_000
	h.sync(t, refund, transfer, deleted, dupRefund)
	h.run(t)
	h.wantPosts(t, 0)
	if k := h.kinds(t); len(k) != 0 {
		t.Fatalf("alerts stored: %v", k)
	}
	// None of them counts as history: a later normal charge is judged on the six earlier charges.
	h.sync(t, h.charge("Amazon", "2026-11-03", 59))
	h.run(t)
	h.wantPosts(t, 0)
}

func TestAnomalies_BaselineIsSilent(t *testing.T) {
	h := newAnomalyHarness(t)
	rows := h.monthly("Netflix", 6, 15.99)
	rows = append(rows, h.charge("Netflix", "2026-10-15", 99), h.charge("Netflix", "2026-10-16", 99))
	h.sync(t, rows...)
	h.run(t)
	h.run(t)
	h.wantPosts(t, 0)
}

func TestAnomalies_NoAlertTwiceAcrossRuns(t *testing.T) {
	h := newAnomalyHarness(t)
	h.sync(t, h.charge("Costco", "2026-11-01", 84.12))
	h.run(t)

	// A pair arriving together alerts once, not once per side.
	a, b := h.charge("Costco", "2026-11-02", 50), h.charge("Costco", "2026-11-02", 50)
	h.sync(t, a, b)
	h.run(t)
	h.wantPosts(t, 1)
	// A third copy of the first charge pairs with it: one more.
	c := h.charge("Costco", "2026-11-03", 84.12)
	h.sync(t, c)
	h.run(t)
	h.wantPosts(t, 2)

	for range 3 {
		h.run(t)
	}
	// Rewind the cursor so every row is evaluated again: still nothing new.
	if err := h.db.SetAlertCursor(context.Background(), plan, h.synced.AddDate(-1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	h.run(t)
	h.wantPosts(t, 2)
}

func TestAnomalies_FailedPostRetriesNextRun(t *testing.T) {
	h := newAnomalyHarness(t)
	h.sync(t, h.monthly("Netflix", 3, 15.99)...)
	h.run(t)

	h.sync(t, h.charge("Netflix", "2026-11-01", 17.99))
	h.poster.fail = errors.New("slack down")
	h.run(t)
	h.wantPosts(t, 0)
	cur, _, err := h.db.AlertCursor(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if !cur.Before(h.synced) {
		t.Fatalf("cursor %v advanced past the unsent row (synced %v)", cur, h.synced)
	}

	h.poster.fail = nil
	h.run(t)
	h.run(t)
	h.wantPosts(t, 1)
}

func TestAnomalies_BurstRespectsBudget(t *testing.T) {
	h := newAnomalyHarness(t)
	payees := []string{"Amazon", "Costco", "Netflix", "Target", "Shell", "Kroger"}
	var hist []store.Transaction
	for _, p := range payees {
		hist = append(hist, h.monthly(p, 5, 20)...)
	}
	h.sync(t, hist...)
	h.run(t)

	var burst []store.Transaction
	for _, p := range payees {
		burst = append(burst, h.charge(p, "2026-11-02", 500))
	}
	h.sync(t, burst...)
	h.run(t)
	h.wantPosts(t, MonthlyBudget)

	all, err := h.db.Alerts(context.Background(), plan, time.Time{}, h.now.AddDate(1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	var sent, rolled int
	for _, a := range all {
		if a.Critical {
			t.Fatalf("anomaly %s stored as critical", a.Kind)
		}
		switch a.Status {
		case store.AlertSent:
			sent++
		case store.AlertRolledUp:
			rolled++
		}
	}
	if sent != MonthlyBudget || rolled != len(payees)-MonthlyBudget {
		t.Fatalf("sent %d, rolled up %d; want %d and %d", sent, rolled, MonthlyBudget, len(payees)-MonthlyBudget)
	}
}

func TestAnomalies_NeedsMirror(t *testing.T) {
	h := newHarness(t)
	if err := h.a.Anomalies(context.Background()); err == nil {
		t.Fatal("Anomalies without a Mirror: want an error")
	}
}

func TestAnomalies_OneAlertPerTransactionAcrossKinds(t *testing.T) {
	h := newAnomalyHarness(t)
	h.sync(t, h.monthly("Netflix", 3, 15.99)...)
	h.run(t)
	big := h.charge("Netflix", "2026-11-01", 99)
	h.sync(t, big)
	h.run(t)
	if k := h.kinds(t)[big.ID]; k != KindPriceChange {
		t.Fatalf("alert = %q, want %q", k, KindPriceChange)
	}
	// Back-dated charges make Netflix no longer a fixed-price bill, so on a
	// re-evaluation the same row would be a large charge. It must stay quiet.
	h.sync(t, h.charge("Netflix", "2026-06-15", 7), h.charge("Netflix", "2026-06-16", 40))
	if err := h.db.SetAlertCursor(context.Background(), plan, h.synced.AddDate(-1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	h.run(t)
	h.wantPosts(t, 1)
}
