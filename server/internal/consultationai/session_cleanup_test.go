package consultationai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/models"
	"issue-desk-intake/server/internal/tierpolicy"
)

func TestCleanupRequiresMatchingSessionOwnerAndPreservesOtherFiles(t *testing.T) {
	s, _ := testService(t, nil)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	id := "22222222-2222-4222-8222-222222222222"
	work := filepath.Join(s.options.RuntimeDir, "intake-workspace")
	if err := os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "rollout-"+id+".jsonl")
	other := filepath.Join(dir, "other.jsonl")
	os.WriteFile(other, []byte("private"), 0600)
	state := sessionState{ID: id, Draft: draftID, Provider: "codex", Workspace: work}
	os.WriteFile(s.sessionMarker(id), marshal(state), 0600)
	os.WriteFile(file, []byte(`{"type":"session_meta","payload":{"id":"`+id+`","cwd":"elsewhere"}}`), 0600)
	if err := s.cleanupSession(state); err == nil {
		t.Fatal("所有者不一致を削除した")
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(Record{"payload": Record{"id": id, "cwd": work}})
	os.WriteFile(file, raw, 0600)
	if err := s.cleanupSession(state); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("会話記録が残った")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("無関係な記録を変更した")
	}
}
func TestExpiredOrChangedMessagesCannotResume(t *testing.T) {
	s, d := testService(t, nil)
	config, _ := s.options.Config(context.Background())
	settings, _ := s.resolve(context.Background(), config, map[string]any(d), "intake")
	state, _ := s.session(context.Background(), d, config, settings, false)
	state.EndedAt = s.options.Now().Add(-61 * time.Minute)
	state.ID = "22222222-2222-4222-8222-222222222222"
	if err := s.rememberSession(context.Background(), d, state, state.ID); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", t.TempDir())
	next, err := s.session(context.Background(), d, config, settings, false)
	if err != nil || next.ID != "" {
		t.Fatal(next, err)
	}
}
func TestNamedSuccessorRejectsAmbiguousCandidates(t *testing.T) {
	known := map[string]treeEntry{"new/one.go": {}, "new/two.go": {}}
	if got := successor("old/file.go", []any{Record{"filename": "new/one.go", "status": "added"}, Record{"filename": "new/two.go", "status": "added"}}, known); got != "" {
		t.Fatal(got)
	}
	if got := successor("old/file.go", []any{Record{"filename": "new/one.go", "status": "added"}}, known); got != "new/one.go" {
		t.Fatal(got)
	}
}

func sessionFixture(t *testing.T, provider string) (*Service, Record, map[string]any, models.Settings, *sessionState, string) {
	t.Helper()
	s, d := testService(t, nil)
	config, _ := s.options.Config(context.Background())
	settings := models.Settings{Provider: provider, Model: "fixture", Effort: "high"}
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	state, err := s.session(context.Background(), d, config, settings, false)
	if err != nil {
		t.Fatal(err)
	}
	state.EndedAt = s.options.Now()
	state.ID = "22222222-2222-4222-8222-222222222222"
	if err := s.rememberSession(context.Background(), d, state, state.ID); err != nil {
		t.Fatal(err)
	}
	dir := "sessions"
	if provider == "claude" {
		dir = "projects"
	}
	if err := os.MkdirAll(filepath.Join(home, dir), 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(home, dir, state.ID+".jsonl")
	var metadata any = Record{"payload": Record{"id": state.ID, "cwd": state.Workspace}}
	if provider == "claude" {
		metadata = Record{"sessionId": state.ID, "cwd": state.Workspace}
	}
	if err := os.WriteFile(file, marshal(metadata), 0600); err != nil {
		t.Fatal(err)
	}
	return s, d, config, settings, state, file
}

func TestSessionRenewedWhenInputFormatChanges(t *testing.T) {
	s, d, config, settings, prior, transcript := sessionFixture(t, "codex")
	// A stored provider session from before the prompt's refusal metadata fix.
	prior.Key = digest(string(marshal(Record{
		"settings": settings, "config": sessionConfig(config),
		"tierPolicy": digest(tierpolicy.Guidance()), "instructions": digest(turnInstructions),
	})))
	if err := s.rememberSession(t.Context(), d, prior, prior.ID); err != nil {
		t.Fatal(err)
	}
	next, err := s.session(t.Context(), d, config, settings, false)
	if err != nil {
		t.Fatal(err)
	}
	var request ai.Request
	prepareSessionRequest(&request, next)
	if request.Resume || request.Session != "" || next.ResetReason != "key_changed" {
		t.Fatalf("以前の会話を引き継ぎました: resume=%t reason=%q", request.Resume, next.ResetReason)
	}
	if _, err := os.Stat(transcript); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("以前の会話を片付けられませんでした: %v", err)
	}
}

