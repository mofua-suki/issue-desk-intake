package consultationai

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/operations"
	"issue-desk-intake/server/internal/statistics"
	"issue-desk-intake/server/internal/store"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBudgetArchiveUsesLegacyMeaningThroughActualAttentionCaller(t *testing.T) {
	s, d := testService(t, callFunc(func(context.Context, ai.Request) (ai.Answer, error) {
		t.Fatal("通知読取でAIを呼びました")
		return ai.Answer{}, nil
	}))
	s.options.Config = func(context.Context) (map[string]any, error) {
		return map[string]any{"intake": map[string]any{"publicUrl": "https://desk.example.invalid/"}}, nil
	}
	stopped := Record{"at": "2026-10-01T00:00:00.000Z", "calls": 40, "weighted": 6000000, "limits": Record{"calls": 40, "weighted": 6000000}, "questions": []string{"資料の確認", "依存の確認"}, "reply": "保存済みの返答"}
	snapshots := []any{}
	for i, status := range []string{"submitted", "closed", "closed"} {
		id := fmt.Sprintf("%08d-1111-4111-8111-111111111111", i+1)
		value := copyRecord(d)
		value["id"] = id
		value["status"] = status
		value["title"] = "保存済みの相談"
		value["updatedAt"] = "2026-10-01T00:00:01.000Z"
		value["consultationBudget"] = Record{"stopped": stopped, "stops": []any{stopped}}
		if i == 1 {
			value["consultationChildren"] = []string{"00000003-1111-4111-8111-111111111111"}
		}
		if i == 2 {
			value["parentSplit"] = "00000002-1111-4111-8111-111111111111"
		}
		if _, err := s.options.Records.Change(t.Context(), "drafts", id, func(*sql.Tx, Record) (Record, error) { return value, nil }); err != nil {
			t.Fatal(err)
		}
		// Fixed legacy projection, intentionally independent from the new formatter and fingerprint implementation.
		reason := "相談ID: " + id + "。回数 40 / 上限 40、量 6000000 / 上限 6000000。最後の未確認事項: 資料の確認、依存の確認。保存済みの返答\n相談の呼び出しの上限に達したため停止しました。利用者が受付画面の要対応から続行を選ぶまで受付AIを呼びません。返信・再試行では再開できません。\n受付画面でこの相談を開き、「上限を足して続行する」を選んでください: https://desk.example.invalid/#/drafts/" + id
		old := map[string]any{"key": "consultation-budget:" + id, "kind": "consultation_budget", "project": "owner/app", "draftId": id, "consultationId": id, "consultationUrl": "https://desk.example.invalid/#/drafts/" + id, "consultationUrlReason": nil, "stopCount": 1, "budgetStop": stopped, "title": "保存済みの相談", "state": "waiting", "stateLabel": "相談の呼び出し上限で停止", "startedAt": stopped["at"], "updatedAt": value["updatedAt"], "reason": reason}
		snapshots = append(snapshots, map[string]any{"target": "attention:attention:consultation-budget:" + id, "fingerprint": "legacy-json-order-hash", "snapshot": old, "reason": "利用者が終了した通知", "at": "2026-10-01T00:00:02.000Z"})
	}
	db := s.options.Records.DB
	archive, _ := json.Marshal(snapshots)
	if _, err := db.Exec("INSERT INTO settings(id,json)VALUES('desk-cleanup-archive',?)", string(archive)); err != nil {
		t.Fatal(err)
	}
	before := budgetAlertRawRecords(t, db)
	raw, err := s.DraftAlerts(t.Context())
	if err != nil || len(raw) != 3 {
		t.Fatal(raw, err)
	}
	for _, v := range raw {
		row, ok := v.(map[string]any)
		if !ok {
			t.Fatal("共通archivefilterが読めないnamed map", v)
		}
		if !strings.Contains(row["reason"].(string), "資料の確認、依存の確認") {
			t.Fatal(row)
		}
	}
	ops := &operations.Service{DB: db, PostgreSQL: budgetAlertPostgres(t), DraftAlerts: s.DraftAlerts, Now: s.options.Now, Repositories: func() ([]string, error) { return []string{"owner/app"}, nil }}
	ops.Statistics = &statistics.Service{DB: ops.PostgreSQL, Now: s.options.Now}
	rows, err := ops.Attention(t.Context())
	if err != nil || len(rows) != 0 {
		t.Fatal("同じ終了通知を再表示しました", rows, err)
	}
	if after := budgetAlertRawRecords(t, db); !reflect.DeepEqual(before, after) {
		t.Fatal("読取がclosed family/archive原文を書き換えました")
	}
	id := "00000001-1111-4111-8111-111111111111"
	if _, err := s.options.Records.Change(t.Context(), "drafts", id, func(_ *sql.Tx, saved Record) (Record, error) {
		next := copyRecord(saved)
		budget := fleet.Object(next["consultationBudget"])
		stop := fleet.Object(budget["stopped"])
		stop["reply"] = "別の未確認事項が見つかりました"
		return next, nil
	}); err != nil {
		t.Fatal(err)
	}
	rows, err = ops.Attention(t.Context())
	if err != nil || len(rows) != 1 {
		t.Fatal("元の停止内容の変化を隠しました", rows, err)
	}
	var retained string
	if err := db.QueryRow("SELECT json FROM settings WHERE id='desk-cleanup-archive'").Scan(&retained); err != nil || retained != string(archive) {
		t.Fatal("archiveを失いました", err)
	}
}
func budgetAlertRawRecords(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, table := range []string{"drafts", "settings", "intake_error_stalls"} {
		var exists bool
		if err := db.QueryRow("SELECT to_regclass(?) IS NOT NULL", table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			continue
		}
		rows, err := db.Query("SELECT id,json FROM " + table)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, raw string
			if err := rows.Scan(&id, &raw); err != nil {
				t.Fatal(err)
			}
			out[table+":"+id] = raw
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	return out
}
func TestBudgetAlertLinksKeepLegacyMissingAndUnsafeReasons(t *testing.T) {
	for _, test := range []struct {
		id          string
		base        any
		url, reason string
	}{
		{draftID, "https://desk.example.invalid", "https://desk.example.invalid/#/drafts/" + draftID, ""},
		{draftID, nil, "", "受付の公開URLが設定されていません。"},
		{draftID, "", "", "受付の公開URLが設定されていません。"},
		{"bad", nil, "", "相談IDが受付画面の形式と一致しません。"},
		{draftID, "https://user:password@desk.example.invalid", "", "受付の公開URLを安全に使用できません。"},
		{draftID, "https://desk.example.invalid/?", "", "受付の公開URLを安全に使用できません。"},
		{draftID, "https://desk.example.invalid/#", "", "受付の公開URLを安全に使用できません。"},
		{draftID, "https://desk.example.invalid/\u0000", "", "受付の公開URLを安全に使用できません。"},
	} {
		got := consultationBudgetLinks(test.id, test.base)
		if fleet.Text(got["consultationUrl"]) != test.url || fleet.Text(got["consultationUrlReason"]) != test.reason {
			t.Fatal(test, got)
		}
	}
	if got := budgetAlertNumber(json.Number("6e6")); got != "6000000" {
		t.Fatal("JS数値文字列と異なります", got)
	}
}

func budgetAlertPostgres(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("隔離PostgreSQL DSNがありません")
	}
	u, err := url.Parse(dsn)
	if err != nil || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "issue_desk_test") {
		t.Fatal("専用試験DBが必要です")
	}
	admin, err := store.OpenPostgres(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("budget_archive_%d", time.Now().UnixNano())
	if _, err = admin.Exec("CREATE SCHEMA " + schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	var db *sql.DB
	t.Cleanup(func() {
		if db != nil {
			db.Close()
		}
		if _, err := admin.Exec("DROP SCHEMA " + schema + " CASCADE"); err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err = store.OpenPostgres(t.Context(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestErrorStallArchiveUsesPlainMapThroughActualStallsCaller(t *testing.T) {
	s, _ := testService(t, callFunc(func(context.Context, ai.Request) (ai.Answer, error) {
		t.Fatal("通知読取でAIを呼びました")
		return ai.Answer{}, nil
	}))
	consultationsService, err := consultations.New(consultations.Options{Records: s.options.Records, Auth: &auth.Authenticator{}, Project: func(string) error { return nil }, Now: s.options.Now})
	if err != nil {
		t.Fatal(err)
	}
	stalls, err := consultations.NewErrorStalls(t.Context(), consultationsService, "fixture", s.options.RuntimeDir)
	if err != nil {
		t.Fatal(err)
	}
	db := s.options.Records.DB
	row := map[string]any{"id": "saved-error", "project": "owner/app", "gist": "保存済み例外", "autoFixStopped": true, "reason": "確認済みの原因"}
	raw, _ := json.Marshal(row)
	if _, err := db.Exec("INSERT INTO intake_error_stalls(id,json)VALUES('saved-error',?)", string(raw)); err != nil {
		t.Fatal(err)
	}
	old := map[string]any{}
	for k, v := range row {
		old[k] = v
	}
	old["kind"] = "intake_error"
	old["status"] = "intake_error_stopped"
	old["title"] = "相談処理の例外: 保存済み例外"
	archive, _ := json.Marshal([]any{map[string]any{"target": "stall:saved-error", "snapshot": old, "fingerprint": "legacy-order-hash"}})
	if _, err := db.Exec("INSERT INTO settings(id,json)VALUES('desk-cleanup-archive',?)", string(archive)); err != nil {
		t.Fatal(err)
	}
	before := budgetAlertRawRecords(t, db)
	produced, err := stalls.Alerts(t.Context())
	if err != nil || len(produced) != 1 {
		t.Fatal(produced, err)
	}
	if _, ok := produced[0].(map[string]any); !ok {
		t.Fatal("共通archivefilterが読めないnamed map", produced)
	}
	ops := &operations.Service{DB: db, PostgreSQL: budgetAlertPostgres(t), DraftStallAlerts: stalls.Alerts, Now: s.options.Now, RuntimeDir: s.options.RuntimeDir, Repositories: func() ([]string, error) { return []string{"owner/app"}, nil }}
	visible, err := ops.Stalls(t.Context())
	if err != nil || len(visible) != 0 {
		t.Fatal("終了済み相談例外を再表示", visible, err)
	}
	if !reflect.DeepEqual(before, budgetAlertRawRecords(t, db)) {
		t.Fatal("読取で保存原文を書き換えました")
	}
	row["reason"] = "元の状態が変わった別の原因"
	raw, _ = json.Marshal(row)
	if _, err := db.Exec("UPDATE intake_error_stalls SET json=? WHERE id='saved-error'", string(raw)); err != nil {
		t.Fatal(err)
	}
	visible, err = ops.Stalls(t.Context())
	if err != nil || len(visible) != 1 {
		t.Fatal("相談例外の原因変化を隠しました", visible, err)
	}
}
