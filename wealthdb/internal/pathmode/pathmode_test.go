package pathmode

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestForceReadOnlyFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exists.db")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := Detect(path, true, false)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if d.Mode != ModeReadOnly || d.Reason != ReasonExplicitFlag {
		t.Errorf("got %+v, want ModeReadOnly/ReasonExplicitFlag", d)
	}
}

func TestMissingDBInit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.db")
	d, err := Detect(path, false, true)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if d.Mode != ModeReadWrite {
		t.Errorf("init on missing DB should be ModeReadWrite, got %v", d.Mode)
	}
	if d.Reason != ReasonDBMissingForInit {
		t.Errorf("reason = %v, want ReasonDBMissingForInit", d.Reason)
	}
	if d.DBExists {
		t.Error("DBExists should be false")
	}
}

func TestMissingDBNonInit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.db")
	d, err := Detect(path, false, false)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if d.Mode != ModeReadOnly || d.Reason != ReasonDBMissingForRO {
		t.Errorf("got %+v, want ModeReadOnly/ReasonDBMissingForRO", d)
	}
}

func TestExistingWriteableDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exists.db")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := Detect(path, false, false)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if d.Mode != ModeReadWrite || d.Reason != ReasonDBWriteable {
		t.Errorf("got %+v, want ModeReadWrite/ReasonDBWriteable", d)
	}
}

func TestReadOnlyFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod semantics differ on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "ro.db")
	if err := os.WriteFile(path, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	// Sanity: chmod must have worked. Skip if running as root
	// (root bypasses mode bits and would falsely pass).
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses file mode bits")
	}

	d, err := Detect(path, false, false)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if d.Mode != ModeReadOnly {
		t.Errorf("mode = %v, want ModeReadOnly", d.Mode)
	}
	if d.Reason != ReasonFileNotWriteable {
		t.Errorf("reason = %v, want ReasonFileNotWriteable", d.Reason)
	}
}

func TestReadOnlyDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod semantics differ on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses dir mode bits")
	}

	dir := t.TempDir()
	roDir := filepath.Join(dir, "ro_parent")
	if err := os.Mkdir(roDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(roDir, "x.db")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Make the parent read-only.
	if err := os.Chmod(roDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) })

	d, err := Detect(path, false, false)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if d.Mode != ModeReadOnly {
		t.Errorf("mode = %v, want ModeReadOnly", d.Mode)
	}
	if d.Reason != ReasonDirNotWriteable {
		t.Errorf("reason = %v, want ReasonDirNotWriteable", d.Reason)
	}
}

func TestParentDir(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/foo/bar.db", "/foo"},
		{"/bar.db", "/"},
		{"bar.db", "."},
		{"./bar.db", "."},
	}
	for _, c := range cases {
		if got := parentDir(c.in); got != c.want {
			t.Errorf("parentDir(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
