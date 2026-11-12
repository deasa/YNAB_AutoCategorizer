package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/deasa/YNAB_AutoCategorizer/ynab"
)

// `finbot restructure` is a one-off: it moves a plan's category groups and
// categories to the layout a spec file describes, by name. It is self-contained
// so `finbot seed-plan` (M4) can reuse or replace it. It never prints an id.

const (
	rsTokenLimit  = 200 // YNAB's per-token hourly limit; YNAB sends no header saying what's left
	rsOtherMargin = 10  // unledgered use of the token assumed per hour, as tests/acceptance/M3 does
	rsMinLeft     = 120 // refuse to write below this many requests left this hour
	rsRunCap      = 100 // every request of one run, reads included
)

// rsNow is the clock for the stale-target check (tests).
var rsNow = time.Now

// rsSpec is the layout: groups in order, each with its categories in order.
type rsSpec struct {
	Groups []rsGroup `json:"groups"`
}

// rsGroup ops: create-group (Name is new), rename-group (From is the current
// name, Name the new one), keep-group (Name stays).
type rsGroup struct {
	Op         string  `json:"op"`
	Name       string  `json:"name"`
	From       string  `json:"from,omitempty"`
	Categories []rsCat `json:"categories"`
}

// rsCat ops: stay (already in this group), move (from FromGroup), rename+move
// (From in FromGroup, renamed to Name; FromGroup may be this group's old
// name for a plain rename), create (a new category), set-target (stays, with
// Target set). Target may also go with any other op.
type rsCat struct {
	Op        string    `json:"op"`
	Name      string    `json:"name"`
	From      string    `json:"from,omitempty"`
	FromGroup string    `json:"from_group,omitempty"`
	Target    *rsTarget `json:"target,omitempty"`
	// NoTarget says the category should have no target. The API can't clear
	// one, so an existing target is reported for a hand fix, never changed.
	NoTarget bool `json:"no_target,omitempty"`
}

type rsTarget struct {
	Milliunits int64  `json:"milliunits"`
	Date       string `json:"date,omitempty"` // YYYY-MM-DD; empty keeps the current date
}

// categoryAPI is what restructure needs from YNAB; *ynab.Client has it.
type categoryAPI interface {
	GetCategories(ctx context.Context) ([]*ynab.CategoryGroup, error)
	CreateCategoryGroup(ctx context.Context, name string) (*ynab.CategoryGroup, error)
	RenameCategoryGroup(ctx context.Context, groupID, name string) (*ynab.CategoryGroup, error)
	CreateCategory(ctx context.Context, groupID, name string) (*ynab.Category, error)
	UpdateCategory(ctx context.Context, categoryID string, e ynab.CategoryEdit) (*ynab.Category, error)
}

// countingAPI counts requests and refuses past the run cap.
type countingAPI struct {
	api categoryAPI
	n   int
}

var errRunCap = fmt.Errorf("run cap of %d requests reached", rsRunCap)

func (c *countingAPI) take() error {
	if c.n >= rsRunCap {
		return errRunCap
	}
	c.n++
	return nil
}

func (c *countingAPI) GetCategories(ctx context.Context) ([]*ynab.CategoryGroup, error) {
	if err := c.take(); err != nil {
		return nil, err
	}
	return c.api.GetCategories(ctx)
}

func (c *countingAPI) CreateCategoryGroup(ctx context.Context, name string) (*ynab.CategoryGroup, error) {
	if err := c.take(); err != nil {
		return nil, err
	}
	return c.api.CreateCategoryGroup(ctx, name)
}

func (c *countingAPI) RenameCategoryGroup(ctx context.Context, id, name string) (*ynab.CategoryGroup, error) {
	if err := c.take(); err != nil {
		return nil, err
	}
	return c.api.RenameCategoryGroup(ctx, id, name)
}

func (c *countingAPI) CreateCategory(ctx context.Context, gid, name string) (*ynab.Category, error) {
	if err := c.take(); err != nil {
		return nil, err
	}
	return c.api.CreateCategory(ctx, gid, name)
}

func (c *countingAPI) UpdateCategory(ctx context.Context, id string, e ynab.CategoryEdit) (*ynab.Category, error) {
	if err := c.take(); err != nil {
		return nil, err
	}
	return c.api.UpdateCategory(ctx, id, e)
}

