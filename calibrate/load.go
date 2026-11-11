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
	return history, set, nil
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
