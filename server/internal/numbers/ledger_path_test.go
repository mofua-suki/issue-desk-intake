package numbers

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"issue-desk-intake/server/internal/pgtest"
)

func TestOpenLedgerAssignsAndRecoversFromExplicitPath(t *testing.T) {
	ctx := context.Background()
	db := pgtest.New(t)
	path := filepath.Join(t.TempDir(), "custom-ledger.jsonl")
	first, err := OpenLedger(ctx, db, path)
	if err != nil {
		t.Fatal(err)
	}
	lists := []List{{Project: "owner/repo", Issues: []Candidate{{Number: 7, CreatedAt: "2026-01-01"}}}}
	if ok, err := first.Assign(ctx, lists, []string{"owner/repo"}, true); err != nil || !ok {
		t.Fatalf("assign: %v %v", ok, err)
	}
	number, found, err := first.Number(ctx, "owner/repo", 7)
	if err != nil || !found || number != 8 {
		t.Fatalf("number: %d %v %v", number, found, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("ledger was not written to the supplied path: %v", err)
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM desk_numbers"); err != nil {
		t.Fatal(err)
	}
	// A later start with the same path restores the number from the ledger.
	second, err := OpenLedger(ctx, db, path)
	if err != nil {
		t.Fatal(err)
	}
	check, err := second.Reconcile(ctx)
	if err != nil || len(check.Restored) != 1 || check.Restored[0] != number {
		t.Fatalf("reconcile: %+v %v", check, err)
	}
	got, found, err := second.Number(ctx, "owner/repo", 7)
	if err != nil || !found || got != number {
		t.Fatalf("recovered: %d %v %v", got, found, err)
	}
}

func TestOpenLedgerPathAndFailures(t *testing.T) {
	ctx := context.Background()
	if _, err := OpenLedger(ctx, nil, "journal"); err == nil {
		t.Fatal("nil DB accepted")
	}
	db := pgtest.New(t)
	if _, err := OpenLedger(ctx, db, ""); err == nil {
		t.Fatal("empty journal accepted")
	}
	dir := t.TempDir()
	path := dir + "/./journal.jsonl"
	s, err := OpenLedger(ctx, db, path)
	if err != nil || s.Journal != path {
		t.Fatalf("path changed: %q %v", s.Journal, err)
	}
	// A directory cannot be read as a journal or opened for append.
	s.Journal = dir
	if _, err := s.Reconcile(ctx); err == nil {
		t.Fatal("journal read failure ignored")
	}
	s.Journal = filepath.Join(dir, "missing", "journal")
	if _, err := s.Assign(ctx, []List{{Project: "owner/repo", Issues: []Candidate{{Number: 1}}}}, []string{"owner/repo"}, true); err == nil {
		t.Fatal("journal write failure ignored")
	}
	if _, found, err := s.Number(ctx, "owner/repo", 1); err != nil || found {
		t.Fatalf("failed assignment persisted: %v %v", found, err)
	}
	if _, err := os.Stat(s.Journal); !os.IsNotExist(err) {
		t.Fatalf("unexpected journal: %v", err)
	}
}
