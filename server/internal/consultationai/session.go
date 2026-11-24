package consultationai

import (
	"context"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/models"
	"issue-desk-intake/server/internal/tierpolicy"
)

const sessionRetention = 60 * time.Minute

// Earlier provider transcripts may include the bodies of refused documents.
const sessionInputVersion = 2

func logSessionReset(d Record, reason string) {
	slog.Info("相談AIの会話を再利用できません", "id", fleet.Text(d["id"]), "reason", reason, "event", "consultation_session_reset")
}

type sessionState struct {
	ResetReason         string          `json:"-"`
	ID                  string          `json:"id"`
	Draft               string          `json:"draft"`
	Provider            string          `json:"provider"`
	Key                 string          `json:"key"`
	Workspace           string          `json:"workspace"`
	Messages            []string        `json:"messages"`
	EndedAt             time.Time       `json:"endedAt"`
	LastCost            *float64        `json:"lastCost"`
	Context             string          `json:"context"`
	Sent                map[string]bool `json:"sent"`
	LastOpenRequestsKey string          `json:"lastOpenRequestsKey,omitempty"`
}

func (s *Service) sessionMarker(id string) string {
	return filepath.Join(s.options.RuntimeDir, "intake-workspace", "intake-session-"+id+".json")
}
func sessionKey(settings models.Settings, config map[string]any) string {
	return digest(string(marshal(Record{"settings": settings, "config": sessionConfig(config), "tierPolicy": digest(tierpolicy.Guidance()), "instructions": digest(turnInstructions), "inputVersion": sessionInputVersion})))
}
func messageKeys(d Record) []string {
	keys := []string{}
	for _, message := range fleet.Elements(d["messages"]) {
		keys = append(keys, digest(string(marshal(message))))
	}
	return keys
}
func (s *Service) session(ctx context.Context, d Record, config map[string]any, settings models.Settings, images bool) (*sessionState, error) {
	var prior sessionState
	_ = json.Unmarshal(marshal(d["aiSession"]), &prior)
	key := sessionKey(settings, config)
	keys := messageKeys(d)
	resetReason := fleet.Text(d["aiSessionResetReason"])
	if resetReason != "" {
		if err := s.save(ctx, d, Record{"aiSessionResetReason": nil}); err != nil {
			return nil, err
		}
	}
	reason := ""
	if prior.ID != "" {
		switch {
		case prior.Draft != fleet.Text(d["id"]):
			reason = "draft_mismatch"
		case prior.Provider != settings.Provider:
			reason = "provider_mismatch"
		case prior.Key != key:
			reason = "key_changed"
		case prior.Workspace != filepath.Join(s.options.RuntimeDir, "intake-workspace"):
			reason = "workspace_mismatch"
		case images:
			reason = "images"
		case prior.EndedAt.IsZero():
			reason = "missing_end_time"
		case !s.options.Now().Before(prior.EndedAt.Add(sessionRetention)):
			reason = "expired"
		case len(prior.Messages) > len(keys):
			reason = "messages_shortened"
		default:
			for i, prefix := range prior.Messages {
				if keys[i] != prefix {
					reason = "messages_mismatch"
					break
				}
			}
			if reason == "" {
				if _, err := os.Stat(s.sessionMarker(prior.ID)); err != nil {
					reason = "marker_unavailable"
				}
			}
		}
		if reason == "" {
			return &prior, nil
		}
		resetReason = reason
		logSessionReset(d, reason)
		if err := s.dropSession(ctx, d, &prior); err != nil {
			return nil, err
		}
	}
	return &sessionState{ResetReason: resetReason, Draft: fleet.Text(d["id"]), Provider: settings.Provider, Key: key, Workspace: filepath.Join(s.options.RuntimeDir, "intake-workspace"), Messages: keys}, nil
}
func (s *Service) rememberSession(ctx context.Context, d Record, state *sessionState, id string) error {
	if !uuid.MatchString(id) {
		return errors.New("相談の会話IDが不正です")
	}
	state.ID = id
	if err := os.MkdirAll(state.Workspace, 0700); err != nil {
		return err
	}
	marker := s.sessionMarker(id)
	if raw, err := os.ReadFile(marker); err == nil {
		var saved sessionState
		if json.Unmarshal(raw, &saved) != nil || saved.Draft != state.Draft || saved.ID != id || saved.Workspace != state.Workspace {
			return errors.New("会話の所有者が保存記録と一致しません")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.WriteFile(marker, marshal(state), 0600); err != nil {
		return err
	}
	return s.save(ctx, d, Record{"aiSession": state})
}
func (s *Service) finishSession(ctx context.Context, d Record, state *sessionState, answer Record) error {
	if state == nil || state.ID == "" {
		return nil
	}
	// 確定できる回答（ready）の後と、回答の検査で断ったターンの後（呼び出し側が検査した回答を
	// 渡す）も会話を残す。了承や訂正の返信は直前の会話へ続けて送り、全文の入れ直しを避ける。
	// 起票後の会話は保持時間の後と起動時の掃除で消える。
	if answer == nil || answer["escalate"] == true || len(fleet.Elements(answer["consultationParts"])) > 0 || d["_ephemeral"] == true {
		return s.dropSession(ctx, d, state)
	}
	state.EndedAt = s.options.Now()
	state.Messages = messageKeys(d)
	if err := s.rememberSession(ctx, d, state, state.ID); err != nil {
		return err
	}
	s.expireSession(*state)
	return nil
}
func (s *Service) dropSession(ctx context.Context, d Record, state *sessionState) error {
	if state == nil || state.ID == "" {
		return nil
	}
	if err := s.cleanupSession(*state); err != nil {
		return err
	}
	state.ID = ""
	state.LastCost = nil
	return s.save(ctx, d, Record{"aiSession": nil})
}
func prepareSessionRequest(request *ai.Request, state *sessionState) {
	request.PersistSession = true
	request.Session = state.ID
	request.Resume = state.ID != ""
	request.LastSessionCost = state.LastCost
}
