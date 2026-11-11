package classify

import (
	"strings"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

// Skip says why a guardrail kept a transaction from the classifier. The zero
// value means classify it.
type Skip string

const (
	SkipNone          Skip = ""
	SkipTransfer      Skip = "transfer"
	SkipCCPayment     Skip = "cc_payment"
	SkipP2P           Skip = "p2p"
	SkipSplit         Skip = "split"
	SkipReadyToAssign Skip = "ready_to_assign"
	SkipLocked        Skip = "locked"
	SkipNoPayee       Skip = "no_payee_or_memo"
)

// Row is a transaction with what the guardrails need beyond the mirror row.
type Row struct {
	Detail        store.TransactionDetail
	AccountType   string            // YNAB account type: "checking", "creditCard", …
	CategoryGroup string            // group of the current category; "" if none
	Locks         []store.HumanLock // store.Locks for this transaction
}

// Guard applies §3.2 step 1. Transfers, credit-card and debt payments, P2P
// payments, existing splits, unapproved rows already in Ready to Assign, and
// locked rows are skipped, as is a row with neither payee nor memo.
func Guard(r Row) Skip {
	d := r.Detail
	switch {
	case d.TransferAccountID != nil && *d.TransferAccountID != "":
		return SkipTransfer
	case isCCPayment(r):
		return SkipCCPayment
	// A split parent's category is YNAB's sentinel, which the mirror doesn't
	// hold, so a category id with no name is a split whose children are gone.
	case d.IsSplit || (d.CategoryID != nil && d.CategoryName == nil):
		return SkipSplit
	case !d.Approved && d.CategoryName != nil && fold(*d.CategoryName) == fold(readyToAssignName):
		return SkipReadyToAssign
	case len(r.Locks) > 0:
		return SkipLocked
	case isP2P(r):
		return SkipP2P
	case strings.TrimSpace(PayeeName(d)) == "" && (d.Memo == nil || strings.TrimSpace(*d.Memo) == ""):
		return SkipNoPayee
	}
	return SkipNone
}

// Account types a card or loan payment lands on as an inflow.
var debtAccountTypes = map[string]bool{
	"creditCard": true, "lineOfCredit": true,
	"mortgage": true, "autoLoan": true, "studentLoan": true, "personalLoan": true,
	"medicalDebt": true, "otherDebt": true,
}

var (
	paymentWords = wordSet("payment", "pmt", "pymt", "autopay", "epay", "paymt")
	// cardWords mark the paying side of a card payment, so a utility's
	// "AUTOPAY" from checking isn't one.
	cardWords = wordSet("card", "crd", "cardmember", "applecard", "amex", "barclaycard", "discover", "creditcard")
)

// isCCPayment spots a card or debt payment that isn't a YNAB transfer: one
// already in the Credit Card Payments group, a debt account's "payment", an
// inflow to a card or loan account that reads like a payment, or an outflow
// elsewhere that reads like a card payment.
func isCCPayment(r Row) bool {
	d := r.Detail
	if fold(r.CategoryGroup) == fold(creditCardPaymentsGroup) {
		return true
	}
	if d.DebtTransactionType != nil && *d.DebtTransactionType == "payment" {
		return true
	}
	words := strings.Fields(store.NormalizePayee(PayeeName(d)))
	if !hasAny(words, paymentWords) {
		return false
	}
	if debtAccountTypes[r.AccountType] {
		return d.Amount > 0
	}
	return d.Amount < 0 && hasAny(words, cardWords)
}

func wordSet(ws ...string) map[string]bool {
	m := make(map[string]bool, len(ws))
	for _, w := range ws {
		m[w] = true
	}
	return m
}

func hasAny(words []string, set map[string]bool) bool {
	for _, w := range words {
		if set[w] {
			return true
		}
	}
	return false
}
