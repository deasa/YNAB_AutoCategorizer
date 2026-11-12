package classify

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

//go:embed policy.json
var policyJSON []byte

// Policy is the household configuration the classifier runs under. It is
// versioned with calibration: τ holds only for the PolicyHash it was measured on.
type Policy struct {
	Version int `json:"version"`
	// SetAsideKeywords name the categories the classifier never suggests: the
	// event funds (Birthday, Holiday, Trip, Giving) and the never-spent-from
	// tracking and investment lines (Retirement, Investment, Payoff). Both are
	// savings only, so one keyword list covers them: a person can still pick one
	// in Slack, and their labels never become rules or calibration rows. A
	// category is a set-aside when a word of its name is a keyword, or a keyword
	// plus "s", ignoring case: "Holiday - Christmas", "Stock Investments" and
	// "Mortgage Payoff" match, while "Church - Tithing" and "Thanksgiving
	// Dinner" do not match "Giving", nor "Home - Mortgage" anything. Every
	// keyword must be the first to match some category, or Candidates warns.
	SetAsideKeywords []string `json:"set_aside_keywords"`
	// Descriptions override a category's YNAB note, by category name ignoring
	// case. They steer the model at the boundaries between everyday categories.
	Descriptions map[string]string `json:"descriptions"`
}

// DefaultPolicy returns the embedded policy.json.
func DefaultPolicy() (Policy, error) { return LoadPolicy(policyJSON) }

// LoadPolicy parses a policy. Unknown fields are an error, so a typo can't
// silently drop a rule.
func LoadPolicy(data []byte) (Policy, error) {
	var p Policy
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("classify: policy: %w", err)
	}
	if p.Version <= 0 {
		return Policy{}, errors.New("classify: policy: version must be positive")
	}
	for _, k := range p.SetAsideKeywords {
		if strings.TrimSpace(k) == "" || strings.Contains(strings.TrimSpace(store.NormalizePayee(k)), " ") {
			return Policy{}, fmt.Errorf("classify: policy: set-aside keyword %q must be one word", k)
		}
	}
	seen := map[string]bool{}
	for name := range p.Descriptions {
		f := fold(name)
		if seen[f] {
			return Policy{}, fmt.Errorf("classify: policy: description for %q given twice", name)
		}
		seen[f] = true
	}
	return p, nil
}

// IsSetAside reports whether a category name is an event set-aside.
func (p Policy) IsSetAside(name string) bool {
	_, ok := p.setAsideKeyword(name)
	return ok
}

// setAsideKeyword returns the keyword that makes name a set-aside.
func (p Policy) setAsideKeyword(name string) (string, bool) {
	words := strings.Fields(store.NormalizePayee(name))
	for _, k := range p.SetAsideKeywords {
		kw := store.NormalizePayee(k)
		for _, w := range words {
			if w == kw || w == kw+"s" {
				return k, true
			}
		}
	}
	return "", false
}

// description returns the override for name, if any.
func (p Policy) description(name string) (string, bool) {
	for n, d := range p.Descriptions {
		if fold(n) == fold(name) {
			return d, true
		}
	}
	return "", false
}

// PolicyHash identifies the policy's content. Keyword and description order
// don't matter; any change to a keyword, name or description changes it.
func PolicyHash(p Policy) string {
	kws := make([]string, len(p.SetAsideKeywords))
	for i, k := range p.SetAsideKeywords {
		kws[i] = fold(strings.TrimSpace(k))
	}
	sort.Strings(kws)
	type desc struct{ Name, Text string }
	descs := make([]desc, 0, len(p.Descriptions))
	for n, d := range p.Descriptions {
		descs = append(descs, desc{n, d})
	}
	sort.Slice(descs, func(i, j int) bool { return descs[i].Name < descs[j].Name })
	return hashJSON(struct {
		Version  int
		Keywords []string
		Descs    []desc
	}{p.Version, kws, descs})
}

// CandidatesHash identifies a candidate list: names, groups and descriptions,
// in any order. The model sees all three, so a change to any re-keys decisions.
func CandidatesHash(cands []Candidate) string {
	sorted := append([]Candidate(nil), cands...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		return a.Description < b.Description
	})
	return hashJSON(sorted)
}

// hashJSON is the hex sha256 of v's JSON, which quotes every string, so no
// two distinct inputs share an encoding.
func hashJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic("classify: hash: " + err.Error()) // only strings and ints: can't fail
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func fold(s string) string { return strings.ToLower(s) }