// rsOpts are restructure's inputs after flag parsing.
type rsOpts struct {
	spec        rsSpec
	live        bool // the plan is YNAB_BUDGET_ID
	apply       bool
	confirmLive bool
	used        int // requests others used on this token in the last hour (--used + the ledger)
}

func restructureCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("restructure", flag.ContinueOnError)
	fs.SetOutput(stderr)
	specPath := fs.String("spec", "", "layout spec file (JSON, names only)")
	planID := fs.String("plan", "", "plan to restructure (default YNAB_BUDGET_ID)")
	apply := fs.Bool("apply", false, "make the changes (default: print them only)")
	confirmLive := fs.Bool("confirm-live", false, "allow --apply on YNAB_BUDGET_ID, the live plan")
	used := fs.Int("used", -1, "with --apply: YNAB requests already used on this token in the last hour (hoid's, doubled)")
	ledger := fs.String("ledger", "", "optional hourly request ledger shared with tests/acceptance/M3 (read, then appended); must exist")
	ledgerCreate := fs.Bool("ledger-create", false, "allow --ledger to name a file that doesn't exist yet (first use)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *specPath == "" || fs.NArg() != 0 {
		fmt.Fprintf(stderr, "finbot restructure: --spec <file> is required\n\n%s", usage)
		return 2
	}
	spec, err := loadRsSpec(*specPath)
	if err != nil {
		fmt.Fprintf(stderr, "finbot restructure: %v\n", err)
		return 1
	}
	_ = godotenv.Load()
	token, live := os.Getenv("YNAB_ACCESS_TOKEN"), os.Getenv("YNAB_BUDGET_ID")
	if token == "" || live == "" {
		fmt.Fprintln(stderr, "finbot restructure: YNAB_ACCESS_TOKEN and YNAB_BUDGET_ID must be set")
		return 1
	}
	plan, err := rsPlanID(*planID, live)
	if err != nil {
		fmt.Fprintf(stderr, "finbot restructure: %v; nothing was read or written\n", err)
		return 1
	}
	o := rsOpts{spec: spec, live: strings.EqualFold(plan, strings.TrimSpace(live)), apply: *apply, confirmLive: *confirmLive}
	var ledgerUsed int
	if *ledger != "" {
		if ledgerUsed, err = rsLedgerLastHour(*ledger, *ledgerCreate); err != nil {
			fmt.Fprintf(stderr, "finbot restructure: %v; nothing was read or written\n", err)
			return 1
		}
	}
	if *apply {
		if *used < 0 {
			fmt.Fprintln(stderr, "finbot restructure: --apply needs --used <n>: the token's requests in the last hour (hoid's, doubled)")
			return 1
		}
		o.used = *used + ledgerUsed
	}
	// A frozen clock: the bucket never refills, so it caps the run's HTTP
	// attempts (retries included) at rsRunCap.
	t0 := time.Now()
	gov := ynab.NewGovernor(rsRunCap, 0, func() time.Time { return t0 })
	client := ynab.NewClient(token, plan, log.New(io.Discard, "", 0), ynab.WithGovernor(gov))
	code := runRestructure(context.Background(), client, o, stdout, stderr)
	if _, left := gov.Remaining(); *ledger != "" && left < rsRunCap {
		rsLedgerAppend(*ledger, rsRunCap-left, stderr)
	}
	return code
}

// rsPlanID returns the plan to act on: live (YNAB_BUDGET_ID) when flag is
// empty, else flag, which must be a plan UUID. YNAB's aliases "last-used"
// and "default" are refused: they can name the live plan without matching
// YNAB_BUDGET_ID, which would slip past the --confirm-live guard.
func rsPlanID(flag, live string) (string, error) {
	p := strings.TrimSpace(flag)
	if p == "" {
		return strings.TrimSpace(live), nil
	}
	if strings.EqualFold(p, "last-used") || strings.EqualFold(p, "default") {
		return "", fmt.Errorf("--plan %q is a YNAB alias; give the plan's id", p)
	}
	if !rsUUID.MatchString(p) {
		return "", errors.New("--plan must be a plan id (a UUID)")
	}
	return p, nil
}

var rsUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func loadRsSpec(path string) (rsSpec, error) {
	var s rsSpec
	b, err := os.ReadFile(path)
	if err != nil {
		return s, fmt.Errorf("read spec: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, fmt.Errorf("parse spec: %w", err)
	}
	return s, validateRsSpec(s)
}

// validateRsSpec checks each entry's op has the fields it needs.
func validateRsSpec(s rsSpec) error {
	if len(s.Groups) == 0 {
		return errors.New("spec: no groups")
	}
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf("spec: "+f, a...)) }
	for _, g := range s.Groups {
		if g.Name == "" {
			bad("a group has no name")
		}
		switch g.Op {
		case "create-group", "keep-group":
			if g.From != "" {
				bad("group %q: %s takes no from", g.Name, g.Op)
			}
		case "rename-group":
			if g.From == "" || g.From == g.Name {
				bad("group %q: rename-group needs a different from", g.Name)
			}
		default:
			bad("group %q: unknown op %q", g.Name, g.Op)
		}
		for _, c := range g.Categories {
			if c.Name == "" {
				bad("group %q: a category has no name", g.Name)
			}
			switch c.Op {
			case "stay", "create":
				if c.From != "" || c.FromGroup != "" {
					bad("category %q: %s takes no from or from_group", c.Name, c.Op)
				}
			case "set-target":
				if c.Target == nil || c.From != "" || c.FromGroup != "" {
					bad("category %q: set-target needs a target and no from", c.Name)
				}
			case "move":
				if c.FromGroup == "" || c.From != "" {
					bad("category %q: move needs from_group and no from", c.Name)
				}
			case "rename+move":
				if c.From == "" || c.FromGroup == "" || c.From == c.Name {
					bad("category %q: rename+move needs a different from, and from_group", c.Name)
				}
			default:
				bad("category %q: unknown op %q", c.Name, c.Op)
			}
			if c.NoTarget && c.Target != nil {
				bad("category %q: no_target and target conflict", c.Name)
			}
			if t := c.Target; t != nil && t.Date != "" {
				if _, err := ynab.ParseDate(t.Date); err != nil {
					bad("category %q: target date %q is not YYYY-MM-DD", c.Name, t.Date)
				}
			}
		}
	}
	return errors.Join(errs...)
}

// rsGroupRef is a group as the run sees it; id is empty until a pending
// create-group runs.
type rsGroupRef struct {
	id   string
	name string // current name, for printing
}

type rsCatRef struct{ id string }

type rsChange struct {
	text string
	do   func(ctx context.Context, api categoryAPI) error
}

// rsPlan is the resolved diff.
type rsPlan struct {
	lines       []string // spec-order output; changes are numbered later
	changes     []rsChange
	lineChange  []int // per line: index into changes, or -1
	problems    []string
	warnings    []string
	unmentioned []string
}

func (p *rsPlan) note(s string) {
	p.lines = append(p.lines, s)
	p.lineChange = append(p.lineChange, -1)
}

func (p *rsPlan) change(text string, do func(context.Context, categoryAPI) error) {
	p.lines = append(p.lines, text)
	p.lineChange = append(p.lineChange, len(p.changes))
	p.changes = append(p.changes, rsChange{text: text, do: do})
}

func (p *rsPlan) problem(f string, a ...any) { p.problems = append(p.problems, fmt.Sprintf(f, a...)) }

type rsHit struct {
	cat   *ynab.Category
	group *ynab.CategoryGroup
}

// excludedGroup: internal groups (Ready to Assign, hidden categories) and
// Credit Card Payments are never touched or listed.
func excludedGroup(g *ynab.CategoryGroup) bool {
	return g.Deleted || g.Internal || g.Name == "Credit Card Payments"
}

