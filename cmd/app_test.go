package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestMain_UsageExits2(t *testing.T) {
	for _, args := range [][]string{nil, {"calibrate"}, {"sync", "--bogus"}, {"sync", "extra"}} {
		var out, errOut bytes.Buffer
		if code := Main(args, &out, &errOut); code != 2 {
			t.Errorf("Main(%q) = %d, want 2", args, code)
		}
		if !strings.Contains(errOut.String(), "usage: finbot") && !strings.Contains(errOut.String(), "Usage of finbot") {
			t.Errorf("Main(%q) printed no usage: %q", args, errOut.String())
		}
	}
}
