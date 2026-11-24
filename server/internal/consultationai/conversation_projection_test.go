package consultationai

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/models"
)

func TestConversationProjectionChatToPrompt(t *testing.T) {
	ctx := context.Background()
	var inputs []string
	s, d := testService(t, func(_ context.Context, req ai.Request) (ai.Answer, error) {
		inputs = append(inputs, req.Input)
		return measuredAnswer(reply()), nil
	})
	refs := make([]any, 40)
	for i := range refs {
		refs[i] = Record{"desk": i + 1, "project": "owner/app", "version": fmt.Sprintf("v%d", i), "history": strings.Repeat("参", 1600)}
	}
	messages := []any{}
	for i := 0; i < 5; i++ {
		messages = append(messages, Record{"role": "user", "id": fmt.Sprintf("original-%d", i), "text": strings.Repeat("発", 7000), "at": "2026-11-01T00:00:00Z", "source": "screen-session", "references": refs[i*8 : (i+1)*8]})
	}
	d["messages"], d["references"], d["status"] = messages, refs[32:], "error"
	_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, _ Record) (Record, error) { return d, nil })
	if err != nil {
		t.Fatal(err)
	}
	before, found, err := s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil || !found {
		t.Fatalf("read: %v", err)
	}
	if contract.UTF16Length(string(marshal(before["messages"]))) <= 90000 {
		t.Fatal("fixture too small")
	}
	cs, err := consultations.New(consultations.Options{Records: s.options.Records, Auth: &auth.Authenticator{}, Now: s.options.Now, Project: func(string) error { return nil }, Start: func(_ context.Context, _ string, d consultations.Record, _ auth.Principal) (consultations.Record, error) {
		return d, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	p := auth.Principal{Kind: auth.Screen}
	queued, err := cs.Chat(ctx, draftID, Record{"id": "22222222-2222-4222-8222-222222222222", "text": "短い補完"}, p)
	if err != nil {
		t.Fatal(err)
	}
	if queued["id"] != draftID {
		t.Fatalf("id=%v", queued["id"])
	}
	if _, err = s.Turn(ctx, queued); err != nil {
		t.Fatal(err)
	}
	if len(inputs) == 0 {
		t.Fatal("AI input missing")
	}
	_, payload, ok := strings.Cut(inputs[0], "以下は相談と資料のデータです:\n")
	if !ok {
		t.Fatal("prompt missing")
	}
	var sent map[string]any
	if err = json.Unmarshal([]byte(payload), &sent); err != nil {
		t.Fatal(err)
	}
	projection := Record{"messages": sent["messages"], "omitted": fleet.Object(sent["conversationOmissions"])["positions"]}
	if contract.UTF16Length(string(marshal(projection))) > 80000 || !strings.Contains(payload, "短い補完") || len(fleet.Elements(fleet.Object(sent["conversationOmissions"])["positions"])) == 0 {
		t.Fatal("projection or supplement missing")
	}
	after, err := cs.Draft(ctx, draftID, p)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, found, err := s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil || !found {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fleet.Elements(after["messages"])[:len(messages)], before["messages"]) || !reflect.DeepEqual(after["messages"], reloaded["messages"]) {
		t.Fatal("original records changed")
	}
}

func TestConversationProjectionFailures(t *testing.T) {
	ctx := context.Background()
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) { return measuredAnswer(reply()), nil })
	d["status"] = "error"
	d["messages"] = []any{Record{"role": "user", "text": strings.Repeat("あ", 79900)}}
	_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, _ Record) (Record, error) { return d, nil })
	if err != nil {
		t.Fatal(err)
	}
	before, found, err := s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil || !found {
		t.Fatal(err)
	}
	cs, err := consultations.New(consultations.Options{Records: s.options.Records, Auth: &auth.Authenticator{}, Now: s.options.Now, Project: func(string) error { return nil }, Start: func(_ context.Context, _ string, d consultations.Record, _ auth.Principal) (consultations.Record, error) {
		return d, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	p := auth.Principal{Kind: auth.Screen}
	if _, err = cs.Chat(ctx, draftID, Record{"text": strings.Repeat("追", 200)}, p); err == nil {
		t.Fatal("post-append projection overflow accepted")
	}
	stored, found, err := s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil || !found || !reflect.DeepEqual(stored["messages"], before["messages"]) {
		t.Fatal("rejected message changed records")
	}
	if _, err = cs.Chat(ctx, draftID, Record{"text": strings.Repeat("追", 8001)}, p); err == nil {
		t.Fatal("overlong message accepted")
	}
	_, err = s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, current Record) (Record, error) {
		current["messages"] = []any{}
		return current, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cs.Chat(ctx, draftID, Record{"text": strings.Repeat("追", 8000)}, p); err != nil {
		t.Fatalf("8000文字の発言を拒否: %v", err)
	}
}

func TestConversationProjectionChatFences(t *testing.T) {
	ctx := context.Background()
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) { return measuredAnswer(reply()), nil })
	d["status"] = "error"
	d["owner"] = "pc:owner"
	_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, _ Record) (Record, error) { return d, nil })
	if err != nil {
		t.Fatal(err)
	}
	cs, err := consultations.New(consultations.Options{Records: s.options.Records, Auth: &auth.Authenticator{}, Now: s.options.Now, Project: func(string) error { return nil }, Start: func(_ context.Context, _ string, d consultations.Record, _ auth.Principal) (consultations.Record, error) {
		return d, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	p := auth.Principal{Kind: auth.Screen}
	id := "22222222-2222-4222-8222-222222222222"
	if _, err = cs.Chat(ctx, draftID, Record{"text": "hello", "expectedRevision": 42}, p); err == nil {
		t.Fatal("stale revision accepted")
	}
	first, err := cs.Chat(ctx, draftID, Record{"id": id, "text": "hello", "expectedRevision": 0}, p)
	if err != nil {
		t.Fatal(err)
	}
	again, err := cs.Chat(ctx, draftID, Record{"id": id, "text": "hello", "expectedRevision": 0}, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(fleet.Elements(again["messages"])) != len(fleet.Elements(first["messages"])) {
		t.Fatal("duplicate saved")
	}
	if _, err = cs.Chat(ctx, draftID, Record{"id": id, "text": "different"}, p); err == nil {
		t.Fatal("conflicting id accepted")
	}
	if _, err = cs.Chat(ctx, draftID, Record{"text": "parallel"}, p); err == nil {
		t.Fatal("busy draft accepted")
	}
}

func TestConversationProjectionBudgets(t *testing.T) {
	ctx := context.Background()
	calls := 0
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) { calls++; return measuredAnswer(reply()), nil })
	d["status"] = "error"
	d["consultationBudget"] = Record{"calls": 40}
	_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, _ Record) (Record, error) { return d, nil })
	if err != nil {
		t.Fatal(err)
	}
	cs, err := consultations.New(consultations.Options{Records: s.options.Records, Auth: &auth.Authenticator{}, Now: s.options.Now, Project: func(string) error { return nil }, Start: func(_ context.Context, _ string, d consultations.Record, _ auth.Principal) (consultations.Record, error) {
		return d, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := cs.Chat(ctx, draftID, Record{"text": "追加"}, auth.Principal{Kind: auth.Screen})
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Turn(ctx, queued)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 || out["ready"] == true || fleet.Object(out["consultationBudget"])["stopped"] == nil {
		t.Fatalf("budget bypass: calls=%d out=%v", calls, out)
	}
}

func TestConversationProjectionActualInputs(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	var requests []ai.Request
	s, d := testService(t, func(_ context.Context, req ai.Request) (ai.Answer, error) {
		requests = append(requests, req)
		if len(requests) == 1 {
			return measuredAnswer(reply()), measuredTimeout{}
		}
		return measuredAnswer(reply()), nil
	})
	const omittedBody = "OMITTED_REFERENCE_BODY"
	d["status"] = "error"
	d["messages"] = []any{Record{"role": "user", "text": strings.Repeat("発", 70000), "references": []any{Record{"desk": 7, "history": omittedBody + strings.Repeat("参", 15000)}}}}
	config, err := s.options.Config(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings := models.Settings{Provider: "claude", Model: "fixture", Effort: "high"}
	s.resolve = func(context.Context, map[string]any, map[string]any, string) (models.Settings, error) {
		return settings, nil
	}
	state := sessionState{ID: "33333333-3333-4333-8333-333333333333", Draft: draftID, Provider: settings.Provider, Key: sessionKey(settings, config), Workspace: filepath.Join(s.options.RuntimeDir, "intake-workspace"), EndedAt: s.options.Now(), Messages: messageKeys(d), Sent: map[string]bool{}}
	if err = os.MkdirAll(state.Workspace, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.sessionMarker(state.ID), marshal(state), 0600); err != nil {
		t.Fatal(err)
	}
	d["aiSession"] = state
	_, err = s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, _ Record) (Record, error) { return d, nil })
	if err != nil {
		t.Fatal(err)
	}
	cs, err := consultations.New(consultations.Options{Records: s.options.Records, Auth: &auth.Authenticator{}, Now: s.options.Now, Project: func(string) error { return nil }, Start: func(_ context.Context, _ string, d consultations.Record, _ auth.Principal) (consultations.Record, error) {
		return d, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := cs.Chat(ctx, draftID, Record{"text": "今回の補完"}, auth.Principal{Kind: auth.Screen})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Turn(ctx, queued); err != nil {
		t.Fatal(err)
	}
	if len(requests) < 2 || requests[0].Resume || requests[1].Resume || requests[0].Input != requests[1].Input || !strings.Contains(requests[0].Input, "今回の補完") || strings.Contains(requests[0].Input, omittedBody) {
		t.Fatalf("投影済み全入力での再開にならない: calls=%d", len(requests))
	}
	_, payload, ok := strings.Cut(requests[0].Input, "以下は相談と資料のデータです:\n")
	if !ok {
		t.Fatal("入力なし")
	}
	var sent map[string]any
	if err = json.Unmarshal([]byte(payload), &sent); err != nil {
		t.Fatal(err)
	}
	positions := fleet.Elements(fleet.Object(sent["conversationOmissions"])["positions"])
	if len(positions) == 0 || fleet.Object(positions[0])["unconfirmed"] != true || fleet.Number(fleet.Object(fleet.Object(positions[0])["source"])["desk"]) != 7 {
		t.Fatal("省略の出典・未確認が欠落")
	}
}

func TestConversationProjectionSaveFailure(t *testing.T) {
	ctx := context.Background()
	started := 0
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
		t.Fatal("AIを呼び出した")
		return ai.Answer{}, nil
	})
	d["status"] = "error"
	d["messages"] = []any{Record{"role": "user", "text": strings.Repeat("発", 70000), "references": []any{Record{"desk": 7, "body": strings.Repeat("参", 15000)}}}}
	_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, _ Record) (Record, error) { return d, nil })
	if err != nil {
		t.Fatal(err)
	}
	before, found, err := s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil || !found {
		t.Fatal(err)
	}
	_, err = s.options.Records.DB.ExecContext(ctx, `CREATE FUNCTION projection_chat_save_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'projection save failure'; END $$; CREATE TRIGGER projection_chat_save_failure BEFORE UPDATE ON drafts FOR EACH ROW EXECUTE FUNCTION projection_chat_save_failure()`)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := consultations.New(consultations.Options{Records: s.options.Records, Auth: &auth.Authenticator{}, Now: s.options.Now, Project: func(string) error { return nil }, Start: func(_ context.Context, _ string, d consultations.Record, _ auth.Principal) (consultations.Record, error) {
		started++
		return d, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cs.Chat(ctx, draftID, Record{"text": "補完"}, auth.Principal{Kind: auth.Screen}); err == nil {
		t.Fatal("保存失敗を受領")
	}
	after, found, err := s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil || !found || started != 0 || !reflect.DeepEqual(before, after) {
		t.Fatal("保存失敗で原本が変わった、または処理を開始した")
	}
}

func TestConversationProjectionWeightedLimit(t *testing.T) {
	ctx := context.Background()
	calls := 0
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) { calls++; return measuredAnswer(reply()), nil })
	d["status"] = "error"
	d["messages"] = []any{Record{"role": "user", "text": strings.Repeat("発", 70000), "references": []any{Record{"desk": 7, "body": strings.Repeat("参", 15000)}}}}
	d["consultationBudget"] = Record{"calls": 1, "weighted": 6000000}
	_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, _ Record) (Record, error) { return d, nil })
	if err != nil {
		t.Fatal(err)
	}
	cs, err := consultations.New(consultations.Options{Records: s.options.Records, Auth: &auth.Authenticator{}, Now: s.options.Now, Project: func(string) error { return nil }, Start: func(_ context.Context, _ string, d consultations.Record, _ auth.Principal) (consultations.Record, error) {
		return d, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := cs.Chat(ctx, draftID, Record{"text": "補完"}, auth.Principal{Kind: auth.Screen})
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Turn(ctx, queued)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 || out["ready"] == true || fleet.Object(out["consultationBudget"])["stopped"] == nil {
		t.Fatalf("weighted budget bypass calls=%d", calls)
	}
}

func TestConversationProjectionInputLimits(t *testing.T) {
	for _, tc := range []struct {
		name         string
		chars, bytes int
	}{{"chars", 10000, 400000}, {"bytes", 200000, 10000}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			calls := 0
			s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) { calls++; return measuredAnswer(reply()), nil })
			d["status"] = "error"
			d["messages"] = []any{Record{"role": "user", "text": strings.Repeat("発", 70000), "references": []any{Record{"desk": 7, "body": strings.Repeat("参", 15000)}}}}
			base := s.options.Config
			s.options.Config = func(ctx context.Context) (map[string]any, error) {
				c, e := base(ctx)
				fleet.Object(c["intake"])["maxInputChars"] = tc.chars
				fleet.Object(c["intake"])["maxInputBytes"] = tc.bytes
				return c, e
			}
			_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, _ Record) (Record, error) { return d, nil })
			if err != nil {
				t.Fatal(err)
			}
			cs, err := consultations.New(consultations.Options{Records: s.options.Records, Auth: &auth.Authenticator{}, Now: s.options.Now, Project: func(string) error { return nil }, Start: func(_ context.Context, _ string, d consultations.Record, _ auth.Principal) (consultations.Record, error) {
				return d, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			queued, err := cs.Chat(ctx, draftID, Record{"text": "補完"}, auth.Principal{Kind: auth.Screen})
			if err != nil {
				t.Fatal(err)
			}
			accepted := fleet.Elements(queued["messages"])
			if len(accepted) != 2 || fleet.Object(accepted[1])["text"] != "補完" {
				t.Fatal("補完が受領されていない")
			}
			_, err = s.Turn(ctx, queued)
			var failure *ai.Failure
			if !errors.As(err, &failure) || failure.DeskCode != "INPUT_TOO_LARGE" || calls != 0 {
				t.Fatalf("超過の停止: calls=%d err=%v", calls, err)
			}
			saved, found, err := s.options.Records.Get(ctx, "drafts", draftID)
			if err != nil || !found || len(fleet.Elements(fleet.Object(saved["inputBudget"])["records"])) == 0 {
				t.Fatal("入力上限の記録がない")
			}
			messages := fleet.Elements(saved["messages"])
			if len(messages) != 2 || fleet.Object(messages[1])["id"] != fleet.Object(accepted[1])["id"] || fleet.Object(messages[1])["text"] != "補完" || fleet.Object(messages[0])["text"] != fleet.Object(accepted[0])["text"] {
				t.Fatal("入力超過後に受領済みの発言が失われた")
			}
		})
	}
}
