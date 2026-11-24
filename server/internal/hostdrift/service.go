package hostdrift

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/hosts"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Hold struct {
	Revision, Desired, Kind string
	Since                   time.Time
	Behind                  *int
	IncludesAlign           bool
}
type Snapshot struct {
	Holds      map[string]Hold
	Drifts     map[string]Hold
	Candidates map[string]Hold
	Allowed    map[string][]string
	Alerts     []any
}
type acquireState struct {
	Revision                        string
	Seen, Sequence, First, Detected float64
	Baseline, Last                  float64
}
type Service struct {
	SettingsVersion func(context.Context) (string, error)
	settingsSeen    map[string]settingsDrift
	DB              *sql.DB
	Now             func() time.Time
	CompatLimit     int
	Notify          func(context.Context, map[string]any) error
	mu              sync.Mutex
	acquire         map[string]*acquireState
	notified        map[string]string
}

var sha = regexp.MustCompile(`(?i)^[0-9a-f]{40}$`)

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
func compatible(h Hold, limit int) bool {
	return !h.IncludesAlign && h.Behind != nil && *h.Behind >= 0 && *h.Behind <= limit
}
func (s *Service) Evaluate(ctx context.Context, a *fleet.Assignments) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Snapshot{Drifts: map[string]Hold{}, Holds: map[string]Hold{}, Candidates: map[string]Hold{}, Allowed: map[string][]string{}, Alerts: []any{}}
	if s.acquire == nil {
		s.acquire = map[string]*acquireState{}
		s.notified = map[string]string{}
	}
	now := s.now()
	live, e := s.liveHosts(ctx, a, now)
	if e != nil {
		return out, e
	}
	present := map[string]bool{}
	for _, r := range live {
		host, current := fleet.Text(r["host"]), strings.ToLower(fleet.Text(r["revision"]))
		if !sha.MatchString(current) {
			continue
		}
		release, times, behind, e := s.release(ctx, host, current)
		if e != nil {
			out.Alerts = append(out.Alerts, readProblem(host, now))
			continue
		}
		if release == nil || strings.EqualFold(release.Revision, current) {
			delete(s.acquire, host)
			continue
		}
		if times == nil {
			out.Alerts = append(out.Alerts, readProblem(host, now))
			continue
		}
		hold := driftHold(current, release.Revision, times, behind, now)
		if hold != nil {
			out.Drifts[host] = *hold
			out.Alerts = append(out.Alerts, map[string]any{"id": "host-drift-revision-" + host, "kind": "host_drift", "status": "stall_alert", "project": fleet.SystemProject, "host": host, "detectedAt": iso(hold.Since), "title": "監視の版が発行済みの版とそろっていません（" + host + "）", "detail": fmt.Sprintf("PC: %s\n監視の版: %s\n発行済みの版: %s\n更新の種類: %s\n発行時刻: %s", host, current[:12], release.Revision[:12], hold.Kind, iso(hold.Since))})
			if !compatible(*hold, s.CompatLimit) {
				out.Candidates[host] = *hold
				out.Holds[host] = *hold
			}
		}
		present[host] = true
		if s.notified[host] != release.Revision {
			delete(s.notified, host)
		}
		cycle := fleet.Object(fleet.Object(r["hostd_health"])["cycle"])
		released, alert := s.acquireRelease(host, release.Revision, cycle, hold != nil && !compatible(*hold, s.CompatLimit), now)
		if released {
			delete(out.Holds, host)
			if alert != nil {
				out.Alerts = append(out.Alerts, alert)
			}
			if s.notified[host] != release.Revision {
				s.notified[host] = release.Revision
				if s.Notify != nil {
					_ = s.Notify(ctx, map[string]any{"title": "新しい版を取得できないため今の版のまま受け取りを続けています", "body": "対象PC: " + host, "url": "/"})
				}
			}
		}
	}
	for host := range s.acquire {
		if !present[host] {
			delete(s.acquire, host)
		}
	}
	pending := map[string]Hold{}
	for host, hold := range out.Holds {
		pending[host] = hold
	}
	for repo, assigned := range a.Implementers {
		held := []map[string]any{}
		allBlocked := true
		for _, r := range live {
			host := fleet.Text(r["host"])
			matches := false
			for _, name := range assigned {
				if name == host {
					matches = true
				}
			}
			if !matches {
				continue
			}
			_, candidate := pending[host]
			if candidate {
				held = append(held, r)
			}
			if !candidate && r["ready"] != false && hosts.ActiveDrain(fleet.Object(r["drain"]), now, hosts.DefaultDuration()) == nil {
				allBlocked = false
			}
		}
		if len(held) == 0 || !allBlocked {
			continue
		}
		for _, r := range held {
			host := fleet.Text(r["host"])
			delete(out.Holds, host)
			out.Allowed[host] = append(out.Allowed[host], repo)
		}
	}
	if s.SettingsVersion != nil {
		version, err := s.SettingsVersion(ctx)
		if err != nil {
			return out, err
		}
		out.Alerts = append(out.Alerts, s.settingsAlerts(live, version, now)...)
	}
	decorateAlerts(&out)
	return out, nil
}

