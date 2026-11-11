package classify

import (
	"reflect"
	"strings"
	"testing"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

func cat(group, name string) store.CategoryWithGroup {
	return store.CategoryWithGroup{ID: "id-" + name, Name: name, GroupID: "g-" + group, GroupName: group}
}

func testPolicy(t *testing.T) Policy {
	t.Helper()
	p, err := DefaultPolicy()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func names(cs []Candidate) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func TestDefaultPolicy(t *testing.T) {
	p := testPolicy(t)
	for _, k := range []string{"Birthday", "Trip", "Christmas", "Giving"} {
		if !p.IsSetAside("x " + k) {
			t.Errorf("policy doesn't exclude %q", k)
		}
	}
	if d, ok := p.description("discretionary"); !ok || !strings.Contains(d, "Amazon orders") {
		t.Errorf("Discretionary policy missing: %q", d)
	}
}

func TestPolicy_SetAsideMatching(t *testing.T) {
	p := testPolicy(t)
	for name, want := range map[string]bool{
		"Holiday - Christmas": true,
		"Fun - Next Trip":     true,
		"Giving - Generosity": true,
		"Kids Birthdays":      true,
		"Gifts":               true,
		"Thanksgiving Dinner": false,
		"Tripod Rental":       false,
		"Discretionary":       false,
		"Home - Electric":     false,
	} {
		if got := p.IsSetAside(name); got != want {
			t.Errorf("IsSetAside(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestLoadPolicy_Rejects(t *testing.T) {
	for name, js := range map[string]string{
		"unknown field":  `{"version":1,"set_aside":["Trip"]}`,
		"no version":     `{"set_aside_keywords":["Trip"]}`,
		"multi-word kw":  `{"version":1,"set_aside_keywords":["Next Trip"]}`,
		"duplicate desc": `{"version":1,"descriptions":{"Discretionary":"a","discretionary":"b"}}`,
		"not json":       `{`,
	} {
		if _, err := LoadPolicy([]byte(js)); err == nil {
			t.Errorf("%s: LoadPolicy accepted %s", name, js)
		}
	}
}

func TestCandidates_Exclusions(t *testing.T) {
	hidden := cat("Home", "Home - Old Phone")
	hidden.Hidden = true
	deleted := cat("Home", "Home - Gone")
	deleted.Deleted = true
	inHiddenGroup := cat("Old Stuff", "Old Thing")
	inHiddenGroup.GroupHidden = true
	inDeletedGroup := cat("Dead Group", "Dead Thing")
	inDeletedGroup.GroupDeleted = true
	hiddenCC := cat("Hidden Categories", "Old Visa")
	hiddenCC.Hidden = true
	note := "Power bill"
	electric := cat("Home", "Home - Electric")
	electric.Note = &note

	cats := []store.CategoryWithGroup{
		cat("Internal Master Category", "Inflow: Ready to Assign"),
		cat("Internal Master Category", "Uncategorized"),
		cat("Internal Master Category", "Deferred Income SubCategory"),
		cat("", "Split (Multiple Categories)"), // group not mirrored
		cat("Credit Card Payments", "Chase Sapphire"),
		hiddenCC,
		cat("Guilt-Free", "Discretionary"),
		cat("Savings", "Holiday - Christmas"),
		cat("Savings", "Fun - Next Trip"),
		cat("Savings", "Giving - Generosity"),
		cat("Savings", "Kids Birthdays"),
		electric,
		hidden, deleted, inHiddenGroup, inDeletedGroup,
	}
	set := Candidates(cats, testPolicy(t))
	if got, want := names(set.Candidates), []string{"Discretionary", "Home - Electric"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	if got := set.Candidates[1]; got.Group != "Home" || got.Description != "Power bill" {
		t.Errorf("note not used as description: %+v", got)
	}
	if !strings.Contains(set.Candidates[0].Description, "Amazon orders") {
		t.Errorf("Discretionary override not applied: %q", set.Candidates[0].Description)
	}
	wantSetAsides := []string{"Holiday - Christmas", "Fun - Next Trip", "Giving - Generosity", "Kids Birthdays"}
	if !reflect.DeepEqual(set.SetAsides, wantSetAsides) {
		t.Errorf("SetAsides = %v, want %v", set.SetAsides, wantSetAsides)
	}
}

func TestCandidates_OverrideBeatsNote(t *testing.T) {
	note := "catch-all"
	d := cat("Guilt-Free", "Discretionary")
	d.Note = &note
	set := Candidates([]store.CategoryWithGroup{d}, testPolicy(t))
	if set.Candidates[0].Description == note {
		t.Fatal("YNAB note won over the policy override")
	}
}

func TestCandidates_MissingPolicyEntriesWarn(t *testing.T) {
	p := Policy{Version: 1, SetAsideKeywords: []string{"Trip", "Wedding"},
		Descriptions: map[string]string{"Discretionary": "x", "Fun Money": "y", "Fun - Next Trip": "z"}}
	cats := []store.CategoryWithGroup{cat("Guilt-Free", "Discretionary"), cat("Savings", "Fun - Next Trip")}
	set := Candidates(cats, p)
	if got := names(set.Candidates); !reflect.DeepEqual(got, []string{"Discretionary"}) {
		t.Fatalf("candidates = %v: a missing policy name must never be created", got)
	}
	joined := strings.Join(set.Warnings, "\n")
	for _, want := range []string{`"Wedding"`, `"Fun Money": no such category`, `"Fun - Next Trip": category is not a classifier target`} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings %q lack %s", set.Warnings, want)
		}
	}
	if len(set.Warnings) != 3 {
		t.Errorf("warnings = %q, want 3", set.Warnings)
	}
	for _, w := range set.Warnings {
		if strings.Contains(w, "id-") {
			t.Errorf("warning carries an id: %q", w)
		}
	}
}

func TestCandidatesHash(t *testing.T) {
	a := []Candidate{{"Discretionary", "Guilt-Free", "everyday"}, {"Home - Electric", "Home", "power"}}
	b := []Candidate{a[1], a[0]}
	if CandidatesHash(a) != CandidatesHash(b) {
		t.Fatal("hash depends on order")
	}
	renamed := []Candidate{{"Discretionary!", "Guilt-Free", "everyday"}, a[1]}
	redesc := []Candidate{{"Discretionary", "Guilt-Free", "everyday spending"}, a[1]}
	// A name/description boundary shift must not collide.
	shifted := []Candidate{{"Discretionaryeveryday", "Guilt-Free", ""}, a[1]}
	seen := map[string]bool{CandidatesHash(a): true}
	for name, c := range map[string][]Candidate{"renamed": renamed, "redescribed": redesc, "shifted": shifted} {
		h := CandidatesHash(c)
		if seen[h] {
			t.Errorf("%s: hash unchanged", name)
		}
		seen[h] = true
	}
	if !reflect.DeepEqual(a[0].Name, "Discretionary") {
		t.Fatal("CandidatesHash mutated its input")
	}
}

func TestPolicyHash(t *testing.T) {
	p := Policy{Version: 1, SetAsideKeywords: []string{"Trip", "Birthday"},
		Descriptions: map[string]string{"Discretionary": "everyday", "Groceries": "food"}}
	q := Policy{Version: 1, SetAsideKeywords: []string{"Birthday", "Trip"},
		Descriptions: map[string]string{"Groceries": "food", "Discretionary": "everyday"}}
	if PolicyHash(p) != PolicyHash(q) {
		t.Fatal("hash depends on order")
	}
	base := PolicyHash(p)
	for name, edit := range map[string]func(*Policy){
		"keyword":     func(p *Policy) { p.SetAsideKeywords = []string{"Trip", "Gift"} },
		"desc name":   func(p *Policy) { p.Descriptions = map[string]string{"Fun": "everyday", "Groceries": "food"} },
		"description": func(p *Policy) { p.Descriptions = map[string]string{"Discretionary": "all", "Groceries": "food"} },
		"version":     func(p *Policy) { p.Version = 2 },
	} {
		c := Policy{Version: p.Version, SetAsideKeywords: p.SetAsideKeywords, Descriptions: p.Descriptions}
		edit(&c)
		if PolicyHash(c) == base {
			t.Errorf("%s change left the hash unchanged", name)
		}
	}
	d := testPolicy(t)
	if PolicyHash(d) == "" || len(PolicyHash(d)) != 64 {
		t.Fatalf("default policy hash = %q", PolicyHash(d))
	}
}
