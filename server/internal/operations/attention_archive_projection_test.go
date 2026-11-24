package operations

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"issue-desk-intake/server/internal/deskcleanup"
	"issue-desk-intake/server/internal/fleet"
)

func TestSavedAttentionPublicLinksPreserveArchivedSnapshots(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	s.PublicLink = func(repo, kind string, number int64) string {
		return fmt.Sprintf("https://git.example/%s/%s/%d", repo, kind, number)
	}
	original := `{"key":"owner/repo#1","kind":"failed","state":"notify","project":"owner/repo","number":1,"title":"保存済み","reason":"判断が必要","gist":"同じ原因","streak":1,"investigation":{"project":"owner/repo","number":2}}`
	if _, e := s.DB.Exec("INSERT INTO attention_cases(key,json) VALUES(?,?)", "owner/repo#1", original); e != nil {
		t.Fatal(e)
	}
	rows, e := s.savedAttention(ctx, map[string]bool{"owner/repo": true})
	if e != nil || len(rows) != 1 {
		t.Fatal(rows, e)
	}
	snapshot := fleet.Object(rows[0])
	if snapshot["url"] != "https://git.example/owner/repo/issues/1" || fleet.Object(snapshot["investigation"])["url"] != "https://git.example/owner/repo/issues/2" {
		t.Fatal(snapshot)
	}
	// 旧publicLinkで保存されたsnapshotをそのまま次の版でも照合する。
	raw, _ := json.Marshal([]any{map[string]any{"target": "attention:attention:owner/repo#1", "snapshot": snapshot, "fingerprint": deskcleanup.AlertFingerprint(snapshot)}})
	if _, e := s.DB.Exec("INSERT INTO settings(id,json) VALUES('desk-cleanup-archive',?)", string(raw)); e != nil {
		t.Fatal(e)
	}
	visible, e := deskcleanup.VisibleAlerts(ctx, s.DB, "attention", rows)
	if e != nil || len(visible) != 0 {
		t.Fatal("終了済みの通知が復活しました", visible, e)
	}
	changed := fleet.Object(rows[0])
	changed["gist"] = "原因が変わった"
	visible, e = deskcleanup.VisibleAlerts(ctx, s.DB, "attention", []any{changed})
	if e != nil || len(visible) != 1 {
		t.Fatal("原因が変わった通知まで隠しました", visible, e)
	}
	var after string
	if e = s.DB.QueryRow("SELECT json FROM attention_cases WHERE key=?", "owner/repo#1").Scan(&after); e != nil || after != original {
		t.Fatal("保存記録が変わりました", e)
	}
	var archiveAfter string
	if e = s.DB.QueryRow("SELECT json FROM settings WHERE id='desk-cleanup-archive'").Scan(&archiveAfter); e != nil || archiveAfter != string(raw) {
		t.Fatal("終了記録が変わりました", e)
	}
}