// liveHosts は直近15分に応答があり、退役していないPCの行を読む。
func (s *Service) liveHosts(ctx context.Context, a *fleet.Assignments, now time.Time) ([]map[string]any, error) {
	rows, e := s.DB.QueryContext(ctx, "SELECT row_to_json(h)FROM hosts h ORDER BY host")
	if e != nil {
		return nil, e
	}
	live := []map[string]any{}
	retired := map[string]bool{}
	for _, host := range fleet.Strings(a.Output["retiredHosts"]) {
		retired[host] = true
	}
	for rows.Next() {
		var raw []byte
		if e = rows.Scan(&raw); e != nil {
			rows.Close()
			return nil, e
		}
		var r map[string]any
		if e = json.Unmarshal(raw, &r); e != nil {
			rows.Close()
			return nil, e
		}
		at, e := time.Parse(time.RFC3339Nano, fleet.Text(r["heartbeat_at"]))
		if e == nil && !at.After(now) && now.Sub(at) <= 15*time.Minute && !retired[fleet.Text(r["host"])] {
			live = append(live, r)
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	return live, nil
}

// driftHold は発行済みの版の種類ごとの猶予を過ぎた最も古い版ずれを返す。猶予内なら nil を返す。
func driftHold(current, desired string, times map[string]time.Time, behind *int, now time.Time) *Hold {
	var hold *Hold
	includesAlign := !times["align"].IsZero()
	for _, kind := range []string{"align", "soon", "lazy"} {
		at := times[kind]
		grace := map[string]time.Duration{"align": 30 * time.Minute, "soon": 3 * time.Hour, "lazy": 24 * time.Hour}[kind]
		if !at.IsZero() && now.Sub(at) >= grace && (hold == nil || at.Before(hold.Since)) {
			h := Hold{Revision: current, Desired: desired, Kind: kind, Since: at, Behind: behind, IncludesAlign: includesAlign}
			hold = &h
		}
	}
	return hold
}

func iso(t time.Time) string { return contract.ISO(t) }
func readProblem(host string, at time.Time) any {
	return map[string]any{"id": "host-drift-read-" + host, "kind": "host_drift", "status": "stall_alert", "project": fleet.SystemProject, "host": host, "detectedAt": iso(at), "title": "監視の発行履歴を確認できません（" + host + "）", "detail": "版が違いますが発行時刻をたどれません。発行履歴を確認できるまで版ずれだけを根拠に新規配布を止めません。"}
}
func (s *Service) acquireRelease(host, desired string, cycle map[string]any, candidate bool, now time.Time) (bool, any) {
	at := float64(now.UnixMilli())
	valid := hosts.ValidStoredCycle(cycle) && fleet.Number(cycle["receivedAt"]) <= at
	state := s.acquire[host]
	if state == nil || state.Revision != desired {
		state = &acquireState{Revision: desired, Seen: at, Sequence: at}
		if valid {
			state.Baseline = fleet.Number(cycle["failures"])
			state.First = fleet.Number(cycle["firstFailureAt"])
			state.Last = state.Baseline
		}
		s.acquire[host] = state
	}
	if !valid || cycle["result"] != "failed" || cycle["stage"] != "acquire" {
		state.Baseline = 0
		state.First = 0
		state.Last = 0
		state.Detected = 0
		if valid {
			state.Baseline = fleet.Number(cycle["failures"])
			state.Last = state.Baseline
			state.Sequence = max(state.Sequence, fleet.Number(cycle["receivedAt"]))
		}
		return false, nil
	}
	first, failures := fleet.Number(cycle["firstFailureAt"]), fleet.Number(cycle["failures"])
	if state.First != first || failures < state.Last {
		if state.First == 0 && first >= state.Seen {
			state.Baseline = min(state.Baseline, failures-1)
		} else {
			state.Baseline = failures - 1
		}
		state.Sequence = max(state.Sequence, first)
		state.Detected = 0
		state.First = first
	}
	state.Last = failures
	count := failures - state.Baseline
	since := max(state.Sequence, first)
	if count < 3 && at-since < 30*60000 || !candidate {
		return false, nil
	}
	if state.Detected == 0 {
		state.Detected = at
	}
	return true, map[string]any{"id": "host-drift-acquire-" + host, "kind": "host_drift", "status": "stall_alert", "project": fleet.SystemProject, "host": host, "detectedAt": iso(time.UnixMilli(int64(state.Detected))), "title": "新しい版を取得できないため今の版のまま受け取りを続けています（" + host + "）", "detail": fmt.Sprintf("PC: %s\n発行済みの版: %s\n失敗の段: %v\n失敗の理由: %v\nこの版を見た後の失敗回数: %.0f回（周期の連続失敗: %v回）", host, desired, cycle["stage"], cycle["reason"], count, cycle["failures"])}
}
