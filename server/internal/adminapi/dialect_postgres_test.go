package adminapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/pgtest"
)

// PostgreSQL側の生成SQLを確かめる(SQLiteの展開は使わない)。
func TestDecisionSQLFragmentsPostgres(t *testing.T) {
	previous := appsql.SetActive(appsql.Postgres)
	defer appsql.SetActive(previous)
	where := decisionsWhere()
	if !strings.Contains(where, "removedAt") || !strings.Contains(where, "IS NULL") || strings.Contains(where, "json_extract") || strings.Count(where, "?") != 2 {
		t.Errorf("decisionsWhere = %q", where)
	}
	if got := decisionsOrder(); !strings.Contains(got, "source_rowid DESC") {
		t.Errorf("decisionsOrder = %q", got)
	}
	aiWhere := aiDecisionsWhere()
	if strings.Contains(aiWhere, "json_extract") || strings.Count(aiWhere, "?") != 4 {
		t.Errorf("aiDecisionsWhere = %q", aiWhere)
	}
	order := aiDecisionsOrder()
	if strings.Contains(order, "json_extract") || !strings.Contains(order, "COLLATE") || !strings.HasSuffix(order, "source_rowid ASC") {
		t.Errorf("aiDecisionsOrder = %q", order)
	}
}

// 実PostgreSQLで決定一覧のSQLが文法として通り、順序・絞り込みが正しいこと。
func TestDecisionListsPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := pgtest.New(t)
	previous := appsql.SetActive(appsql.Postgres)
	defer appsql.SetActive(previous)
	insert := func(table, id, raw string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, "INSERT INTO "+table+"(id,json) VALUES(?,?)", id, raw); err != nil {
			t.Fatal(err)
		}
	}
	insert("decisions", "d1", `{"id":"d1","at":"2026-01-04","removedAt":null,"project":null}`)
	insert("decisions", "d2", `{"id":"d2","at":"2026-01-03","removedAt":"2026-01-01T00:00:00Z","project":null}`)
	insert("decisions", "d3", `{"id":"d3","at":"2026-01-02","removedAt":null,"project":"a/b"}`)
	insert("decisions", "d4", `{"id":"d4","at":"2026-01-01","removedAt":null,"project":"c/d"}`)
	insert("ai_decisions", "x1", `{"id":"x1","at":"2026-01-02","kind":"k1","who":"w"}`)
	insert("ai_decisions", "x2", `{"id":"x2","at":"2026-01-01","kind":"k1","who":"w"}`)
	insert("ai_decisions", "B", `{"id":"B","at":"2026-01-01","kind":"k2","who":"w"}`)
	insert("ai_decisions", "a", `{"id":"a","at":"2026-01-01","kind":"k1","who":"v"}`)
	page := contract.Pagination{Page: 1, Limit: 10}
	ids := func(table, where, order string, args ...any) string {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		result, err := jsonPage(ctx, tx, table, where, order, args, page)
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, item := range result.Items {
			out = append(out, item["id"].(string))
		}
		return strings.Join(out, ",")
	}
	if got := ids("decisions", decisionsWhere(), decisionsOrder(), "a/b", "a/b"); got != "d3,d1" {
		t.Errorf("decisions = %s", got)
	}
	if got := ids("decisions", decisionsWhere(), decisionsOrder(), "", ""); got != "d4,d3,d1" {
		t.Errorf("decisions all = %s", got)
	}
	// "C"照合順はバイト順なので大文字Bが小文字aより前に来る。
	if got := ids("ai_decisions", aiDecisionsWhere(), aiDecisionsOrder(), "", "", "", ""); got != "B,a,x2,x1" {
		t.Errorf("ai_decisions = %s", got)
	}
	if got := ids("ai_decisions", aiDecisionsWhere(), aiDecisionsOrder(), "k1", "k1", "w", "w"); got != "x2,x1" {
		t.Errorf("ai_decisions filtered = %s", got)
	}
	// ID順・時刻順と異なる追加順も確認する。
	insert("decisions", "a0", `{"id":"a0","at":"2026-01-05","removedAt":null}`)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	items, err := jsonList(ctx, tx, "decisions")
	if err != nil {
		t.Fatal(err)
	}
	var listIDs []string
	for _, item := range items {
		listIDs = append(listIDs, item["id"].(string))
	}
	if got := strings.Join(listIDs, ","); got != "d1,d2,d3,d4,a0" {
		t.Errorf("jsonList = %s", got)
	}
}
