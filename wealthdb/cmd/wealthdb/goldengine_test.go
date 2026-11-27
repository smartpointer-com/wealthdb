package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func engineKindFile(t *testing.T, gold string) string {
	t.Helper()
	b, err := os.ReadFile(gold + goldEngineSuffix)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A write command states its engine's kind beside a database that
// states none, passes one of its own kind, and refuses one of the other
// kind with both kinds and the way out in the message.
func TestAWriteCommandHoldsTheDatabaseToItsEnginesKind(t *testing.T) {
	gold := filepath.Join(t.TempDir(), "wealthdb.db")

	t.Setenv(engineKindEnv, "")
	if err := claimGoldForEngine(gold, "load", "", false); err != nil {
		t.Fatalf("an engine with no kind: %v", err)
	}
	if got := engineKindFile(t, gold); got != "" {
		t.Fatalf("an engine with no kind stated %q", got)
	}

	t.Setenv(engineKindEnv, engineNative)
	for range 2 {
		if err := claimGoldForEngine(gold, "load", "", false); err != nil {
			t.Fatalf("the native engine: %v", err)
		}
		if got := engineKindFile(t, gold); got != "native\n" {
			t.Fatalf("the kind stated is %q, want native", got)
		}
	}

	t.Setenv(engineKindEnv, engineContainer)
	err := claimGoldForEngine(gold, "load", "", false)
	if err == nil {
		t.Fatal("the container engine wrote a database the native engine writes")
	}
	for _, want := range []string{"'load'", "the native engine writes", "this is the container engine",
		"wealthdb mcp stop", gold + goldEngineSuffix} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal lacks %q: %v", want, err)
		}
	}
	if got := engineKindFile(t, gold); got != "native\n" {
		t.Errorf("a refused command changed the kind to %q", got)
	}

	// A new database takes its creator's kind over a file left behind.
	if err := claimGoldForEngine(gold, "init", "", true); err != nil {
		t.Fatalf("a new database: %v", err)
	}
	if got := engineKindFile(t, gold); got != "container\n" {
		t.Errorf("a new database states %q, want container", got)
	}

	t.Setenv(engineKindEnv, "docker")
	if err := claimGoldForEngine(gold, "load", "", false); err == nil || !strings.Contains(err.Error(), "no kind of engine") {
		t.Errorf("an unknown kind: %v", err)
	}
}

