package consultationai

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/models"
	"issue-desk-intake/server/internal/usage"
)

func TestClaudeCallObservesInitialResumeAndExpiredInputs(t *testing.T) {
	s, d := testService(t, nil)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previous)
	calls := 0
	s.options.Runner = &ai.Runner{Execute: func(_ context.Context, r ai.ProcessRequest) (ai.ProcessResult, error) {
		if r.Arguments[0] == "auth" {
			return ai.ProcessResult{Stdout: []byte(`{"loggedIn":true,"apiProvider":"firstParty","authMethod":"claude.ai"}`)}, nil
		}
		args := strings.Join(r.Arguments, " ")
		wantResume := calls == 1
		if strings.Contains(args, "--resume ") != wantResume || strings.Contains(args, "--session-id ") == wantResume {
			t.Fatalf("呼出し%dの再開指定が不正", calls)
		}
		calls++
		return ai.ProcessResult{Stdout: []byte(`{"type":"result","structured_output":{"ok":true},"usage":{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":2,"cache_creation_input_tokens":0}}`)}, nil
	}}
	config, _ := s.options.Config(t.Context())
	settings := models.Settings{Provider: "claude", Model: "fixture", Effort: "medium"}
	state, err := s.session(t.Context(), d, config, settings, false)
	if err != nil {
		t.Fatal(err)
	}
	budget, err := s.budget(d, config)
	if err != nil {
		t.Fatal(err)
	}
	for i, input := range []string{"秘密本文", "差分", "秘密本文"} {
		if i == 2 {
			state.EndedAt = s.options.Now().Add(-time.Hour)
			if err := s.rememberSession(t.Context(), d, state, state.ID); err != nil {
				t.Fatal(err)
			}
			state, err = s.session(t.Context(), d, config, settings, false)
			if err != nil {
				t.Fatal(err)
			}
		}
		_, err := s.call(t.Context(), d, config, settings, usage.DraftStep, measurementSchema(t, s), input, "秘密本文", state, budget, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	rows := []Record{}
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		row := Record{}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row["event"] == "consultation_session_input" {
			rows = append(rows, row)
		}
	}
	if calls != 3 || len(rows) != 3 || budget.value["calls"] != float64(3) {
		t.Fatalf("calls=%d reserved=%v rows=%v", calls, budget.value["calls"], rows)
	}
	for i, want := range []string{"full", "resumed", "full"} {
		if rows[i]["mode"] != want {
			t.Fatalf("呼出し%d: %v", i, rows[i])
		}
	}
	if rows[0]["reinput_bytes"] != float64(0) || rows[1]["reinput_bytes"] != float64(0) || rows[2]["reset_reason"] != "expired" || rows[2]["reinput_bytes"] != float64(len("秘密本文")) {
		t.Fatal(rows)
	}
	for _, secret := range []string{"秘密本文", "差分", state.ID} {
		if strings.Contains(output.String(), secret) {
			t.Fatal("計測ログへ秘密を出力した")
		}
	}
}

func TestImageCommentKeepsFlatSchemaAndPrompt(t *testing.T) {
	s, _ := testService(t, nil)
	s.resolve = func(context.Context, map[string]any, map[string]any, string) (models.Settings, error) {
		return models.Settings{Provider: "claude", Model: "fixture", Effort: "medium"}, nil
	}
	calls := 0
	s.options.Runner = &ai.Runner{Execute: func(_ context.Context, r ai.ProcessRequest) (ai.ProcessResult, error) {
		if r.Arguments[0] == "auth" {
			return ai.ProcessResult{Stdout: []byte(`{"loggedIn":true,"apiProvider":"firstParty","authMethod":"claude.ai"}`)}, nil
		}
		calls++
		if strings.Contains(r.Input, turnResponseInstructions) || !strings.Contains(r.Input, "全画像のimageReviews") {
			t.Fatal("画像確認と相談ターンの回答指示が混在")
		}
		return ai.ProcessResult{Stdout: []byte(`{"type":"result","structured_output":` + completeResponseFixture + `,"usage":{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":2,"cache_creation_input_tokens":0}}`)}, nil
	}}
	result, err := s.Answer(t.Context(), Record{"kind": "image-comment", "provider": "claude", "project": "owner/app", "messages": []any{}})
	if err != nil || calls != 1 || result["reply"] != "質問です" || result["response"] != nil {
		t.Fatalf("calls=%d result=%v err=%v", calls, result, err)
	}
}
