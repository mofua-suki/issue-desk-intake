package consultations

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/pgtest"
)

func TestSelectedDraftsPostgres(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	r, err := OpenRecords(ctx, env.DB)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 200)
	for i := range ids {
		ids[i] = fmt.Sprintf("key-%03d", i)
		raw := fmt.Sprintf(`{"id":"duplicate","body":"full-%03d"}`, i)
		switch i {
		case 0:
			raw = `{"body":"missing-id"}`
		case 1:
			raw = `{"id":42,"body":"number-id"}`
		case 2:
			raw = `{"id":"different","body":"different-id"} trailing`
		}
		if _, err := r.DB.ExecContext(ctx, bindSQL("INSERT INTO drafts(id,json) VALUES(?,?)"), ids[i], raw); err != nil {
			t.Fatal(err)
		}
	}
	config, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = env.Schema
	counts := &readCounts{}
	db := sql.OpenDB(countConnector{appsql.NewPostgresBindingConnector(stdlib.GetConnector(*config)), counts})
	defer db.Close()
	reader := &Records{DB: db}
	for _, n := range []int{0, 1, 100, 200} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			selection := append([]string(nil), ids[:n]...)
			for _, keyed := range []bool{false, true} {
				*counts = readCounts{}
				if keyed {
					got, err := reader.GetDraftEntries(ctx, selection)
					if err != nil || got == nil || len(got) != n {
						t.Fatalf("entries: %v %v", got, err)
					}
				} else {
					got, err := reader.GetDrafts(ctx, selection)
					if err != nil || got == nil || len(got) != n {
						t.Fatalf("drafts: %v %v", got, err)
					}
				}
				want := 1
				if n == 0 {
					want = 0
				}
				if counts.calls != want {
					t.Fatalf("keyed=%v calls=%d want=%d", keyed, counts.calls, want)
				}
			}
		})
	}
	// Compare against List's actual order and decode, independently of JSON IDs.
	all, err := r.List(ctx, "drafts")
	if err != nil {
		t.Fatal(err)
	}
	selection := append(append([]string{}, ids...), ids[0], "absent")
	entries, err := reader.GetDraftEntries(ctx, selection)
	if err != nil {
		t.Fatal(err)
	}
	drafts, err := reader.GetDrafts(ctx, selection)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(all) || !reflect.DeepEqual(drafts, all) {
		t.Fatal("selection differs from List")
	}
	for i, e := range entries {
		if e.Key != ids[len(ids)-1-i] || !reflect.DeepEqual(e.Record, all[i]) {
			t.Fatalf("entry %d: %#v", i, e)
		}
	}
	var raw string
	if err := r.DB.QueryRowContext(ctx, bindSQL("SELECT json FROM drafts WHERE id=?"), ids[2]).Scan(&raw); err != nil || raw != `{"id":"different","body":"different-id"} trailing` {
		t.Fatalf("stored JSON changed: %q %v", raw, err)
	}
	if _, err := r.DB.ExecContext(ctx, bindSQL("DELETE FROM drafts WHERE id=?"), ids[0]); err != nil {
		t.Fatal(err)
	}
	for _, keyed := range []bool{false, true} {
		if keyed {
			got, err := reader.GetDraftEntries(ctx, []string{ids[0], "absent", ids[1], ids[1]})
			if err != nil || len(got) != 1 || got[0].Key != ids[1] {
				t.Fatalf("deleted entries: %v %v", got, err)
			}
		} else {
			got, err := reader.GetDrafts(ctx, []string{ids[0], "absent", ids[1], ids[1]})
			if err != nil || len(got) != 1 || got[0]["id"] == nil || got[0]["body"] != "number-id" {
				t.Fatalf("deleted drafts: %v %v", got, err)
			}
		}
	}
	if _, err := r.DB.ExecContext(ctx, bindSQL("UPDATE drafts SET json=? WHERE id=?"), "{", ids[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.GetDrafts(ctx, []string{ids[1]}); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	if _, err := reader.GetDraftEntries(ctx, []string{ids[1]}); err == nil {
		t.Fatal("invalid JSON accepted with key")
	}
}

func TestSelectedDraftsFailures(t *testing.T) {
	registerReadFailureDriver.Do(func() { sql.Register("draft-read-failures", readFailureDriver{}) })
	for _, mode := range []string{"query", "scan", "decode", "rows"} {
		t.Run(mode, func(t *testing.T) {
			db, err := sql.Open("draft-read-failures", mode)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			r := &Records{DB: db}
			_, a := r.GetDrafts(context.Background(), []string{"a"})
			_, b := r.GetDraftEntries(context.Background(), []string{"a"})
			if a == nil || b == nil {
				t.Fatalf("failure hidden: %v %v", a, b)
			}
		})
	}
}
