package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/errs"
)

// engineKindEnv is how a launcher tells the engine its kind: the
// container engine runs in Docker, the native engine is a program on
// the computer. An engine started with no kind checks nothing.
const engineKindEnv = "WEALTHDB_ENGINE"

const (
	engineContainer = "container"
	engineNative    = "native"
)

// goldEngineSuffix names the sidecar file that states which kind of
// engine writes a gold database.
//
// The write mutex and DuckDB's own lock are file locks, and on a Mac a
// file lock does not reach from the computer into Docker's virtual
// machine, in either direction. A native engine and a container engine
// on one database would each take the mutex and write. So a database
// has one kind of engine: the first write command states it here, and a
// write command of the other kind refuses. A report does not refuse,
// and never brings a database of the other kind up to date
// (openGoldForRead).
const goldEngineSuffix = ".wealthdb.engine"

// engineKind is the kind the launcher stated, or "" for none.
func engineKind() (string, error) {
	switch kind := os.Getenv(engineKindEnv); kind {
	case "", engineContainer, engineNative:
		return kind, nil
	default:
		return "", errs.Newf(errs.ExitUsage, "%s=%q is no kind of engine (want %s or %s)",
			engineKindEnv, kind, engineContainer, engineNative)
	}
}

// engineStatedWait is how long goldEngineStated waits for a sidecar
// that another command has created and not yet written.
var engineStatedWait = time.Second

// goldEngineStated reads the kind the sidecar of the gold database at
// goldPath states, "" when there is none. A file that holds neither
// kind is an error that names it: nothing is guessed from it.
func goldEngineStated(goldPath string) (string, error) {
	path := goldPath + goldEngineSuffix
	for deadline := time.Now().Add(engineStatedWait); ; time.Sleep(20 * time.Millisecond) {
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		if err != nil {
			return "", errs.Wrap(errs.ExitOpenFailed, fmt.Errorf("read the engine kind %q: %w", path, err))
		}
		switch stated := strings.TrimSpace(string(b)); stated {
		case engineContainer, engineNative:
			return stated, nil
		case "":
			// Created by a command that has not written it yet.
			if time.Now().Before(deadline) {
				continue
			}
		}
		return "", errs.Newf(errs.ExitOpenFailed,
			"%q states no kind of engine. Delete it; the next write command states the kind again.", path)
	}
}

// otherEngineWrites reports whether the gold database at goldPath is
// held to another kind of engine than this one, or cannot be told. An
// engine with no kind holds no database to anything.
func otherEngineWrites(goldPath string) bool {
	kind, err := engineKind()
	if err != nil || kind == "" {
		return false
	}
	stated, err := goldEngineStated(goldPath)
	return err != nil || (stated != "" && stated != kind)
}

// claimGoldForEngine holds the gold database at goldPath to this
// engine's kind: it states the kind where none is stated, and refuses a
// database the other kind writes. The write mutex excludes commands of
// this kind only, so the first statement is the creation of the file,
// which one command alone can win: the loser reads the winner's kind. A
// new database takes the kind whatever a file left from an earlier one
// says.
func claimGoldForEngine(goldPath, cmdName string, created bool) error {
	kind, err := engineKind()
	if err != nil || kind == "" {
		return err
	}
	path := goldPath + goldEngineSuffix
	if created {
		tmp := fmt.Sprintf("%s.tmp-%d", path, os.Getpid())
		if err := os.WriteFile(tmp, []byte(kind+"\n"), 0o600); err != nil {
			return errs.Wrap(errs.ExitOpenFailed, fmt.Errorf("state the engine kind in %q: %w", path, err))
		}
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			return errs.Wrap(errs.ExitOpenFailed, fmt.Errorf("state the engine kind in %q: %w", path, err))
		}
		return nil
	}
	for {
		stated, err := goldEngineStated(goldPath)
		if err != nil {
			return err
		}
		if stated == kind {
			return nil
		}
		if stated != "" {
			return errs.Newf(errs.ExitOpenFailed,
				"'%s' cannot write %q: the %s engine writes this database, and this is the %s engine. "+
					"Two kinds of engine must not write one database, because a file lock does not reach "+
					"between this computer and Docker. To move the database to this engine, stop the other "+
					"engine's servers (wealthdb mcp stop, wealthdb web stop), then delete %q.",
				cmdName, goldPath, stated, kind, path)
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			// The other kind stated it first: read what it stated.
			continue
		}
		if err == nil {
			_, err = f.WriteString(kind + "\n")
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
		if err != nil {
			return errs.Wrap(errs.ExitOpenFailed, fmt.Errorf("state the engine kind in %q: %w", path, err))
		}
		return nil
	}
}