// planRestructure resolves spec against groups and returns every change, in
// order. It makes no request.
func planRestructure(spec rsSpec, groups []*ynab.CategoryGroup, today ynab.Date) *rsPlan {
	p := &rsPlan{}
	var live []*ynab.CategoryGroup
	for _, g := range groups {
		if !excludedGroup(g) {
			live = append(live, g)
		}
	}
	groupsNamed := func(name string) []*ynab.CategoryGroup {
		var out []*ynab.CategoryGroup
		for _, g := range live {
			if g.Name == name {
				out = append(out, g)
			}
		}
		return out
	}
	catsNamed := func(name string) []rsHit {
		var out []rsHit
		for _, g := range live {
			for i := range g.Categories {
				if c := &g.Categories[i]; !c.Deleted && c.Name == name {
					out = append(out, rsHit{c, g})
				}
			}
		}
		return out
	}

	// Groups first, so categories can name any spec group by old or new name.
	refs := make([]*rsGroupRef, len(spec.Groups))
	alias := map[string]*rsGroupRef{}
	claimed := map[string]string{} // plan group id -> spec group name
	for i, g := range spec.Groups {
		ref := &rsGroupRef{name: g.Name}
		refs[i] = ref
		alias[g.Name] = ref
		if g.From != "" {
			alias[g.From] = ref
		}
		now, old := groupsNamed(g.Name), []*ynab.CategoryGroup(nil)
		if g.From != "" {
			old = groupsNamed(g.From)
		}
		switch {
		case len(now) > 1:
			p.problem("ambiguous: %d groups named %q", len(now), g.Name)
			continue
		case len(old) > 1:
			p.problem("ambiguous: %d groups named %q", len(old), g.From)
			continue
		case len(now) == 1 && len(old) == 1:
			p.problem("ambiguous: groups %q and %q both exist; rename-group expects one", g.From, g.Name)
			continue
		case len(now) == 1:
			ref.id = now[0].ID
		case len(old) == 1:
			ref.id, ref.name = old[0].ID, old[0].Name
		case g.Op == "create-group":
		default:
			p.problem("missing: group %q%s", g.Name, orFrom(g.From))
			continue
		}
		if ref.id != "" {
			if prev, dup := claimed[ref.id]; dup {
				p.problem("ambiguous: spec groups %q and %q are the same plan group", prev, g.Name)
			}
			claimed[ref.id] = g.Name
		}
	}
	groupRef := func(name string) (*rsGroupRef, bool) {
		if r, ok := alias[name]; ok {
			return r, true
		}
		gs := groupsNamed(name)
		switch len(gs) {
		case 1:
			return &rsGroupRef{id: gs[0].ID, name: gs[0].Name}, true
		case 0:
			p.problem("missing: group %q", name)
		default:
			p.problem("ambiguous: %d groups named %q", len(gs), name)
		}
		return nil, false
	}

	matched := map[string]string{} // category id -> spec name
	for i, g := range spec.Groups {
		ref := refs[i]
		switch {
		case g.Op == "create-group" && ref.id == "":
			p.change(fmt.Sprintf("create group %q", g.Name), func(ctx context.Context, api categoryAPI) error {
				ng, err := api.CreateCategoryGroup(ctx, g.Name)
				if err != nil {
					return err
				}
				if ng == nil || ng.ID == "" {
					return errors.New("YNAB returned no group")
				}
				ref.id = ng.ID
				return nil
			})
		case g.Op == "rename-group" && ref.id != "" && ref.name != g.Name:
			from := ref.name
			p.change(fmt.Sprintf("rename group %q to %q", from, g.Name), func(ctx context.Context, api categoryAPI) error {
				_, err := api.RenameCategoryGroup(ctx, ref.id, g.Name)
				return err
			})
		default:
			p.note(fmt.Sprintf("no change: group %q", g.Name))
		}

		for _, c := range g.Categories {
			var from *rsGroupRef
			if c.FromGroup != "" {
				r, ok := groupRef(c.FromGroup)
				if !ok {
					continue
				}
				from = r
			}
			pick := func(name string) (*rsHit, bool) {
				hs := catsNamed(name)
				if len(hs) > 1 { // narrow to the groups this entry names
					var in []rsHit
					for _, h := range hs {
						if (ref.id != "" && h.group.ID == ref.id) || (from != nil && from.id != "" && h.group.ID == from.id) {
							in = append(in, h)
						}
					}
					hs = in
				}
				switch len(hs) {
				case 0:
					return nil, true
				case 1:
					return &hs[0], true
				}
				p.problem("ambiguous: %d categories named %q", len(hs), name)
				return nil, false
			}
			now, ok := pick(c.Name)
			if !ok {
				continue
			}
			var old *rsHit
			if c.From != "" {
				if old, ok = pick(c.From); !ok {
					continue
				}
			}
			if now != nil && old != nil {
				p.problem("ambiguous: categories %q and %q both exist; rename+move expects one", c.From, c.Name)
				continue
			}
			hit := now
			if hit == nil {
				hit = old
			}
			inTarget := hit != nil && ref.id != "" && hit.group.ID == ref.id
			inFrom := hit != nil && from != nil && from.id != "" && hit.group.ID == from.id
			cref := &rsCatRef{}
			switch c.Op {
			case "create":
				if hit != nil && !inTarget {
					p.problem("conflict: spec creates %q in %q, but a category with that name is in %q", c.Name, g.Name, hit.group.Name)
					continue
				}
			case "stay", "set-target":
				if hit == nil {
					p.problem("missing: category %q", c.Name)
					continue
				}
				if !inTarget {
					p.problem("conflict: %q should already be in %q, but it is in %q", c.Name, g.Name, hit.group.Name)
					continue
				}
			default: // move, rename+move
				if hit == nil {
					p.problem("missing: category %q%s", c.Name, orFrom(c.From))
					continue
				}
				if !inTarget && !inFrom {
					p.problem("conflict: %q is in %q, not %q", hit.cat.Name, hit.group.Name, c.FromGroup)
					continue
				}
			}
			if hit != nil {
				if prev, dup := matched[hit.cat.ID]; dup {
					p.problem("ambiguous: spec entries %q and %q are the same category", prev, c.Name)
					continue
				}
				matched[hit.cat.ID] = c.Name
				cref.id = hit.cat.ID
			}

			// The structural change: create, rename and/or move.
			name := c.Name
			switch {
			case hit == nil:
				p.change(fmt.Sprintf("create %q in %q", name, g.Name), func(ctx context.Context, api categoryAPI) error {
					if ref.id == "" {
						return fmt.Errorf("group %q doesn't exist yet", g.Name)
					}
					nc, err := api.CreateCategory(ctx, ref.id, name)
					if err != nil {
						return err
					}
					if nc == nil || nc.ID == "" {
						return errors.New("YNAB returned no category")
					}
					cref.id = nc.ID
					return nil
				})
			case hit.cat.Name != name || !inTarget:
				var text string
				switch {
				case hit.cat.Name != name && !inTarget:
					text = fmt.Sprintf("rename %q to %q and move it from %q to %q", hit.cat.Name, name, hit.group.Name, g.Name)
				case hit.cat.Name != name:
					text = fmt.Sprintf("rename %q to %q (stays in %q)", hit.cat.Name, name, g.Name)
				default:
					text = fmt.Sprintf("move %q from %q to %q", name, hit.group.Name, g.Name)
				}
				rename, move := hit.cat.Name != name, !inTarget
				p.change(text, func(ctx context.Context, api categoryAPI) error {
					var e ynab.CategoryEdit
					if rename {
						e.Name = &name
					}
					if move {
						if ref.id == "" {
							return fmt.Errorf("group %q doesn't exist yet", g.Name)
						}
						gid := ref.id
						e.GroupID = &gid
					}
					_, err := api.UpdateCategory(ctx, cref.id, e)
					return err
				})
			default:
				p.note(fmt.Sprintf("no change: %q in %q", name, g.Name))
			}

			// Targets: an explicit one from the spec, else roll a stale date forward.
			var cur *ynab.Category
			if hit != nil {
				cur = hit.cat
			}
			targetChange(p, c, cur, cref, today)
		}
	}

	for _, g := range live {
		for i := range g.Categories {
			c := &g.Categories[i]
			if c.Deleted {
				continue
			}
			if _, ok := matched[c.ID]; !ok {
				h := ""
				if c.Hidden || g.Hidden {
					h = " (hidden)"
				}
				p.unmentioned = append(p.unmentioned, fmt.Sprintf("%q in %q%s", c.Name, g.Name, h))
			}
		}
	}
	return p
}

