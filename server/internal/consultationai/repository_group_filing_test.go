package consultationai

import (
	"context"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/filing"
)

func TestRepositoryGroupExceptionApprovalReachesFiling(t *testing.T) {
	ctx := context.Background()
	s := &Service{options: Options{Now: time.Now}}
	m := &materials{allowed: map[string]bool{"owner/repo": true, "owner/other": true}}
	draft := Record{"_ephemeral": true, "project": "owner/repo"}
	cross := Record{"reason": "連携", "scopes": []any{Record{"project": "owner/repo", "scope": "実装"}, Record{"project": "owner/other", "scope": "除外理由: 変更不要 / 条件: 契約が変わらない"}}}
	answer := Record{"crossProject": cross, "splitIssues": []any{}}
	if err := s.crossApproval(ctx, draft, answer, m); err != nil {
		t.Fatal(err)
	}
	if len(fleet.Elements(answer["questions"])) == 0 {
		t.Fatal("了承を求めなかった")
	}
	fp := fleet.Text(fleet.Object(answer["crossProposal"])["fingerprint"])
	cross["approvalMessageId"] = "user1"
	draft["messages"] = []any{Record{"role": "assistant", "crossProposal": fp}, Record{"role": "user", "id": "user1", "text": "了承します", "groupExceptionApproval": Record{"fingerprint": fp, "via": "screen"}}}
	fleet.Object(fleet.Elements(draft["messages"])[1])["text"] = "了承しません"
	answer = Record{"crossProject": cross, "splitIssues": []any{}}
	if err := s.crossApproval(ctx, draft, answer, m); err != nil {
		t.Fatal(err)
	}
	if len(fleet.Elements(answer["crossApprovals"])) != 0 {
		t.Fatal("否定を了承として保存した")
	}
	fleet.Object(fleet.Elements(draft["messages"])[1])["text"] = "了承します"
	answer = Record{"crossProject": cross, "splitIssues": []any{}}
	if err := s.crossApproval(ctx, draft, answer, m); err != nil {
		t.Fatal(err)
	}
	if len(fleet.Elements(answer["crossApprovals"])) != 1 {
		t.Fatal("了承が保存されなかった")
	}
	receipt := fleet.Object(fleet.Object(fleet.Elements(answer["crossApprovals"])[0])["exceptionApproval"])
	if fleet.Text(receipt["fingerprint"]) != fp || receipt["via"] != "screen" {
		t.Fatal("了承の出典が失われた", receipt)
	}
	doc := func() Record {
		return Record{"revision": strings.Repeat("a", 40), "files": []any{Record{"path": "README.md", "bytes": 1, "sha256": strings.Repeat("b", 64)}}}
	}
	ev := doc()
	ev["projects"] = Record{"owner/other": doc()}
	draft["context"] = ev
	draft["changeScope"] = Record{"known": true, "paths": []any{"src/a"}}
	raw, _ := json.Marshal(draft)
	var restored filing.Record
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	c := filing.BuildConfig{Config: map[string]any{"assignments": map[string]any{"repositoryGroups": []any{[]any{"owner/repo", "owner/other"}}}}, Project: func(string) error { return nil }}
	if err := filing.ValidateSplit(ctx, c, restored); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryGroupConsultationExceptionToFiling(t *testing.T) {
	for _, split := range []bool{false, true} {
		s, d, m := groupFixture(t, "owner/a", "")
		groupRead(t, m, "owner/a")
		makeAnswer := func() Record {
			a := groupReady()
			a["splitIssues"] = []any{}
			if split {
				a["splitIssues"] = []any{Record{"project": "owner/a", "title": "実装", "purpose": "変更", "acceptance": []any{"動く"}, "dependsOn": []any{}, "changeScope": Record{"known": true, "paths": []any{"a.go"}}}}
			}
			a["crossProject"] = Record{"reason": "契約を整合する", "scopes": []any{Record{"project": "owner/a", "scope": "API変更"}, Record{"project": "owner/b", "scope": "除外: 対応不要の条件"}}}
			return a
		}
		first := makeAnswer()
		if err := s.crossApproval(context.Background(), d, first, m); err != nil {
			t.Fatal(err)
		}
		fp := fleet.Text(fleet.Object(first["crossProposal"])["fingerprint"])
		d["messages"] = []any{Record{"role": "assistant", "crossProposal": fp}, Record{"role": "user", "id": "yes", "text": "了承します", "groupExceptionApproval": Record{"fingerprint": fp, "via": "screen"}}}
		second := makeAnswer()
		fleet.Object(second["crossProject"])["approvalMessageId"] = "yes"
		if err := s.crossApproval(context.Background(), d, second, m); err != nil {
			t.Fatal(err)
		}
		if second["ready"] != true {
			t.Fatalf("split=%v: %+v", split, second)
		}
		if err := s.finalize(context.Background(), d, second, m); err != nil {
			t.Fatal(err)
		}
		d["splitIssues"] = second["splitIssues"]
		d["changeScope"] = Record{"known": true, "paths": []any{"a.go"}}
		raw, _ := json.Marshal(d)
		var saved filing.Record
		if err := json.Unmarshal(raw, &saved); err != nil {
			t.Fatal(err)
		}
		c := filing.BuildConfig{Config: map[string]any{"assignments": map[string]any{"repositoryGroups": []any{[]any{"owner/a", "owner/b"}}}}, Project: func(string) error { return nil }}
		if err := filing.ValidateSplit(context.Background(), c, saved); err != nil {
			t.Fatalf("split=%v: %v", split, err)
		}
		fleet.Object(saved["crossProposal"])["reason"] = "違う理由"
		if filing.ValidateSplit(context.Background(), c, saved) == nil {
			t.Fatal("変更された理由を受理")
		}
	}
}

func TestRepositoryGroupConsultationApprovalToFiling(t *testing.T) {
	s, d, m := groupFixture(t, "owner/a", "")
	groupRead(t, m, "owner/a")
	makeAnswer := func() Record {
		a := groupReady()
		a["splitIssues"] = []any{
			Record{"project": "owner/a", "title": "変更", "purpose": "実装", "acceptance": []any{"動く"}, "dependsOn": []any{}, "changeScope": Record{"known": true, "paths": []any{"a.go"}}},
			Record{"project": "owner/b", "title": "確認", "purpose": "契約", "acceptance": []any{"動く"}, "dependsOn": []any{}, "changeScope": Record{"known": true, "paths": []any{"b.go"}}},
		}
		return a
	}
	first := makeAnswer()
	if err := s.crossApproval(context.Background(), d, first, m); err != nil {
		t.Fatal(err)
	}
	fp := fleet.Text(fleet.Object(first["crossProposal"])["fingerprint"])
	d["messages"] = []any{Record{"role": "assistant", "crossProposal": fp}, Record{"role": "user", "id": "yes", "text": "了承します", "groupExceptionApproval": Record{"fingerprint": fp, "via": "screen"}}}
	second := makeAnswer()
	fleet.Object(second["crossProject"])["approvalMessageId"] = "yes"
	if err := s.crossApproval(context.Background(), d, second, m); err != nil {
		t.Fatal(err)
	}
	if second["ready"] != true {
		t.Fatal("了承済みの案が保留", second)
	}
	if err := s.finalize(context.Background(), d, second, m); err != nil {
		t.Fatal(err)
	}
	d["splitIssues"] = second["splitIssues"]
	raw, _ := json.Marshal(d)
	var saved filing.Record
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	c := filing.BuildConfig{Config: map[string]any{"assignments": map[string]any{"repositoryGroups": []any{[]any{"owner/a", "owner/b"}}}}, Project: func(string) error { return nil }}
	if err := filing.ValidateSplit(context.Background(), c, saved); err != nil {
		t.Fatal(err)
	}
	fleet.Object(saved["crossProposal"])["reason"] = "違う理由"
	if filing.ValidateSplit(context.Background(), c, saved) == nil {
		t.Fatal("変更された理由を受理")
	}
}

func TestRepositoryGroupInvalidReceiptDoesNotShowApproved(t *testing.T) {
	s, d, m := groupFixture(t, "owner/a", "")
	groupRead(t, m, "owner/a")
	first := groupReady()
	if err := s.crossApproval(context.Background(), d, first, m); err != nil {
		t.Fatal(err)
	}
	proposal := fleet.Object(first["crossProposal"])
	fp := fleet.Text(proposal["fingerprint"])
	d["crossApprovals"] = []any{Record{"fingerprint": fp, "scopes": proposal["scopes"], "exceptionApproval": Record{"fingerprint": fp, "via": "chat", "pc": "pc"}}}
	next := groupReady()
	if err := s.crossApproval(context.Background(), d, next, m); err != nil {
		t.Fatal(err)
	}
	if next["ready"] != false || fleet.Object(next["crossProposal"])["approved"] != false {
		t.Fatal("不完全な出典を了承済みと表示", next)
	}
}
