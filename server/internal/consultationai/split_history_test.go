package consultationai

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
)

func TestSplitChildrenLongHistoryStorageAndPrompt(t *testing.T) {
	ctx := context.Background()
	s, _ := testService(t, nil)
	history := make([]any, 100)
	for i := range history {
		history[i] = Record{"id": fmt.Sprintf("message-%d", i), "role": "user", "text": fmt.Sprintf("発言 %d: %s", i, strings.Repeat("内容", 500))}
	}
	parent := Record{"id": "11111111-1111-4111-8111-111111111111", "project": "owner/app", "owner": "owner", "_ephemeral": true, "messages": history}
	result, err := s.SplitChildren(ctx, parent, []string{"子A", "子B"})
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, id := range fleet.Elements(result["consultationChildren"]) {
		child, found, err := s.options.Records.Get(ctx, "drafts", fleet.Text(id))
		if err != nil || !found {
			t.Fatalf("child: %v %v", found, err)
		}
		messages := fleet.Elements(child["messages"])
		if len(messages) != len(history)+1 || string(marshal(messages[1:])) != string(marshal(history)) {
			t.Fatal("履歴が保存・再読込後に変化しました")
		}
		input := prompt(child, nil, nil, 0, nil, "")
		baseline := copyRecord(child)
		baseline["messages"] = messages[:1]
		delta := len(input) - len(prompt(baseline, nil, nil, 0, nil, ""))
		if delta != len(marshal(history))-1 {
			t.Fatalf("AI入力の履歴増分: %d", delta)
		}
		size := len(marshal(child))
		total += size
		t.Logf("child stored JSON=%d bytes, history=%d bytes, prompt=%d bytes, history prompt increase=%d bytes", size, len(marshal(history)), len(input), delta)
	}
	t.Logf("two children stored JSON total=%d bytes", total)
}

func TestSplitChildrenImageStatementsRetainScope(t *testing.T) {
	ctx := context.Background()
	s, _ := testService(t, nil)
	original := Record{"id": "approval", "role": "user", "text": "了承します", "at": "2026-11-07T10:00:00Z", "source": "cli-key", "images": []any{Record{"id": "33333333-3333-4333-8333-333333333333"}}}
	parent := Record{"id": "11111111-1111-4111-8111-111111111111", "project": "owner/app", "owner": "owner", "_ephemeral": true, "messages": []any{original}}
	result, err := s.SplitChildren(ctx, parent, []string{"子A", "子B"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range fleet.Elements(result["consultationChildren"]) {
		child, found, err := s.options.Records.Get(ctx, "drafts", fleet.Text(id))
		if err != nil || !found {
			t.Fatalf("child: %v %v", found, err)
		}
		msg := fleet.Object(fleet.Elements(child["messages"])[1])
		for _, key := range []string{"id", "role", "text", "at", "source", "images"} {
			if string(marshal(msg[key])) != string(marshal(original[key])) {
				t.Fatalf("%s changed: %v", key, msg)
			}
		}
		if fleet.Text(msg["imageScope"]) != "共通の添付画像です。対象はこの子の依頼文に限ります。" || !strings.Contains(prompt(child, nil, nil, 0, nil, ""), fleet.Text(msg["imageScope"])) {
			t.Fatal("AI入力に画像の対象範囲がありません")
		}
	}
	if original["imageScope"] != nil {
		t.Fatal("親の発言が変更されました")
	}
}
