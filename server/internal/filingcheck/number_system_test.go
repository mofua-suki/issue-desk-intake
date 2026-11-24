package filingcheck

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/pgtest"
)

func TestNumberSystemDatabaseSelection(t *testing.T) {
	ctx := context.Background()
	draft := Record{"project": "owner/repo", "body": "共通番号 42"}
	deps := []any{Record{"number": 42}}
	// This case also confirms that Postgres is not a fallback for the application DB.
	if err := (&Service{options: Options{Postgres: new(sql.DB)}}).numberSystem(ctx, draft, deps, nil); err != nil {
		t.Fatal(err)
	}
	good, bad := pgtest.New(t), pgtest.New(t)
	for _, f := range []struct {
		db      *sql.DB
		project string
		issue   int
	}{{good, "OWNER/REPO", 42}, {bad, "other/repo", 9}} {
		if _, err := f.db.ExecContext(ctx, "INSERT INTO desk_numbers(number,project,gitea,assigned_at) VALUES(?,?,?,?)", 42, f.project, f.issue, "now"); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name     string
		opts     Options
		mismatch bool
	}{
		{"matching", Options{AppDB: good}, false},
		{"mismatch", Options{AppDB: bad}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := (&Service{options: tc.opts}).numberSystem(ctx, draft, deps, nil)
			if tc.mismatch {
				if err == nil || !strings.Contains(err.Error(), "共通番号42") {
					t.Fatalf("expected mismatch, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNumberSystemMatchingAndErrors(t *testing.T) {
	ctx := context.Background()
	db := pgtest.New(t)
	if _, err := db.ExecContext(ctx, "INSERT INTO desk_numbers(number,project,gitea,assigned_at) VALUES(?,?,?,?),(?,?,?,?),(?,?,?,?)", 42, "owner/repo", 42, "now", 43, "other/repo", 43, "now", 44, "owner/repo", 2, "now"); err != nil {
		t.Fatal(err)
	}
	s := &Service{options: Options{AppDB: db}}
	for _, tc := range []struct {
		name, body string
		deps       []any
		material   Record
		mismatch   bool
	}{
		{"matching case insensitive", "共通番号42", []any{Record{"number": 42}}, nil, false},
		{"project mismatch", "共通番号43", []any{Record{"number": 43}}, nil, true},
		{"issue mismatch", "共通番号44", []any{Record{"number": 44}}, nil, true},
		{"unregistered", "共通番号45", []any{Record{"number": 45}}, nil, false},
		{"explicit dependency excluded", "共通番号43", []any{Record{"number": 43}}, Record{"target": Record{"dependsOnIssues": []any{Record{"number": 43}}}}, false},
		{"not referenced", "共通番号42", []any{Record{"number": 43}}, nil, false},
		{"multiple references", "共通番号42 と共通番号 43", []any{Record{"number": 42}, Record{"number": 43}}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := s.numberSystem(ctx, Record{"project": "OWNER/REPO", "body": tc.body}, tc.deps, tc.material)
			if tc.mismatch {
				if err == nil || !strings.Contains(err.Error(), "取り違え") {
					t.Fatalf("expected mismatch, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.numberSystem(ctx, Record{"project": "owner/repo", "body": "共通番号42"}, []any{Record{"number": 42}}, nil); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("expected database error, got %v", err)
	}
	// Filtering must happen before the database read, even on a failed connection.
	if err := s.numberSystem(ctx, Record{"body": "共通番号42"}, []any{Record{"number": 42}}, Record{"target": Record{"dependsOnIssues": []any{Record{"number": 42}}}}); err != nil {
		t.Fatal(err)
	}
}
