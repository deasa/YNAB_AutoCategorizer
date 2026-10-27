package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Per-category difficulty (task M0-1b): which categories Jev struggles with,
// computed from cached answers only. Aggregates and category names only.

const (
	percatVariant = "batch10+history+policy" // recommended, pooled over off0 and off100
	contrastVar   = "single"                 // bare variant, off0 only
	percatTau     = 0.8
	lowSample     = 3
)

// labeled is one cached answer joined with its true category name.
type labeled struct {
	Truth, Pick string
	PTop        float64
}

type catRow struct {
	Name      string
	N, Hit    int
	MeanPTop  float64
	BelowTau  int
	WrongPick string // most common wrong pick ("" if none)
	WrongN    int
}

func (c catRow) acc() float64 { return float64(c.Hit) / float64(c.N) }

// byTruth groups answers by true category, hardest first: lowest accuracy,
// then lowest mean p_top, then name.
func byTruth(rows []labeled, tau float64) []catRow {
	idx := map[string]*catRow{}
	wrong := map[string]map[string]int{}
	for _, r := range rows {
		c := idx[r.Truth]
		if c == nil {
			c = &catRow{Name: r.Truth}
			idx[r.Truth] = c
			wrong[r.Truth] = map[string]int{}
		}
		c.N++
		c.MeanPTop += r.PTop
		if r.PTop < tau {
			c.BelowTau++
		}
		if r.Pick == r.Truth {
			c.Hit++
		} else {
			wrong[r.Truth][r.Pick]++
		}
	}
	var out []catRow
	for name, c := range idx {
		c.MeanPTop /= float64(c.N)
		c.WrongPick, c.WrongN = mostCommon(wrong[name])
		out = append(out, *c)
	}
	sort.Slice(out, func(a, b int) bool {
		x, y := out[a], out[b]
		if x.acc() != y.acc() {
			return x.acc() < y.acc()
		}
		if x.MeanPTop != y.MeanPTop {
			return x.MeanPTop < y.MeanPTop
		}
		return x.Name < y.Name
	})
	return out
}

// mostCommon returns the highest count, ties broken by name.
func mostCommon(m map[string]int) (string, int) {
	best, n := "", 0
	for k, v := range m {
		if v > n || (v == n && k < best) {
			best, n = k, v
		}
	}
	return best, n
}

type predRow struct {
	Name              string
	Picked, Hit       int
	PickedTau, HitTau int // only answers with p_top ≥ τ
}

// byPrediction scores each predicted category's precision, lowest first.
func byPrediction(rows []labeled, tau float64) []predRow {
	idx := map[string]*predRow{}
	for _, r := range rows {
		p := idx[r.Pick]
		if p == nil {
			p = &predRow{Name: r.Pick}
			idx[r.Pick] = p
		}
		p.Picked++
		ok := r.Pick == r.Truth
		if ok {
			p.Hit++
		}
		if r.PTop >= tau {
			p.PickedTau++
			if ok {
				p.HitTau++
			}
		}
	}
	var out []predRow
	for _, p := range idx {
		out = append(out, *p)
	}
	sort.Slice(out, func(a, b int) bool {
		x, y := out[a], out[b]
		px, py := float64(x.Hit)/float64(x.Picked), float64(y.Hit)/float64(y.Picked)
		if px != py {
			return px < py
		}
		if x.Picked != y.Picked {
			return x.Picked > y.Picked
		}
		return x.Name < y.Name
	})
	return out
}

type pair struct {
	Truth, Pick string
	N, NTau     int // NTau: confusions that would pass the τ gate (auto-applied wrong)
}

// confusions counts true → predicted mistakes, most frequent first.
func confusions(rows []labeled, tau float64) []pair {
	idx := map[[2]string]*pair{}
	for _, r := range rows {
		if r.Pick == r.Truth {
			continue
		}
		k := [2]string{r.Truth, r.Pick}
		p := idx[k]
		if p == nil {
			p = &pair{Truth: r.Truth, Pick: r.Pick}
			idx[k] = p
		}
		p.N++
		if r.PTop >= tau {
			p.NTau++
		}
	}
	var out []pair
	for _, p := range idx {
		out = append(out, *p)
	}
	sort.Slice(out, func(a, b int) bool {
		x, y := out[a], out[b]
		if x.N != y.N {
			return x.N > y.N
		}
		if x.NTau != y.NTau {
			return x.NTau > y.NTau
		}
		return x.Truth+"\x00"+x.Pick < y.Truth+"\x00"+y.Pick
	})
	return out
}

