package consultationai

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/models"
)

func TestTurnRejectsConversationProjectionShortfall(t *testing.T) {
	calls := 0
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
		calls++
		return measuredAnswer(reply()), nil
	})
	d["messages"] = []any{Record{"role": "user", "text": strings.Repeat("超", 80001), "references": []any{Record{"history": strings.Repeat("参", 1000)}}}}
	view, omitted, err := conversationInput(d)
	var shortfall *consultations.ProjectionShortfall
	if view != nil || omitted || !errors.As(err, &shortfall) || shortfall.Limit != 80000 || shortfall.Required <= shortfall.Limit || shortfall.Shortfall != shortfall.Required-shortfall.Limit || shortfall.Messages <= 0 || shortfall.Omitted <= 0 {
		t.Fatalf("投影不足: view=%v omitted=%v error=%v", view, omitted, err)
	}
	_, err = s.Turn(context.Background(), d)
	if !errors.As(err, &shortfall) || calls != 0 {
		t.Fatalf("Turn error=%v calls=%d", err, calls)
	}
}

func TestTurnOmitsMismatchedTopLevelReferences(t *testing.T) {
	var requests []ai.Request
	s, d := testService(t, func(_ context.Context, req ai.Request) (ai.Answer, error) {
		requests = append(requests, req)
		return measuredAnswer(reply()), nil
	})
	const secret = "OMITTED_UNIQUE_REFERENCE_BODY"
	refs := []any{Record{"history": secret + strings.Repeat("参", 12000)}, Record{"history": "kept"}}
	d["messages"] = []any{Record{"role": "user", "text": strings.Repeat("依", 70000), "references": refs}}
	d["references"] = []any{refs[0], Record{"history": "different"}}
	ctx := context.Background()
	if _, err := s.options.Records.Change(ctx, "drafts", draftID, func(*sql.Tx, Record) (Record, error) { return d, nil }); err != nil {
		t.Fatal(err)
	}
	before, found, err := s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil || !found {
		t.Fatalf("保存読取: %v", err)
	}
	original := string(marshal(d))
	if _, err := s.Turn(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	after, found, err := s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil || !found || !reflect.DeepEqual(before["messages"], after["messages"]) || !reflect.DeepEqual(before["references"], after["references"]) || string(marshal(d)) != original {
		t.Fatalf("原本が変更された: %v", err)
	}
	if len(requests) == 0 {
		t.Fatal("AI 呼出しがない")
	}
	for _, request := range requests {
		if strings.Contains(request.Input, secret) {
			t.Fatal("省略本文が AI 入力に残った")
		}
	}
	_, payload, ok := strings.Cut(requests[0].Input, "以下は相談と資料のデータです:\n")
	if !ok {
		t.Fatal("相談入力がない")
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(payload), &sent); err != nil {
		t.Fatal(err)
	}
	last := fleet.Object(fleet.Elements(sent["messages"])[0])
	if len(fleet.Elements(sent["references"])) != 0 || len(fleet.Elements(last["references"])) != 1 {
		t.Fatalf("参照が不一致: top=%v message=%v", sent["references"], last["references"])
	}
}

func TestConversationInputPreservesUnprojectedRecord(t *testing.T) {
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) { return measuredAnswer(reply()), nil })
	d["messages"] = []any{Record{"role": "user", "text": "依頼", "references": []any{Record{"history": "資料"}}}}
	d["references"] = []any{Record{"history": "別資料"}}
	ctx := context.Background()
	if _, err := s.options.Records.Change(ctx, "drafts", draftID, func(*sql.Tx, Record) (Record, error) { return d, nil }); err != nil {
		t.Fatal(err)
	}
	before, found, err := s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil || !found {
		t.Fatalf("保存読取: %v", err)
	}
	inputBefore := string(marshal(d))
	view, omitted, err := conversationInput(d)
	if err != nil || omitted || !reflect.DeepEqual(view, d) || string(marshal(d)) != inputBefore {
		t.Fatalf("省略なし: %v", err)
	}
	after, found, err := s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil || !found || !reflect.DeepEqual(before["messages"], after["messages"]) || !reflect.DeepEqual(before["references"], after["references"]) {
		t.Fatalf("保存原本が変更された: %v", err)
	}
}

