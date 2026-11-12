package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/ynab"
)

// fakeCatAPI is an in-memory plan with UUID ids, so a leaked id shows up.
type fakeCatAPI struct {
	groups  []*ynab.CategoryGroup
	next    int
	writes  int
	reads   int
	failAt  int // the failAt-th write fails (1-based); 0 never
	failErr error
}

func (f *fakeCatAPI) id() string {
	f.next++
	return fmt.Sprintf("%08x-0000-4000-8000-%012x", f.next, f.next)
}

func (f *fakeCatAPI) write() error {
	f.writes++
	if f.failAt != 0 && f.writes == f.failAt {
		return f.failErr
	}
	return nil
}

func (f *fakeCatAPI) GetCategories(context.Context) ([]*ynab.CategoryGroup, error) {
	f.reads++
	out := make([]*ynab.CategoryGroup, len(f.groups))
	for i, g := range f.groups {
		cp := *g
		cp.Categories = append([]ynab.Category(nil), g.Categories...)
		out[i] = &cp
	}
	return out, nil
}

func (f *fakeCatAPI) group(id string) *ynab.CategoryGroup {
	for _, g := range f.groups {
		if g.ID == id {
			return g
		}
	}
	return nil
}

func (f *fakeCatAPI) CreateCategoryGroup(_ context.Context, name string) (*ynab.CategoryGroup, error) {
	if err := f.write(); err != nil {
		return nil, err
	}
	g := &ynab.CategoryGroup{ID: f.id(), Name: name}
	f.groups = append(f.groups, g)
	return g, nil
}

func (f *fakeCatAPI) RenameCategoryGroup(_ context.Context, id, name string) (*ynab.CategoryGroup, error) {
	if err := f.write(); err != nil {
		return nil, err
	}
	g := f.group(id)
	if g == nil {
		return nil, errors.New("no such group")
	}
	g.Name = name
	return g, nil
}

func (f *fakeCatAPI) CreateCategory(_ context.Context, gid, name string) (*ynab.Category, error) {
	if err := f.write(); err != nil {
		return nil, err
	}
	g := f.group(gid)
	if g == nil {
		return nil, errors.New("no such group")
	}
	c := ynab.Category{CategoryBase: ynab.CategoryBase{ID: f.id(), CategoryGroupID: gid, Name: name}}
	g.Categories = append(g.Categories, c)
	return &c, nil
}

func (f *fakeCatAPI) UpdateCategory(_ context.Context, id string, e ynab.CategoryEdit) (*ynab.Category, error) {
	if err := f.write(); err != nil {
		return nil, err
	}
	for _, g := range f.groups {
		for i := range g.Categories {
			c := g.Categories[i]
			if c.ID != id {
				continue
			}
			if e.Name != nil {
				c.Name = *e.Name
			}
			if e.GoalTarget != nil {
				v := *e.GoalTarget
				c.GoalTarget = &v
			}
			if e.GoalTargetDate != nil {
				v := *e.GoalTargetDate
				c.GoalTargetDate = &v
			}
			if e.GroupID != nil && *e.GroupID != g.ID {
				to := f.group(*e.GroupID)
				if to == nil {
					return nil, errors.New("no such group")
				}
				g.Categories = append(g.Categories[:i], g.Categories[i+1:]...)
				c.CategoryGroupID = to.ID
				to.Categories = append(to.Categories, c)
			} else {
				g.Categories[i] = c
			}
			return &c, nil
		}
	}
	return nil, errors.New("no such category")
}

func (f *fakeCatAPI) addGroup(name string, cats ...string) *ynab.CategoryGroup {
	g := &ynab.CategoryGroup{ID: f.id(), Name: name}
	for _, n := range cats {
		g.Categories = append(g.Categories, ynab.Category{CategoryBase: ynab.CategoryBase{ID: f.id(), CategoryGroupID: g.ID, Name: n}})
	}
	f.groups = append(f.groups, g)
	return g
}

func (f *fakeCatAPI) cat(name string) *ynab.Category {
	for _, g := range f.groups {
		for i := range g.Categories {
			if g.Categories[i].Name == name {
				return &g.Categories[i]
			}
		}
	}
	return nil
}

func (f *fakeCatAPI) setTarget(name string, amt int64, date string) {
	c := f.cat(name)
	m := ynab.Milliunits(amt)
	c.GoalTarget = &m
	if date != "" {
		d, err := ynab.ParseDate(date)
		if err != nil {
			panic(err)
		}
		c.GoalTargetDate = &d
	}
}

