package classify

import (
	"fmt"
	"sort"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

const (
	readyToAssignName       = "Inflow: Ready to Assign"
	creditCardPaymentsGroup = "Credit Card Payments"
	// internalGroupName is the group YNAB keeps its internal categories in.
	// store.CategoryWithGroup has no internal flag, so the group name and the
	// known internal names stand in for it.
	internalGroupName = "Internal Master Category"
)

// internalNames are YNAB's internal categories, by folded name.
var internalNames = map[string]bool{
	fold(readyToAssignName):             true,
	fold("Uncategorized"):               true,
	fold("Deferred Income SubCategory"): true,
	fold("Split (Multiple Categories)"): true,
}

// CandidateSet is the active plan's classifier targets under a policy.
type CandidateSet struct {
	Candidates []Candidate // by group, then name, as the store returns them
	// SetAsides are the plan's live set-aside category names. Pass them to
	// store.LabeledHistory as excludedNames, so they never become rules.
	SetAsides []string
	// Warnings name policy entries that match nothing in the plan. They are
	// ignored, never created; the caller logs them. Names only, never ids.
	Warnings []string
}

// Candidates draws the classifier targets from the active plan's categories
// (store.CategoriesWithGroups). Left out: deleted or hidden categories, those in
// a deleted or hidden group, YNAB's internal ones (Ready to Assign included),
// the Credit Card Payments group, and the policy's set-asides. A description is
// the policy override if there is one, else the YNAB note. Nothing is renamed or
// created.
func Candidates(cats []store.CategoryWithGroup, p Policy) CandidateSet {
	var set CandidateSet
	usedKeyword := map[string]bool{}
	present := map[string]bool{} // folded names of live categories, hidden included
	for _, c := range cats {
		if c.Deleted {
			continue
		}
		present[fold(c.Name)] = true
		kw, setAside := p.setAsideKeyword(c.Name)
		if setAside {
			usedKeyword[kw] = true
			set.SetAsides = append(set.SetAsides, c.Name)
		}
		if setAside || !Eligible(c) {
			continue
		}
		desc, ok := p.description(c.Name)
		if !ok && c.Note != nil {
			desc = *c.Note
		}
		set.Candidates = append(set.Candidates, Candidate{Name: c.Name, Group: c.GroupName, Description: desc})
	}
	for _, k := range p.SetAsideKeywords {
		if !usedKeyword[k] {
			set.Warnings = append(set.Warnings, fmt.Sprintf("policy set-aside keyword %q matches no category in the plan", k))
		}
	}
	targets := map[string]bool{}
	for _, c := range set.Candidates {
		targets[fold(c.Name)] = true
	}
	var names []string
	for n := range p.Descriptions {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		switch {
		case !present[fold(n)]:
			set.Warnings = append(set.Warnings, fmt.Sprintf("policy description for %q: no such category in the plan", n))
		case !targets[fold(n)]:
			set.Warnings = append(set.Warnings, fmt.Sprintf("policy description for %q: category is not a classifier target", n))
		}
	}
	return set
}

// Eligible reports whether a person or the classifier may answer with c: not
// deleted or hidden, its group neither, not one of YNAB's internal categories,
// and not in the Credit Card Payments group. Set-asides are eligible here;
// Candidates drops them separately.
func Eligible(c store.CategoryWithGroup) bool {
	return !c.Deleted && !c.Hidden && !c.GroupHidden && !c.GroupDeleted && !isInternal(c) &&
		fold(c.GroupName) != fold(creditCardPaymentsGroup)
}

func isInternal(c store.CategoryWithGroup) bool {
	return internalNames[fold(c.Name)] || fold(c.GroupName) == fold(internalGroupName)
}
