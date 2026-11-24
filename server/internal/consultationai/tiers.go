package consultationai

import (
	"context"
	"errors"

	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/models"
)

func isServerDraining(err error) bool {
	var problem *contract.Problem
	return errors.As(err, &problem) && problem.Status == 503 && problem.Code == "SERVER_DRAINING"
}

func (s *Service) raise(ctx context.Context, d Record, config map[string]any, m *materials, reason string) (bool, error) {
	if d["tierRaised"] == true {
		return false, nil
	}
	used := m.state.Rounds
	start := used
	if start > 4 {
		start = 4
	}
	if start < 0 {
		start = 0
	}
	tier, err := models.ChangeTier(config, map[string]any(copyRecord(d)), contract.ClipUTF16(reason, 1000), s.options.Now(), map[string]any{"carried": Record{"roundsUsed": used, "start": start, "roundsGranted": 6 - start}})
	if err != nil || tier == nil {
		return false, err
	}
	m.state.Rounds = start
	if err := s.save(ctx, d, Record{"tier": tier, "tierRaised": true, "carriedRounds": start, "attempts": 0, "lastFailure": nil, "timeoutStreak": 0, "failureRetry": false}); err != nil {
		return false, err
	}
	return true, m.persist(ctx)
}
