package consultations

import (
	"context"
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestCoreSQLHasNoDialectSpecificSyntax(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)\b(INSERT\s+OR\s+IGNORE|rowid|AUTOINCREMENT|BLOB)\b`)
	destructive := regexp.MustCompile(`(?i)\b(DELETE|DROP|TRUNCATE)\b`)
	// Exact original deletion lines and counts are checked independently of SQL literals.
	allowedLines := map[string]map[string]int{
		"todo-start.go": {
			"\t\tif _, err := tx.ExecContext(ctx, \"DELETE FROM images WHERE scope=?\", \"todo:\"+id); err != nil {": 1,
		},
		"todos.go": {
			"\t\t\t\tif _, err := tx.ExecContext(ctx, \"DELETE FROM images WHERE id=? AND scope=?\", text(i[\"id\"]), \"todo:\"+id); err != nil {": 1,
		},
		"todo-routes.go": {
			"\t\tif _, err := tx.ExecContext(ctx, \"DELETE FROM images WHERE scope=?\", \"todo:\"+id); err != nil {": 1,
		},
		"error-stalls.go": {
			"\t\tif _, err := s.service.options.Records.DB.ExecContext(ctx, \"DELETE FROM intake_error_failures WHERE gist=?\", gist); err != nil {":                          1,
			"\tif _, err := s.service.options.Records.DB.ExecContext(ctx, \"DELETE FROM intake_error_failures WHERE at<?\", timestamp(now.Add(-24*time.Hour))); err != nil {": 1,
			"\t\tif _, err := s.service.options.Records.DB.ExecContext(ctx, \"DELETE FROM intake_error_failures WHERE gist=?\", f[\"gist\"]); err != nil {":                   1,
		},
		"withdrawal.go": {
			"\t\tif _, err = tx.ExecContext(ctx, \"DELETE FROM images WHERE scope=?\", \"draft:\"+id); err != nil {":  1,
			"\t\tif _, err := tx.ExecContext(ctx, \"DELETE FROM images WHERE scope=?\", \"draft:\"+id); err != nil {": 1,
		},
		"split-family.go": {
			"\t\t\t\tif _, err := tx.ExecContext(ctx, \"DELETE FROM drafts WHERE id=?\", c[\"id\"]); err != nil {":                     1,
			"\t\t\t\tif _, err := tx.ExecContext(ctx, \"DELETE FROM images WHERE scope=?\", \"draft:\"+text(c[\"id\"])); err != nil {": 1,
			"\t\t\t_, err := tx.ExecContext(ctx, \"DELETE FROM images WHERE scope=?\", \"draft:\"+id)":                                 1,
		},
	}
	allowedDelete := "DELETE FROM "
	for _, file := range []string{"records.go", "receipts.go", "engine-receipts.go", "drafts.go", "error-stalls.go", "withdrawal.go", "split-family.go", "draft-routes.go", "proxy-records.go", "todo-start.go", "todos.go", "todo-routes.go", "proxy-undo.go", "draft_edit.go", "screen_views.go", "list-family.go"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for line, count := range allowedLines[file] {
			if got := strings.Count(string(raw), line+"\n"); got != count {
				t.Errorf("%s: 削除行の出現数 %d、期待 %d", file, got, count)
			}
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, raw, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if literal, ok := n.(*ast.BasicLit); ok && literal.Kind == token.STRING {
				text, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatal(err)
				}
				if forbidden.MatchString(text) {
					t.Errorf("%s: SQL部品を通らない方言固有構文", file)
				}
				line := strings.Split(string(raw), "\n")[fset.Position(literal.Pos()).Line-1]
				if destructive.MatchString(text) && !(file == "records.go" && text == allowedDelete) && allowedLines[file][line] != 1 {
					t.Errorf("%s: 許可されていない削除文の追加・変更", file)
				}
			}
			return true
		})
	}
}

func TestRecordsChangeAndListOnPostgres(t *testing.T) {
	ctx := context.Background()
	sqlite := openPostgresStore(t)
	records, err := OpenRecords(ctx, sqlite.DB())
	if err != nil {
		t.Fatal(err)
	}
	set := func(value string) Change {
		return func(_ *sql.Tx, old Record) (Record, error) {
			next := clone(old)
			next["v"] = value
			return next, nil
		}
	}
	for _, id := range []string{"a", "b", "c"} {
		if _, err := records.Change(ctx, "todos", id, set(id)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := records.Change(ctx, "todos", "a", set("a2")); err != nil {
		t.Fatal(err)
	}
	list, err := records.List(ctx, "todos")
	if err != nil || len(list) != 3 || list[0]["v"] != "c" || list[2]["v"] != "a2" {
		t.Fatalf("新しい順の一覧が違います: %v %v", list, err)
	}
	// A concurrent update between read and write is refused as a conflict.
	_, err = records.Change(ctx, "todos", "b", func(tx *sql.Tx, old Record) (Record, error) {
		if _, err := tx.ExecContext(ctx, "UPDATE todos SET json=? WHERE id=?", `{"v":"other"}`, "b"); err != nil {
			return nil, err
		}
		return set("b2")(tx, old)
	})
	if err == nil || problem(err).Status != 409 {
		t.Fatalf("競合が拒否されていません: %v", err)
	}
	if _, err := records.Change(ctx, "todos", "c", func(*sql.Tx, Record) (Record, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := records.Get(ctx, "todos", "c"); ok {
		t.Fatal("削除されていません")
	}
	var kind string
	if err := sqlite.DB().QueryRowContext(ctx, "SELECT data_type FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='images' AND column_name='bytes'").Scan(&kind); err != nil || kind != "bytea" {
		t.Fatalf("images.bytes: %q %v", kind, err)
	}
}