// oldLayout is the live plan's layout before the restructure (names as of
// 2026-11-11), plus the internal and credit card groups the command skips.
func oldLayout() *fakeCatAPI {
	f := &fakeCatAPI{}
	f.addGroup("Mortgage & Utilities", "Home - Highland City Utilities", "Home - Internet", "Home - Mortgage", "Home - Electric", "Home - Natural Gas")
	f.addGroup("Budget Meeting", "Buffer", "Reimbursement", "Talk About", "Needs a job!", "Discretionary")
	f.addGroup("Fun Money", "Business Bike ", "Voice Lessons", "Fun - Brendan Under The Radar", "Fun - Hilarie Under The Radar", "Fun - Larks Dues", "Fun - Next Trip")
	f.addGroup("Subscriptions", "Subs - Bark", "Subs - TrainerRoad", "Subs - Lawn", "Google One", "Subs - Amazon Prime", "Subs - Canva",
		"Subs - Disney+", "Subs - Google Domains Website", "Subs - Spotify", "Subs - YNAB")
	f.addGroup("Monthly Variable Expenses", "Home - Kids Classes & Education", "Canva", "Home - Riverton City Utilities")
	f.addGroup("Monthly Savings Funds", "Birthday - Marie Party", "Birthday - Elliott Party", "Birthday - Benson Party", "Birthday - Marie Gifts",
		"Birthday - Elliott Gifts", "Birthday - Benson Gifts", "Holiday - Halloween", "Auto - Insurance", "Auto - Repairs and Expenses",
		"Auto - Replacement", "Church - Fast Offerings", "Church - Tithing", "Holiday - Christmas", "Giving - Generosity", "Home - Cell Phone",
		"Home - Improvement / Maintenance", "Home - Insurance", "Home - Tech / Computer Replacement")
	in := f.addGroup("Internal Master Category", "Inflow: Ready to Assign", "Uncategorized")
	in.Internal = true
	f.addGroup("Target Balance Goals", "Wise - Emergency Preparedness", "Wise - Buffer", "Wise - Medical & Dental", "2019 Disneyland")
	f.addGroup("Financial Independence", "Mortgage Payoff", "Retirement", "Stock Investments")
	cc := f.addGroup("Credit Card Payments", "B Sapphire Preferred")
	cc.Internal = true
	f.cat("2019 Disneyland").Hidden = true
	gone := f.addGroup("Old Deleted", "Ghost")
	gone.Deleted = true
	f.setTarget("Holiday - Christmas", 1500000, "2020-12-01")
	f.setTarget("Birthday - Elliott Party", 200000, "2027-09-18") // future: untouched
	f.setTarget("Home - Insurance", 800000, "2026-06-28")
	f.setTarget("Home - Cell Phone", 404000, "2021-11-27")
	return f
}

func cspSpec(t *testing.T) rsSpec {
	t.Helper()
	s, err := loadRsSpec("../deploy/restructure/csp-2026-11.json")
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	return s
}

var uuidPattern = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

func runRS(t *testing.T, f *fakeCatAPI, o rsOpts) (int, string) {
	t.Helper()
	rsNow = func() time.Time { return time.Date(2026, 11, 11, 9, 0, 0, 0, time.Local) }
	t.Cleanup(func() { rsNow = time.Now })
	var out, errb bytes.Buffer
	code := runRestructure(context.Background(), f, o, &out, &errb)
	all := out.String() + errb.String()
	if id := uuidPattern.FindString(all); id != "" {
		t.Errorf("output carries an id %s:\n%s", id, all)
	}
	return code, all
}

