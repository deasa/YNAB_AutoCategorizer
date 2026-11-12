package slack

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"unicode"

	slackapi "github.com/slack-go/slack"

	"github.com/deasa/YNAB_AutoCategorizer/classify"
	"github.com/deasa/YNAB_AutoCategorizer/store"
)

const (
	choiceCat     = "c:" // a select value naming a plan category: "<qid>:c:<catKey>"
	minPrefix     = 4    // a reply this long (normalized) may match as a unique prefix
	maxSuggestion = 3    // closest names offered after a reply that matches none or several
)

// answerable reports whether c can be offered or answered: the classifier's
// eligibility (no hidden, deleted, internal or Credit Card Payments
// categories), set-asides included.
func answerable(c store.CategoryWithGroup) bool { return classify.Eligible(c) }

// liveOptions returns qu with every option whose category is no longer an
// answerable category of qu's plan (cats) blanked, so no path offers or
// accepts it. Indices are kept, so a button for a blanked option is rejected.
func liveOptions(qu store.Question, cats []store.CategoryWithGroup) store.Question {
	ok := map[string]bool{}
	for _, c := range cats {
		if answerable(c) {
			ok[c.ID] = true
		}
	}
	opts := make([]store.QuestionOption, len(qu.Options))
	for i, o := range qu.Options {
		if !ok[o.CategoryID] {
			o.CategoryID = ""
		}
		opts[i] = o
	}
	qu.Options = opts
	return qu
}

// catKey is the opaque handle a select option carries for a plan category: a
// short hash of its id, so the message carries no YNAB id. It resolves only
// against the plan's answerable categories at click time.
func catKey(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:8])
}

// resolveCatKey finds the one answerable category in cats with key k.
func resolveCatKey(cats []store.CategoryWithGroup, k string) (store.CategoryWithGroup, bool) {
	var hit store.CategoryWithGroup
	n := 0
	for _, c := range cats {
		if answerable(c) && catKey(c.ID) == k {
			hit = c
			n++
		}
	}
	return hit, n == 1
}

// selectGroups builds the "other…" select: the question's options first, then
// every other answerable category of the plan by group, at most maxSelect options
// in all. It returns how many answerable categories were left out.
func selectGroups(qu store.Question, cats []store.CategoryWithGroup) ([]*slackapi.OptionGroupBlockObject, int) {
	plain := func(s string) *slackapi.TextBlockObject {
		return slackapi.NewTextBlockObject(slackapi.PlainTextType, clip(s), false, false)
	}
	qid := strconv.FormatInt(qu.ID, 10)
	total := 0
	var groups []*slackapi.OptionGroupBlockObject
	seen := map[string]bool{}
	var sugg []*slackapi.OptionBlockObject
	for i, o := range qu.Options {
		if o.CategoryID == "" || seen[o.CategoryID] {
			continue
		}
		seen[o.CategoryID] = true
		sugg = append(sugg, slackapi.NewOptionBlockObject(qid+":"+strconv.Itoa(i), plain(optionName(o)), nil))
	}
	if len(sugg) > maxSelect {
		sugg = sugg[:maxSelect]
	}
	if len(sugg) > 0 {
		groups = append(groups, slackapi.NewOptionGroupBlockElement(plain("Suggested"), sugg...))
		total += len(sugg)
	}
	dropped := 0
	var cur *slackapi.OptionGroupBlockObject
	curGroup := ""
	for _, c := range cats {
		if !answerable(c) || seen[c.ID] {
			continue
		}
		seen[c.ID] = true
		if total >= maxSelect {
			dropped++
			continue
		}
		if cur == nil || c.GroupID != curGroup {
			label := c.GroupName
			if label == "" {
				label = "Other"
			}
			cur = slackapi.NewOptionGroupBlockElement(plain(label))
			curGroup = c.GroupID
			groups = append(groups, cur)
		}
		cur.Options = append(cur.Options,
			slackapi.NewOptionBlockObject(qid+":"+choiceCat+catKey(c.ID), plain(c.Name), nil))
		total++
	}
	return groups, dropped
}