// loadLabeled joins one cached variant file with its sample's true labels.
// It never calls Jev: a missing cache file is an error.
func loadLabeled(cache string, d *ynabData, v string, offset int) ([]labeled, error) {
	path := filepath.Join(cache, fmt.Sprintf("jev_%s_off%d.json", v, offset))
	res, _, err := cached(path, offlineFill[[]result](filepath.Base(path)))
	if err != nil {
		return nil, err
	}
	name := map[string]string{}
	for _, c := range d.Categories {
		name[c.ID] = c.Name
	}
	sample, _, _ := pickSample(d, offset)
	out := make([]labeled, 0, len(res))
	for _, r := range res {
		out = append(out, labeled{name[sample[r.Idx].CategoryID], r.Pick, r.PTop})
	}
	return out, nil
}

func perCategoryReport(cache string, d *ynabData) error {
	var pooled []labeled
	for _, off := range []int{0, sampleSize} {
		l, err := loadLabeled(cache, d, percatVariant, off)
		if err != nil {
			return err
		}
		pooled = append(pooled, l...)
	}
	single, err := loadLabeled(cache, d, contrastVar, 0)
	if err != nil {
		return err
	}
	singleAcc := map[string]catRow{}
	for _, c := range byTruth(single, percatTau) {
		singleAcc[c.Name] = c
	}

	rows := byTruth(pooled, percatTau)
	hit := 0
	for _, r := range pooled {
		if r.Pick == r.Truth {
			hit++
		}
	}
	fmt.Printf("\n# Per-category difficulty: %s, off0+off%d pooled (n=%d, top-1 %.0f%%); contrast: %s off0 (n=%d)\n\n",
		percatVariant, sampleSize, len(pooled), pct(hit, len(pooled)), contrastVar, len(single))
	fmt.Printf("| True category | n | top-1 | mean p_top | <τ%.1f | most common wrong pick | %s top-1 (n) |\n", percatTau, contrastVar)
	fmt.Println("|---|---|---|---|---|---|---|")
	for _, c := range rows {
		label := strings.TrimSpace(c.Name)
		if c.N < lowSample {
			label += " ⚠ low-n"
		}
		wp := "—"
		if c.WrongN > 0 {
			wp = fmt.Sprintf("%s (%d)", c.WrongPick, c.WrongN)
		}
		sc := "—"
		if s, ok := singleAcc[c.Name]; ok {
			sc = fmt.Sprintf("%.0f%% (%d)", 100*s.acc(), s.N)
		}
		fmt.Printf("| %s | %d | %.0f%% | %.2f | %.0f%% | %s | %s |\n",
			label, c.N, 100*c.acc(), c.MeanPTop, pct(c.BelowTau, c.N), wp, sc)
	}

	fmt.Printf("\n## Predicted categories, lowest precision first (%s pooled)\n\n", percatVariant)
	fmt.Printf("| Predicted | picked | precision | picked at p_top≥%.1f | precision at τ |\n|---|---|---|---|---|\n", percatTau)
	for _, p := range byPrediction(pooled, percatTau) {
		if p.Hit == p.Picked {
			continue // perfect precision: not interesting here
		}
		fmt.Printf("| %s | %d | %s | %d | %s |\n", p.Name, p.Picked, pctOrDash(p.Hit, p.Picked), p.PickedTau, pctOrDash(p.HitTau, p.PickedTau))
	}

	fmt.Printf("\n## Most frequent confusions, true → predicted (%s pooled)\n\n", percatVariant)
	fmt.Printf("| True → predicted | n | of which p_top≥%.1f (auto-applied wrong) |\n|---|---|---|\n", percatTau)
	for i, p := range confusions(pooled, percatTau) {
		if i == 15 || p.N < 2 && i >= 10 {
			break
		}
		fmt.Printf("| %s → %s | %d | %d |\n", p.Truth, p.Pick, p.N, p.NTau)
	}
	return nil
}

// offlineFill makes every fill path of cached() fail instead of calling an API.
func offlineFill[T any](what string) func() (T, error) {
	return func() (T, error) {
		var z T
		return z, fmt.Errorf("offline: %s not in cache, refusing to call the API", what)
	}
}
