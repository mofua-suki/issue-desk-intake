package issueapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/usage"
	"math"
	"sort"
	"time"
)

// dispatchDetailKey selects detailed history while assembling a detail row.
type dispatchDetailKey struct{}

func dispatchValue(key string, value any) any {
	switch v := value.(type) {
	case string:
		limit := 100
		if key == "error" || key == "message" {
			limit = 300
		}
		return clipped(usage.Redact(v), limit)
	case float64:
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			return v
		}
	case bool:
		return v
	}
	return nil
}

// leaseDiagnosticsProjection は詳細だけに保存済みの診断情報を返す。
func leaseDiagnosticsProjection(result Object) Object {
	out := Object{}
	if count, ok := result["leaseExpiredCount"].(float64); ok && !math.IsNaN(count) && !math.IsInf(count, 0) {
		out["leaseExpiredCount"] = count
	}
	for _, key := range []string{"lastHost", "lastStage"} {
		if value, ok := result[key].(string); ok {
			out[key] = dispatchValue(key, value)
		}
	}
	if raw, ok := result["lastExecutionError"].(map[string]any); ok {
		clean := Object{}
		kind, _ := raw["kind"].(string)
		for _, key := range []string{"kind", "at", "stage", "reason"} {
			if value, ok := raw[key].(string); ok {
				clean[key] = dispatchValue(key, value)
			}
		}
		for _, key := range []string{"intakeHttpStatus", "giteaHttpStatus"} {
			if key == "intakeHttpStatus" && kind != "api_rejected" {
				continue
			}
			if value, ok := raw[key].(string); ok && len(value) == 3 && value[0] >= '1' && value[0] <= '5' && value[1] >= '0' && value[1] <= '9' && value[2] >= '0' && value[2] <= '9' {
				clean[key] = value
			}
		}
		if value, ok := raw["intakeCode"].(string); ok && kind == "api_rejected" {
			switch value {
			case "request_failed", "turn_lost", "contract_mismatch":
				clean["intakeCode"] = value
			}
		}
		if len(clean) > 0 {
			out["lastExecutionError"] = clean
		}
	}
	return out
}

func (s *Service) dispatchProjection(ctx context.Context, job *jobs.Job, detail bool) (Object, Object) {
	if s.o.Postgres == nil {
		return nil, nil
	}
	events, skip, ok := s.dispatchEvents(ctx, job, detail)
	if !ok {
		return nil, nil
	}
	return dispatchRecord(job, events), skip
}

// dispatchEvents は依頼の最近の配布の出来事を表示の形で読み、最後の見送りを返す。読めなければ ok=false を返す。
func (s *Service) dispatchEvents(ctx context.Context, job *jobs.Job, detail bool) ([]any, Object, bool) {
	rows, e := s.o.Postgres.QueryContext(ctx, `SELECT kind,at,host,data FROM job_events WHERE job_id=$1 ORDER BY seq DESC LIMIT 1000`, job.ID)
	if e != nil {
		return nil, nil, false
	}
	defer rows.Close()
	fields := map[string][]string{"dispatch_skip": {"reason", "by", "holder", "host", "release"}, "scope_reserved": {"reason", "by", "holder", "host", "release"}, "handed_back": {"reason", "error"}, "leased": {"epoch", "generation"}, "requeued": {"from"}}
	if detail {
		fields["lease_expired"] = []string{"generation", "to"}
		// 断った最終報告の理由は、マージ後に報告が届かず lease が切れ続ける原因を確かめる唯一の記録。
		fields["report_rejected"] = []string{"code", "status", "reportKind", "count", "lastAt", "message"}
	}
	events := []any{}
	// report_rejected は断りの種類ごとに1行で、再発すると時刻だけ更新され seq は古いまま残る。
	// 20件の枠とは別に必ず含め、更新後の時刻の位置へ並べる。
	rejections := []Object{}
	skip := Object(nil)
	for rows.Next() {
		var kind string
		var at time.Time
		var host sql.NullString
		var raw []byte
		if e = rows.Scan(&kind, &at, &host, &raw); e != nil {
			return nil, nil, false
		}
		data := Object{}
		if e = json.Unmarshal(raw, &data); e != nil {
			return nil, nil, false
		}
		var hostname any
		if host.Valid {
			hostname = clipped(usage.Redact(host.String), 100)
		}
		atText := contract.ISO(at)
		if kind == "dispatch_skip" && skip == nil {
			skip = Object{"reason": dispatchValue("reason", data["reason"]), "host": hostname, "at": atText}
		}
		keys, has := fields[kind]
		if !has || kind != "report_rejected" && len(events) >= 20 {
			continue
		}
		event := Object{"at": atText, "host": hostname, "kind": kind}
		for _, key := range keys {
			if value := dispatchValue(key, data[key]); value != nil {
				event[key] = value
			}
		}
		if kind == "report_rejected" {
			event["sortAt"] = at
			rejections = append(rejections, event)
			continue
		}
		event["sortAt"] = at
		events = append(events, event)
	}
	if rows.Err() != nil {
		return nil, nil, false
	}
	return withRejections(events, rejections), skip, true
}

// withRejections は断った報告を、時刻の新しい順に並ぶ位置へ差し込み、並べ替え用の時刻を外す。
func withRejections(events []any, rejections []Object) []any {
	sort.SliceStable(rejections, func(i, j int) bool {
		return rejections[i]["sortAt"].(time.Time).After(rejections[j]["sortAt"].(time.Time))
	})
	out := make([]any, 0, len(events)+len(rejections))
	next := 0
	for _, raw := range events {
		event := raw.(Object)
		for next < len(rejections) && !rejections[next]["sortAt"].(time.Time).Before(event["sortAt"].(time.Time)) {
			out = append(out, rejections[next])
			next++
		}
		out = append(out, event)
	}
	for ; next < len(rejections); next++ {
		out = append(out, rejections[next])
	}
	for _, raw := range out {
		delete(raw.(Object), "sortAt")
	}
	return out
}

// dispatchRecord は依頼の範囲・継続の印・配布の出来事を、配布の記録として表示の形にする。
func dispatchRecord(job *jobs.Job, events []any) Object {
	scope := []any{}
	for index, path := range job.Scope {
		if index >= 10 {
			break
		}
		scope = append(scope, clipped(usage.Redact(path), 200))
	}
	continuation := Object{"present": false}
	if value, ok := job.View["continuation"].(map[string]any); ok {
		continuation["present"] = true
		for _, key := range []string{"holder", "host", "to", "at", "generation", "pending"} {
			if value := dispatchValue(key, value[key]); value != nil {
				continuation[key] = value
			}
		}
	}
	var host any
	if job.LeaseHost != nil {
		host = *job.LeaseHost
	}
	record := Object{"status": job.Status, "leaseHost": host, "scope": scope, "scopeSource": job.ScopeSource, "continuation": continuation, "events": events}
	if len(job.Scope) > 10 {
		record["scopeCount"] = len(job.Scope)
	}
	return record
}