func TestRestructure_DryRunDiff(t *testing.T) {
	f := oldLayout()
	code, out := runRS(t, f, rsOpts{spec: cspSpec(t)})
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if f.writes != 0 || f.reads != 1 {
		t.Errorf("dry run: %d writes, %d reads; want 0, 1", f.writes, f.reads)
	}
	for _, want := range []string{
		`  1. create group "Giving"`,
		`  2. move "Church - Tithing" from "Monthly Savings Funds" to "Giving"`,
		`rename group "Mortgage & Utilities" to "Fixed Costs"`,
		`no change: "Home - Mortgage" in "Fixed Costs"`,
		`rename "Subs - Lawn" to "Home - Lawn Service" and move it from "Subscriptions" to "Fixed Costs"`,
		`rename "Google One" to "Subs - Google One" and move it from "Subscriptions" to "Fixed Costs"`,
		`create "Groceries" in "Fixed Costs"`,
		`no change: "Retirement" in "Investments"`, // exists already in the renamed group
		`move "Wise - Buffer" from "Target Balance Goals" to "Savings Goals"`,
		`roll stale target date of "Holiday - Christmas" forward from 2020-12-01 to 2026-12-01 (target $1,500.00 kept)`,
		`roll stale target date of "Home - Insurance" forward from 2026-06-28 to 2027-06-28 (target $800.00 kept)`,
		`roll stale target date of "Home - Cell Phone" forward from 2021-11-27 to 2026-11-27 (target $404.00 kept)`,
		`move "Home - Riverton City Utilities" from "Monthly Variable Expenses" to "Retired"`,
		`"Business Bike " in "Fun Money"`,
		`"2019 Disneyland" in "Target Balance Goals" (hidden)`,
		`"Mortgage Payoff" in "Financial Independence"`,
		"unresolved: 0\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	for _, not := range []string{"Elliott Party\" forward", "B Sapphire Preferred", "Ready to Assign", "Ghost"} {
		if strings.Contains(out, not) {
			t.Errorf("output has %q", not)
		}
	}
	if !strings.Contains(out, "Not in the spec (left where they are), 7:") {
		t.Errorf("unmentioned count wrong:\n%s", out)
	}
}

func TestRestructure_ApplyThenRerunIsNoChange(t *testing.T) {
	f := oldLayout()
	code, out := runRS(t, f, rsOpts{spec: cspSpec(t), apply: true})
	if code != 0 {
		t.Fatalf("apply exit %d:\n%s", code, out)
	}
	m := regexp.MustCompile(`changes: (\d+)`).FindStringSubmatch(out)
	if m == nil || fmt.Sprint(f.writes) != m[1] {
		t.Errorf("writes %d, printed changes %v", f.writes, m)
	}
	// The layout landed.
	if g := f.group(f.cat("Home - Lawn Service").CategoryGroupID); g == nil || g.Name != "Fixed Costs" {
		t.Errorf("Home - Lawn Service is in %v", g)
	}
	if g := f.group(f.cat("Groceries").CategoryGroupID); g == nil || g.Name != "Fixed Costs" {
		t.Errorf("Groceries is in %v", g)
	}
	if d := f.cat("Holiday - Christmas").GoalTargetDate; d == nil || d.String() != "2026-12-01" || *f.cat("Holiday - Christmas").GoalTarget != 1500000 {
		t.Errorf("Christmas target %v", d)
	}
	before := f.writes
	code, out = runRS(t, f, rsOpts{spec: cspSpec(t), apply: true})
	if code != 0 || f.writes != before || !strings.Contains(out, "changes: 0\n") || strings.Contains(out, "  1. ") {
		t.Errorf("rerun: exit %d, %d new writes:\n%s", code, f.writes-before, out)
	}
}

func TestRestructure_PartialApplyThenRerun(t *testing.T) {
	f := oldLayout()
	f.failAt, f.failErr = 5, errors.New("update category: 500 internal_server_error")
	code, out := runRS(t, f, rsOpts{spec: cspSpec(t), apply: true})
	if code != 1 || !strings.Contains(out, "  5. FAILED ") || !strings.Contains(out, "done: 4 of ") {
		t.Fatalf("partial: exit %d:\n%s", code, out)
	}
	f.failAt = 0
	code, out = runRS(t, f, rsOpts{spec: cspSpec(t), apply: true})
	if code != 0 || !strings.Contains(out, `no change: group "Giving"`) {
		t.Fatalf("resume: exit %d:\n%s", code, out)
	}
	code, out = runRS(t, f, rsOpts{spec: cspSpec(t)})
	if code != 0 || !strings.Contains(out, "changes: 0\n") {
		t.Errorf("after resume: exit %d:\n%s", code, out)
	}
}

