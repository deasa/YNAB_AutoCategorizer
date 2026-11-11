package classify

import (
	"testing"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

func ptr[T any](v T) *T { return &v }

// row is a plain unapproved checking outflow to payee: nothing should skip it.
func row(payee string) Row {
	return Row{
		Detail: store.TransactionDetail{
			Transaction: store.Transaction{ID: "t1", Date: "2026-11-01", Amount: -12_340, AccountID: "a1"},
			AccountName: ptr("Joint Checking"),
			PayeeName:   ptr(payee),
		},
		AccountType: "checking",
	}
}

func TestGuard_PlainPurchasePasses(t *testing.T) {
	if got := Guard(row("Costco")); got != SkipNone {
		t.Fatalf("Guard = %q, want none", got)
	}
}

// Ported from categorizer.TestRun_SkipsTransfers.
func TestGuard_SkipsTransfers(t *testing.T) {
	r := row("Transfer : Savings")
	r.Detail.TransferAccountID = ptr("acct-savings")
	if got := Guard(r); got != SkipTransfer {
		t.Fatalf("Guard = %q, want %q", got, SkipTransfer)
	}
}

// Ported from categorizer.TestRun_SkipsVenmo.
func TestGuard_SkipsVenmo(t *testing.T) {
	if got := Guard(row("Venmo")); got != SkipP2P {
		t.Fatalf("Guard = %q, want %q", got, SkipP2P)
	}
}

// Ported from categorizer.TestRun_NoPayeeNoMemo_Skips.
func TestGuard_NoPayeeNoMemo(t *testing.T) {
	r := row("")
	r.Detail.PayeeName = nil
	if got := Guard(r); got != SkipNoPayee {
		t.Fatalf("Guard = %q, want %q", got, SkipNoPayee)
	}
	r.Detail.Memo = ptr("   ")
	if got := Guard(r); got != SkipNoPayee {
		t.Fatalf("blank memo: Guard = %q, want %q", got, SkipNoPayee)
	}
}

// Ported from categorizer.TestRun_FallsBackToMemo / TestRun_EmptyPayeeString_UseMemo.
func TestGuard_MemoAloneIsEnough(t *testing.T) {
	r := row("")
	r.Detail.Memo = ptr("hardware store")
	if got := Guard(r); got != SkipNone {
		t.Fatalf("Guard = %q, want none", got)
	}
}

// Ported from categorizer.TestPayeeNameOrMemo: the payee, else the imported name.
func TestPayeeName(t *testing.T) {
	d := store.TransactionDetail{}
	if got := PayeeName(d); got != "" {
		t.Fatalf("empty: %q", got)
	}
	d.ImportPayeeName = ptr("SQ *BLUE BOTTLE")
	if got := PayeeName(d); got != "SQ *BLUE BOTTLE" {
		t.Fatalf("import name: %q", got)
	}
	d.PayeeName = ptr("")
	if got := PayeeName(d); got != "SQ *BLUE BOTTLE" {
		t.Fatalf("empty payee falls back: %q", got)
	}
	d.PayeeName = ptr("Blue Bottle")
	if got := PayeeName(d); got != "Blue Bottle" {
		t.Fatalf("payee: %q", got)
	}
}

func TestGuard_CCPayments(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Row)
		want Skip
	}{
		{"in Credit Card Payments group", func(r *Row) { r.CategoryGroup = "Credit Card Payments" }, SkipCCPayment},
		{"debt payment type", func(r *Row) { r.Detail.DebtTransactionType = ptr("payment") }, SkipCCPayment},
		{"card inflow reads like a payment", func(r *Row) {
			r.AccountType, r.Detail.Amount, r.Detail.PayeeName = "creditCard", 250_000, ptr("PAYMENT - THANK YOU")
		}, SkipCCPayment},
		{"checking outflow to a card", func(r *Row) { r.Detail.PayeeName = ptr("CHASE CREDIT CRD AUTOPAY") }, SkipCCPayment},
		{"checking outflow to Discover", func(r *Row) { r.Detail.PayeeName = ptr("DISCOVER E-PAYMENT") }, SkipCCPayment},
		{"utility autopay is a bill", func(r *Row) { r.Detail.PayeeName = ptr("Rocky Mountain Power AUTOPAY") }, SkipNone},
		{"card refund inflow is not a payment", func(r *Row) {
			r.AccountType, r.Detail.Amount, r.Detail.PayeeName = "creditCard", 19_990, ptr("Amazon Refund")
		}, SkipNone},
		{"card charge debt type", func(r *Row) { r.Detail.DebtTransactionType = ptr("charge") }, SkipNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := row("Costco")
			c.edit(&r)
			if got := Guard(r); got != c.want {
				t.Fatalf("Guard = %q, want %q", got, c.want)
			}
		})
	}
}

