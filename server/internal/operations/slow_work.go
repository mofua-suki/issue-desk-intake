package operations

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const slowWorkWindow = 30 * 24 * time.Hour

// slowWork is one comparable execution interval. A nil end denotes ongoing work.
type slowWork struct {
	key, group, title, repo, host string
	number                        any
	start                         time.Time
	end                           *time.Time
	success                       bool
}

// slowWorkRows reads active intervals and completed intervals within the averaging window.
// The first generation's recorded tier, or the first tier history entry, is used;
// a current tier alone is not evidence of the tier at filing.
func (s *Service) slowWorkRows(ctx context.Context) ([]slowWork, error) {
	if s.PostgreSQL == nil {
		return nil, fmt.Errorf("遅延作業の保存先がありません")
	}
	const query = `
 SELECT 'job-'||j.id::text, 'implementation-'||coalesce(
   CASE WHEN jsonb_array_length(CASE WHEN jsonb_typeof(j.view->'tierHistory')='array' THEN j.view->'tierHistory' ELSE '[]'::jsonb END)>0
     THEN coalesce(j.view->'tierHistory'->0->>'from','') ELSE (SELECT u.json::jsonb->>'tier' FROM usage_rows u WHERE u.repo=j.repo AND u.issue=j.issue AND u.source='worker' AND u.step='実装' AND u.json::jsonb->>'generation'='1' ORDER BY u.at,u.seq LIMIT 1) END, ''),
   j.title,j.repo,j.issue::text,coalesce(j.lease_host,''),
   (SELECT CASE WHEN isfinite(e.at) THEN e.at END FROM job_events e WHERE e.job_id=j.id AND e.kind='leased' ORDER BY e.seq LIMIT 1),
   (SELECT CASE WHEN isfinite(e.at) THEN e.at END FROM job_events e WHERE e.job_id=j.id AND e.kind='pr_created' ORDER BY e.seq LIMIT 1),
   true, true, EXISTS(SELECT 1 FROM job_events e WHERE e.job_id=j.id AND e.kind='pr_created'), j.status
 FROM jobs j WHERE j.status = ANY($1::text[])
 AND coalesce((SELECT e.at FROM job_events e WHERE e.job_id=j.id AND e.kind='pr_created' ORDER BY e.seq LIMIT 1), 'infinity'::timestamptz) >= $2
 UNION ALL
 SELECT 'request-'||kind||'-'||id||'-'||coalesce(CASE WHEN isfinite(claimed_at) THEN to_char(claimed_at AT TIME ZONE 'UTC','YYYYMMDDHH24MISS.US') END,''),
   'request-'||kind,kind,repo,coalesce(payload->>'issue',''),coalesce(claimed_by,''),CASE WHEN isfinite(claimed_at) THEN claimed_at END,CASE WHEN isfinite(finished_at) THEN finished_at END,
   coalesce(status='done' AND result->>'status'=CASE kind WHEN 'pr_create' THEN 'created' WHEN 'merge_manual' THEN 'merged' WHEN 'full_test' THEN 'passed' WHEN 'pr_close' THEN 'closed' END,false),
   status='claimed' AND finished_at IS NULL, finished_at IS NOT NULL, status
 FROM (SELECT * FROM requests UNION ALL SELECT * FROM main_test_requests UNION ALL SELECT * FROM pr_close_requests) r
 WHERE status IN ('done','claimed') AND (finished_at IS NULL OR finished_at >= $2)`
	rows, err := s.PostgreSQL.QueryContext(ctx, query, []string{"running", "merged", "held", "queued"}, s.now().Add(-slowWorkWindow))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []slowWork{}
	for rows.Next() {
		var w slowWork
		var number, status string
		var start, end sql.NullTime
		var success, active, hasEnd bool
		if err = rows.Scan(&w.key, &w.group, &w.title, &w.repo, &number, &w.host, &start, &end, &success, &active, &hasEnd, &status); err != nil {
			return nil, err
		}
		if strings.HasPrefix(w.group, "implementation-") {
			if status != "running" && status != "merged" && status != "held" && status != "queued" {
				continue
			}
			active = status == "running"
		}
		if !start.Valid || (!end.Valid && (!active || hasEnd)) {
			continue
		}
		if number != "" {
			if n, err := strconv.ParseInt(number, 10, 64); err == nil && n > 0 {
				w.number = n
			}
		}
		w.start = start.Time
		if end.Valid {
			t := end.Time
			w.end = &t
			w.success = success
		} else {
			w.success = false
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func slowWorkAverages(rows []slowWork, now time.Time) map[string]float64 {
	type total struct {
		n       int
		seconds float64
	}
	totals := map[string]total{}
	valid := map[string]bool{"implementation-light": true, "implementation-standard": true, "implementation-heavy": true, "request-pr_create": true, "request-merge_manual": true, "request-full_test": true, "request-pr_close": true}
	for _, w := range rows {
		if !valid[w.group] || !w.success || w.end == nil || w.start.IsZero() || !w.end.After(w.start) || w.end.After(now) || w.end.Before(now.Add(-slowWorkWindow)) {
			continue
		}
		seconds := w.end.Sub(w.start).Seconds()
		if seconds <= 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
			continue
		}
		t := totals[w.group]
		t.n++
		t.seconds += seconds
		totals[w.group] = t
	}
	out := map[string]float64{}
	for k, t := range totals {
		avg := t.seconds / float64(t.n)
		if t.n >= 10 && avg > 0 && !math.IsInf(avg, 0) && !math.IsNaN(avg) {
			out[k] = avg
		}
	}
	return out
}

// SlowWorkSources may be composed with other sources by the startup integration.
func (s *Service) SlowWorkSources(ctx context.Context) ([]IncidentSource, error) {
	rows, err := s.slowWorkRows(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	averages := slowWorkAverages(rows, now)
	out := []IncidentSource{}
	for _, w := range rows {
		avg, ok := averages[w.group]
		if !ok || w.end != nil || w.start.IsZero() || w.start.After(now) {
			continue
		}
		elapsed := now.Sub(w.start).Seconds()
		if elapsed < 2*avg {
			continue
		}
		out = append(out, IncidentSource{Key: "slow-" + w.key, Kind: "slow", Title: "作業の遅延: " + w.title, Target: map[string]any{"project": w.repo, "number": w.number, "workKey": w.key, "workType": w.group, "host": w.host, "startedAt": iso(w.start), "elapsedSeconds": elapsed, "averageSeconds": avg}})
	}
	return out, nil
}

// ResolveSlowWorkNoProblem records the subsequent judgement for this exact work.
// The immutable source key suppresses another incident while that work continues.
func (s *Service) ResolveSlowWorkNoProblem(ctx context.Context, id string, expected ...map[string]any) error {
	return s.changeIncident(ctx, id, func(row map[string]any) error {
		if len(expected) > 0 {
			before, _ := json.Marshal(expected[0]["target"])
			after, _ := json.Marshal(row["target"])
			if row["key"] != expected[0]["key"] || row["state"] != "expired" || row["slowJudgement"] != nil || string(before) != string(after) {
				return fmt.Errorf("有事が判定中に変更されました")
			}
		}
		if row["kind"] != "slow" {
			return fmt.Errorf("遅延有事ではありません")
		}
		if row["endedAt"] != nil && row["slowJudgement"] != "no_problem" {
			return fmt.Errorf("終了済みの有事です")
		}
		if row["slowJudgement"] == "no_problem" {
			return nil
		}
		if row["slowJudgement"] != nil {
			return fmt.Errorf("判定済みの有事です")
		}
		row["slowJudgement"] = "no_problem"
		row["endedAt"] = iso(s.now())
		row["state"] = "resolved"
		row["acknowledgedAt"] = iso(s.now())
		s.incidentNote(row, "不具合なし", "同じ作業が続く間は再登録しません。")
		return nil
	})
}
