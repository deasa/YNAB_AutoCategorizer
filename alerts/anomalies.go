package alerts

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

// Anomaly alert kinds (R11, §3.7). None is critical: they all spend the
// monthly budget, and the rest roll up into the monthly report.
const (
	KindDuplicate   = "duplicate"    // same payee and amount within DuplicateWindowDays
	KindPriceChange = "price_change" // a recurring charge moved more than PriceChangePct
	KindLargeCharge = "large_charge" // far above the payee's usual charge
)

// anomalyKinds, in the order the rules are tried. A transaction raises at most
// one anomaly alert.
var anomalyKinds = []string{KindDuplicate, KindPriceChange, KindLargeCharge}

// Rule parameters.
const (
	// DuplicateWindowDays: YNAB dates have no time of day, so "within 72h" is
	// "dated at most 3 days apart".
	DuplicateWindowDays = 3

	// PriceChangePct: a recurring charge alerts when it differs from the
	// trailing median by more than this percentage.
	PriceChangePct = 10
	// PriceMinHistory prior charges are needed before a price change can fire.
	PriceMinHistory = 3
	// priceWindow is how many of the payee's most recent prior charges make
	// the trailing median.
	priceWindow = 6
	// recurringTolerancePct: a payee is a recurring bill when its trailing
	// charges sit within this percentage of their median (all of them, or
	// all but one once the window holds 5 or more). This keeps the rule to
	// fixed-price bills; a restaurant whose checks vary never qualifies.
	recurringTolerancePct = 2

	// LargeMinHistory prior charges are needed before a large charge can fire.
	LargeMinHistory = 5
	// largeWindow is how many of the payee's most recent prior charges the
	// large-charge rule looks at.
	largeWindow = 24
	// largeMinExcess (milliunits, $25) is the least a large charge must exceed
	// the payee's median by, so small payees don't alert on pocket change.
	largeMinExcess = 25_000
)

// Mirror is the read side of the mirror and the anomaly cursor; *store.DB
// implements it.
type Mirror interface {
	AlertCursor(ctx context.Context, planID string) (time.Time, bool, error)
	SetAlertCursor(ctx context.Context, planID string, lastFirstSeenAt time.Time) error
	TransactionsFirstSeenAfter(ctx context.Context, planID string, t time.Time) ([]store.TransactionDetail, error)
	PayeeCharges(ctx context.Context, planID, normalizedPayee, before string) ([]store.PayeeCharge, error)
}

