package hosts

import (
	"context"
	"database/sql"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"time"
)

func MonitorHost(a *fleet.Assignments) string {
	if a == nil {
		return ""
	}
	return a.MonitorHost
}
func (s *Service) monitorSnapshot(a *fleet.Assignments, beat *time.Time, now time.Time) any {
	host := MonitorHost(a)
	if host == "" {
		return nil
	}
	from := s.StartedAt
	if from.IsZero() {
		from = now
	}
	var last any
	if beat != nil && beat.UnixMilli() > 0 {
		from = *beat
		last = contract.ISO(*beat)
	}
	return map[string]any{"host": host, "stopped": now.Sub(from) > 15*time.Minute, "lastHeartbeatAt": last}
}
func (s *Service) HomeMonitor(ctx context.Context, a *fleet.Assignments) (any, error) {
	host := MonitorHost(a)
	if host == "" {
		return nil, nil
	}
	var at time.Time
	e := s.DB.QueryRowContext(ctx, "SELECT heartbeat_at FROM hosts WHERE host=$1", host).Scan(&at)
	if e == sql.ErrNoRows {
		return s.monitorSnapshot(a, nil, s.clock()), nil
	}
	if e != nil {
		return nil, e
	}
	return s.monitorSnapshot(a, &at, s.clock()), nil
}
