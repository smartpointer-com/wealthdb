package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
)

// TestConfigRefusesOnlyANonEmptyFile pins the clobber guard: a config
// with content is refused with init's exit code, while an empty file —
// the one the host-side wrapper creates to bind-mount the path — is no
// config at all, and the wizard runs (and here fails on its empty
// stdin, which is a different error).
func TestConfigRefusesOnlyANonEmptyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, tc := range []struct {
		name, body string
		refused    bool
	}{
		{"empty", "", false},
		{"written", `{"gold_db": "/x/gold.db"}`, true},
	} {
		path := filepath.Join(dir, tc.name+".cfg")
		if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		var so, se bytes.Buffer
		code := Run([]string{"-c", path, "config"}, strings.NewReader(""), &so, &se)
		refused := code == errs.ExitInitExisting && strings.Contains(se.String(), "already exists")
		if refused != tc.refused {
			t.Errorf("%s config file: exit %d, stderr %q; refused = %v, want %v",
				tc.name, code, se.String(), refused, tc.refused)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != tc.body {
			t.Errorf("%s config file was rewritten: %q (%v)", tc.name, got, err)
		}
	}
}
