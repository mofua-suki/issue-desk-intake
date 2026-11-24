package pgtest

import (
	"testing"

	"issue-desk-intake/server/internal/appdb"
	"issue-desk-intake/server/internal/appsql"
)

func TestSchemaHasEveryProductionTable(t *testing.T) {
	db := New(t)
	want := appdb.TableNames()
	var got int
	if err := db.QueryRow("SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_type='BASE TABLE'").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != len(want) || got < 30 {
		t.Fatalf("表の数が本番スキーマと合いません: got %d want %d", got, len(want))
	}
	for _, name := range want {
		var exists bool
		if err := db.QueryRow("SELECT to_regclass(?) IS NOT NULL", name).Scan(&exists); err != nil || !exists {
			t.Errorf("表がありません: %s (%v)", name, err)
		}
	}
}

func TestConnectionLayerMatchesProduction(t *testing.T) {
	db := New(t)
	// Exercise placeholder binding through the production connection layer.
	for _, id := range []string{"b", "a", "c"} {
		if _, err := db.Exec("INSERT INTO settings(id,json) VALUES(?,?)", id, "{}"); err != nil {
			t.Fatal(err)
		}
	}
	ordering, err := appsql.Postgres.RowOrder(false)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query("SELECT id FROM settings ORDER BY " + ordering)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	order := ""
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		order += id
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if order != "bac" {
		t.Fatalf("挿入順になっていません: %q", order)
	}
	// The AUTOINCREMENT replacement: an identity column.
	var id1, id2 int64
	if err := db.QueryRow("INSERT INTO intake_error_failures(draft,gist,at,json) VALUES('d','g','t','{}') RETURNING id").Scan(&id1); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("INSERT INTO intake_error_failures(draft,gist,at,json) VALUES('d','g','t','{}') RETURNING id").Scan(&id2); err != nil || id2 <= id1 {
		t.Fatalf("identity が増えません: %d %d %v", id1, id2, err)
	}
}

func TestSchemasAreIsolatedAndParallelSafe(t *testing.T) {
	first, second := NewIsolated(t), NewIsolated(t)
	if first.Schema == second.Schema {
		t.Fatal("スキーマ名が衝突しています")
	}
	if _, err := first.DB.Exec("INSERT INTO settings(id,json) VALUES('x','{}')"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := second.DB.QueryRow("SELECT count(*) FROM settings").Scan(&n); err != nil || n != 0 {
		t.Fatalf("別スキーマの行が見えています: %d %v", n, err)
	}
	for i := 0; i < 4; i++ {
		t.Run("parallel", func(t *testing.T) {
			t.Parallel()
			db := New(t)
			if _, err := db.Exec("INSERT INTO settings(id,json) VALUES('p','{}')"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCleanupDropsTheSchema(t *testing.T) {
	var env Env
	t.Run("inner", func(t *testing.T) { env = NewIsolated(t) })
	var exists bool
	admin := NewIsolated(t)
	if err := admin.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=?)", env.Schema).Scan(&exists); err != nil || exists {
		t.Fatalf("スキーマが残っています: %v %v", exists, err)
	}
}
