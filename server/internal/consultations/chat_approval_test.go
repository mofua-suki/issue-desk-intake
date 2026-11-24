package consultations

import (
	"context"
	"database/sql"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"testing"
)

var (
	delegatedPC = auth.Principal{Kind: auth.PC, Name: "nas", TokenID: "t2", Delegated: true}
	plainPC     = auth.Principal{Kind: auth.PC, Name: "pc", TokenID: "t1"}
)

// fixture の現在時刻（2026-11-01T03:00Z）の2分前に利用者がチャットで指示した日時。
const chatInstructionAt = "2026-11-01T11:58:00+09:00"

// 相談の続行（上限を足して続行する）は、運用の代行を許したトークンが利用者のチャットの承認の発言を添えて送れる。
// 発言の無い・定型語だけの続行、代行の無いトークン・一時キーは断り、上限を足さない。
func TestBudgetContinueAcceptsChatApprovalOnly(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	d := Record{"id": todoID, "project": "owner/app", "status": "draft", "consultationBudget": Record{"calls": 40, "weighted": 6000000, "stopped": Record{"at": "now"}, "stops": []any{Record{"at": "now"}}}}
	if _, err := s.options.Records.Change(ctx, "drafts", todoID, func(*sql.Tx, Record) (Record, error) { return d, nil }); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		p    auth.Principal
		data Record
	}{
		{auth.Principal{Kind: auth.CLI}, Record{"statement": "共通番号12の相談の続行を承認する"}},
		{plainPC, Record{"statement": "共通番号12の相談の続行を承認する"}},
		{delegatedPC, Record{}},
		{delegatedPC, Record{"statement": "どうぞよろしくお願いいたします", "instructionAt": chatInstructionAt}},
		{delegatedPC, Record{"statement": "共通番号12の相談の続行を承認する"}},
		{delegatedPC, Record{"statement": "共通番号12の相談の続行を承認する", "instructionAt": "2026-11-01T11:58:00"}},
		{delegatedPC, Record{"statement": "共通番号12の相談の続行を承認する", "instructionAt": "2026-11-01T12:01:00+09:00"}},
	} {
		c.data["operationId"], c.data["stopCount"] = "continue-1", 1
		if _, err := s.ContinueBudget(ctx, todoID, c.data, c.p); err == nil {
			t.Fatalf("利用者の発言の無い続行を受け付けた: %v %v", c.p, c.data)
		}
	}
	statement := "質問「共通番号12の相談を上限を足して続行しますか」への利用者の回答: A"
	saved, err := s.ContinueBudget(ctx, todoID, Record{"operationId": "continue-1", "stopCount": 1, "statement": statement, "instructionAt": chatInstructionAt}, delegatedPC)
	if err != nil {
		t.Fatal(err)
	}
	entries := fleet.Items(record(saved["consultationBudget"])["continuations"])
	if saved["status"] != "thinking" || len(entries) != 1 {
		t.Fatal(saved)
	}
	if e := record(entries[0]); e["via"] != "chat" || e["pc"] != "nas" || e["statement"] != statement || e["instructionAt"] != chatInstructionAt {
		t.Fatal("チャットの承認を記録していない", e)
	}
}

// 代理回答の取消は、運用の代行を許したトークンが利用者のチャットの承認の発言を添えて送れる。
func TestProxyUndoAcceptsChatApprovalOnly(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	d := Record{"id": todoID, "project": "owner/app", "status": "thinking", "run": "active", "revision": 2, "messages": []any{Record{"id": "u", "role": "user", "text": "元の依頼"}, Record{"id": "p", "role": "proxy", "advanced": true}}, "questions": []any{}}
	if _, err := s.options.Records.Change(ctx, "drafts", todoID, func(*sql.Tx, Record) (Record, error) { return d, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.options.Records.SaveProxy(ctx, todoID, "active", Record{}, []Record{{"id": sendID, "project": "owner/app", "draft": todoID, "messageId": "p", "at": "now", "snapshot": Record{"messageCount": 1, "questions": []any{"どちらか"}, "title": "元の題名", "body": "元の本文"}}}); err != nil {
		t.Fatal(err)
	}
	saved, _, err := s.options.Records.Get(ctx, "drafts", todoID)
	if err != nil {
		t.Fatal(err)
	}
	offered := func(p auth.Principal) bool {
		for _, a := range fleet.Items(s.DraftView(saved, p, "")["actions"]) {
			if a == "proxy_undo" {
				return true
			}
		}
		return false
	}
	if !offered(auth.Principal{Kind: auth.Screen}) || !offered(delegatedPC) || offered(plainPC) {
		t.Fatal("取消の操作の出し分けが認証と合わない")
	}
	for _, c := range []struct {
		p    auth.Principal
		data Record
	}{
		{auth.Principal{Kind: auth.CLI}, Record{"statement": "共通番号12の代理回答を取り消して"}},
		{plainPC, Record{"statement": "共通番号12の代理回答を取り消して"}},
		{delegatedPC, Record{}},
		{delegatedPC, Record{"statement": "よろしくお願いいたします！", "instructionAt": chatInstructionAt}},
		{delegatedPC, Record{"statement": "共通番号12の代理回答を取り消して"}},
		{delegatedPC, Record{"statement": "共通番号12の代理回答を取り消して", "instructionAt": "2026-10-25T11:59:00+09:00"}},
	} {
		c.data["messageId"] = "p"
		if _, err := s.UndoProxy(ctx, todoID, c.data, c.p); err == nil {
			t.Fatalf("利用者の発言の無い取消を受け付けた: %v %v", c.p, c.data)
		}
	}
	statement := "共通番号12の代理回答を取り消して、私が答え直す"
	out, err := s.UndoProxy(ctx, todoID, Record{"messageId": "p", "statement": statement, "instructionAt": chatInstructionAt}, delegatedPC)
	if err != nil {
		t.Fatal(err)
	}
	undos := fleet.Items(out["proxyUndos"])
	if out["status"] != "intake_waiting" || len(undos) != 1 || record(undos[0])["via"] != "chat" || record(undos[0])["statement"] != statement || record(undos[0])["instructionAt"] != chatInstructionAt {
		t.Fatal("チャットの取消を記録していない", out["status"], undos)
	}
}