func orFrom(from string) string {
	if from == "" {
		return ""
	}
	return fmt.Sprintf(" (or %q)", from)
}

// targetChange adds the target line for one spec category, if any: the spec's
// own target, or a stale target date rolled forward by whole years to the
// first date on or after today, keeping the amount.
func targetChange(p *rsPlan, c rsCat, cur *ynab.Category, cref *rsCatRef, today ynab.Date) {
	var curAmt *ynab.Milliunits
	var curDate *ynab.Date
	if cur != nil {
		curAmt, curDate = cur.GoalTarget, cur.GoalTargetDate
	}
	// A category marked no_target, or one the spec creates that already
	// exists, should have no target: say whether it has one.
	if c.NoTarget || (c.Op == "create" && cur != nil) {
		if cur == nil {
			return // created without a target
		}
		if has, desc := hasTarget(cur); has {
			w := fmt.Sprintf("%q should have no target but has one (%s); remove it in YNAB by hand", c.Name, desc)
			p.note("WARNING: " + w)
			p.warnings = append(p.warnings, w)
		} else {
			p.note(fmt.Sprintf("no change: %q has no target, as the spec wants", c.Name))
		}
		return
	}
	if t := c.Target; t != nil {
		amt := ynab.Milliunits(t.Milliunits)
		date := curDate
		if t.Date != "" {
			d, _ := ynab.ParseDate(t.Date) // validated with the spec
			date = &d
		}
		if curAmt != nil && *curAmt == amt && sameDate(curDate, date) {
			p.note(fmt.Sprintf("no change: target of %q", c.Name))
			return
		}
		text := fmt.Sprintf("set target of %q to %s", c.Name, money(amt))
		if date != nil {
			text += " by " + date.String()
		}
		p.change(text, func(ctx context.Context, api categoryAPI) error {
			_, err := api.UpdateCategory(ctx, cref.id, ynab.CategoryEdit{GoalTarget: &amt, GoalTargetDate: date})
			return err
		})
		return
	}
	if curDate == nil || !curDate.Before(today) {
		return
	}
	next := rollForward(*curDate, today)
	e := ynab.CategoryEdit{GoalTargetDate: &next}
	kept := ""
	if curAmt != nil {
		amt := *curAmt
		e.GoalTarget = &amt
		kept = fmt.Sprintf(" (target %s kept)", money(amt))
	}
	p.change(fmt.Sprintf("roll stale target date of %q forward from %s to %s%s", c.Name, curDate, next, kept),
		func(ctx context.Context, api categoryAPI) error {
			_, err := api.UpdateCategory(ctx, cref.id, e)
			return err
		})
}

