package consultations

import (
	"log/slog"
	"time"
)

// The logical byte count is the sum of UTF-8 bytes of each database key and
// the projection JSON returned for that row when it was last fetched. It is
// not Go heap usage. Successful snapshots log purpose, count, logical_bytes,
// and measured_at; failures log purpose and status=missing without stale values.
// State is protected by draftGate, and unchanged reads and repeated failures
// do not emit additional events.
type draftCacheMetric struct {
	missing     bool
	initialized bool
}

func (s *Records) draftMetricMissing(purpose DraftReadPurpose) {
	if s.draftMetrics == nil {
		s.draftMetrics = make(map[DraftReadPurpose]draftCacheMetric)
	}
	state := s.draftMetrics[purpose]
	if !state.initialized || !state.missing {
		slog.Info("draft cache observation", "purpose", string(purpose), "status", "missing", "measured_at", time.Now().UTC())
	}
	s.draftMetrics[purpose] = draftCacheMetric{initialized: true, missing: true}
}

func (s *Records) draftMetricSuccess(purpose DraftReadPurpose, count, bytes int, changed bool) {
	if s.draftMetrics == nil {
		s.draftMetrics = make(map[DraftReadPurpose]draftCacheMetric)
	}
	state := s.draftMetrics[purpose]
	if !state.initialized || state.missing || changed {
		slog.Info("draft cache observation", "purpose", string(purpose), "status", "success", "count", count, "logical_bytes", bytes, "measured_at", time.Now().UTC())
	}
	s.draftMetrics[purpose] = draftCacheMetric{initialized: true}
}
