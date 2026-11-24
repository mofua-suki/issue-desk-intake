package deskcleanup

import (
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
)

func TestArchivedDetailTimestampDoesNotChangeCause(t *testing.T) {
	for _, times := range [][2]string{
		{"2026-11-01 19:00", "2026-11-01 19:03"},
		{"2026-11-01T10:00:17.180Z", "2026-11-01T10:03:17.169Z"},
		{"2026-11-01T19:00:17+09:00", "2026-11-01T19:03:17+09:00"},
	} {
		t.Run(times[0], func(t *testing.T) {
			f := setup(t)
			before := Object{"id": "hostd-silent-x1-pro1", "detail": "PC: x1-pro1\nheartbeat: " + times[0] + "\n最後の周期: 未受信"}
			raw, _ := json.Marshal([]Object{{"target": "stall:hostd-silent-x1-pro1", "fingerprint": Fingerprint(before), "snapshot": before}})
			if _, e := f.s.DB.Exec(`INSERT INTO settings(id,json)VALUES('desk-cleanup-archive',?)`, string(raw)); e != nil {
				t.Fatal(e)
			}
			after := Object{"id": before["id"], "detail": "PC: x1-pro1\nheartbeat: " + times[1] + "\n最後の周期: 未受信"}
			visible, e := VisibleAlerts(t.Context(), f.s.DB, "stall", []any{after})
			if e != nil || len(visible) != 0 {
				t.Fatal("時刻だけの更新で記録を無効にした", visible, e)
			}
			after["detail"] = after["detail"].(string) + "\n原因が変わりました"
			visible, e = VisibleAlerts(t.Context(), f.s.DB, "stall", []any{after})
			if e != nil || len(visible) != 1 {
				t.Fatal("原因の変更を隠した", visible, e)
			}
		})
	}
}

func TestLegacySystemProjectArchiveMatchesOnlySameAlert(t *testing.T) {
	for _, snapshot := range []bool{false, true} {
		t.Run(fmt.Sprint("snapshot=", snapshot), func(t *testing.T) {
			f := setup(t)
			old := Object{"id": "one", "project": fleet.LegacySystemProject, "detail": "cause"}
			record := Object{"target": "stall:one", "fingerprint": AlertFingerprint(old)}
			if snapshot {
				record["snapshot"] = old
			}
			raw, _ := json.Marshal([]Object{record})
			if _, err := f.s.DB.Exec(`INSERT INTO settings(id,json)VALUES('desk-cleanup-archive',?)`, string(raw)); err != nil {
				t.Fatal(err)
			}
			current := Object{"id": "one", "project": fleet.SystemProject, "detail": "cause"}
			other := Object{"id": "two", "project": fleet.SystemProject, "detail": "cause"}
			changed := Object{"id": "one", "project": fleet.SystemProject, "detail": "changed"}
			unrelated := Object{"id": "one", "project": "owner/repo", "detail": "cause"}
			visible, err := VisibleAlerts(t.Context(), f.s.DB, "stall", []any{current, other, changed, unrelated})
			if err != nil || len(visible) != 3 {
				t.Fatalf("visible=%v error=%v", visible, err)
			}
			if current["project"] != fleet.SystemProject {
				t.Fatal("input mutated")
			}
		})
	}
}

