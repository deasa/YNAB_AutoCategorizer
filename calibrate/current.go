package calibrate

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// DefaultPath is where `finbot calibrate` writes, relative to the repo root.
// The file is checked in and embedded, so a new τ takes effect on the next
// build.
const DefaultPath = "calibrate/calibration.json"

//go:embed calibration.json
var calibrationJSON []byte

var (
	currentOnce sync.Once
	current     Calibration
)

// Current returns the embedded calibration. If it can't be parsed, it returns
// one with AutoEligible false, so nothing auto-applies.
func Current() Calibration {
	currentOnce.Do(func() {
		c, err := Parse(calibrationJSON)
		if err != nil {
			c = Calibration{}
		}
		current = c
	})
	return current
}

// Parse decodes a calibration.json. Unknown fields and a wrong version are errors.
func Parse(data []byte) (Calibration, error) {
	var c Calibration
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Calibration{}, fmt.Errorf("calibrate: parse: %w", err)
	}
	if c.Version != FormatVersion {
		return Calibration{}, fmt.Errorf("calibrate: version %d, want %d", c.Version, FormatVersion)
	}
	return c, nil
}

// Applies reports whether a model answer with p_top may be auto-applied: the
// calibration is auto-eligible, was measured on this model, policy and
// candidate list, and p_top reaches τ.
func (c Calibration) Applies(model, policyHash, candidatesHash string, pTop float64) bool {
	return c.AutoEligible && c.Tau != nil && pTop >= *c.Tau &&
		model == c.Model && policyHash == c.PolicyHash && candidatesHash == c.CandidatesHash
}

// Marshal encodes c as calibration.json.
func Marshal(c *Calibration) ([]byte, error) {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("calibrate: encode: %w", err)
	}
	return append(b, '\n'), nil
}

// WriteFile writes c to path, replacing it whole or not at all.
func WriteFile(path string, c *Calibration) error {
	b, err := Marshal(c)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".calibration-*.json")
	if err != nil {
		return fmt.Errorf("calibrate: write: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("calibrate: write: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return fmt.Errorf("calibrate: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("calibrate: write: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("calibrate: write: %w", err)
	}
	return nil
}