func TestSessionResumesAfterThirtyMinutes(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			s, d, cfg, settings, state, _ := sessionFixture(t, provider)
			s.options.Now = func() time.Time { return state.EndedAt.Add(30 * time.Minute) }
			next, err := s.session(context.Background(), d, cfg, settings, false)
			if err != nil {
				t.Fatal(err)
			}
			var req ai.Request
			prepareSessionRequest(&req, next)
			if !req.Resume || req.Session != state.ID {
				t.Fatalf("request: %+v", req)
			}
		})
	}
}

func TestSessionRetentionBoundary(t *testing.T) {
	for _, age := range []time.Duration{time.Hour - time.Nanosecond, time.Hour, 61 * time.Minute} {
		t.Run(age.String(), func(t *testing.T) {
			s, d, cfg, settings, state, file := sessionFixture(t, "codex")
			s.options.Now = func() time.Time { return state.EndedAt.Add(age) }
			id := state.ID
			next, err := s.session(context.Background(), d, cfg, settings, false)
			if err != nil {
				t.Fatal(err)
			}
			var req ai.Request
			prepareSessionRequest(&req, next)
			if age < time.Hour {
				if !req.Resume || req.Session != id {
					t.Fatal("会話を再開できない")
				}
				return
			}
			if req.Resume {
				t.Fatal("期限切れを再開した")
			}
			for _, path := range []string{file, s.sessionMarker(id)} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("削除されていない: %v", err)
				}
			}
		})
	}
}

func TestSessionExpiryUsesRetentionDuration(t *testing.T) {
	s, _, _, _, state, file := sessionFixture(t, "codex")
	var delay time.Duration
	var fire func()
	s.expireSessionWithTimer(*state, func(d time.Duration, f func()) { delay = d; fire = f })
	if delay != time.Hour || fire == nil {
		t.Fatalf("delay=%v", delay)
	}
	fire()
	for _, path := range []string{file, s.sessionMarker(state.ID)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("削除されていない: %v", err)
		}
	}
}

