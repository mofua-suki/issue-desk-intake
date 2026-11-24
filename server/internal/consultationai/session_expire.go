package consultationai

import (
	"context"
	"database/sql"
	"encoding/json"
	"issue-desk-intake/server/internal/contract"
	"os"
	"time"
)

func (s *Service) expireSession(state sessionState) {
	s.expireSessionWithTimer(state, func(delay time.Duration, f func()) { time.AfterFunc(delay, f) })
}

func (s *Service) expireSessionWithTimer(state sessionState, schedule func(time.Duration, func())) {
	schedule(sessionRetention, func() {
		lock := s.lock(state.Draft)
		lock.Lock()
		defer lock.Unlock()
		raw, err := os.ReadFile(s.sessionMarker(state.ID))
		if err != nil {
			return
		}
		var saved sessionState
		if json.Unmarshal(raw, &saved) != nil || saved.ID != state.ID || !saved.EndedAt.Equal(state.EndedAt) {
			return
		}
		if err := s.cleanupSession(saved); err != nil {
			return
		}
		_, changeErr := s.options.Records.Change(context.Background(), "drafts", state.Draft, func(_ *sql.Tx, d Record) (Record, error) {
			if d == nil {
				return d, nil
			}
			var current sessionState
			_ = json.Unmarshal(marshal(d["aiSession"]), &current)
			if current.ID == saved.ID && current.EndedAt.Equal(saved.EndedAt) {
				d["aiSession"] = nil
				d["aiSessionResetReason"] = "expired"
				logSessionReset(d, "expired")
			}
			return d, nil
		})
		contract.LogFailure("終わった会話の復旧情報の消去 "+state.Draft, changeErr)
	})
}
