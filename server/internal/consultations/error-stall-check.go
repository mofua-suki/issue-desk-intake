package consultations

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"sort"
	"time"
)

func (s *ErrorStalls) Check(ctx context.Context, e *Engine) (Record, error) {
	s.mu.Lock()
	open, err := s.open(ctx)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if open == nil || open["autoFixStopped"] == true || s.checking || s.revision == "" || open["revision"] == s.revision || open["checkedRevision"] == s.revision {
		s.mu.Unlock()
		return Record{"checked": false}, nil
	}
	open["checkedRevision"] = s.revision
	if err := s.save(ctx, open); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	entries := fleet.Items(open["drafts"])
	sort.SliceStable(entries, func(i, j int) bool { return text(record(entries[i])["at"]) > text(record(entries[j])["at"]) })
	targets, err := s.retryTargets(ctx, entries)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if len(targets) == 0 {
		s.mu.Unlock()
		return Record{"checked": false, "reason": "no_target"}, nil
	}
	s.checking = true
	s.outcomes = map[string]Record{}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.checking = false; s.mu.Unlock() }()
	for _, d := range targets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p, err := e.principal(ctx, d)
		if err != nil {
			continue
		}
		if _, err := s.service.Chat(ctx, text(d["id"]), Record{"retry": true}, p); err != nil {
			continue
		}
		if err := s.waitRetriedTurn(ctx, e, text(d["id"])); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.open(ctx)
	if err != nil {
		return nil, err
	}
	if current == nil || current["id"] != open["id"] {
		return Record{"checked": true, "result": "fixed"}, nil
	}
	failures, gist := s.retryFailures(targets, current)
	if len(failures) == 0 {
		return Record{"checked": true, "result": "unknown"}, nil
	}
	if err := s.resolve(ctx, current, "unfixed"); err != nil {
		return nil, err
	}
	series, _ := integer(current["series"])
	count, _ := integer(current["failures"])
	next, err := s.record(ctx, failures, gist, "新しい版で相談を続けましたが例外が解消しませんでした", series, count+1)
	if err != nil {
		return nil, err
	}
	if err := s.snapshot(ctx); err != nil {
		return nil, err
	}
	return Record{"checked": true, "result": "unfixed", "stall": next}, nil
}

// retryTargets reads the latest drafts of the stall that are still in error, at most three.
// The caller holds s.mu.
func (s *ErrorStalls) retryTargets(ctx context.Context, entries []any) ([]Record, error) {
	targets := []Record{}
	for _, item := range entries {
		d, found, err := s.service.options.Records.Get(ctx, "drafts", text(record(item)["id"]))
		if err != nil {
			return nil, err
		}
		if found && d["status"] == "error" {
			targets = append(targets, d)
			if len(targets) == 3 {
				break
			}
		}
	}
	return targets, nil
}

// waitRetriedTurn waits until the retried turn ends or schedules another retry.
func (s *ErrorStalls) waitRetriedTurn(ctx context.Context, e *Engine, id string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	for e.Busy(id) {
		select {
		case <-ctx.Done():
			ticker.Stop()
			return ctx.Err()
		case <-ticker.C:
		}
		current, found, err := s.service.options.Records.Get(ctx, "drafts", id)
		if err != nil {
			ticker.Stop()
			return err
		}
		if !found || text(current["retryAt"]) != "" {
			break
		}
	}
	ticker.Stop()
	return nil
}

// retryFailures collects the retried drafts that failed again and the gist to report,
// preferring the gist of the open stall. The caller holds s.mu.
func (s *ErrorStalls) retryFailures(targets []Record, current Record) ([]Record, string) {
	failures := []Record{}
	gist := ""
	for _, d := range targets {
		outcome := s.outcomes[text(d["id"])]
		if text(outcome["gist"]) != "" {
			failures = append(failures, outcome)
			if gist == "" || outcome["gist"] == current["gist"] {
				gist = text(outcome["gist"])
			}
		}
	}
	return failures, gist
}