func TestSessionExpiryPreservesRenewedSession(t *testing.T) {
	for _, change := range []string{"id", "time"} {
		t.Run(change, func(t *testing.T) {
			s, _, _, _, state, file := sessionFixture(t, "codex")
			old := *state
			if change == "id" {
				old.ID = "33333333-3333-4333-8333-333333333333"
				if err := os.WriteFile(s.sessionMarker(old.ID), marshal(state), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				old.EndedAt = old.EndedAt.Add(-time.Minute)
			}
			var fire func()
			s.expireSessionWithTimer(old, func(_ time.Duration, f func()) { fire = f })
			fire()
			for _, path := range []string{file, s.sessionMarker(state.ID)} {
				if _, err := os.Stat(path); err != nil {
					t.Fatal(err)
				}
			}
			saved, ok, err := s.options.Records.Get(t.Context(), "drafts", draftID)
			if err != nil || !ok {
				t.Fatalf("保存記録: %v", err)
			}
			var current sessionState
			_ = json.Unmarshal(marshal(saved["aiSession"]), &current)
			if current.ID != state.ID {
				t.Fatal("会話が失われた")
			}
		})
	}
}

func TestSessionRejectionLogsReasons(t *testing.T) {
	cases := []struct {
		name, reason string
		mutate       func(*sessionState, Record, *models.Settings, *bool)
	}{
		{"draft", "draft_mismatch", func(s *sessionState, d Record, _ *models.Settings, _ *bool) { s.Draft = "another" }},
		{"key", "key_changed", func(s *sessionState, d Record, _ *models.Settings, _ *bool) { s.Key = "changed" }},
		{"provider", "provider_mismatch", func(s *sessionState, d Record, _ *models.Settings, _ *bool) { s.Provider = "claude" }},
		{"workspace", "workspace_mismatch", func(s *sessionState, d Record, _ *models.Settings, _ *bool) { s.Workspace = "elsewhere" }},
		{"images", "images", func(_ *sessionState, _ Record, _ *models.Settings, b *bool) { *b = true }},
		{"missing time", "missing_end_time", func(s *sessionState, _ Record, _ *models.Settings, _ *bool) { s.EndedAt = time.Time{} }},
		{"expired", "expired", func(s *sessionState, _ Record, _ *models.Settings, _ *bool) { s.EndedAt = s.EndedAt.Add(-time.Hour) }},
		{"shorter", "messages_shortened", func(s *sessionState, _ Record, _ *models.Settings, _ *bool) { s.Messages = append(s.Messages, "extra") }},
		{"changed", "messages_mismatch", func(_ *sessionState, d Record, _ *models.Settings, _ *bool) {
			d["messages"] = []any{Record{"role": "user", "text": "secret message", "references": "secret reference"}}
		}},
		{"marker", "marker_unavailable", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, d, cfg, settings, state, _ := sessionFixture(t, "codex")
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			images := false
			if tc.mutate != nil {
				tc.mutate(state, d, &settings, &images)
			} else {
				if err := os.Remove(s.sessionMarker(state.ID)); err != nil {
					t.Fatal(err)
				}
			}
			d["aiSession"] = state
			_, _ = s.session(t.Context(), d, cfg, settings, images)
			output := logs.String()
			if strings.Count(output, "\n") != 1 || !strings.Contains(output, `"id":"`+draftID+`"`) || !strings.Contains(output, `"reason":"`+tc.reason+`"`) {
				t.Fatalf("log=%s", output)
			}
			for _, secret := range []string{"secret message", "secret reference", "fixture", s.options.RuntimeDir} {
				if strings.Contains(output, secret) {
					t.Fatalf("secret in log: %s", output)
				}
			}
		})
	}
	t.Run("success and first", func(t *testing.T) {
		s, d, cfg, settings, _, _ := sessionFixture(t, "codex")
		var logs bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
		defer slog.SetDefault(previous)
		if _, err := s.session(t.Context(), d, cfg, settings, false); err != nil {
			t.Fatal(err)
		}
		d["aiSession"] = nil
		if _, err := s.session(t.Context(), d, cfg, settings, false); err != nil {
			t.Fatal(err)
		}
		if logs.Len() != 0 {
			t.Fatal(logs.String())
		}
	})
}

func TestSessionTerminalCleanupConditions(t *testing.T) {
	cases := []struct {
		name               string
		answer             Record
		ephemeral, removed bool
	}{
		{"ready", Record{"ready": true}, false, false},
		{"escalate", Record{"escalate": true}, false, true},
		{"split", Record{"consultationParts": []any{Record{"title": "part"}}}, false, true},
		{"ephemeral", Record{}, true, true},
		{"unfinished", Record{}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, d, _, _, state, file := sessionFixture(t, "codex")
			id := state.ID
			d["_ephemeral"] = tc.ephemeral
			if err := s.finishSession(t.Context(), d, state, tc.answer); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{file, s.sessionMarker(id)} {
				_, err := os.Stat(path)
				if tc.removed != os.IsNotExist(err) {
					t.Fatalf("removed=%v err=%v", tc.removed, err)
				}
			}
		})
	}
}