func TestGuard_Splits(t *testing.T) {
	r := row("Costco")
	r.Detail.IsSplit = true
	if got := Guard(r); got != SkipSplit {
		t.Fatalf("live split: Guard = %q, want %q", got, SkipSplit)
	}
	r = row("Costco")
	r.Detail.CategoryID = ptr("split-sentinel") // not in the mirror, so no name
	if got := Guard(r); got != SkipSplit {
		t.Fatalf("sentinel category: Guard = %q, want %q", got, SkipSplit)
	}
}

func TestGuard_ReadyToAssign(t *testing.T) {
	r := row("ACME PAYROLL")
	r.Detail.Amount = 3_000_000
	r.Detail.CategoryID, r.Detail.CategoryName = ptr("c-rta"), ptr("Inflow: Ready to Assign")
	if got := Guard(r); got != SkipReadyToAssign {
		t.Fatalf("unapproved RTA: Guard = %q, want %q", got, SkipReadyToAssign)
	}
	// Approved rows never reach the classifier as RTA candidates via this
	// guard; an unapproved row in another category still passes.
	r.Detail.CategoryName = ptr("Groceries")
	if got := Guard(r); got != SkipNone {
		t.Fatalf("unapproved, other category: Guard = %q, want none", got)
	}
}

func TestGuard_Locked(t *testing.T) {
	r := row("Costco")
	r.Locks = []store.HumanLock{{PlanID: "p", TxnID: "t1", Field: store.LockFieldCategory, Source: store.LockSlack}}
	if got := Guard(r); got != SkipLocked {
		t.Fatalf("Guard = %q, want %q", got, SkipLocked)
	}
}

func TestGuard_P2P(t *testing.T) {
	cases := []struct {
		name, payee, account, memo string
		want                       Skip
	}{
		// P2P with no "Venmo" in the payee.
		{"zelle descriptor", "Zelle Payment To Jane Doe", "Joint Checking", "", SkipP2P},
		{"zelle inbound", "ZELLE FROM JOHN SMITH", "Joint Checking", "", SkipP2P},
		{"cash app descriptor", "CASH APP*JOHN SMITH", "Joint Checking", "", SkipP2P},
		{"cashapp one word", "CASHAPP*MIKE", "Joint Checking", "", SkipP2P},
		{"square cash", "SQ CASH - J DOE", "Joint Checking", "", SkipP2P},
		{"paypal instant transfer", "PAYPAL *INST XFER", "Joint Checking", "", SkipP2P},
		{"apple cash", "Apple Cash Sent Money", "Apple Card", "", SkipP2P},
		{"person on a Venmo wallet account", "Jane Doe", "Venmo", "", SkipP2P},
		{"person on a PayPal wallet account", "Sarah Lee", "PayPal Balance", "", SkipP2P},
		{"person with emoji memo", "Mike Johnson", "Joint Checking", "🍕🍺", SkipP2P},
		{"person with zelle memo", "Sarah Lee", "Joint Checking", "Zelle from Sarah", SkipP2P},
		{"all-caps person with P2P note", "JOHN SMITH", "Joint Checking", "paid back for dinner", SkipP2P},
		// Merchants with a P2P-like word are not P2P.
		{"zelle-like merchant", "Zelle's Pizza", "Joint Checking", "", SkipNone},
		{"cash-like merchant", "Cash Wise Foods", "Joint Checking", "", SkipNone},
		{"cash and carry", "Cash & Carry", "Joint Checking", "", SkipNone},
		{"paypal checkout at a merchant", "PAYPAL *NETFLIX", "Joint Checking", "", SkipNone},
		{"apple merchant", "Apple.com/Bill", "Joint Checking", "", SkipNone},
		{"venmo-like word", "Venmonte Bistro", "Joint Checking", "", SkipNone},
		{"merchant on a Venmo account", "Target", "Venmo", "", SkipNone},
		{"merchant-shaped name with emoji memo", "Mountain West Pizza", "Joint Checking", "🍕", SkipNone},
		{"person with no signal", "Jane Doe", "Joint Checking", "", SkipNone},
		{"person with a plain memo", "Jane Doe", "Joint Checking", "piano lesson", SkipNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := row(c.payee)
			r.Detail.AccountName = ptr(c.account)
			if c.memo != "" {
				r.Detail.Memo = ptr(c.memo)
			}
			if got := Guard(r); got != c.want {
				t.Fatalf("Guard(%q, %q, %q) = %q, want %q", c.payee, c.account, c.memo, got, c.want)
			}
		})
	}
}

func TestGuard_P2PFromImportedName(t *testing.T) {
	r := row("")
	r.Detail.PayeeName = nil
	r.Detail.ImportPayeeName = ptr("ZELLE TO DOE JANE")
	if got := Guard(r); got != SkipP2P {
		t.Fatalf("Guard = %q, want %q", got, SkipP2P)
	}
}
