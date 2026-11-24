package consultationai

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/models"
)

func TestSessionKeyIgnoresOnlyWorkerPollInterval(t *testing.T) {
	settings := models.Settings{Provider: "codex", Model: "fixture", Effort: "medium"}
	config := Record{"assignments": Record{"pollSeconds": 300, "hosts": Record{"nas": true}}, "repositories": []any{"owner/repo"}, "intake": Record{"maxInputBytes": 1000}, "permissions": Record{"write": false}}
	before := string(marshal(config))
	base := sessionKey(settings, config)
	changed := copyRecord(config)
	changed["assignments"].(map[string]any)["pollSeconds"] = 60
	if sessionKey(settings, changed) != base {
		t.Fatal("Workerの確認間隔だけで会話が失効")
	}
	if string(marshal(config)) != before {
		t.Fatal("共有設定が変更された")
	}
	for _, key := range []string{"repositories", "permissions", "intake", "unknownFutureSetting"} {
		changed := copyRecord(config)
		changed[key] = "changed"
		if sessionKey(settings, changed) == base {
			t.Fatalf("%sの変更で会話を再利用", key)
		}
	}
	changed = copyRecord(config)
	changed["assignments"].(map[string]any)["hosts"] = Record{"other": true}
	if sessionKey(settings, changed) == base {
		t.Fatal("担当構成が変わっても再利用")
	}
	settings.Model = "other"
	if sessionKey(settings, config) == base {
		t.Fatal("モデル変更で再利用")
	}
}

func TestSessionInputObservationSeparatesBytesAndReset(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previous)
	state := &sessionState{ID: "private-session", ResetReason: "expired"}
	s := &Service{}
	s.observeSessionInput(Record{"id": "draft"}, state, false, "秘密本文", "秘密本文")
	state.ID = "private-session"
	s.observeSessionInput(Record{"id": "draft"}, state, true, "差分", "秘密本文")
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatal(output.String())
	}
	for i, line := range lines {
		row := Record{}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row["event"] != "consultation_session_input" {
			t.Fatal(row)
		}
		if i == 0 && (row["reset_reason"] != "expired" || row["reinput_bytes"] != float64(len("秘密本文")) || row["mode"] != "full") {
			t.Fatal(row)
		}
		if i == 1 && (row["reset_reason"] != "" || row["reinput_bytes"] != float64(0) || row["mode"] != "resumed") {
			t.Fatal(row)
		}
	}
	for _, secret := range []string{"秘密本文", "差分", "private-session"} {
		if strings.Contains(output.String(), secret) {
			t.Fatal("内容を記録した")
		}
	}
}

func TestSessionExpiryReasonReachesNextInputOnlyOnce(t *testing.T) {
	s, _, config, settings, state, _ := sessionFixture(t, "codex")
	var fire func()
	s.expireSessionWithTimer(*state, func(_ time.Duration, f func()) { fire = f })
	fire()
	saved, found, err := s.options.Records.Get(t.Context(), "drafts", draftID)
	if err != nil || !found || saved["aiSessionResetReason"] != "expired" {
		t.Fatalf("%v %v", saved, err)
	}
	next, err := s.session(t.Context(), saved, config, settings, false)
	if err != nil || next.ResetReason != "expired" || next.ID != "" {
		t.Fatalf("%v %v", next, err)
	}
	reloaded, _, err := s.options.Records.Get(t.Context(), "drafts", draftID)
	if err != nil || reloaded["aiSessionResetReason"] != nil {
		t.Fatalf("%v %v", reloaded, err)
	}
}
