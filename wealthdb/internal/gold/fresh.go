package gold

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// A gold database migrated to the current schema and holding no data is
// expensive to build — every migration runs — and cheap to copy. One image
// is built per process and OpenFresh serves every caller from it.
var fresh struct {
	once sync.Once
	data []byte
	err  error
}

// OpenFresh writes a migrated, empty gold database at path and opens it
// read-write. The path must not exist yet: an existing file is never
// overwritten.
func OpenFresh(path string) (*sql.DB, error) {
	data, err := freshImage()
	if err != nil {
		return nil, fmt.Errorf("fresh gold image: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("fresh gold %q: %w", path, err)
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return nil, fmt.Errorf("fresh gold %q: %w", path, werr)
	}
	return Open(path, ModeReadWrite)
}

// freshImage builds the image on first use: a file Open has migrated,
// checkpointed and closed, read back whole. Whatever Open does to a new
// database it has done to this one, so a copy opens as a reopen.
func freshImage() ([]byte, error) {
	fresh.once.Do(func() {
		fresh.data, fresh.err = buildFreshImage()
	})
	return fresh.data, fresh.err
}

func buildFreshImage() ([]byte, error) {
	dir, err := os.MkdirTemp("", "wealthdb-fresh-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "gold.db")
	db, err := Open(path, ModeReadWrite)
	if err != nil {
		return nil, err
	}
	// The checkpoint folds the write-ahead log into the file, so the
	// bytes on disk are the whole database.
	if _, err := db.Exec(`CHECKPOINT`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path + ".wal"); err == nil {
		return nil, errors.New("write-ahead log survived close")
	}
	return os.ReadFile(path)
}
