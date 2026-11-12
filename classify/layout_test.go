package classify

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

// cspLayout is the active plan after the M3 restructure: the spec's groups and
// categories (names only, no ids), plus the visible categories the spec leaves
// alone, where they land (Brendan, 2026-11-12), YNAB's internal categories and
// a credit card. The Retired group is hidden.
func cspLayout(t *testing.T) []store.CategoryWithGroup {
	t.Helper()
	data, err := os.ReadFile("../deploy/restructure/csp-2026-11.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Groups []struct {
			Name       string `json:"name"`
			Categories []struct {
				Name string `json:"name"`
			} `json:"categories"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	var cats []store.CategoryWithGroup
	add := func(group, name string) {
		c := cat(group, name)
		c.GroupHidden = group == "Retired"
		cats = append(cats, c)
	}
	for _, g := range spec.Groups {
		for _, c := range g.Categories {
			add(g.Name, c.Name)
		}
	}
	add("Savings Goals", "Buffer")
	for _, n := range []string{"Reimbursement", "Subs - Bark", "Subs - GoPro", "Subs - Point.Me"} {
		add("Fixed Costs", n)
	}
	add("Guilt-Free", "Business Bike")
	add("Investments", "Mortgage Payoff")
	add("Internal Master Category", "Inflow: Ready to Assign")
	add("Internal Master Category", "Uncategorized")
	add("Credit Card Payments", "Chase Sapphire")
	return cats
}

func TestCandidates_CSPLayout(t *testing.T) {
	set := Candidates(cspLayout(t), testPolicy(t))
	if len(set.Warnings) != 0 {
		t.Errorf("warnings on the new layout: %q", set.Warnings)
	}
	cands := map[string]Candidate{}
	for _, c := range set.Candidates {
		cands[c.Name] = c
	}
	for _, n := range []string{"Groceries", "Dining Out", "Shopping", "Household & Kids", "Gas & Transport",
		"Reimbursement", "Home - Mortgage", "Subs - Spotify", "Church - Tithing", "Church - Fast Offerings"} {
		if _, ok := cands[n]; !ok {
			t.Errorf("%q is not a candidate", n)
		}
	}
	for _, n := range []string{"Groceries", "Dining Out", "Shopping", "Household & Kids", "Gas & Transport", "Reimbursement"} {
		if cands[n].Description == "" {
			t.Errorf("%q has no description", n)
		}
	}
	setAside := map[string]bool{}
	for _, n := range set.SetAsides {
		setAside[n] = true
	}
	var events int
	for _, c := range cspLayout(t) {
		event := strings.HasPrefix(c.Name, "Birthday") || strings.HasPrefix(c.Name, "Holiday") || c.Name == "Fun - Next Trip"
		if event {
			events++
		}
		if event || c.Name == "Retirement" || c.Name == "Stock Investments" || c.Name == "Mortgage Payoff" {
			if _, ok := cands[c.Name]; ok {
				t.Errorf("%q is a candidate", c.Name)
			}
			if !setAside[c.Name] {
				t.Errorf("%q is not a set-aside, so its labels would become rules", c.Name)
			}
		}
	}
	if events != 9 {
		t.Fatalf("fixture has %d event funds, want 9 (6 birthday, 2 holiday, 1 trip)", events)
	}
	for _, n := range []string{"Discretionary", "Talk About", "Needs a job!", "Chase Sapphire", "Uncategorized"} {
		if _, ok := cands[n]; ok {
			t.Errorf("%q is a candidate", n)
		}
	}
	for _, c := range set.Candidates {
		if strings.Contains(c.Description, "Discretionary") {
			t.Errorf("%q description mentions Discretionary", c.Name)
		}
	}
}
