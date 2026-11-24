package statistics

import (
	"context"
	"encoding/json"
	"time"
)

type stallLease struct {
	host, phase       string
	generation, epoch float64
	expires           time.Time
	running, unknown  bool
}

func (s *Service) stallEvents(ctx context.Context, end time.Time) ([]fleetSegment, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT seq,job_id,coalesce(host,''),kind,at,data FROM job_events WHERE at < $1 ORDER BY at,seq`, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []event
	for rows.Next() {
		var e event
		var raw []byte
		if err = rows.Scan(&e.Seq, &e.Job, &e.Host, &e.Kind, &e.At, &raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &e.Data); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return replayStallEvents(events, end), nil
}

func stallText(v any) string      { s, _ := v.(string); return s }
func stallExpiry(v any) time.Time { t, _ := time.Parse(time.RFC3339Nano, stallText(v)); return t }
func stallMatches(s stallLease, e event) bool {
	host := stallText(e.Data["leaseHost"])
	if host == "" && e.Kind == "lease_renewed" {
		host = e.Host
	}
	generation, gok := e.Data["generation"].(float64)
	epoch, eok := e.Data["epoch"].(float64)
	return s.host != "" && host == s.host && gok && generation == s.generation && eok && epoch == s.epoch
}
func stallUncertain(s stallLease, e event) bool {
	generation, gok := e.Data["generation"].(float64)
	epoch, eok := e.Data["epoch"].(float64)
	host := stallText(e.Data["leaseHost"])
	if e.Kind == "lease_renewed" {
		host = e.Host
	}
	if s.host == "" || !gok || !eok || host == "" {
		return true
	}
	return generation > s.generation || generation == s.generation && epoch >= s.epoch
}
func replayStallEvents(events []event, end time.Time) []fleetSegment {
	if len(events) == 0 {
		return nil
	}
	states := map[int64]stallLease{}
	var out []fleetSegment
	cursor := events[0].At
	for _, e := range events {
		if e.At.After(end) {
			break
		}
		out, cursor = appendStallSegments(out, states, cursor, e.At)
		applyStallEvent(states, e)
	}
	out, _ = appendStallSegments(out, states, cursor, end)
	return out
}

// appendStallSegments appends the segments from cursor to to in which every
// running lease is known, with the hosts running implementation, and returns
// the new cursor.
func appendStallSegments(out []fleetSegment, states map[int64]stallLease, cursor, to time.Time) ([]fleetSegment, time.Time) {
	for cursor.Before(to) {
		next := to
		hosts := map[string]int{}
		known := true
		for _, s := range states {
			if s.unknown {
				known = false
				continue
			}
			if !s.running {
				continue
			}
			if s.expires.IsZero() || !cursor.Before(s.expires) {
				known = false
				continue
			}
			if s.expires.Before(next) {
				next = s.expires
			}
			if s.phase == "" {
				known = false
			} else if s.phase == "running" {
				hosts[s.host]++
			}
		}
		if known && next.After(cursor) {
			out = append(out, fleetSegment{cursor, next, hosts})
		}
		cursor = next
	}
	return out, cursor
}

// applyStallEvent updates the job's lease state with the event.
func applyStallEvent(states map[int64]stallLease, e event) {
	old, exists := states[e.Job]
	if !exists {
		old.unknown = true
		states[e.Job] = old
	}
	switch e.Kind {
	case "leased":
		expiry := stallExpiry(e.Data["expiresAt"])
		generation, gok := e.Data["generation"].(float64)
		epoch, eok := e.Data["epoch"].(float64)
		if exists && gok && eok && (generation < old.generation || generation == old.generation && epoch < old.epoch) {
			return
		}
		if exists && old.host == e.Host && gok && eok && old.generation == generation && old.epoch == epoch {
			return
		}
		phase := stallText(e.Data["phase"])
		states[e.Job] = stallLease{host: e.Host, phase: phase, generation: generation, epoch: epoch, expires: expiry, running: true, unknown: e.Host == "" || !gok || !eok || expiry.IsZero()}
	case "lease_renewed":
		if stallMatches(old, e) {
			expiry := stallExpiry(e.Data["expiresAt"])
			if expiry.After(e.At) {
				old.expires = expiry
				states[e.Job] = old
			}
		} else if stallUncertain(old, e) {
			old.unknown = true
			states[e.Job] = old
		}
	case "phase":
		if stallMatches(old, e) && old.running {
			old.phase = stallText(e.Data["to"])
			states[e.Job] = old
		} else if stallUncertain(old, e) {
			old.unknown = true
			states[e.Job] = old
		}
	case "state", "control", "lease_expired", "scope_wait", "scope_over_limit":
		to := stallText(e.Data["to"])
		if to != "" && to != "running" {
			if stallMatches(old, e) {
				old.running = false
				old.unknown = false
				states[e.Job] = old
			} else if old.host == "" && e.Kind == "state" && e.Data["created"] == true {
				old.unknown = false
				states[e.Job] = old
			} else if stallUncertain(old, e) {
				old.unknown = true
				states[e.Job] = old
			}
		}
	}
}

func intersectStallSegments(logs, events []fleetSegment) []fleetSegment {
	var out []fleetSegment
	for i, j := 0, 0; i < len(logs) && j < len(events); {
		a, b := logs[i], events[j]
		from, to := a.From, a.To
		if b.From.After(from) {
			from = b.From
		}
		if b.To.Before(to) {
			to = b.To
		}
		if from.Before(to) {
			out = append(out, fleetSegment{from, to, b.Hosts})
		}
		if a.To.Before(b.To) {
			i++
		} else {
			j++
		}
	}
	return out
}