func TestTurnSessionResetLogsReasons(t *testing.T) {
	for _, reason := range []string{"material_identity_changed", "input_compacted", "difficulty_raised"} {
		t.Run(reason, func(t *testing.T) {
			s, d, cfg, settings, state, _ := sessionFixture(t, "codex")
			if reason == "material_identity_changed" {
				state.Context = "different identity"
			}
			if reason == "input_compacted" {
				fleet.Object(cfg["intake"])["maxInputChars"] = 1000
			}
			if reason == "difficulty_raised" {
				fleet.Object(cfg["intake"])["tiers"] = map[string]any{"standard": map[string]any{"model": "fixture"}}
				d["tier"] = Record{"level": "light"}
				d["consultationProvider"] = "codex"
			}
			state.Key = sessionKey(settings, cfg)
			if err := s.rememberSession(t.Context(), d, state, state.ID); err != nil {
				t.Fatal(err)
			}
			calls := 0
			s.options.Runner = callFunc(func(_ context.Context, r ai.Request) (ai.Answer, error) {
				calls++
				v := reply()
				if reason == "difficulty_raised" && calls == 1 {
					v["escalate"] = true
					v["escalateReason"] = "raise"
				}
				return measuredAnswer(v), nil
			})
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			_, err := s.Turn(t.Context(), d)
			if err != nil && reason != "input_compacted" {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
			count := 0
			for _, line := range lines {
				if strings.Contains(line, `"reason":"`+reason+`"`) {
					count++
					if !strings.Contains(line, `"id":"`+draftID+`"`) {
						t.Fatal(line)
					}
					for _, secret := range []string{"依頼", "fixture", s.options.RuntimeDir} {
						if strings.Contains(line, secret) {
							t.Fatal(line)
						}
					}
				}
			}
			if count != 1 {
				t.Fatalf("reason=%s logs=%s error=%v", reason, logs.String(), err)
			}
		})
	}
}

func TestUpdatedHistoricalReferencesPreventUnsafeResume(t *testing.T) {
	s, d, cfg, settings, state, _ := sessionFixture(t, "codex")
	// 次の利用者発言より前の発言に保存された参照情報が更新される。
	messages := fleet.Elements(d["messages"])
	first := fleet.Object(messages[0])
	first["references"] = []any{Record{"history": "old"}}
	state.Messages = messageKeys(d)
	if err := s.rememberSession(t.Context(), d, state, state.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.saveResolvedReferences(t.Context(), d, []any{Record{"history": "updated"}}); err != nil {
		t.Fatal(err)
	}
	d["messages"] = append(fleet.Elements(d["messages"]), Record{"role": "user", "text": "next"})
	next, err := s.session(t.Context(), d, cfg, settings, false)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != "" {
		t.Fatal("更新された参照を会話に伝えず再開した")
	}
}
func TestCleanupAbandonedContinuesPastBrokenMarker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	s := &Service{options: Options{RuntimeDir: t.TempDir()}}
	work := filepath.Join(s.options.RuntimeDir, "intake-workspace")
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	// The broken marker sorts before the healthy one and the image directory.
	broken := filepath.Join(work, "intake-codex-session-00000000-0000-4000-8000-000000000000.json")
	id := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	images := filepath.Join(work, "intake-images-"+id)
	if err := os.MkdirAll(images, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(broken, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	healthy := s.sessionMarker(id)
	if err := os.WriteFile(healthy, marshal(sessionState{ID: id, Draft: draftID, Provider: "codex", Workspace: work}), 0600); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(home, "sessions", "rollout-"+id+".jsonl")
	raw, _ := json.Marshal(Record{"payload": Record{"id": id, "cwd": work}})
	if err := os.WriteFile(record, raw, 0600); err != nil {
		t.Fatal(err)
	}
	err := s.CleanupAbandoned()
	if err == nil || !strings.Contains(err.Error(), filepath.Base(broken)) || !strings.Contains(err.Error(), "中断した会話の記録が不正です") {
		t.Fatal("失敗した目印の名前と原因が返らない", err)
	}
	for _, path := range []string{healthy, images, record} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatal("壊れた目印の後ろが片付いていない", path, statErr)
		}
	}
	if _, statErr := os.Stat(broken); statErr != nil {
		t.Fatal("確認できない目印を消した", statErr)
	}
}

// abandonedMarker writes a Codex marker and its conversation record whose cwd is recordCwd.
func abandonedMarker(t *testing.T, home, work, id, recordCwd string) (marker, record string) {
	t.Helper()
	if err := os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	marker = filepath.Join(work, "intake-session-"+id+".json")
	if err := os.WriteFile(marker, marshal(sessionState{ID: id, Draft: draftID, Provider: "codex", Workspace: work}), 0600); err != nil {
		t.Fatal(err)
	}
	record = filepath.Join(home, "sessions", "rollout-"+id+".jsonl")
	raw, _ := json.Marshal(Record{"payload": Record{"id": id, "cwd": recordCwd}})
	if err := os.WriteFile(record, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return marker, record
}

func TestCleanupAbandonedKeepsUnownedRecordAndContinues(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	s := &Service{options: Options{RuntimeDir: t.TempDir()}}
	work := filepath.Join(s.options.RuntimeDir, "intake-workspace")
	unownedMarker, unownedRecord := abandonedMarker(t, home, work, "11111111-1111-4111-8111-111111111111", "elsewhere")
	nextMarker, nextRecord := abandonedMarker(t, home, work, "22222222-2222-4222-8222-222222222222", work)
	err := s.CleanupAbandoned()
	if err == nil || !strings.Contains(err.Error(), filepath.Base(unownedMarker)) || !errors.Is(err, errUnowned) {
		t.Fatal("所有者を確認できないことが名前付きで返らない", err)
	}
	if strings.Contains(err.Error(), "読み取れない") {
		t.Fatal("所有者の不一致を読み取りの失敗として報告した", err)
	}
	for _, path := range []string{unownedMarker, unownedRecord} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatal("所有者を確認できない会話を消した", path, statErr)
		}
	}
	for _, path := range []string{nextMarker, nextRecord} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatal("次の目印へ進んでいない", path, statErr)
		}
	}
}