func TestTurnDeliversOmittedConversationReferences(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var requests []ai.Request
	runner := callFunc(func(_ context.Context, req ai.Request) (ai.Answer, error) {
		requests = append(requests, req)
		value := reply()
		if len(requests) == 1 {
			value["readFiles"] = []any{42}
		}
		return measuredAnswer(value), nil
	})
	schemaDir, err := filepath.Abs(filepath.Join("..", "..", "schemas"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Config: func(context.Context) (map[string]any, error) { return map[string]any{}, nil }, Models: &models.Service{}, Runner: runner, Gitea: &gitea.Client{}, Records: &consultations.Records{}, RuntimeDir: t.TempDir(), SchemaDir: schemaDir, Now: func() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	s.resolve = func(context.Context, map[string]any, map[string]any, string) (models.Settings, error) {
		return models.Settings{Provider: "claude", Model: "fixture", Effort: "high"}, nil
	}
	history := strings.Repeat("旧", 1500)
	refs := []any{}
	for i := 1; i <= 4; i++ {
		refs = append(refs, Record{"desk": i, "project": "owner/repo", "number": i + 100, "resolution": "resolved", "history": history})
	}
	messages := []any{}
	for i := 0; i < 10; i++ {
		m := Record{"id": fmt.Sprintf("u%d", i), "role": "user", "text": strings.Repeat("依", 7000)}
		if i == 0 {
			m["references"] = refs
		}
		messages = append(messages, m)
	}
	messages = append(messages, Record{"id": "latest", "role": "user", "text": strings.Repeat("追", 6000)})
	d := Record{"id": draftID, "status": "thinking", "run": "run", "_ephemeral": true, "tier": Record{"level": "fixed"}, "messages": messages}
	s.options.ResolveNumbers = func(_ context.Context, draft Record) (any, error) {
		first := fleet.Object(fleet.Elements(draft["messages"])[0])
		resolved := make([]map[string]any, len(refs))
		for i, ref := range refs {
			resolved[i] = fleet.Object(ref)
		}
		first["references"] = resolved
		return []map[string]any{{"desk": 10, "resolution": "resolved", "history": "最新の参照"}}, nil
	}
	_, err = s.Turn(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) < 2 || !requests[1].Resume {
		t.Fatalf("初回と再開の AI 呼び出しがない: %d", len(requests))
	}
	input := requests[0].Input
	const marker = "以下は相談と資料のデータです:\n"
	_, payload, found := strings.Cut(input, marker)
	if !found {
		t.Fatal("相談入力がない")
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(payload), &sent); err != nil {
		t.Fatal(err)
	}
	delivered := fleet.Elements(sent["messages"])
	kept := fleet.Elements(fleet.Object(delivered[0])["references"])
	if len(kept) != 2 || fleet.Number(fleet.Object(kept[0])["desk"]) != 3 || fleet.Text(fleet.Object(fleet.Elements(fleet.Object(delivered[10])["references"])[0])["history"]) != "最新の参照" {
		t.Fatalf("送信参照の件数または内容が不正: %d", len(kept))
	}
	omissions := fleet.Object(sent["conversationOmissions"])
	positions := fleet.Elements(omissions["positions"])
	if len(positions) != 2 || fleet.Object(positions[0])["messageIndex"] != float64(0) || fleet.Object(positions[0])["referenceIndex"] != float64(0) || fleet.Object(positions[0])["unconfirmed"] != true || fleet.Object(positions[1])["messageIndex"] != float64(0) || fleet.Object(positions[1])["referenceIndex"] != float64(1) || fleet.Object(positions[1])["unconfirmed"] != true {
		t.Fatalf("省略位置: %v", omissions)
	}
	if !strings.Contains(fleet.Text(omissions["meaning"]), "未確認") || !strings.Contains(fleet.Text(omissions["meaning"]), "未配信") {
		t.Fatalf("省略の意味: %v", omissions)
	}
	_, resumed, found := strings.Cut(requests[1].Input, "\n")
	if !found {
		t.Fatal("再開入力がない")
	}
	var next map[string]any
	if err := json.Unmarshal([]byte(resumed), &next); err != nil {
		t.Fatal(err)
	}
	continued := fleet.Elements(fleet.Object(next["conversationOmissions"])["positions"])
	for _, positions := range [][]any{positions, continued} {
		for i, raw := range positions {
			source := fleet.Object(fleet.Object(raw)["source"])
			if fleet.Number(source["desk"]) != float64(i+1) || fleet.Number(source["number"]) != float64(i+101) || source["project"] != "owner/repo" || source["incomplete"] != false || source["history"] != nil || source["version"] != nil {
				t.Fatalf("出典: %v", source)
			}
		}
	}
	if count := strings.Count(requests[0].Input, history); count != 2 {
		t.Fatalf("送信された経緯の件数: %d、期待値: 2", count)
	}
	if len(continued) != 2 || fleet.Object(continued[0])["unconfirmed"] != true || fleet.Number(fleet.Object(continued[1])["referenceIndex"]) != 1 {
		t.Fatal("再開入力に未確認位置がない")
	}
	if fleet.Text(fleet.Object(fleet.Elements(fleet.Object(fleet.Elements(d["messages"])[0])["references"])[0])["history"]) != history {
		t.Fatal("元の会話が変更された")
	}

	// The resolver also delivers the last message's references at top level.
	// When they are omitted from the message, they must not reappear there.
	requests = nil
	delete(fleet.Object(messages[0]), "references")
	s.options.ResolveNumbers = func(context.Context, Record) (any, error) {
		current := make([]map[string]any, len(refs))
		for i, ref := range refs {
			current[i] = fleet.Object(ref)
		}
		return current, nil
	}
	if _, err := s.Turn(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	_, payload, found = strings.Cut(requests[0].Input, marker)
	if !found {
		t.Fatal("2回目の相談入力がない")
	}
	sent = nil
	if err := json.Unmarshal([]byte(payload), &sent); err != nil {
		t.Fatal(err)
	}
	lastRefs := fleet.Elements(fleet.Object(fleet.Elements(sent["messages"])[10])["references"])
	topRefs := fleet.Elements(sent["references"])
	if len(lastRefs) != 2 || len(topRefs) != 0 || fleet.Number(fleet.Object(lastRefs[0])["desk"]) != 3 {
		t.Fatalf("省略済みの参照が再送された: message=%d top=%d", len(lastRefs), len(topRefs))
	}
	lastOmissions := fleet.Elements(fleet.Object(sent["conversationOmissions"])["positions"])
	if len(lastOmissions) != 2 || fleet.Object(lastOmissions[0])["messageIndex"] != float64(10) || fleet.Object(lastOmissions[0])["unconfirmed"] != true {
		t.Fatal("最新発言の省略が未確認として伝わらない")
	}
}