// hasTarget reports whether c has a target (a goal type, a nonzero amount or
// a target date), and describes it.
func hasTarget(c *ynab.Category) (bool, string) {
	if c.GoalType == nil && (c.GoalTarget == nil || *c.GoalTarget == 0) && c.GoalTargetDate == nil {
		return false, ""
	}
	var parts []string
	if c.GoalType != nil {
		parts = append(parts, "type "+string(*c.GoalType))
	}
	if c.GoalTarget != nil && *c.GoalTarget != 0 {
		parts = append(parts, money(*c.GoalTarget))
	}
	if c.GoalTargetDate != nil {
		parts = append(parts, "by "+c.GoalTargetDate.String())
	}
	return true, strings.Join(parts, ", ")
}

func sameDate(a, b *ynab.Date) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// rollForward moves d by whole years to the first date on or after today. A
// 29 February lands on the 28th in a common year.
func rollForward(d, today ynab.Date) ynab.Date {
	for y := d.Year() + 1; ; y++ {
		day := d.Day()
		if d.Month() == time.February && day == 29 && !isLeap(y) {
			day = 28
		}
		n := ynab.NewDate(y, d.Month(), day)
		if !n.Before(today) {
			return n
		}
	}
}

func isLeap(y int) bool { return y%4 == 0 && (y%100 != 0 || y%400 == 0) }

// money formats milliunits as dollars, e.g. $1,500.00.
func money(m ynab.Milliunits) string {
	v := int64(m)
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	cents := (v + 5) / 10
	d, c := cents/100, cents%100
	s := fmt.Sprint(d)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return fmt.Sprintf("%s$%s.%02d", sign, s, c)
}