// Anomalies runs the deterministic anomaly rules (no LLM) over the plan's
// transactions first stored since the last run, sending each through Send, so
// every alert is non-critical, budgeted and raised at most once. Run it after
// each successful sync.
//
// The first run against a plan (no cursor yet) is a baseline: it moves the
// cursor to the newest row and alerts on nothing, so the initial export never
// floods Slack with history.
//
// Only live outflows are evaluated: refunds and other inflows, transfers and
// deleted rows never alert, and they never count as history. Rules, in order;
// a transaction raises at most one alert:
//
//   - Duplicate: another outflow to the same payee for the same amount, dated
//     at most DuplicateWindowDays apart, both imported with different import
//     ids. A pair (or a cluster) alerts once.
//   - Price change: the payee is a recurring bill (see recurringTolerancePct)
//     with at least PriceMinHistory earlier charges, and the charge differs
//     from the trailing median by more than PriceChangePct.
//   - Large charge: at least LargeMinHistory earlier charges, and the charge
//     is above all of: 3× the median, the median plus 6 robust standard
//     deviations (1.4826 × the median absolute deviation, the usual robust
//     estimate; MAD ignores the odd outlier where a mean and standard
//     deviation would not), and the median plus $25. On a payee whose charges
//     barely vary, MAD is near zero and 3× the median governs; on a payee
//     whose charges vary widely, the MAD term raises the bar.
//
// "Earlier" history is the payee's charges dated before the transaction.
// Payees match by normalized name (the payee, else the imported name), the
// same matching as the classifier's history.
//
// The cursor advances past every row only when all of them were handled; a
// store error or a failed post holds it before the first such row, so the
// next run retries it (rows already alerted are skipped as duplicates).
func (a *Alerter) Anomalies(ctx context.Context) error {
	if a.mirror == nil {
		return fmt.Errorf("alerts: anomalies need a Mirror")
	}
	cursor, ok, err := a.mirror.AlertCursor(ctx, a.planID)
	if err != nil {
		return fmt.Errorf("alerts: %w", err)
	}
	txns, err := a.mirror.TransactionsFirstSeenAfter(ctx, a.planID, cursor)
	if err != nil {
		return fmt.Errorf("alerts: %w", err)
	}
	if len(txns) == 0 {
		return nil
	}
	if !ok {
		a.log.Info("anomaly baseline taken", "rows", len(txns))
		return a.setCursor(ctx, txns[len(txns)-1].FirstSeenAt)
	}

	var (
		errs   []error
		held   bool      // a row failed; the cursor stops before it
		failAt time.Time // first_seen_at of the first failed row
	)
	for _, t := range txns {
		done, err := a.evaluate(ctx, t)
		if err != nil {
			errs = append(errs, err)
		}
		if !done && !held {
			held, failAt = true, t.FirstSeenAt
		}
	}
	// Rows sharing a first_seen_at stand or fall together: the cursor is a
	// time, so it may only pass times every row of which was handled.
	next := cursor
	for _, t := range txns {
		if held && !t.FirstSeenAt.Before(failAt) {
			break
		}
		next = t.FirstSeenAt
	}
	if next.After(cursor) {
		if err := a.setCursor(ctx, next); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (a *Alerter) setCursor(ctx context.Context, t time.Time) error {
	if err := a.mirror.SetAlertCursor(ctx, a.planID, t); err != nil {
		return fmt.Errorf("alerts: %w", err)
	}
	return nil
}

// evaluate runs the rules on t. done is false when t must be tried again: a
// store error (returned) or a failed post.
func (a *Alerter) evaluate(ctx context.Context, t store.TransactionDetail) (done bool, err error) {
	if t.Deleted || t.Amount >= 0 || t.TransferAccountID != nil {
		return true, nil
	}
	payee := payeeName(t)
	norm := store.NormalizePayee(payee)
	if norm == "" {
		return true, nil
	}
	for _, kind := range anomalyKinds {
		has, err := a.store.HasAlert(ctx, a.planID, kind, t.ID)
		if err != nil {
			return false, fmt.Errorf("alerts: %w", err)
		}
		if has {
			return true, nil // this transaction already alerted
		}
	}
	day, err := time.Parse(time.DateOnly, t.Date)
	if err != nil {
		return true, nil // a malformed date can't be windowed; skip it, don't wedge the cursor
	}
	// History up to the end of the duplicate window: duplicates may be dated
	// after t; the other rules use only charges dated before it.
	before := day.AddDate(0, 0, DuplicateWindowDays+1).Format(time.DateOnly)
	charges, err := a.mirror.PayeeCharges(ctx, a.planID, norm, before)
	if err != nil {
		return false, fmt.Errorf("alerts: %w", err)
	}

	al, ok, err := a.duplicate(ctx, t, payee, day, charges)
	if err != nil {
		return false, err
	}
	if !ok {
		var prior []int64
		for _, c := range charges {
			if c.Date < t.Date && c.TxnID != t.ID {
				prior = append(prior, -c.Amount)
			}
		}
		if al, ok = priceChange(t, payee, prior); !ok {
			al, ok = largeCharge(t, payee, prior)
		}
	}
	if !ok {
		return true, nil
	}
	out, err := a.Send(ctx, al)
	if err != nil {
		return false, err
	}
	return out != Unsent, nil
}

// duplicate finds an imported charge to the same payee for the same amount,
// dated within DuplicateWindowDays of t, with a different import id. If any
// such partner already alerted as a duplicate, the cluster has been reported
// and t stays quiet.
func (a *Alerter) duplicate(ctx context.Context, t store.TransactionDetail, payee string, day time.Time,
	charges []store.PayeeCharge) (Alert, bool, error) {
	if t.ImportID == nil {
		return Alert{}, false, nil
	}
	var partner *store.PayeeCharge
	for i, c := range charges {
		if c.TxnID == t.ID || c.Amount != t.Amount || c.ImportID == nil || *c.ImportID == *t.ImportID {
			continue
		}
		cd, err := time.Parse(time.DateOnly, c.Date)
		if err != nil {
			continue
		}
		if gap := day.Sub(cd); gap > DuplicateWindowDays*24*time.Hour || gap < -DuplicateWindowDays*24*time.Hour {
			continue
		}
		has, err := a.store.HasAlert(ctx, a.planID, KindDuplicate, c.TxnID)
		if err != nil {
			return Alert{}, false, fmt.Errorf("alerts: %w", err)
		}
		if has {
			return Alert{}, false, nil
		}
		if partner == nil {
			partner = &charges[i]
		}
	}
	if partner == nil {
		return Alert{}, false, nil
	}
	first, second := partner.Date, t.Date
	if second < first {
		first, second = second, first
	}
	return Alert{
		Kind: KindDuplicate, DedupeKey: t.ID,
		Text: fmt.Sprintf(":repeat: Possible duplicate: *%s* charged %s twice, on %s and %s%s. "+
			"If it's a double charge, dispute it with the merchant or bank.",
			payee, dollars(-t.Amount), shortDate(first), shortDate(second), inAccount(t)),
	}, true, nil
}

// priceChange fires when the payee is a recurring bill and t moved more than
// PriceChangePct from the trailing median. prior holds earlier charges as
// positive milliunits, oldest first.
func priceChange(t store.TransactionDetail, payee string, prior []int64) (Alert, bool) {
	if len(prior) < PriceMinHistory {
		return Alert{}, false
	}
	window := prior[max(0, len(prior)-priceWindow):]
	med := median(window)
	if med <= 0 || !recurring(window, med) {
		return Alert{}, false
	}
	amt := -t.Amount
	diff := amt - med
	// |diff| / med > PriceChangePct%, in integers.
	if abs(diff)*100 <= med*PriceChangePct {
		return Alert{}, false
	}
	dir := "up"
	if diff < 0 {
		dir = "down"
	}
	pct := (abs(diff)*100 + med/2) / med
	return Alert{
		Kind: KindPriceChange, DedupeKey: t.ID,
		Text: fmt.Sprintf(":chart_with_upwards_trend: *%s* charged %s on %s%s, %s %d%% from its usual %s.",
			payee, dollars(amt), shortDate(t.Date), inAccount(t), dir, pct, dollars(med)),
	}, true
}

// recurring reports whether the window's charges sit within
// recurringTolerancePct of med: all of them, or all but one when the window
// holds 5 or more.
func recurring(window []int64, med int64) bool {
	off := 0
	for _, x := range window {
		if abs(x-med)*100 > med*recurringTolerancePct {
			off++
		}
	}
	allowed := 0
	if len(window) >= 5 {
		allowed = 1
	}
	return off <= allowed
}

// largeCharge fires when t is far above the payee's usual charge; see
// Anomalies for the rule. prior holds earlier charges as positive milliunits,
// oldest first.
func largeCharge(t store.TransactionDetail, payee string, prior []int64) (Alert, bool) {
	if len(prior) < LargeMinHistory {
		return Alert{}, false
	}
	window := prior[max(0, len(prior)-largeWindow):]
	med := median(window)
	devs := make([]int64, len(window))
	for i, x := range window {
		devs[i] = abs(x - med)
	}
	mad := median(devs)
	threshold := max(3*med, med+int64(6*1.4826*float64(mad)), med+largeMinExcess)
	amt := -t.Amount
	if amt <= threshold {
		return Alert{}, false
	}
	return Alert{
		Kind: KindLargeCharge, DedupeKey: t.ID,
		Text: fmt.Sprintf(":money_with_wings: *%s* charged %s on %s%s, well above its usual %s.",
			payee, dollars(amt), shortDate(t.Date), inAccount(t), dollars(med)),
	}, true
}

// payeeName is the name a transaction's payee goes by: the payee, else the
// imported name, as store.PayeeCharges matches it.
func payeeName(t store.TransactionDetail) string {
	if t.PayeeName != nil {
		return *t.PayeeName
	}
	if t.ImportPayeeName != nil {
		return *t.ImportPayeeName
	}
	return ""
}

func inAccount(t store.TransactionDetail) string {
	if t.AccountName == nil || *t.AccountName == "" {
		return ""
	}
	return " (" + *t.AccountName + ")"
}

// median of xs (not modified); the mean of the middle two for an even count.
func median(xs []int64) int64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// dollars renders positive milliunits: 15990 → "$15.99".
func dollars(milli int64) string {
	cents := (milli + 5) / 10
	return fmt.Sprintf("$%d.%02d", cents/100, cents%100)
}

// shortDate renders "2026-11-03" as "Nov 3".
func shortDate(date string) string {
	d, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return date
	}
	return d.Format("Jan 2")
}
