package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Generations is a per-user token generation counter persisted to a small JSON
// file. Every token records the user's generation at issue time; bumping the
// counter (the `revoke` command) invalidates all of that user's access and
// refresh tokens in one step. The file is re-read on every check so a revoke
// from another process takes effect immediately.
type Generations struct {
	path string
	mu   sync.Mutex
}

// allUsers is the key whose generation applies to every user (revoke --all).
const allUsers = "*"

func NewGenerations(path string) *Generations { return &Generations{path: path} }

func (g *Generations) load() (map[string]int, error) {
	m := map[string]int{}
	b, err := os.ReadFile(g.path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	return m, json.Unmarshal(b, &m)
}

// Current returns the combined generation for userID (user + global).
func (g *Generations) Current(userID string) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	m, err := g.load()
	if err != nil {
		return 0, err
	}
	return m[userID] + m[allUsers], nil
}

// Bump increments userID's generation (or every user's, for allUsers).
func (g *Generations) Bump(userID string) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	m, err := g.load()
	if err != nil {
		return 0, err
	}
	m[userID]++
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(g.path), 0o700); err != nil {
		return 0, err
	}
	tmp := g.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return 0, err
	}
	return m[userID], os.Rename(tmp, g.path)
}
