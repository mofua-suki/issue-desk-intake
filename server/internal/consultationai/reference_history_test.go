package consultationai

import (
	"context"
	"database/sql"
	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/references"
	"strings"
	"testing"
)

func TestUnreadClosedHistoryStopsReadyAndIsAskedOnlyOnce(t *testing.T) {
	calls := 0
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
		calls++
		v := reply()
		v["ready"], v["title"], v["body"], v["questions"] = true, "題名", "資料を確認した実装内容", []any{}
		return measuredAnswer(v), nil
	})
	config, _ := s.options.Config(context.Background())
	fleet.Object(config["intake"])["proxy"] = map[string]any{"enabled": true}
	s.options.ResolveNumbers = func(context.Context, Record) (any, error) {
		return []map[string]any{{"desk": float64(12), "resolution": "resolved", "historyUnread": true}}, nil
	}
	answer, e := s.Turn(context.Background(), d)
	if e != nil || calls != 2 || answer["ready"] != false || !strings.Contains(fleet.Text(answer["reply"]), references.HistoryUnreadNotice) || len(fleet.Elements(answer["questions"])) == 0 {
		t.Fatal(answer, e)
	}
	saved, _, e := s.options.Records.Get(context.Background(), "drafts", draftID)
	if e != nil {
		t.Fatal(e)
	}
	messages := fleet.Elements(saved["messages"])
	if len(fleet.Elements(fleet.Object(messages[0])["references"])) != 1 {
		t.Fatal("user発言に参照を保存しない", saved)
	}
	saved["messages"] = append(messages, map[string]any{"id": "u2", "role": "user", "text": "その依頼は仕様変更で閉じました。失敗ではありません。"})
	saved["revision"] = float64(1)
	saved["run"] = "run2"
	saved["status"] = "thinking"
	_, e = s.options.Records.Change(context.Background(), "drafts", draftID, func(*sql.Tx, Record) (Record, error) { return saved, nil })
	if e != nil {
		t.Fatal(e)
	}
	answer, e = s.Turn(context.Background(), saved)
	if e != nil || answer["ready"] != true {
		t.Fatal("返答後にも同じ経緯を聞き直した", answer, e)
	}
}

func TestUnreadHistoryQuestionRemainsControllerOwnedWhenAIAlreadyAskedIt(t *testing.T) {
	d := Record{"messages": []any{map[string]any{"role": "user", "references": []any{map[string]any{"desk": float64(12), "historyUnread": true}}}}}
	q := references.HistoryQuestion([]int64{12})
	answer := Record{"ready": true, "questions": []any{q}, "reply": "確認を終えました"}
	holdUnreadHistory(d, answer)
	if answer["ready"] != false || len(fleet.Elements(answer["questionReasons"])) != 1 || fleet.Object(fleet.Elements(answer["questionReasons"])[0])["controller"] != true {
		t.Fatal(answer)
	}
}
