package consultations

import (
	"context"
	"database/sql"
	"issue-desk-intake/server/internal/fleet"
	"testing"

	"issue-desk-intake/server/internal/auth"
)

func TestGroupExceptionChatApproval(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	screen := auth.Principal{Kind: auth.Screen}
	_, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, screen, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, d Record) (Record, error) {
		d["owner"] = "token:t1"
		d["crossProposal"] = Record{"fingerprint": "proposal", "scopes": []any{Record{"project": "owner/other", "scope": "除外: 変更なし"}}}
		d["messages"] = []any{Record{"role": "assistant", "crossProposal": "proposal"}}
		return d, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		p    auth.Principal
		data Record
	}{
		{plainPC, Record{"text": "了承します", "approvalFingerprint": "proposal"}},
		{delegatedPC, Record{"text": "了承します", "approvalFingerprint": "proposal"}},
		{delegatedPC, Record{"text": "了承します", "statement": "対象の除外を承認します", "approvalFingerprint": "proposal"}},
		{delegatedPC, Record{"text": "了承します", "statement": "owner/otherの除外を承認します", "instructionAt": chatInstructionAt, "approvalFingerprint": "old"}},
		{delegatedPC, Record{"text": "了承します", "statement": "owner/otherの除外を承認します", "instructionAt": "2026-10-01T00:00:00Z", "approvalFingerprint": "proposal"}},
	} {
		if _, err = s.Chat(ctx, todoID, tc.data, tc.p); err == nil {
			t.Fatal("不完全な了承を受理")
		}
		if tc.p == plainPC && problem(err).Status != 403 {
			t.Fatalf("非代行PCの拒否: %v", err)
		}
	}
	data := Record{"text": "了承します", "statement": "owner/otherの除外を承認します", "instructionAt": chatInstructionAt, "approvalFingerprint": "proposal"}
	if _, err = s.Chat(ctx, todoID, data, delegatedPC); err != nil {
		t.Fatal(err)
	}
	saved, _, err := s.options.Records.Get(ctx, "drafts", todoID)
	if err != nil {
		t.Fatal(err)
	}
	msgs := fleet.Items(saved["messages"])
	receipt := record(record(msgs[len(msgs)-1])["groupExceptionApproval"])
	if receipt["via"] != "chat" || receipt["pc"] != "nas" || receipt["statement"] != data["statement"] || receipt["instructionAt"] != data["instructionAt"] {
		t.Fatal(receipt)
	}
}

func TestGroupExceptionScreenApproval(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	screen := auth.Principal{Kind: auth.Screen}
	if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, screen, false); err != nil {
		t.Fatal(err)
	}
	_, err := s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, d Record) (Record, error) {
		d["crossProposal"] = Record{"fingerprint": "proposal", "scopes": []any{Record{"project": "owner/other", "scope": "除外: 変更なし"}}}
		d["messages"] = []any{Record{"role": "assistant", "crossProposal": "proposal"}}
		return d, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Chat(ctx, todoID, Record{"text": "了承します"}, screen); err == nil {
		t.Fatal("会話版なしを受理")
	}
	if _, err = s.Chat(ctx, todoID, Record{"text": "了承します", "expectedRevision": 1}, screen); err == nil {
		t.Fatal("古い会話版を受理")
	}
	if _, err = s.Chat(ctx, todoID, Record{"text": "了承します", "expectedRevision": 0}, screen); err != nil {
		t.Fatal(err)
	}
	saved, _, err := s.options.Records.Get(ctx, "drafts", todoID)
	if err != nil {
		t.Fatal(err)
	}
	msgs := fleet.Items(saved["messages"])
	receipt := record(record(msgs[len(msgs)-1])["groupExceptionApproval"])
	if receipt["via"] != "screen" || receipt["fingerprint"] != "proposal" {
		t.Fatal(receipt)
	}
}

func TestGroupChangeChatApproval(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	if _, err := s.CreateDraft(ctx, Record{"tier": "light", "id": todoID, "project": "owner/app"}, auth.Principal{Kind: auth.Screen}, false); err != nil {
		t.Fatal(err)
	}
	_, err := s.options.Records.Change(ctx, "drafts", todoID, func(_ *sql.Tx, d Record) (Record, error) {
		d["owner"] = "token:t1"
		d["crossProposal"] = Record{"fingerprint": "group", "repositoryGroup": true, "scopes": []any{Record{"project": "owner/app", "scope": "実装"}, Record{"project": "owner/other", "scope": "連携"}}}
		d["messages"] = []any{Record{"role": "assistant", "crossProposal": "group"}}
		return d, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Chat(ctx, todoID, Record{"text": "了承します", "approvalFingerprint": "group"}, plainPC); err == nil || problem(err).Status != 403 {
		t.Fatalf("非代行PC: %v", err)
	}
	if _, err = s.Chat(ctx, todoID, Record{"text": "了承します", "approvalFingerprint": "group"}, delegatedPC); err == nil {
		t.Fatal("利用者の発言なしを受理")
	}
	data := Record{"text": "了承します", "approvalFingerprint": "group", "statement": "owner/appとowner/otherの変更を承認します", "instructionAt": chatInstructionAt}
	if _, err = s.Chat(ctx, todoID, data, delegatedPC); err != nil {
		t.Fatal(err)
	}
	saved, _, err := s.options.Records.Get(ctx, "drafts", todoID)
	if err != nil {
		t.Fatal(err)
	}
	messages := fleet.Items(saved["messages"])
	receipt := record(record(messages[len(messages)-1])["groupExceptionApproval"])
	if receipt["pc"] != "nas" || receipt["statement"] != data["statement"] || receipt["fingerprint"] != "group" {
		t.Fatal(receipt)
	}
}
