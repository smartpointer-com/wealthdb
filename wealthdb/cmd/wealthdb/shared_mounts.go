package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
)

// On a Mac, Docker runs in a virtual machine that sees only the host
// folders it shares: Colima shares the home folder alone unless its
// colima.yaml lists more. A bind mount of a folder outside them does not
// fail. Docker makes an empty folder on the machine's own disk instead,
// and what the engine saves there never reaches the Mac. The engine
// wrapper names the host's system in WEALTHDB_HOST_OS and the folders
// it mounts in WEALTHDB_MOUNTS (separated by ':', which a mount path
// cannot hold), and the engine refuses to run on a folder whose mount
// is the machine's own disk. A shared folder arrives over a file-sharing
// filesystem (virtiofs, fuse, 9p and the like), never as one of these.
var vmDiskTypes = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true,
	"btrfs": true, "overlay": true, "tmpfs": true,
}

// checkSharedMounts returns an error naming each folder of
// WEALTHDB_MOUNTS that the container sees on the virtual machine's own
// disk, when WEALTHDB_HOST_OS says the host is a Mac. A host that is not
// a Mac, a folder the mount table does not list, or a mount table that
// cannot be read is no error: on Linux, Docker mounts the host's own
// disk.
func checkSharedMounts(mountinfoPath string) error {
	if os.Getenv("WEALTHDB_HOST_OS") != "Darwin" {
		return nil
	}
	var want []string
	for _, p := range strings.Split(os.Getenv("WEALTHDB_MOUNTS"), ":") {
		// The kernel lists a mount point in clean form, and Docker cleans
		// a mount's target the same way: a trailing slash or a ./ in the
		// path as the wrapper wrote it names the same folder.
		if p != "" {
			want = append(want, filepath.Clean(p))
		}
	}
	if len(want) == 0 {
		return nil
	}
	data, err := os.ReadFile(mountinfoPath)
	if err != nil {
		return nil
	}
	bad := unsharedMounts(string(data), want)
	if len(bad) == 0 {
		return nil
	}
	verb, which := "are", "these"
	if len(bad) == 1 {
		verb, which = "is", "this one"
	}
	return errs.Newf(errs.ExitUsage, "%s %s not shared with Docker's virtual machine, so nothing "+
		"saved there would reach this Mac. Colima shares the home folder alone. Use a folder in "+
		"the home folder, or add %s under `mounts:` in ~/.colima/default/colima.yaml and "+
		"run `colima restart`.", joinAnd(bad), verb, which)
}

// unsharedMounts are the paths of want that mountinfo (the text of
// /proc/self/mountinfo) lists as a mount point on a disk of the virtual
// machine's own, in the order of want.
func unsharedMounts(mountinfo string, want []string) []string {
	fsType := map[string]string{}
	for _, line := range strings.Split(mountinfo, "\n") {
		// id parent major:minor root mount-point options [optional...] - fstype source super
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		for i := 5; i < len(f)-1; i++ {
			if f[i] == "-" {
				fsType[unescapeMountPath(f[4])] = f[i+1]
				break
			}
		}
	}
	var out []string
	for _, p := range want {
		if t, ok := fsType[p]; ok && vmDiskTypes[t] {
			out = append(out, p)
		}
	}
	return out
}

// unescapeMountPath undoes the octal escapes the kernel writes in a
// mountinfo path: \040 for a space, \011 a tab, \012 a newline, \134 a
// backslash.
func unescapeMountPath(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

// joinAnd is the paths joined for a sentence: "a", "a and b",
// "a, b and c".
func joinAnd(paths []string) string {
	if len(paths) == 1 {
		return paths[0]
	}
	return strings.Join(paths[:len(paths)-1], ", ") + " and " + paths[len(paths)-1]
}
