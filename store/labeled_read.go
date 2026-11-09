package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

const (
	readyToAssignName     = "Inflow: Ready to Assign"
	creditCardPaymentsGrp = "Credit Card Payments"
)

// LabeledRow is one transaction a human categorized.
type LabeledRow struct {
	Date         string
	Amount       int64  // milliunits; negative is an outflow
	Payee        string // NormalizePayee of the payee name, or of the imported name if there is none
	Memo         string
	Account      string
	Category     string
	InActiveList bool   // Category is in activeCategoryNames, ignoring case
	PlanID       string // the plan this row came from
}

// NormalizePayee reduces a payee name to a comparable form: lower case,
// apostrophes removed, and every other run of non-alphanumerics a single space.
func NormalizePayee(name string) string {
	var b strings.Builder
	pendingSpace := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r == '\'' || r == '’':
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			if pendingSpace && b.Len() > 0 {
				b.WriteByte(' ')
			}
			pendingSpace = false
			b.WriteRune(r)
		default:
			pendingSpace = true
		}
	}
	return b.String()
}

// labeledCandidate is a LabeledRow with what de-duplication needs.
type labeledCandidate struct {
	LabeledRow
	importID *string
}

// fallbackKey identifies a copied row that can't be matched by import_id.
func (r labeledCandidate) fallbackKey() string {
	return fmt.Sprintf("%s|%d|%s", r.Date, r.Amount, r.Payee)
}

// LabeledHistory returns the human-labelled transactions of planIDs, newest
// date first. A row counts if it is approved, has a real category, and is not
// deleted, a split or a transfer. Rows in an internal category, Ready to Assign,
// the Credit Card Payments group or excludedNames are left out entirely. Names
// compare ignoring case.
//
// A row finbot wrote is approved too, but its category is the model's, so it
// counts only with a category lock, which says a person chose it. Without that,
// finbot's guesses would feed the rules and calibration as ground truth. An open
// intent counts as written, since it may have reached YNAB; a failed write doesn't.
//
// A transaction copied across plans comes back once, from the newest plan that
// has a label for it: an older plan's label still counts when the newer copy is
// unapproved. See newestFirst for what newest means. Rows are the same
// transaction when their import_ids match or, if either has none, when date,
// amount and normalized payee match. Look-alikes within one plan are separate
// purchases and are all kept.
func (d *DB) LabeledHistory(ctx context.Context, planIDs, activeCategoryNames, excludedNames []string) ([]LabeledRow, error) {
	active := foldSet(activeCategoryNames)
	excluded := foldSet(excludedNames)
	plans, err := d.newestFirst(ctx, planIDs)
	if err != nil {
		return nil, err
	}

	seenImports := map[string]bool{}
	seenAll := map[string]bool{}      // fallback keys of every kept row
	seenNoImport := map[string]bool{} // fallback keys of kept rows with no import_id
	var out []LabeledRow
	for _, planID := range plans {
		rows, err := d.labeledCandidates(ctx, planID, active, excluded)
		if err != nil {
			return nil, err
		}
		var kept []labeledCandidate
		for _, r := range rows {
			key := r.fallbackKey()
			var dup bool
			if r.importID != nil {
				dup = seenImports[*r.importID] || seenNoImport[key]
			} else {
				dup = seenAll[key]
			}
			if !dup {
				kept = append(kept, r)
				out = append(out, r.LabeledRow)
			}
		}
		// Only after the whole plan, so look-alikes within it don't collapse.
		for _, r := range kept {
			key := r.fallbackKey()
			seenAll[key] = true
			if r.importID != nil {
				seenImports[*r.importID] = true
			} else {
				seenNoImport[key] = true
			}
		}
	}
	// Stable: on the same date, newer plans stay first.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date > out[j].Date })
	return out, nil
}

// newestFirst orders plans by their latest live transaction date, newest first.
// The mirror has no plan creation date, and a plan copied by Fresh Start stops
// receiving transactions. Ties keep the caller's order.
func (d *DB) newestFirst(ctx context.Context, planIDs []string) ([]string, error) {
	latest := make(map[string]string, len(planIDs))
	for _, id := range planIDs {
		var date string
		err := d.r.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(date), '') FROM transactions WHERE plan_id = ? AND deleted = 0`, id).Scan(&date)
		if err != nil {
			return nil, fmt.Errorf("store: plan recency: %w", err)
		}
		latest[id] = date
	}
	plans := append([]string(nil), planIDs...)
	sort.SliceStable(plans, func(i, j int) bool { return latest[plans[i]] > latest[plans[j]] })
	return plans, nil
}

// labeledCandidates reads one plan's labelled rows, newest date first. The inner
// join on categories drops a split parent left with no live children, whose
// sentinel category isn't in the table.
//
// A hidden category moves to YNAB's hidden group, so the Credit Card Payments
// check also looks at the group it came from.
func (d *DB) labeledCandidates(ctx context.Context, planID string, active, excluded map[string]bool) ([]labeledCandidate, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT t.date, t.amount, COALESCE(p.name, t.import_payee_name, ''), COALESCE(t.memo, ''), COALESCE(a.name, ''),
       c.name, COALESCE(g.name, ''), COALESCE(og.name, ''), t.import_id
FROM transactions t
JOIN categories c            ON c.plan_id = t.plan_id AND c.id = t.category_id
LEFT JOIN category_groups g  ON g.plan_id = t.plan_id AND g.id = c.category_group_id
LEFT JOIN category_groups og ON og.plan_id = t.plan_id AND og.id = c.original_category_group_id
LEFT JOIN accounts a         ON a.plan_id = t.plan_id AND a.id = t.account_id
LEFT JOIN payees p           ON p.plan_id = t.plan_id AND p.id = t.payee_id
WHERE t.plan_id = ? AND t.approved = 1 AND t.deleted = 0 AND t.transfer_account_id IS NULL AND c.internal = 0
  AND NOT EXISTS (SELECT 1 FROM subtransactions s
                  WHERE s.plan_id = t.plan_id AND s.transaction_id = t.id AND s.deleted = 0)
  AND (EXISTS (SELECT 1 FROM human_locks hl
               WHERE hl.plan_id = t.plan_id AND hl.txn_id = t.id AND hl.field = ?)
       OR NOT EXISTS (SELECT 1 FROM write_log wl
                      WHERE wl.plan_id = t.plan_id AND wl.txn_id = t.id AND wl.status IN ('intent', 'done')))
ORDER BY t.date DESC, t.id`, planID, LockFieldCategory)
	if err != nil {
		return nil, fmt.Errorf("store: labeled history: %w", err)
	}
	defer rows.Close()
	var out []labeledCandidate
	for rows.Next() {
		var (
			r                labeledCandidate
			group, origGroup string
		)
		if err := rows.Scan(&r.Date, &r.Amount, &r.Payee, &r.Memo, &r.Account, &r.Category, &group, &origGroup, &r.importID); err != nil {
			return nil, fmt.Errorf("store: scan labeled row: %w", err)
		}
		name := fold(r.Category)
		if name == fold(readyToAssignName) || excluded[name] ||
			fold(group) == fold(creditCardPaymentsGrp) || fold(origGroup) == fold(creditCardPaymentsGrp) {
			continue
		}
		r.Payee = NormalizePayee(r.Payee)
		r.PlanID = planID
		r.InActiveList = active[name]
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: labeled history: %w", err)
	}
	return out, nil
}

func fold(s string) string { return strings.ToLower(s) }

func foldSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[fold(n)] = true
	}
	return set
}