// A file that states neither kind is refused by name, never read as a
// kind, and one that cannot be read is an error too.
func TestAnEngineKindFileThatStatesNoKindIsRefused(t *testing.T) {
	realWait := engineStatedWait
	engineStatedWait = 50 * time.Millisecond
	t.Cleanup(func() { engineStatedWait = realWait })
	t.Setenv(engineKindEnv, engineNative)
	for _, content := range []string{"", "podman\n"} {
		gold := filepath.Join(t.TempDir(), "wealthdb.db")
		if err := os.WriteFile(gold+goldEngineSuffix, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		err := claimGoldForEngine(gold, "load", "", false)
		if err == nil || !strings.Contains(err.Error(), "states no kind of engine") || !strings.Contains(err.Error(), gold+goldEngineSuffix) {
			t.Errorf("content %q: %v", content, err)
		}
		if !otherEngineWrites(gold) {
			t.Errorf("content %q: a database that cannot be told is not held back from a report's migration", content)
		}
		if got := engineKindFile(t, gold); got != content {
			t.Errorf("content %q: the file was changed to %q", content, got)
		}
	}
	gold := filepath.Join(t.TempDir(), "wealthdb.db")
	if err := os.Mkdir(gold+goldEngineSuffix, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := claimGoldForEngine(gold, "load", "", false); err == nil || !strings.Contains(err.Error(), "read the engine kind") {
		t.Errorf("a file that cannot be read: %v", err)
	}
}

// Of two engines of different kinds that meet a database with no kind
// stated, one states its kind and the other is refused.
func TestOneOfTwoFirstWritersStatesTheKind(t *testing.T) {
	gold := filepath.Join(t.TempDir(), "wealthdb.db")
	// The other kind has created the file and not yet written it.
	if err := os.WriteFile(gold+goldEngineSuffix, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = os.WriteFile(gold+goldEngineSuffix, []byte("container\n"), 0o600)
	}()
	t.Setenv(engineKindEnv, engineNative)
	err := claimGoldForEngine(gold, "load", "", false)
	if err == nil || !strings.Contains(err.Error(), "the container engine writes") {
		t.Errorf("the engine that lost the first statement: %v", err)
	}
}

// otherEngineWrites: only a database another kind is stated for, seen
// by an engine that has a kind.
func TestOtherEngineWrites(t *testing.T) {
	gold := filepath.Join(t.TempDir(), "wealthdb.db")
	t.Setenv(engineKindEnv, engineNative)
	if otherEngineWrites(gold) {
		t.Error("a database with no kind stated")
	}
	if err := os.WriteFile(gold+goldEngineSuffix, []byte("native\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if otherEngineWrites(gold) {
		t.Error("a database of this kind")
	}
	t.Setenv(engineKindEnv, engineContainer)
	if !otherEngineWrites(gold) {
		t.Error("a database of the other kind is not held back")
	}
	t.Setenv(engineKindEnv, "")
	if otherEngineWrites(gold) {
		t.Error("an engine with no kind holds a database back")
	}
}

// The mutex holds the database to the kind, and a refused command
// leaves the mutex free.
func TestTheWriteMutexRefusesTheOtherKindOfEngine(t *testing.T) {
	gold := filepath.Join(t.TempDir(), "wealthdb.db")
	t.Setenv(engineKindEnv, engineNative)
	lock, err := lockGoldForWrite(gold, "load", "")
	if err != nil {
		t.Fatal(err)
	}
	lock.unlock()

	t.Setenv(engineKindEnv, engineContainer)
	if _, err := lockGoldForWrite(gold, "load", ""); err == nil {
		t.Fatal("the container engine took the mutex of a database the native engine writes")
	}

	t.Setenv(engineKindEnv, engineNative)
	lock, err = lockGoldForWrite(gold, "load", "")
	if err != nil {
		t.Fatalf("the refused command kept the mutex: %v", err)
	}
	lock.unlock()
}

// init states its engine's kind beside the database it creates, over a
// file an earlier database left.
func TestInitStatesTheEnginesKind(t *testing.T) {
	cfg := setupCLITest(t)
	gold := goldPathFromCfg(cfg)
	if err := os.WriteFile(gold+goldEngineSuffix, []byte("container\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(engineKindEnv, engineNative)
	if _, se, code := run(t, "-c", cfg, "init"); code != 0 {
		t.Fatalf("init: exit %d: %s", code, se)
	}
	if got := engineKindFile(t, gold); got != "native\n" {
		t.Errorf("after init the kind stated is %q, want native", got)
	}
}

// A report brings a database of its own kind up to date, and reads one
// the other kind writes as it is.
func TestAReportDoesNotMigrateADatabaseOfTheOtherKind(t *testing.T) {
	cfg := setupCLITest(t)
	gold := goldPathFromCfg(cfg)
	t.Setenv(engineKindEnv, engineNative)
	freshGoldFor(t, cfg)
	if so, se, code := run(t, "-c", cfg, "load", "-a"); code != 0 {
		t.Fatalf("load -a: exit %d\n%s%s", code, so, se)
	}
	// A derived set that is not this binary's, as another build leaves it.
	stored := func(set string) string {
		t.Helper()
		db, err := sql.Open("duckdb", gold)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if set != "" {
			if _, err := db.Exec(`UPDATE derived_state SET hash = ?`, set); err != nil {
				t.Fatal(err)
			}
		}
		var hash string
		if err := db.QueryRow(`SELECT max(hash) FROM derived_state`).Scan(&hash); err != nil {
			t.Fatal(err)
		}
		return hash
	}
	stored("another-build")

	t.Setenv(engineKindEnv, engineContainer)
	if so, se, code := run(t, "-c", cfg, "holdings", "global"); code != 0 {
		t.Fatalf("a report by the other kind of engine: exit %d\n%s%s", code, so, se)
	}
	if got := stored(""); got != "another-build" {
		t.Errorf("a report by the other kind of engine brought the database up to date (hash %q)", got)
	}

	t.Setenv(engineKindEnv, engineNative)
	if so, se, code := run(t, "-c", cfg, "holdings", "global"); code != 0 {
		t.Fatalf("a report by the database's own kind: exit %d\n%s%s", code, so, se)
	}
	if got := stored(""); got == "another-build" {
		t.Error("a report by the database's own kind left it out of date")
	}
}

// The refusal's commands read the run's config, as every command a
// message names does (commandFor).
func TestTheEngineKindRefusalNamesTheRunsConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/x")
	gold := filepath.Join(t.TempDir(), "wealthdb.db")
	t.Setenv(engineKindEnv, engineNative)
	if err := claimGoldForEngine(gold, "load", "/x/try.cfg", false); err != nil {
		t.Fatal(err)
	}
	t.Setenv(engineKindEnv, engineContainer)
	err := claimGoldForEngine(gold, "load", "/x/try.cfg", false)
	if err == nil {
		t.Fatal("the container engine wrote a database the native engine writes")
	}
	for _, want := range []string{"wealthdb -c /x/try.cfg mcp stop", "wealthdb -c /x/try.cfg web stop"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal lacks %q: %v", want, err)
		}
	}
}
