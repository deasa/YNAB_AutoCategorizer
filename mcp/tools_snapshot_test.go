package mcp

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

func i64(v int64) *int64 { return &v }

// seedSnapshot builds the snapshot fixture. now is 2026-11-05, so the default
// 12-month window is 2025-11-01..2026-10-31, clipped to the first transaction
// on 2026-08-16: 77 days, an uneven 2.5 months.
//
// In the window, all on Checking:
//   - income 3000 + 6000 + 6000 + 300 (split part) = 15300
//   - Everyday (Groceries) 400 + 500 + 600 - 50 refund - 200 (split part) = 1250
//   - Housing (Mortgage) 2000 + 2000 = 4000
//   - uncategorized 100 + 300 = 400, two lines
//
// Left out: a transfer in from the brokerage categorized to Ready to Assign, a
// deleted inflow, an uncategorized transfer out to the brokerage, the split
// parent, and a 2026-11-02 paycheck after the window (an income event only).
func seedSnapshot(t *testing.T, o Options) {
	t.Helper()
	tx := func(id, date string, amount int64, payee, cat string) store.Transaction {
		r := store.Transaction{ID: id, Date: date, Amount: amount, AccountID: "acct-chk", Approved: true, Cleared: "cleared"}
		if payee != "" {
			r.PayeeID = ptr(payee)
		}
		if cat != "" {
			r.CategoryID = ptr(cat)
		}
		return r
	}
	xferIn := tx("t-xfer-in", "2026-09-10", 1000000, "p-xfer-brk", "cat-rta")
	xferIn.TransferAccountID = ptr("acct-brk")
	xferOut := tx("t-xfer-out", "2026-10-25", -1000000, "p-xfer-brk", "")
	xferOut.TransferAccountID = ptr("acct-brk")
	deleted := tx("t-deleted", "2026-10-15", 9999000, "p-emp", "cat-rta")
	deleted.Deleted = true
	err := o.DB.Update(context.Background(), func(t *store.Tx) error {
		if err := t.UpsertAccounts(planID, []store.Account{
			{ID: "acct-chk", Name: "Checking", Type: "checking", OnBudget: true, Balance: 5000000},
			{ID: "acct-brk", Name: "Brokerage", Type: "otherAsset", Balance: 100000000},
			{ID: "acct-401k", Name: "401k", Type: "otherAsset", Balance: 200000000},
			{ID: "acct-roth", Name: "Roth IRA", Type: "otherAsset", Balance: 50000000},
			{ID: "acct-hsa", Name: "HSA", Type: "otherAsset", Balance: 10000000},
			{ID: "acct-mort", Name: "House", Type: "mortgage", Balance: -300000000, DebtOriginalBalance: i64(-350000000),
				DebtInterestRates:   map[string]int64{"2020-01-01": 3500, "2027-01-01": 4000},
				DebtMinimumPayments: map[string]int64{"2020-01-01": 1500000, "2026-01-01": 1600000},
				DebtEscrowAmounts:   map[string]int64{"2020-01-01": 400000}},
			{ID: "acct-car", Name: "Car Loan", Type: "autoLoan", Balance: -8000000},
			{ID: "acct-old", Name: "Old Savings", Type: "savings", OnBudget: true, Closed: true},
			{ID: "acct-gone", Name: "Gone", Type: "checking", OnBudget: true, Balance: 777000, Deleted: true},
		}); err != nil {
			return err
		}
		for id, b := range map[string]string{"acct-chk": "cash", "acct-brk": "taxable", "acct-401k": "traditional",
			"acct-roth": "roth", "acct-mort": "none", "acct-old": "cash"} {
			if err := t.SetAccountTaxBucket(planID, id, ptr(b)); err != nil {
				return err
			}
		}
		if err := t.UpsertCategoryGroups(planID, []store.CategoryGroup{
			{ID: "g-every", Name: "Everyday"}, {ID: "g-house", Name: "Housing"}, {ID: "g-save", Name: "Savings"},
			{ID: "g-int", Name: "Internal Master Category", Internal: true},
		}); err != nil {
			return err
		}
		snoozed := now.Add(-24 * time.Hour)
		if err := t.UpsertCategories(planID, []store.Category{
			{ID: "cat-groc", CategoryGroupID: "g-every", Name: "Groceries"},
			{ID: "cat-mort", CategoryGroupID: "g-house", Name: "Mortgage"},
			{ID: "cat-rta", CategoryGroupID: "g-int", Name: "Inflow: Ready to Assign", Internal: true},
			{ID: "cat-ef", CategoryGroupID: "g-save", Name: "Emergency Fund", Balance: 4000000, GoalType: ptr("TB"),
				GoalTarget: i64(10000000), GoalPercentageComplete: i64(40), GoalOverallFunded: i64(4000000),
				GoalOverallLeft: i64(6000000), GoalUnderFunded: i64(500000)},
			{ID: "cat-vac", CategoryGroupID: "g-save", Name: "Vacation", Balance: 3000000, GoalType: ptr("TBD"),
				GoalTarget: i64(3000000), GoalTargetDate: ptr("2027-06-01"), GoalPercentageComplete: i64(100),
				GoalOverallFunded: i64(3000000), GoalOverallLeft: i64(0), GoalSnoozedAt: &snoozed},
			{ID: "cat-gone", CategoryGroupID: "g-save", Name: "Old Goal", GoalType: ptr("TB"), GoalTarget: i64(1000),
				GoalOverallLeft: i64(1000), Deleted: true},
		}); err != nil {
			return err
		}
		if err := t.UpsertPayees(planID, []store.Payee{
			{ID: "p-emp", Name: "Employer"}, {ID: "p-store", Name: "Smith's"}, {ID: "p-bank", Name: "Mortgage Co"},
			{ID: "p-xfer-brk", Name: "Transfer : Brokerage", TransferAccountID: ptr("acct-brk")},
		}); err != nil {
			return err
		}
		if err := t.UpsertTransactions(planID, []store.Transaction{
			tx("t-groc0", "2026-08-16", -400000, "p-store", "cat-groc"),
			tx("t-inc-aug", "2026-08-20", 3000000, "p-emp", "cat-rta"),
			tx("t-inc-sep", "2026-09-01", 6000000, "p-emp", "cat-rta"),
			tx("t-mort-sep", "2026-09-05", -2000000, "p-bank", "cat-mort"),
			tx("t-groc1", "2026-09-10", -500000, "p-store", "cat-groc"),
			tx("t-split", "2026-09-15", 500000, "p-emp", "split-sentinel"),
			tx("t-uncat1", "2026-09-20", -100000, "p-store", ""),
			tx("t-inc-oct", "2026-10-01", 6000000, "p-emp", "cat-rta"),
			tx("t-mort-oct", "2026-10-05", -2000000, "p-bank", "cat-mort"),
			tx("t-refund", "2026-10-05", 50000, "p-store", "cat-groc"),
			tx("t-groc2", "2026-10-10", -600000, "p-store", "cat-groc"),
			tx("t-uncat2", "2026-10-20", -300000, "p-store", ""),
			tx("t-inc-nov", "2026-11-02", 2000000, "p-emp", "cat-rta"),
			xferIn, xferOut, deleted,
		}, now); err != nil {
			return err
		}
		if err := t.UpsertSubtransactions(planID, []store.Subtransaction{
			{ID: "s-inc", TransactionID: "t-split", Amount: 300000, CategoryID: ptr("cat-rta")},
			{ID: "s-groc", TransactionID: "t-split", Amount: 200000, CategoryID: ptr("cat-groc")},
		}); err != nil {
			return err
		}
		return t.SetSyncState(store.SyncState{PlanID: planID, ServerKnowledge: 1, SyncedAt: now.Add(-time.Minute)})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func snapshotSession(t *testing.T) *sdkmcp.ClientSession {
	t.Helper()
	o, _ := testOptions(t)
	seedSnapshot(t, o)
	srv := httptest.NewServer(NewHandler(o))
	t.Cleanup(srv.Close)
	return connect(t, srv.URL, "good")
}

func TestFinancialSnapshot_BalancesBucketsAndDebts(t *testing.T) {
	s := snapshotSession(t)
	d := call[snapshotData](t, s, "get_financial_snapshot", nil).Data
	if d.AsOf != "2026-11-05" || d.NetWorth != "57000.00" {
		t.Errorf("as_of %s net_worth %s", d.AsOf, d.NetWorth)
	}
	var buckets []string
	for _, b := range d.Buckets {
		buckets = append(buckets, b.Bucket+"="+b.Balance+"/"+string(rune('0'+b.Accounts)))
	}
	if got := strings.Join(buckets, " "); got != "taxable=100000.00/1 traditional=200000.00/1 roth=50000.00/1 cash=5000.00/1 none=-300000.00/1 untagged=2000.00/2" {
		t.Errorf("buckets %s", got)
	}
	tags := map[string]string{}
	for _, a := range d.Accounts {
		tags[a.Name] = a.TaxBucket
	}
	if len(tags) != 7 || tags["HSA"] != "untagged" || tags["Car Loan"] != "untagged" || tags["Checking"] != "cash" {
		t.Errorf("accounts %v", tags)
	}
	if _, ok := tags["Old Savings"]; ok {
		t.Error("closed account listed")
	}
	if len(d.Debts) != 2 {
		t.Fatalf("debts %+v", d.Debts)
	}
	car, house := d.Debts[0], d.Debts[1]
	if house.Account != "House" || house.Balance != "-300000.00" || house.OriginalBalance != "-350000.00" ||
		house.InterestRate != "3.5" || house.MinimumPayment != "1600.00" || house.Escrow != "400.00" ||
		len(house.RateHistory) != 2 || house.RateHistory[1] != (datedValue{"2027-01-01", "4"}) || house.TermsMissingNote != "" {
		t.Errorf("house %+v", house)
	}
	if car.Account != "Car Loan" || car.InterestRate != "" || car.TermsMissingNote == "" {
		t.Errorf("car %+v", car)
	}
}

func TestFinancialSnapshot_CashFlowUnevenWindow(t *testing.T) {
	s := snapshotSession(t)
	r := call[snapshotData](t, s, "get_financial_snapshot", nil)
	cf := r.Data.CashFlow
	w := cf.Window
	if w != (windowInfo{Start: "2026-08-16", End: "2026-10-31", MonthsRequested: 12, MonthsCovered: "2.5",
		HistoryStarts: "2026-08-16", MonthsWithCategorized: 3, MonthsWithActivity: 3}) {
		t.Errorf("window %+v", w)
	}
	if cf.Totals.Income != "15300.00" || cf.Totals.Categorized != "5250.00" || cf.Totals.Uncategorized != "400.00" {
		t.Errorf("totals %+v", cf.Totals)
	}
	// 77 days: each average is total × 30.4375 / 77.
	if cf.AvgMonthlyIncome != "6047.971" || cf.AvgMonthlySpending != "2233.401" ||
		cf.AvgMonthlyCategorized != "2075.284" || cf.AvgMonthlyUncategorized != "158.117" {
		t.Errorf("averages %s %s %s %s", cf.AvgMonthlyIncome, cf.AvgMonthlySpending, cf.AvgMonthlyCategorized, cf.AvgMonthlyUncategorized)
	}
	if cf.UncategorizedShare != "7.1" || cf.UncategorizedLines != 2 || cf.SavingsRate != "63.1" {
		t.Errorf("share %s lines %d savings %s", cf.UncategorizedShare, cf.UncategorizedLines, cf.SavingsRate)
	}
	var groups []string
	for _, g := range cf.SpendingByGroup {
		groups = append(groups, g.CategoryGroup+"="+g.Total)
	}
	if got := strings.Join(groups, " "); got != "Housing=4000.00 Everyday=1250.00 Uncategorized=400.00" {
		t.Errorf("groups %s", got)
	}
	if !strings.HasPrefix(cf.CSPGrouping, "PROPOSED") {
		t.Errorf("csp %q", cf.CSPGrouping)
	}

	// A one-month window isn't clipped: October only.
	cf = call[snapshotData](t, s, "get_financial_snapshot", map[string]any{"months": 1}).Data.CashFlow
	if cf.Window.Start != "2026-10-01" || cf.Window.MonthsCovered != "1.0" || cf.Totals.Income != "6000.00" ||
		cf.Totals.Categorized != "2550.00" || cf.AvgMonthlyIncome != "5891.129" {
		t.Errorf("one month %+v %+v %s", cf.Window, cf.Totals, cf.AvgMonthlyIncome)
	}
	for _, m := range []int{-1, 61} {
		if msg := callErr(t, s, "get_financial_snapshot", map[string]any{"months": m}); !strings.Contains(msg, "months must be") {
			t.Errorf("months %d: %q", m, msg)
		}
	}
}

func TestFinancialSnapshot_NoHistory(t *testing.T) {
	o, _ := testOptions(t)
	if err := o.DB.SetSyncState(context.Background(), store.SyncState{PlanID: planID, ServerKnowledge: 1, SyncedAt: now}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHandler(o))
	t.Cleanup(srv.Close)
	d := call[snapshotData](t, connect(t, srv.URL, "good"), "get_financial_snapshot", nil).Data
	cf := d.CashFlow
	if cf.Window.Start != "2025-11-01" || cf.AvgMonthlyIncome != "0.00" || cf.SavingsRate != "" || cf.UncategorizedShare != "" ||
		d.NetWorth != "0.00" || len(d.Buckets) != 0 || d.GoalsInfo.Count != 0 {
		t.Errorf("empty snapshot %+v", d)
	}
}

func TestGoals(t *testing.T) {
	s := snapshotSession(t)
	g := call[goalsData](t, s, "get_goals", nil).Data.Goals
	if len(g) != 2 {
		t.Fatalf("goals %+v", g)
	}
	ef, vac := g[0], g[1]
	if ef.Category != "Emergency Fund" || ef.CategoryGroup != "Savings" || ef.GoalType != "TB" || ef.Target != "10000.00" ||
		*ef.PercentComplete != 40 || ef.Funded != "4000.00" || ef.Left != "6000.00" || ef.UnderFundedMonth != "500.00" ||
		ef.Balance != "4000.00" || ef.Snoozed {
		t.Errorf("emergency fund %+v", ef)
	}
	if vac.Category != "Vacation" || vac.TargetDate != "2027-06-01" || *vac.PercentComplete != 100 || vac.Left != "0.00" || !vac.Snoozed {
		t.Errorf("vacation %+v", vac)
	}
	sum := call[snapshotData](t, s, "get_financial_snapshot", nil).Data.GoalsInfo
	if sum.Count != 2 || sum.Complete != 1 || sum.Snoozed != 1 || sum.TotalLeft != "6000.00" || len(sum.Goals) != 2 {
		t.Errorf("snapshot goals %+v", sum)
	}
}

func TestIncomeEvents(t *testing.T) {
	s := snapshotSession(t)
	d := call[incomeData](t, s, "get_income_events", nil).Data
	var ids []string
	for _, e := range d.Events {
		ids = append(ids, e.TransactionID+"/"+e.SubtransactionID+"="+e.Amount)
	}
	if got := strings.Join(ids, " "); got != "t-inc-nov/=2000.00 t-inc-oct/=6000.00 t-split/s-inc=300.00 t-inc-sep/=6000.00 t-inc-aug/=3000.00" {
		t.Errorf("events %s", got)
	}
	if d.Start != "2025-11-05" || d.End != "2026-11-05" || d.Count != 5 || d.Total != "17300.00" || d.Truncated ||
		!strings.Contains(d.Kind, "Not confirmed paychecks") {
		t.Errorf("summary %+v", d)
	}
	if e := d.Events[0]; e.Payee != "Employer" || e.Account != "Checking" || e.Date != "2026-11-02" {
		t.Errorf("event %+v", e)
	}

	d = call[incomeData](t, s, "get_income_events", map[string]any{"start": "2026-09-01", "end": "2026-10-31", "limit": 2}).Data
	if len(d.Events) != 2 || d.Count != 3 || d.Total != "12300.00" || !d.Truncated {
		t.Errorf("limited %+v", d)
	}
	if msg := callErr(t, s, "get_income_events", map[string]any{"start": "2026-10-31", "end": "2026-10-01"}); !strings.Contains(msg, "after end") {
		t.Errorf("bad range %q", msg)
	}
}

func TestSnapshotTools_CarryFreshness(t *testing.T) {
	s := snapshotSession(t)
	for _, name := range []string{"get_financial_snapshot", "get_goals", "get_income_events"} {
		f := call[map[string]any](t, s, name, nil).DataFreshness
		if f.LastSyncedAt == "" || f.Stale {
			t.Errorf("%s freshness %+v", name, f)
		}
	}
}

func TestAvgMonthly(t *testing.T) {
	for _, c := range []struct {
		total int64
		days  int
		want  int64
	}{{304375, 10000, 926}, {1000, 30, 1015}, {-1000, 30, -1015}, {5, 0, 0}, {-15000000, 77, -5929383}} {
		if got := avgMonthly(c.total, c.days); got != c.want {
			t.Errorf("avgMonthly(%d, %d) = %d, want %d", c.total, c.days, got, c.want)
		}
	}
}