func TestCleanupAbandonedCleansOtherWorkspaceWhenOneIsUnreadable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	s := &Service{options: Options{RuntimeDir: t.TempDir()}}
	// A regular file in place of the directory makes ReadDir fail with something other than not-exist.
	if err := os.WriteFile(filepath.Join(s.options.RuntimeDir, "intake-workspace"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(s.options.RuntimeDir, "filing-scope-workspace")
	marker, record := abandonedMarker(t, home, work, "33333333-3333-4333-8333-333333333333", work)
	err := s.CleanupAbandoned()
	if err == nil || !strings.HasPrefix(err.Error(), "intake-workspace: ") || strings.Contains(err.Error(), s.options.RuntimeDir) {
		t.Fatal("読めないワークスペースが短い名前で返らない", err)
	}
	for _, path := range []string{marker, record} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatal("もう一方のワークスペースを片付けていない", path, statErr)
		}
	}
}

func TestCleanupAbandonedReturnsEveryFailureByName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	s := &Service{options: Options{RuntimeDir: t.TempDir()}}
	work := filepath.Join(s.options.RuntimeDir, "intake-workspace")
	if err := os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, id := range []string{"44444444-4444-4444-8444-444444444444", "55555555-5555-4555-8555-555555555555", "66666666-6666-4666-8666-666666666666"} {
		name := "intake-session-" + id + ".json"
		if err := os.WriteFile(filepath.Join(work, name), []byte("{"), 0600); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	unowned, _ := abandonedMarker(t, home, work, "77777777-7777-4777-8777-777777777777", "elsewhere")
	names = append(names, filepath.Base(unowned))
	err := s.CleanupAbandoned()
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok || len(joined.Unwrap()) != len(names) {
		t.Fatal("失敗が1件ずつ返らない", err)
	}
	for i, failure := range joined.Unwrap() {
		if !strings.HasPrefix(failure.Error(), "intake-workspace/"+names[i]+": ") || strings.Contains(failure.Error(), s.options.RuntimeDir) {
			t.Fatal("失敗に短い名前が付いていない", failure)
		}
	}
}
