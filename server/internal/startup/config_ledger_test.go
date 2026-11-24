package startup

import (
	"path/filepath"
	"testing"
)

func TestNumberLedgerPathConfiguration(t *testing.T) {
	file, dir, values := configFixture(t)
	writeConfig(t, file, values)
	config, err := Load(file, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "runtime", "state", "intake-desk-numbers.jsonl")
	if config.LedgerFile != want || config.NumberLedgerPath() != want {
		t.Fatalf("ledger: %q %q", config.LedgerFile, config.NumberLedgerPath())
	}
	if config.SQLiteFile != filepath.Join(dir, "runtime", "state", "intake.sqlite") {
		t.Fatalf("legacy file: %q", config.SQLiteFile)
	}
	config.LedgerFile = "relative/./custom.jsonl"
	if got := config.NumberLedgerPath(); got != config.LedgerFile {
		t.Fatalf("priority: %q", got)
	}
	// The ledger path never falls back to the SQLite-era file name.
	config.LedgerFile = ""
	if got := config.NumberLedgerPath(); got != "" {
		t.Fatalf("ledger path was derived from %q: %q", config.SQLiteFile, got)
	}
}
