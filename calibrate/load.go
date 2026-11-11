package calibrate

import (
	"context"
	"fmt"

	"github.com/deasa/YNAB_AutoCategorizer/classify"
	"github.com/deasa/YNAB_AutoCategorizer/store"
)

// Inputs reads what Run needs from the mirror: the active plan's candidates
// under policy, and the labelled history of the active plan plus every plan
// whose last scan found Discretionary. Labels are matched to candidates by
// exact name (ignoring case); there is no old→new mapping.
//
// Each row's Payee is the payee's display name, as the runtime sees it, not
// the normalized form LabeledHistory returns: the P2P guardrail reads the
// casing of a payee ("Jane Doe" is person-shaped, "jane doe" isn't), and the
// model sees the payee as written.
func Inputs(ctx context.Context, db *store.DB, activePlan string, policy classify.Policy) ([]store.LabeledRow, classify.CandidateSet, error) {
	cats, err := db.CategoriesWithGroups(ctx, activePlan)
	if err != nil {
		return nil, classify.CandidateSet{}, fmt.Errorf("calibrate: categories: %w", err)
	}
	set := classify.Candidates(cats, policy)
	scans, err := db.PlanScans(ctx)
	if err != nil {
		return nil, set, fmt.Errorf("calibrate: plan scans: %w", err)
	}
	names := make([]string, len(set.Candidates))
	for i, c := range set.Candidates {
		names[i] = c.Name
	}
	history, err := db.LabeledHistory(ctx, labelPlans(activePlan, scans), names, set.SetAsides)
	if err != nil {
		return nil, set, fmt.Errorf("calibrate: labelled history: %w", err)
	}
	if err := restorePayees(ctx, db, history); err != nil {
		return nil, set, err
	}
	return history, set, nil
}

// restorePayees replaces each row's normalized Payee with the display name it
// came from in the row's plan. When several names normalize alike ("JANE DOE",
// "Jane Doe"), the one on the most transactions wins, then the first in byte
// order. A row with no match keeps its normalized payee.
func restorePayees(ctx context.Context, db *store.DB, rows []store.LabeledRow) error {
	names := map[string]map[string]string{} // plan → normalized → display
	for i := range rows {
		r := &rows[i]
		if r.Payee == "" {
			continue
		}
		m, ok := names[r.PlanID]
		if !ok {
			var err error
			if m, err = displayNames(ctx, db, r.PlanID); err != nil {
				return err
			}
			names[r.PlanID] = m
		}
		if name, ok := m[r.Payee]; ok {
			r.Payee = name
		}
	}
	return nil
}

// displayNames maps each normalized payee of planID to its display name,
// using the same name the labelled-history query reads.
func displayNames(ctx context.Context, db *store.DB, planID string) (map[string]string, error) {
	rows, err := db.Reader().QueryContext(ctx, `
SELECT COALESCE(p.name, t.import_payee_name, '') AS display, COUNT(*)
FROM transactions t
LEFT JOIN payees p ON p.plan_id = t.plan_id AND p.id = t.payee_id
WHERE t.plan_id = ? AND t.deleted = 0
GROUP BY display`, planID)
	if err != nil {
		return nil, fmt.Errorf("calibrate: payee names: %w", err)
	}
	defer rows.Close()
	type pick struct {
		name  string
		count int
	}
	best := map[string]pick{}
	for rows.Next() {
		var p pick
		if err := rows.Scan(&p.name, &p.count); err != nil {
			return nil, fmt.Errorf("calibrate: payee names: %w", err)
		}
		norm := store.NormalizePayee(p.name)
		if norm == "" {
			continue
		}
		if b, ok := best[norm]; !ok || p.count > b.count || (p.count == b.count && p.name < b.name) {
			best[norm] = p
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("calibrate: payee names: %w", err)
	}
	out := make(map[string]string, len(best))
	for norm, p := range best {
		out[norm] = p.name
	}
	return out, nil
}

// labelPlans is the active plan, then each plan with Discretionary, each once.
func labelPlans(active string, scans []store.PlanScan) []string {
	plans := []string{active}
	seen := map[string]bool{active: true}
	for _, s := range scans {
		if s.HasDiscretionary && !seen[s.PlanID] {
			seen[s.PlanID] = true
			plans = append(plans, s.PlanID)
		}
	}
	return plans
}
