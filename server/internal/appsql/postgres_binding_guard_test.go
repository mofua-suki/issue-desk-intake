package appsql

import (
	"testing"

	"github.com/jackc/pgx/v5/stdlib"
)

// pgxの実体 *stdlib.Conn が包み込みの要求する形を満たすこと（実機PG無しで確かめる）。
var _ PostgresConn = (*stdlib.Conn)(nil)

func TestBindPostgresQueryPreservesRowID(t *testing.T) {
	// Comparison input only; PostgreSQL does not execute this SQL.
	input := "SELECT rowid, t.ROWID FROM t WHERE a=? AND b=?"
	want := "SELECT rowid, t.ROWID FROM t WHERE a=$1 AND b=$2"
	got, err := BindPostgresQuery(input)
	if err != nil || got != want {
		t.Fatalf("got %q, %v; want %q", got, err, want)
	}
}

func TestBindPostgresQuery(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "SELECT a FROM t WHERE x=? AND y=?", "SELECT a FROM t WHERE x=$1 AND y=$2"},
		{"quoted", "SELECT '?' , \"a?\" FROM t WHERE x=?", "SELECT '?' , \"a?\" FROM t WHERE x=$1"},
		{"escaped quote", "SELECT 'it''s ?' WHERE x=?", "SELECT 'it''s ?' WHERE x=$1"},
		{"line comment", "SELECT 1 -- why?\nWHERE x=?", "SELECT 1 -- why?\nWHERE x=$1"},
		{"block comment", "SELECT /* ? */ x WHERE y=?", "SELECT /* ? */ x WHERE y=$1"},
		{"dollar quote", "SELECT $q$?$q$ WHERE y=?", "SELECT $q$?$q$ WHERE y=$1"},
		{"already numbered", "SELECT a WHERE x=$1 AND y='?'", "SELECT a WHERE x=$1 AND y='?'"},
		{"multiple statements", "DELETE FROM a WHERE x=?; DELETE FROM b WHERE y=?", "DELETE FROM a WHERE x=$1; DELETE FROM b WHERE y=$2"},
		{"no placeholder", "SELECT 1", "SELECT 1"},
		{"source row order", "SELECT json FROM t ORDER BY source_rowid", "SELECT json FROM t ORDER BY source_rowid"},
		{"source row with placeholder", "SELECT json FROM t WHERE x=? ORDER BY source_rowid DESC", "SELECT json FROM t WHERE x=$1 ORDER BY source_rowid DESC"},
		// Keep this comparison when PostgresRowID is removed: these rowid occurrences must remain unchanged.
		{"rowid in quotes and long identifiers", "SELECT 'rowid', \"rowid\", old_rowid, source_rowid FROM t -- rowid", "SELECT 'rowid', \"rowid\", old_rowid, source_rowid FROM t -- rowid"},
		{"rowid in quotes and long identifiers with placeholder", "SELECT 'rowid', \"rowid\", old_rowid, source_rowid FROM t WHERE x=? -- rowid", "SELECT 'rowid', \"rowid\", old_rowid, source_rowid FROM t WHERE x=$1 -- rowid"},
	}
	for _, c := range cases {
		got, err := BindPostgresQuery(c.in)
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, c.want)
		}
		again, err := BindPostgresQuery(got)
		if err != nil || again != got {
			t.Errorf("%s: not idempotent: %q, %v", c.name, again, err)
		}
	}
	if _, err := BindPostgresQuery("SELECT $1 WHERE x=?"); err == nil {
		t.Error("mixed parameter styles must fail closed")
	}
	if _, err := BindPostgresQuery("SELECT 'open ?"); err == nil {
		t.Error("unterminated quote must fail closed")
	}
}
