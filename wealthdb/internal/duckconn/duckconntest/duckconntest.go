// Package duckconntest is test support for code that reads days out of
// DuckDB.
package duckconntest

import (
	"os"
	"os/exec"
	"regexp"
	"testing"
)

// childEnv marks the run of a test that AlsoInZone started.
const childEnv = "WEALTHDB_TEST_TZ_CHILD"

// AlsoInZone runs the calling test once more with the machine in the
// time zone zone, and fails it when that run fails. A test calls it
// last. DuckDB takes the machine's zone from the process, once, so the
// second zone needs a process of its own: the test binary, told to run
// this one test. In that process AlsoInZone returns at once.
func AlsoInZone(t *testing.T, zone string) {
	t.Helper()
	if os.Getenv(childEnv) != "" {
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$")
	cmd.Env = append(os.Environ(), childEnv+"=1", "TZ="+zone)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("in the time zone %s: %v\n%s", zone, err, out)
	}
}
