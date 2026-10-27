package main

import (
	"strings"
	"testing"
)

func TestByTruth(t *testing.T) {
	rows := []labeled{
		{"A", "A", 0.9}, {"A", "B", 0.5}, {"A", "B", 0.85}, {"A", "C", 0.7},
		{"B", "B", 0.95}, {"B", "B", 0.9},
	}
	got := byTruth(rows, 0.8)
	if len(got) != 2 || got[0].Name != "A" {
		t.Fatalf("want A hardest first, got %+v", got)
	}
	a := got[0]
	if a.N != 4 || a.Hit != 1 || a.BelowTau != 2 || a.WrongPick != "B" || a.WrongN != 2 {
		t.Errorf("A row: %+v", a)
	}
	if want := (0.9 + 0.5 + 0.85 + 0.7) / 4; a.MeanPTop != want {
		t.Errorf("mean p_top %v, want %v", a.MeanPTop, want)
	}
}

func TestByPredictionAndConfusions(t *testing.T) {
	rows := []labeled{{"A", "B", 0.9}, {"A", "B", 0.5}, {"B", "B", 0.9}, {"C", "C", 0.9}}
	p := byPrediction(rows, 0.8)
	if p[0].Name != "B" || p[0].Picked != 3 || p[0].Hit != 1 || p[0].PickedTau != 2 || p[0].HitTau != 1 {
		t.Errorf("precision rows: %+v", p)
	}
	c := confusions(rows, 0.8)
	if len(c) != 1 || c[0] != (pair{"A", "B", 2, 1}) {
		t.Errorf("confusions: %+v", c)
	}
}

func TestOfflineNeverCallsJev(t *testing.T) {
	j := &jevClient{offline: true, callsPath: t.TempDir() + "/jev_calls"}
	_, _, err := j.ask(t.Context(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "offline") || j.calls != 0 {
		t.Fatalf("offline ask: err=%v calls=%d", err, j.calls)
	}
	if _, err := offlineFill[[]result]("x")(); err == nil {
		t.Fatal("offlineFill must fail")
	}
}