func TestElapsedDetailFingerprintAndVisibility(t *testing.T) {
	examples := map[string]struct{ before, after, changed string }{
		"urgent_fix_hold":    {"状態: 保留 / -1分停止\n保留・失敗の理由: 原因A", "状態: 保留 / 31分停止\n保留・失敗の理由: 原因A", "状態: 保留 / 31分停止\n保留・失敗の理由: 原因B"},
		"job_dispatch_stall": {"実行待ち: -1分（担当PCは待ち始めより後に別のジョブを受け取っています）\n直近の見送り: 原因A（5分前） / 取得PC: nas / 2026-11-01T10:00:00Z（-1分前）\n過去の予約による見送り / 予約元: ジョブID: 2 / 見送り記録時の待ち時間: 30分", "実行待ち: 61分（担当PCは待ち始めより後に別のジョブを受け取っています）\n直近の見送り: 原因A（5分前） / 取得PC: nas / 2026-11-01T10:00:00Z（11分前）\n過去の予約による見送り / 予約元: ジョブID: 2 / 見送り記録時の待ち時間: 31分", "実行待ち: 61分（担当PCは待ち始めより後に別のジョブを受け取っています）\n直近の見送り: 原因A（6分前） / 取得PC: nas / 2026-11-01T10:00:00Z（11分前）\n過去の予約による見送り / 予約元: ジョブID: 3 / 見送り記録時の待ち時間: 31分"},
	}
	if len(examples) != len(elapsedDetailRules) {
		t.Fatal("通知の対応表と検証例の種類が一致しません")
	}
	for kind, rules := range elapsedDetailRules {
		t.Run(kind, func(t *testing.T) {
			tc, ok := examples[kind]
			if !ok {
				t.Fatal("検証例がありません")
			}
			for _, rule := range rules {
				before, after := rule.pattern.FindString(tc.before), rule.pattern.FindString(tc.after)
				if before == "" || after == "" || before == after || rule.pattern.ReplaceAllString(before, rule.replacement) != rule.pattern.ReplaceAllString(after, rule.replacement) {
					t.Fatalf("経過時間の置換を検証できません: %s", rule.pattern)
				}
			}
			before := Object{"id": "one", "kind": kind, "detail": tc.before}
			after := Object{"id": "one", "kind": kind, "detail": tc.after}
			changed := Object{"id": "one", "kind": kind, "detail": tc.changed}
			if AlertFingerprint(before) != AlertFingerprint(after) || AlertFingerprint(before) == AlertFingerprint(changed) {
				t.Fatal("fingerprint mismatch")
			}
			f := setup(t)
			raw, _ := json.Marshal([]Object{{"target": "stall:one", "snapshot": before}})
			if _, err := f.s.DB.Exec(`INSERT INTO settings(id,json)VALUES('desk-cleanup-archive',?)`, string(raw)); err != nil {
				t.Fatal(err)
			}
			visible, err := VisibleAlerts(t.Context(), f.s.DB, "stall", []any{after, changed})
			if err != nil || len(visible) != 1 || fleet.Object(visible[0])["detail"] != tc.changed {
				t.Fatal(visible, err)
			}
		})
	}
}

func TestReservationSkipArchiveCauseChanges(t *testing.T) {
	f := setup(t)
	base := "実行待ち: 60分\n直近の見送り: scope_reserved / 取得PC: worker / 2026-11-01T10:00:00Z（1分前）\n過去の予約による見送り / 予約元: ジョブID: 2 / リポジトリ: owner/repo / Gitea Issue: #22 / 題名: 件名 / 見送り記録時の待ち時間: 40分"
	old := Object{"id": "one", "kind": "job_dispatch_stall", "detail": base}
	raw, _ := json.Marshal([]Object{{"target": "stall:one", "snapshot": old, "fingerprint": "old-format"}})
	if _, err := f.s.DB.Exec(`INSERT INTO settings(id,json)VALUES('desk-cleanup-archive',?)`, string(raw)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		detail string
		count  int
	}{
		{strings.ReplaceAll(strings.ReplaceAll(base, "60分", "61分"), "40分", "41分"), 0},
		{strings.Replace(base, "ジョブID: 2", "ジョブID: 3", 1), 1},
		{strings.Replace(base, "scope_reserved", "overlap", 1), 1},
	} {
		visible, err := VisibleAlerts(t.Context(), f.s.DB, "stall", []any{Object{"id": "one", "kind": "job_dispatch_stall", "detail": tc.detail}})
		if err != nil || len(visible) != tc.count {
			t.Fatal(visible, err)
		}
	}
}