func TestRestructure_RefusesUnresolvedNames(t *testing.T) {
	for name, mutate := range map[string]func(*fakeCatAPI){
		"missing category": func(f *fakeCatAPI) { f.cat("Subs - YNAB").Name = "YNAB" },
		"missing group":    func(f *fakeCatAPI) { f.groups[1].Name = "Meeting" }, // Budget Meeting
		"ambiguous category": func(f *fakeCatAPI) {
			f.groups[1].Categories = append(f.groups[1].Categories, ynab.Category{CategoryBase: ynab.CategoryBase{ID: f.id(), Name: "Home - Cell Phone"}})
			f.groups[0].Categories = append(f.groups[0].Categories, ynab.Category{CategoryBase: ynab.CategoryBase{ID: f.id(), Name: "Home - Cell Phone"}})
		},
		"ambiguous group": func(f *fakeCatAPI) { f.addGroup("Fun Money") },
		"create collides": func(f *fakeCatAPI) { f.addGroup("Food", "Groceries") },
	} {
		t.Run(name, func(t *testing.T) {
			f := oldLayout()
			mutate(f)
			code, out := runRS(t, f, rsOpts{spec: cspSpec(t), apply: true})
			if code != 1 || f.writes != 0 || strings.Contains(out, "unresolved: 0") || !strings.Contains(out, "nothing was written") {
				t.Errorf("exit %d, %d writes:\n%s", code, f.writes, out)
			}
		})
	}
}

func TestRestructure_LivePlanNeedsConfirm(t *testing.T) {
	f := oldLayout()
	code, out := runRS(t, f, rsOpts{spec: cspSpec(t), live: true, apply: true})
	if code != 1 || f.writes != 0 || f.reads != 0 || !strings.Contains(out, "--confirm-live") {
		t.Errorf("exit %d, %d reads, %d writes:\n%s", code, f.reads, f.writes, out)
	}
	// A live dry run is allowed.
	if code, out = runRS(t, f, rsOpts{spec: cspSpec(t), live: true}); code != 0 || f.writes != 0 {
		t.Errorf("live dry run: exit %d:\n%s", code, out)
	}
	if code, out = runRS(t, f, rsOpts{spec: cspSpec(t), live: true, apply: true, confirmLive: true}); code != 0 || f.writes == 0 {
		t.Errorf("confirmed: exit %d:\n%s", code, out)
	}
}

func TestRestructure_RateBudget(t *testing.T) {
	f := oldLayout()
	// 200 - 80 used - 10 margin - 1 read = 109 < 120.
	code, out := runRS(t, f, rsOpts{spec: cspSpec(t), apply: true, used: 80})
	if code != 1 || f.writes != 0 || !strings.Contains(out, "estimated 109 of 200") {
		t.Errorf("exit %d, %d writes:\n%s", code, f.writes, out)
	}
	if code, _ = runRS(t, f, rsOpts{spec: cspSpec(t), apply: true, used: 69}); code != 0 {
		t.Errorf("120 left: exit %d", code)
	}
}

func TestRollForward(t *testing.T) {
	today := ynab.NewDate(2026, 11, 11)
	for in, want := range map[string]string{
		"2020-12-01": "2026-12-01",
		"2025-11-11": "2026-11-11", // today counts
		"2025-11-10": "2027-11-10",
		"2024-02-29": "2027-02-28",
		"2020-02-29": "2027-02-28",
	} {
		d, _ := ynab.ParseDate(in)
		if got := rollForward(d, today).String(); got != want {
			t.Errorf("rollForward(%s) = %s, want %s", in, got, want)
		}
	}
	if got := rollForward(mustDate("2023-02-28"), ynab.NewDate(2028, 2, 29)).String(); got != "2029-02-28" {
		t.Errorf("got %s", got)
	}
}

func mustDate(s string) ynab.Date {
	d, err := ynab.ParseDate(s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestRestructure_SpecValidation(t *testing.T) {
	bad := rsSpec{Groups: []rsGroup{{Op: "rename-group", Name: "A", Categories: []rsCat{{Op: "move", Name: "x"}, {Op: "teleport", Name: "y"}}}}}
	err := validateRsSpec(bad)
	if err == nil || !strings.Contains(err.Error(), "needs a different from") || !strings.Contains(err.Error(), "move needs from_group") ||
		!strings.Contains(err.Error(), `unknown op "teleport"`) {
		t.Errorf("got %v", err)
	}
	if err := validateRsSpec(cspSpec(t)); err != nil {
		t.Errorf("csp spec: %v", err)
	}
}

func TestMoney(t *testing.T) {
	for m, want := range map[int64]string{0: "$0.00", 1500000: "$1,500.00", 12990: "$12.99", 2529200: "$2,529.20", 1234567890: "$1,234,567.89"} {
		if got := money(ynab.Milliunits(m)); got != want {
			t.Errorf("money(%d) = %s, want %s", m, got, want)
		}
	}
}
