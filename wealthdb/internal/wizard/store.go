package wizard

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/config"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/jsonedit"
)

// newFileMode is the mode of a config file the setup creates: the file
// can hold an API key.
const newFileMode fs.FileMode = 0o600

// saveMu is held while a write is under way, so a stop on a signal
// (WaitForSave) never cuts one in half.
var saveMu sync.Mutex

// WaitForSave waits for a write under way to finish and keeps any other
// from starting. The command calls it before it exits on a signal.
func WaitForSave() { saveMu.Lock() }

// store is the config file: the saved state of the setup. Every change
// is written at once, atomically, and only when it changes a byte.
type store struct {
	// path is the config's absolute path, which anchors its relative
	// paths. target is the file it names once its links are followed:
	// the file every write replaces, so a link stays a link.
	path, target string
	doc          *jsonedit.Doc
	// prior is the file's text when the run began, and mode its mode:
	// the mode every write keeps.
	prior    []byte
	mode     fs.FileMode
	backedUp bool
	// gold is the gold_db value written with the first change to a file
	// that names none, or an empty one.
	gold string
}

// configFile is the config at path: its absolute path, the file its
// links name, and that file's text, empty when there is none. A link
// whose file has no folder to be written in is refused.
func configFile(path string) (abs, target string, data []byte, err error) {
	if abs, err = filepath.Abs(path); err != nil {
		return "", "", nil, err
	}
	if target, err = followLinks(abs); err != nil {
		return "", "", nil, err
	}
	if target != abs {
		if fi, err := os.Stat(filepath.Dir(target)); err != nil || !fi.IsDir() {
			return "", "", nil, fmt.Errorf("the config %s is a link to %s, whose folder does not exist; "+
				"nothing is changed", abs, target)
		}
	}
	data, err = os.ReadFile(target)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", "", nil, fmt.Errorf("read %s: %w", target, err)
	}
	return abs, target, data, nil
}

// followLinks is the file p names once every symbolic link on the way is
// followed: p itself when it is no link.
func followLinks(p string) (string, error) {
	for range 40 {
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			return p, nil
		}
		t, err := os.Readlink(p)
		if err != nil {
			return "", fmt.Errorf("read the link %s: %w", p, err)
		}
		if !filepath.IsAbs(t) {
			t = filepath.Join(filepath.Dir(p), t)
		}
		p = filepath.Clean(t)
	}
	return "", fmt.Errorf("the config %s is a link that names itself", p)
}

// openStore is the store of the config at abs, whose links name target
// and whose text is data.
func openStore(abs, target string, data []byte, gold string) (*store, error) {
	s := &store{path: abs, target: target, mode: newFileMode, gold: gold}
	if len(bytes.TrimSpace(data)) > 0 {
		s.prior = data
		if fi, err := os.Stat(target); err == nil {
			s.mode = fi.Mode().Perm()
		}
	}
	var err error
	if s.doc, err = jsonedit.Parse(data); err != nil {
		return nil, fmt.Errorf("%s: %w", abs, err)
	}
	return s, nil
}

// refusal is a change the engine would refuse: the config it makes does
// not load. Nothing is written.
type refusal struct{ err error }

func (r *refusal) Error() string { return r.err.Error() }

// prepare applies edit to a copy of the document, with gold_db written
// first into a file that names none, or an empty one: the engine reads
// both as a setup not finished (config.InspectBytes). It returns the
// new text, or nil when nothing changes, and a refusal when the engine
// would refuse the result. A config that is not finished yet is no
// refusal.
func (s *store) prepare(edit func(d *jsonedit.Doc) error) ([]byte, error) {
	d, err := jsonedit.Parse(s.doc.Bytes())
	if err != nil {
		return nil, err
	}
	var gold string
	if has, err := s.doc.Decode(&gold, "gold_db"); s.gold != "" && (!has || err == nil && gold == "") {
		if err := d.Set(s.gold, "gold_db"); err != nil {
			return nil, err
		}
	}
	if err := edit(d); err != nil {
		return nil, err
	}
	out := d.Bytes()
	if bytes.Equal(out, s.doc.Bytes()) {
		return nil, nil
	}
	if st, _, err := config.InspectBytes(out, s.path); st == config.StateRefused {
		return nil, &refusal{err: err}
	}
	return out, nil
}

// commit writes text prepared by prepare: first, once per run, the
// file's prior text to <config>.bak, then the text itself. Both go
// beside the file the config's links name.
func (s *store) commit(text []byte) error {
	if text == nil {
		return nil
	}
	saveMu.Lock()
	defer saveMu.Unlock()
	if !s.backedUp && s.prior != nil {
		if err := writeAtomic(s.target+".bak", s.prior, s.mode); err != nil {
			return err
		}
		s.backedUp = true
	}
	if err := writeAtomic(s.target, text, s.mode); err != nil {
		return err
	}
	d, err := jsonedit.Parse(text)
	if err != nil {
		return err
	}
	s.doc = d
	return nil
}

// refresh reads the file again when it changed since the setup last
// read or wrote it, so an edit made by hand while the setup runs is kept
// and the next change applies on top of it. A file that does not parse
// now is refused: no change overwrites an edit under way.
func (s *store) refresh() error {
	data, err := os.ReadFile(s.target)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", s.target, err)
	}
	if bytes.Equal(data, s.doc.Bytes()) {
		return nil
	}
	d, err := jsonedit.Parse(data)
	if err != nil {
		return &refusal{err: fmt.Errorf("%s changed and does not parse now (%w); fix it first", s.path, err)}
	}
	s.doc = d
	if !s.backedUp && len(bytes.TrimSpace(data)) > 0 {
		s.prior = data
	}
	return nil
}

// apply is refresh, prepare, then commit.
func (s *store) apply(edit func(d *jsonedit.Doc) error) error {
	if err := s.refresh(); err != nil {
		return err
	}
	text, err := s.prepare(edit)
	if err != nil {
		return err
	}
	return s.commit(text)
}

// writeAtomic replaces path with data: a temporary file in the same
// folder, written, given mode and synced, then renamed over path.
func writeAtomic(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("save %s: %w", path, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op once renamed
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		return fmt.Errorf("save %s: %w", path, err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
