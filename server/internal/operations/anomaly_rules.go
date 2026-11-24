package operations

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// IncidentHold is a source history event. ID must be stable across rereads and
// unique within a cause; At is the time the hold was recorded.
type IncidentHold struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
}

// IncidentCause is the persisted handoff to the anomaly evaluator and responder.
// Prerequisite identifies the complete observed prerequisite state, not its label.
type IncidentCause struct {
	Key                   string         `json:"key"`
	Prerequisite          string         `json:"prerequisite"`
	EvaluatedPrerequisite string         `json:"evaluatedPrerequisite,omitempty"`
	PendingPrerequisite   string         `json:"pendingPrerequisite,omitempty"`
	State                 string         `json:"state"` // observing, detected, resolved or unknown
	Reason                string         `json:"reason,omitempty"`
	LastKnownState        string         `json:"lastKnownState,omitempty"`
	Holds                 []IncidentHold `json:"holds"`
}

// EvaluateIncidentHolds merges a complete or incremental history snapshot. It
// returns reevaluate once when a detected cause is new or its prerequisite has
// changed. It never performs a retry or another operational action.
func (s *Service) EvaluateIncidentHolds(ctx context.Context, key, prerequisite string, history []IncidentHold, now time.Time) (IncidentCause, bool, error) {
	if key == "" || prerequisite == "" || now.IsZero() {
		return IncidentCause{}, false, fmt.Errorf("cause key, prerequisite and evaluation time are required")
	}
	for _, h := range history {
		if h.ID == "" || h.At.IsZero() {
			return IncidentCause{}, false, fmt.Errorf("hold ID and time are required")
		}
	}
	var result IncidentCause
	reevaluate := false
	err := s.withIncidentCauses(ctx, true, func(causes map[string]IncidentCause) error {
		old := causes[key]
		seen := map[string]time.Time{}
		for _, h := range append(old.Holds, history...) {
			if prev, ok := seen[h.ID]; ok && !prev.Equal(h.At) {
				return fmt.Errorf("conflicting hold history ID %q", h.ID)
			}
			seen[h.ID] = h.At
		}
		holds := make([]IncidentHold, 0, len(seen))
		for id, at := range seen {
			if !at.Before(now.Add(-6*time.Hour)) && !at.After(now) {
				holds = append(holds, IncidentHold{ID: id, At: at})
			}
		}
		sort.Slice(holds, func(i, j int) bool {
			if holds[i].At.Equal(holds[j].At) {
				return holds[i].ID < holds[j].ID
			}
			return holds[i].At.Before(holds[j].At)
		})
		state := "observing"
		if len(holds) >= 3 {
			state = "detected"
		}
		reevaluate = state == "detected" && old.EvaluatedPrerequisite != prerequisite
		evaluated := old.EvaluatedPrerequisite
		if reevaluate {
			evaluated = prerequisite
		}
		pending := old.PendingPrerequisite
		if reevaluate {
			pending = prerequisite
		}
		if state != "detected" {
			pending = ""
		}
		result = IncidentCause{Key: key, Prerequisite: prerequisite, EvaluatedPrerequisite: evaluated, PendingPrerequisite: pending, State: state, Holds: holds}
		causes[key] = result
		return nil
	})
	return result, reevaluate, err
}

func (s *Service) withIncidentCauses(ctx context.Context, write bool, change func(map[string]IncidentCause) error) error {
	if s.PostgreSQL == nil {
		return fmt.Errorf("incident cause store unavailable")
	}
	tx, err := s.PostgreSQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext('issue-desk-incident-causes'))"); err != nil {
		return err
	}
	var raw string
	err = tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='incident-causes' FOR UPDATE").Scan(&raw)
	causes := map[string]IncidentCause{}
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		if err = json.Unmarshal([]byte(raw), &causes); err != nil {
			return err
		}
		if causes == nil {
			return fmt.Errorf("invalid incident cause ledger")
		}
	}
	if err = change(causes); err != nil {
		return err
	}
	if write {
		value, e := json.Marshal(causes)
		if e != nil {
			return e
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value,expires_at,updated_at,updated_by) VALUES('incident-causes',$1::jsonb,NULL,$2,'incident-causes') ON CONFLICT(key) DO UPDATE SET value=excluded.value,expires_at=NULL,updated_at=excluded.updated_at,updated_by=excluded.updated_by", string(value), s.now())
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