// norm folds a name or reply for loose matching: lower case, "&" and "and"
// dropped, every run of non-letters and non-digits one space.
func norm(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	var words []string
	for _, w := range strings.Fields(b.String()) {
		if w != "and" {
			words = append(words, w)
		}
	}
	return strings.Join(words, " ")
}

// entry is one answerable category and the folded forms of its names.
type entry struct {
	id, name string
	keys     []string // norm of each name, and of each name without a "Group - " prefix
}

func newEntry(id, name string, names ...string) entry {
	e := entry{id: id, name: name}
	add := func(k string) {
		if k == "" {
			return
		}
		for _, have := range e.keys {
			if have == k {
				return
			}
		}
		e.keys = append(e.keys, k)
	}
	for _, n := range names {
		add(norm(n))
		if _, rest, ok := strings.Cut(n, " - "); ok {
			add(norm(rest))
		}
	}
	return e
}

// entries lists the question's options first, then the plan's other visible
// categories, one per category id.
func entries(qu store.Question, cats []store.CategoryWithGroup) []entry {
	var out []entry
	byID := map[string]int{}
	for _, o := range qu.Options {
		if o.CategoryID == "" {
			continue
		}
		if _, ok := byID[o.CategoryID]; ok {
			continue
		}
		byID[o.CategoryID] = len(out)
		out = append(out, newEntry(o.CategoryID, optionName(o), o.Label, o.CategoryName))
	}
	for _, c := range cats {
		if !answerable(c) {
			continue
		}
		if _, ok := byID[c.ID]; ok {
			continue
		}
		byID[c.ID] = len(out)
		out = append(out, newEntry(c.ID, c.Name, c.Name))
	}
	return out
}

// matchStages runs the loose matches in order and returns the first stage's
// single match, or the candidates of the first stage that matched several.
func matchStages(es []entry, reply string) (hit *entry, several []entry) {
	r := norm(reply)
	if r == "" {
		return nil, nil
	}
	padded := " " + r + " "
	stages := []func(k string) bool{
		func(k string) bool { return k == r },                                                 // folded name
		func(k string) bool { return strings.Contains(padded, " "+k+" ") },                    // whole words
		func(k string) bool { return len([]rune(r)) >= minPrefix && strings.HasPrefix(k, r) }, // prefix
	}
	for _, match := range stages {
		var got []entry
		for _, e := range es {
			for _, k := range e.keys {
				if match(k) {
					got = append(got, e)
					break
				}
			}
		}
		switch {
		case len(got) == 1:
			return &got[0], nil
		case len(got) > 1 && several == nil:
			several = got
		}
	}
	return nil, several
}

// suggest names up to maxSuggestion categories close to reply: those a stage
// matched several of, else the nearest by edit distance.
func suggest(es []entry, reply string, several []entry) []string {
	pool := several
	if len(pool) == 0 {
		pool = es
	}
	r := norm(reply)
	type scored struct {
		name string
		d    int
	}
	var ss []scored
	for _, e := range pool {
		best := -1
		for _, k := range e.keys {
			d := distance(r, k)
			if len(r) >= 1 && strings.HasPrefix(k, r) {
				d = 0
			}
			if best < 0 || d < best {
				best = d
			}
		}
		ss = append(ss, scored{e.name, best})
	}
	sort.SliceStable(ss, func(i, j int) bool {
		if ss[i].d != ss[j].d {
			return ss[i].d < ss[j].d
		}
		return ss[i].name < ss[j].name
	})
	var out []string
	for _, s := range ss {
		if len(out) == maxSuggestion {
			break
		}
		out = append(out, s.name)
	}
	return out
}

// distance is the Levenshtein distance between a and b, by rune.
func distance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}
