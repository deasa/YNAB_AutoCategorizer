package ynab

import (
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestGovernor_ReadsStopAtWriteReserve(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	g := NewGovernor(DefaultHourlyLimit, DefaultWriteReserve, clock.Now)

	for i := range 180 {
		if err := g.Take(false); err != nil {
			t.Fatalf("read %d refused: %v", i, err)
		}
	}
	err := g.Take(false)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("read 181: got %v, want ErrBudgetExhausted", err)
	}
	if r, w := g.Remaining(); r != 0 || w != 20 {
		t.Errorf("Remaining() = %d reads, %d writes; want 0, 20", r, w)
	}

	for i := range 20 {
		if err := g.Take(true); err != nil {
			t.Fatalf("write %d refused: %v", i, err)
		}
	}
	if err := g.Take(true); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("write 21: got %v, want ErrBudgetExhausted", err)
	}
}

func TestGovernor_RefillsContinuously(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	g := NewGovernor(200, 20, clock.Now)
	for range 200 {
		if err := g.Take(true); err != nil {
			t.Fatal(err)
		}
	}

	// 200/hour is one token per 18s. Reads need the bucket above the
	// 20-token reserve, so the first read is 21 tokens (378s) away.
	clock.Advance(18 * time.Second)
	if err := g.Take(true); err != nil {
		t.Fatalf("write after 18s: %v", err)
	}
	if err := g.Take(false); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("read with empty bucket: got %v", err)
	}
	clock.Advance(21 * 18 * time.Second)
	if err := g.Take(false); err != nil {
		t.Fatalf("read after refill: %v", err)
	}

	// Refill caps at capacity.
	clock.Advance(10 * time.Hour)
	if r, w := g.Remaining(); r != 180 || w != 200 {
		t.Errorf("after long idle: %d reads, %d writes; want 180, 200", r, w)
	}
}

func TestGovernor_AllowDoesNotSpend(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	g := NewGovernor(3, 2, clock.Now)
	for range 5 {
		if err := g.Allow(false); err != nil {
			t.Fatalf("Allow: %v", err)
		}
	}
	if err := g.Take(false); err != nil {
		t.Fatal(err)
	}
	if err := g.Allow(false); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("Allow after budget spent: got %v", err)
	}
	if err := g.Allow(true); err != nil {
		t.Fatalf("Allow(write) inside reserve: %v", err)
	}
}

func TestGovernor_ExhaustedErrorSaysWhen(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	g := NewGovernor(200, 0, clock.Now)
	for range 200 {
		_ = g.Take(false)
	}
	err := g.Take(false)
	if err == nil || err.Error() != "ynab: local request budget exhausted: next read in 18s" {
		t.Errorf("got %v", err)
	}
}
