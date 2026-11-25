package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A mount table as a container under Colima sees it: the shared home
// folder over virtiofs, a folder outside it on the machine's own disk,
// and a shared path with a space, which the kernel escapes.
const colimaMountinfo = `1393 1166 0:340 / / rw,relatime master:568 - overlay overlay rw,lowerdir=/x
1400 1393 0:358 / /Users/u/wealthdb/data rw,relatime - virtiofs lima-tag0 rw
1401 1393 254:1 /tmp/try /tmp/try rw,relatime - ext4 /dev/root rw,discard
1402 1393 0:358 / /Users/u/my\040config rw,relatime master:1 shared:2 - virtiofs lima-tag0 rw
1403 1393 254:1 /var/lib/docker/x /private/tmp/elsewhere/wealthdb.cfg rw - ext4 /dev/root rw
`

func TestUnsharedMounts(t *testing.T) {
	t.Parallel()
	got := unsharedMounts(colimaMountinfo, []string{
		"/Users/u/wealthdb/data", "/tmp/try", "/Users/u/my config",
		"/private/tmp/elsewhere/wealthdb.cfg", "/not/listed",
	})
	want := []string{"/tmp/try", "/private/tmp/elsewhere/wealthdb.cfg"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unsharedMounts = %q, want %q", got, want)
	}
}

func TestCheckSharedMounts(t *testing.T) {
	mi := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(mi, []byte(colimaMountinfo), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, os, mounts, want string
	}{
		{"a Mac, every folder shared", "Darwin", "/Users/u/my config:/Users/u/wealthdb/data", ""},
		{"a Mac, one folder on the machine's disk", "Darwin", "/Users/u/my config:/tmp/try",
			"/tmp/try is not shared with Docker's virtual machine"},
		{"a Mac, two folders", "Darwin", "/tmp/try:/private/tmp/elsewhere/wealthdb.cfg",
			"/tmp/try and /private/tmp/elsewhere/wealthdb.cfg are not shared"},
		// A path the wrapper did not write in clean form names the same
		// folder as the kernel's clean mount point.
		{"a Mac, a trailing slash", "Darwin", "/Users/u/wealthdb/data/:/tmp/try/",
			"/tmp/try is not shared with Docker's virtual machine"},
		{"a Mac, a dot in the path", "Darwin", "/tmp/./try", "/tmp/try is not shared with Docker's virtual machine"},
		{"a Mac, a shared folder with a trailing slash", "Darwin", "/Users/u/my config/", ""},
		{"Linux mounts the host's own disk", "Linux", "/tmp/try", ""},
		{"no wrapper, no host named", "", "/tmp/try", ""},
		{"no folder named", "Darwin", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WEALTHDB_HOST_OS", tc.os)
			t.Setenv("WEALTHDB_MOUNTS", tc.mounts)
			err := checkSharedMounts(mi)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), "colima restart"):
				t.Errorf("error %q names no fix", err)
			}
		})
	}
	t.Setenv("WEALTHDB_HOST_OS", "Darwin")
	t.Setenv("WEALTHDB_MOUNTS", "/tmp/try")
	if err := checkSharedMounts(filepath.Join(t.TempDir(), "none")); err != nil {
		t.Errorf("an unreadable mount table blocks: %v", err)
	}
}

// Through the dispatcher: a refused folder stops every subcommand but
// help and version, before the subcommand runs.
func TestDispatchRefusesAnUnsharedFolder(t *testing.T) {
	t.Setenv("WEALTHDB_HOST_OS", "Darwin")
	t.Setenv("WEALTHDB_MOUNTS", "/")
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil || len(unsharedMounts(string(data), []string{"/"})) == 0 {
		t.Skip("no mount table here whose root is on a local disk type")
	}
	cfg := filepath.Join(t.TempDir(), "wealthdb.cfg")
	var out, errb strings.Builder
	if code := Run([]string{"-c", cfg, "status"}, strings.NewReader(""), &out, &errb); code != 2 ||
		!strings.Contains(errb.String(), "is not shared with Docker's virtual machine") {
		t.Errorf("status: code %d, stderr %q", code, errb.String())
	}
	errb.Reset()
	if code := Run([]string{"version"}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Errorf("version: code %d, stderr %q", code, errb.String())
	}
}