// runRestructure prints the diff and, with o.apply, makes the changes. It
// returns the exit code.
func runRestructure(ctx context.Context, raw categoryAPI, o rsOpts, stdout, stderr io.Writer) int {
	if o.apply && o.live && !o.confirmLive {
		fmt.Fprintln(stderr, "finbot restructure: refusing --apply on the live plan (YNAB_BUDGET_ID) without --confirm-live; nothing was written")
		return 1
	}
	api := &countingAPI{api: raw}
	groups, err := api.GetCategories(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "finbot restructure: read categories: %v\n", err)
		return 1
	}
	plan := "sandbox/other plan (--plan)"
	if o.live {
		plan = "live plan (YNAB_BUDGET_ID)"
	}
	t := rsNow()
	today := ynab.NewDate(t.Year(), t.Month(), t.Day())
	p := planRestructure(o.spec, groups, today)

	fmt.Fprintf(stdout, "finbot restructure: %s, today %s\n\n", plan, today)
	n := 0
	for i, l := range p.lines {
		if p.lineChange[i] >= 0 {
			n++
			fmt.Fprintf(stdout, "%3d. %s\n", n, l)
		} else {
			fmt.Fprintf(stdout, "     %s\n", l)
		}
	}
	fmt.Fprintf(stdout, "\nNot in the spec (left where they are), %d:\n", len(p.unmentioned))
	for _, u := range p.unmentioned {
		fmt.Fprintf(stdout, "     %s\n", u)
	}
	if len(p.problems) > 0 {
		fmt.Fprintln(stdout, "\nUnresolved:")
		for _, pr := range p.problems {
			fmt.Fprintf(stdout, "     %s\n", pr)
		}
	}
	if len(p.warnings) > 0 {
		fmt.Fprintln(stdout, "\nCheck in YNAB:")
		for _, w := range p.warnings {
			fmt.Fprintf(stdout, "     %s\n", w)
		}
	}
	fmt.Fprintln(stdout, "\nNote: the API can't order categories within a group or hide anything; do both in YNAB.")
	fmt.Fprintf(stdout, "unresolved: %d\nwarnings: %d\nchanges: %d\n", len(p.problems), len(p.warnings), len(p.changes))
	if len(p.problems) > 0 {
		fmt.Fprintln(stderr, "finbot restructure: the spec doesn't resolve against this plan; nothing was written")
		return 1
	}
	if !o.apply || len(p.changes) == 0 {
		return 0
	}

	// Rate budget, before any write: this hour's use by others + a margin +
	// this run so far, and the run cap.
	left := rsTokenLimit - o.used - rsOtherMargin - api.n
	if left < rsMinLeft {
		fmt.Fprintf(stderr, "finbot restructure: estimated %d of %d requests left this hour (need >= %d); nothing was written\n",
			left, rsTokenLimit, rsMinLeft)
		return 1
	}
	if api.n+len(p.changes) > rsRunCap {
		fmt.Fprintf(stderr, "finbot restructure: %d changes would pass the run cap of %d requests; nothing was written\n",
			len(p.changes), rsRunCap)
		return 1
	}

	fmt.Fprintln(stdout, "\nApplying:")
	for i, ch := range p.changes {
		if err := ch.do(ctx, api); err != nil {
			fmt.Fprintf(stdout, "%3d. FAILED %s: %v\n", i+1, ch.text, err)
			fmt.Fprintf(stdout, "\n%s\nRun it again: done changes show as no change.\n", rsPartial(i, len(p.changes)))
			return 1
		}
		fmt.Fprintf(stdout, "%3d. done: %s\n", i+1, ch.text)
	}
	fmt.Fprintf(stdout, "\ndone: %d of %d\n", len(p.changes), len(p.changes))
	return 0
}

// rsPartial summarizes an apply that stopped at change failed+1 (0-based
// index failed) of n.
func rsPartial(failed, n int) string {
	span := func(a, b int) string {
		switch {
		case a > b:
			return "none"
		case a == b:
			return fmt.Sprintf("#%d", a)
		}
		return fmt.Sprintf("#%d-#%d", a, b)
	}
	return fmt.Sprintf("done: %d of %d (%s); failed: #%d; not done: %s", failed, n, span(1, failed), failed+1, span(failed+2, n))
}

// rsLedgerLastHour sums the ledger's charges ("<unix> <n>" lines) from the
// last 60 minutes. A file that can't be read is an error, so a mistyped path
// can't make the estimate optimistic; a missing one is 0 only with create.
func rsLedgerLastHour(path string, create bool) (int, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && create {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read --ledger: %w (use --ledger-create for a new ledger)", err)
	}
	cut, sum := time.Now().Add(-time.Hour).Unix(), 0
	for _, l := range strings.Split(string(b), "\n") {
		var ts int64
		var n int
		if _, err := fmt.Sscan(l, &ts, &n); err == nil && ts > cut {
			sum += n
		}
	}
	return sum, nil
}

func rsLedgerAppend(path string, n int, stderr io.Writer) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		_, err = fmt.Fprintf(f, "%d %d\n", time.Now().Unix(), n)
		err = errors.Join(err, f.Close())
	}
	if err != nil {
		fmt.Fprintf(stderr, "finbot restructure: ledger not updated: %v\n", err)
	}
}
