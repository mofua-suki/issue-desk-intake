package operations

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"math"
	"sort"
	"strings"
	"time"
)

func dispatchHostReasons(r map[string]any, now time.Time, lease float64) []string {
	if r == nil {
		return []string{"状態の報告なし（ready・heartbeat 未確認）"}
	}
	out := []string{}
	labels := map[string]string{"swap_wait": "入れ替え待ち", "codex_unavailable": "Codex CLI利用不能", "environment_hold": "実行環境の問題", "usage_limit": "利用上限", "color_standby": "退く色", "scan_not_ready": "確認の未完了・終了中"}
	for _, raw := range fleet.List(fleet.Object(r["intakeHold"])["reasons"]) {
		reason := fleet.Object(raw)
		kind := fleet.Text(reason["kind"])
		label := labels[kind]
		if label == "" {
			label = kind
		}
		line := fmt.Sprintf("%s (%s) / 開始: %v", label, kind, reason["since"])
		if detail := fleet.Text(reason["detail"]); detail != "" {
			line += " / " + detail
		}
		if kind == "usage_limit" {
			line += fmt.Sprintf(" / 実行先: %v / 解除の見込み時刻: %v / 上限の種類: %v", reason["provider"], reason["until"], reason["limitKind"])
		}
		out = append(out, line)
	}
	if drain := fleet.Object(r["drain"]); drain != nil {
		label := "手動の drain"
		if strings.HasPrefix(fleet.Text(drain["by"]), "hostd:") {
			label = "hostd の更新待ちの drain"
		}
		until := fleet.Text(drain["until"])
		if drain["indefinite"] == true {
			until = "無期限"
		} else if until == "" {
			until = "不明"
		}
		out = append(out, fmt.Sprintf("%s / 開始: %v / %v / 期限: %s", label, drain["at"], drain["reason"], until))
	}
	implementing, iok := r["implementing"].(float64)
	publishing, pok := r["publishing"].(float64)
	maximum := fleet.Number(r["maxImplementations"])
	if r["maxImplementations"] != nil && maximum >= 0 {
		if capacity, ok := r["capacity"].(float64); ok && capacity >= 0 && capacity <= 8 && math.Trunc(capacity) == capacity && capacity < maximum {
			maximum = capacity
		}
		if iok && pok && implementing >= 0 && publishing >= 0 && implementing+publishing >= maximum {
			out = append(out, fmt.Sprintf("報告値では枠が埋まっている（実装中 %v / 公開中 %v / ホスト上限 %v）", implementing, publishing, maximum))
		}
	}
	if r["ready"] == false {
		out = append(out, "ready=false")
	}
	if r["ready"] == nil {
		out = append(out, "ready 未確認")
	}
	heartbeat, e := time.Parse(time.RFC3339Nano, fleet.Text(r["updatedAt"]))
	if e != nil || now.Sub(heartbeat) > time.Duration(lease*float64(time.Minute)) {
		out = append(out, fmt.Sprintf("heartbeat切れ（%v分より古い、または未報告） / 最終: %v", lease, r["updatedAt"]))
	}
	if len(out) == 0 {
		out = append(out, "配布側の現在の見送り理由は未確認")
	}
	return out
}

func dispatchIdleHostDetails(available []map[string]any, implementers map[string][]string, hostRows map[string]map[string]any, now time.Time, lease float64) []string {
	names := map[string]bool{}
	for _, row := range available {
		repo := fleet.Text(row["repo"])
		hosts := implementers[strings.ToLower(repo)]
		if len(hosts) == 0 {
			hosts = implementers[repo]
		}
		for _, host := range hosts {
			names[host] = true
		}
	}
	hosts := make([]string, 0, len(names))
	for host := range names {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	detail := []string{}
	for _, host := range hosts {
		for _, reason := range dispatchHostReasons(hostRows[host], now, lease) {
			detail = append(detail, host+": "+reason)
		}
	}
	return detail
}

func (s *Service) dispatchAlerts(ctx context.Context, hostView map[string]any) ([]any, error) {
	out := []any{}
	if s.PostgreSQL == nil || s.Assignments == nil {
		return out, nil
	}
	names := map[string]bool{}
	for _, list := range s.Assignments.Implementers {
		for _, name := range list {
			names[name] = true
		}
	}
	hosts := []string{}
	for h := range names {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	if len(hosts) == 0 {
		return out, nil
	}
	rawHosts, e := json.Marshal(hosts)
	if e != nil {
		return nil, e
	}
	rows, e := s.PostgreSQL.QueryContext(ctx, `SELECT row_to_json(v) FROM(SELECT j.id,j.repo,j.scope,j.status,j.updated_at,j.view,ARRAY(SELECT d.depends_on FROM job_deps d JOIN jobs p ON p.id=d.depends_on WHERE d.job_id=j.id AND NOT (`+jobs.DependencyPairReadySQL+`))waiting_on,(SELECT max(at)FROM job_events WHERE kind='leased' AND host IN(SELECT jsonb_array_elements_text($1::jsonb)))last_received_at FROM jobs j WHERE j.status IN('queued','running'))v`, string(rawHosts))
	if e != nil {
		return nil, e
	}
	list, _, e := appsql.JSONObjects[[]byte](rows, appsql.JSONRowsOptions{})
	if e != nil {
		return nil, e
	}
	counts, available, baseline, last, e := s.classifyQueuedJobs(ctx, list)
	if e != nil {
		return nil, e
	}
	now := s.now()
	lease := fleet.Number(hostView["leaseMinutes"])
	if lease <= 0 {
		lease = 20
	}
	hostRows := map[string]map[string]any{}
	for _, r := range fleet.List(hostView["hosts"]) {
		row := fleet.Object(r)
		hostRows[fleet.Text(row["name"])] = row
	}
	if alert := s.dispatchIdleAlert(counts, available, hostRows, baseline, last, now, lease); alert != nil {
		out = append(out, alert)
	}
	individual, e := s.jobDispatchAlerts(ctx, available, hostRows, lease)
	return append(out, individual...), e
}
func (s *Service) jobDispatchAlerts(ctx context.Context, list []map[string]any, hostRows map[string]map[string]any, lease float64) ([]any, error) {
	out := []any{}
	sort.Slice(list, func(i, j int) bool { return fleet.Text(list[i]["updated_at"]) < fleet.Text(list[j]["updated_at"]) })
	for _, row := range list {
		updated, e := time.Parse(time.RFC3339Nano, fleet.Text(row["updated_at"]))
		if e != nil || s.now().Sub(updated) < time.Hour {
			continue
		}
		repo := fleet.Text(row["repo"])
		hosts := s.Assignments.Implementers[strings.ToLower(repo)]
		if len(hosts) == 0 {
			hosts = s.Assignments.Implementers[repo]
		}
		raw, _ := json.Marshal(hosts)
		var other bool
		e = s.PostgreSQL.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM job_events WHERE kind='leased' AND host IN(SELECT jsonb_array_elements_text($1::jsonb)) AND job_id<>$2 AND at>$3 AND at<=$4)`, string(raw), int64(fleet.Number(row["id"])), updated, s.now()).Scan(&other)
		if e != nil {
			return nil, e
		}
		if !other {
			continue
		}
		var skipSeq int64
		var skipAt time.Time
		var skipHost string
		var data []byte
		e = s.PostgreSQL.QueryRowContext(ctx, "SELECT seq,at,host,data FROM job_events WHERE job_id=$1 AND kind='dispatch_skip' ORDER BY seq DESC LIMIT 1", int64(fleet.Number(row["id"]))).Scan(&skipSeq, &skipAt, &skipHost, &data)
		if e != nil && e != sql.ErrNoRows {
			return nil, e
		}
		detail := []string{fmt.Sprintf("実行待ち: %d分（担当PCは待ち始めより後に別のジョブを受け取っています）", int(s.now().Sub(updated).Minutes()))}
		if e == nil {
			var value map[string]any
			if e = json.Unmarshal(data, &value); e != nil {
				return nil, e
			}
			detail = append(detail, fmt.Sprintf("直近の見送り: %v / 取得PC: %s / %s（%d分前）", value["reason"], skipHost, iso(skipAt), int(s.now().Sub(skipAt).Minutes())))
			if value["reason"] == "scope_reserved" {
				info, err := s.reservationSkipDetail(ctx, value["by"], skipSeq, skipAt)
				if err != nil {
					return nil, err
				}
				detail = append(detail, info)
			}
		} else {
			detail = append(detail, "直近の見送り: 見送りの記録なし")
		}
		for _, host := range hosts {
			for _, reason := range dispatchHostReasons(hostRows[host], s.now(), lease) {
				detail = append(detail, host+": "+reason+"（配布側の現在の見送り理由は未確認）")
			}
		}
		out = append(out, map[string]any{"id": fmt.Sprintf("job-dispatch-stall-%.0f", fleet.Number(row["id"])), "kind": "job_dispatch_stall", "status": "stall_alert", "project": repo, "title": "実行待ちの仕事が60分以上配られていません", "detail": strings.Join(detail, "\n"), "detectedAt": iso(updated.Add(time.Hour))})
		if len(out) >= 50 {
			break
		}
	}
	return out, nil
}

// reservationSkipDetail reconstructs the reservation age from events preceding the
// skip in insertion order, matching jobs.newReservation's age reset rules.
func (s *Service) reservationSkipDetail(ctx context.Context, by any, skipSeq int64, skipAt time.Time) (string, error) {
	id, ok := by.(float64)
	if !ok || id <= 0 || math.Trunc(id) != id || id >= 1<<53 {
		return "過去の予約による見送り / 予約元: 不明 / 見送り記録時の待ち時間: 不明", nil
	}
	identity, filed, found, err := s.reservationIdentity(ctx, id)
	if err != nil {
		return "", err
	}
	if !found {
		return "過去の予約による見送り / 予約元: " + identity + "（取得不可） / 見送り記録時の待ち時間: 不明", nil
	}
	since, known, err := s.reservationWaitSince(ctx, id, skipSeq, filed)
	if err != nil {
		return "", err
	}
	wait := "不明"
	if known && !since.After(skipAt) {
		wait = fmt.Sprintf("%d分", int(skipAt.Sub(since).Minutes()))
	}
	return "過去の予約による見送り / 予約元: " + identity + " / 見送り記録時の待ち時間: " + wait, nil
}

// classifyQueuedJobs は実行待ちのジョブを、配れるもの・依存待ち・担当PCなし・範囲待ちに数え、最後の受け取りの時刻と待ちの基準時刻を返す。
func (s *Service) classifyQueuedJobs(ctx context.Context, list []map[string]any) (map[string]int, []map[string]any, time.Time, any, error) {
	counts := map[string]int{"available": 0, "dependency": 0, "unassigned": 0, "scope": 0}
	available := []map[string]any{}
	baseline := s.StartedAt
	if baseline.IsZero() {
		baseline = s.now()
	}
	var last any
	for _, row := range list {
		if at, e := time.Parse(time.RFC3339Nano, fleet.Text(row["last_received_at"])); e == nil {
			last = iso(at)
			if at.After(baseline) {
				baseline = at
			}
		}
		if row["status"] != "queued" {
			continue
		}
		repo := fleet.Text(row["repo"])
		refsReady, e := jobs.DependenciesReady(ctx, s.PostgreSQL, int64(fleet.Number(row["id"])), fleet.Object(row["view"]))
		if e != nil {
			return nil, nil, time.Time{}, nil, e
		}
		// A blocking acceptance transfer is never leased (jobs.Next), so it is waiting, not stalled.
		transferWaiting := fleet.Object(fleet.Object(row["view"])["acceptanceTransferWait"])["blocking"] == true
		if !refsReady || transferWaiting || len(fleet.List(row["waiting_on"])) > 0 {
			counts["dependency"]++
			continue
		}
		assigned := s.Assignments.Implementers[strings.ToLower(repo)]
		if len(assigned) == 0 {
			assigned = s.Assignments.Implementers[repo]
		}
		if len(assigned) == 0 {
			counts["unassigned"]++
			continue
		}
		blocked := false
		exempt := s.Assignments.Exempt(repo)
		for _, busy := range list {
			if busy["status"] == "running" && strings.EqualFold(fleet.Text(busy["repo"]), repo) && jobs.ScopeOverlap(fleet.Strings(row["scope"]), fleet.Strings(busy["scope"]), exempt) {
				blocked = true
				break
			}
		}
		if blocked {
			counts["scope"]++
			continue
		}
		counts["available"]++
		available = append(available, row)
	}
	return counts, available, baseline, last, nil
}

// dispatchIdleAlert は配れる仕事があるのに、どの実装PCも20分以上新しい仕事を受け取っていないときの要対応を作る。該当しなければ nil を返す。
func (s *Service) dispatchIdleAlert(counts map[string]int, available []map[string]any, hostRows map[string]map[string]any, baseline time.Time, last any, now time.Time, lease float64) map[string]any {
	if counts["available"] <= 0 || now.Sub(baseline) < 20*time.Minute {
		return nil
	}
	lastText := "記録なし（受付の起動時刻から計測）"
	if last != nil {
		lastText = fleet.Text(last)
	}
	detail := []string{fmt.Sprintf("配布可能: %d件 / 全実装PCが20分以上新しい仕事を受け取っていません。", counts["available"]), fmt.Sprintf("除外: 依存待ち %d件 / 担当PCなし %d件 / 範囲待ち %d件", counts["dependency"], counts["unassigned"], counts["scope"]), "最後の受け取り: " + lastText}
	detail = append(detail, dispatchIdleHostDetails(available, s.Assignments.Implementers, hostRows, now, lease)...)
	return map[string]any{"id": "dispatch-idle", "kind": "dispatch_idle", "status": "stall_alert", "project": fleet.SystemProject, "title": "実行待ちの仕事をどのPCも受け取っていません", "detail": strings.Join(detail, "\n"), "detectedAt": iso(baseline.Add(20 * time.Minute))}
}

// reservationIdentity は予約元のジョブのリポジトリ・Issue・題名を表示の形にし、起票の時刻を返す。ジョブが無ければ found=false を返す。
func (s *Service) reservationIdentity(ctx context.Context, id float64) (string, sql.NullTime, bool, error) {
	identity := fmt.Sprintf("ジョブID: %.0f", id)
	var repo, title sql.NullString
	var issue sql.NullInt64
	var filed sql.NullTime
	err := s.PostgreSQL.QueryRowContext(ctx, "SELECT repo,issue,title,filed_at FROM jobs WHERE id=$1", int64(id)).Scan(&repo, &issue, &title, &filed)
	if err == sql.ErrNoRows {
		return identity, filed, false, nil
	}
	if err != nil {
		return "", filed, false, err
	}
	field := func(v sql.NullString) string {
		if !v.Valid || strings.TrimSpace(v.String) == "" {
			return "不明"
		}
		return v.String
	}
	issueText := "不明"
	if issue.Valid && issue.Int64 > 0 {
		issueText = fmt.Sprintf("#%d", issue.Int64)
	}
	identity += fmt.Sprintf(" / リポジトリ: %s / Gitea Issue: %s / 題名: %s", field(repo), issueText, field(title))
	return identity, filed, true, nil
}

// reservationWaitSince は見送りより前の出来事を順にたどり、予約元が待ち始めた時刻を新しい予約の規則と同じに復元する。時刻が分からなければ known=false を返す。
func (s *Service) reservationWaitSince(ctx context.Context, id float64, skipSeq int64, filed sql.NullTime) (time.Time, bool, error) {
	rows, err := s.PostgreSQL.QueryContext(ctx, `SELECT kind,at,data FROM job_events WHERE job_id=$1 AND seq<$2 AND (kind IN ('requeued','lease_expired','leased','state') OR kind='control' AND data->>'action' IN ('retry','resume','approve_deletions','permission_approval')) ORDER BY seq`, int64(id), skipSeq)
	if err != nil {
		return time.Time{}, false, err
	}
	defer rows.Close()
	since := filed.Time
	known := filed.Valid && !since.IsZero()
	var before time.Time
	var beforeKnown, haveLease bool
	for rows.Next() {
		var kind string
		var at time.Time
		var raw []byte
		if err = rows.Scan(&kind, &at, &raw); err != nil {
			return time.Time{}, false, err
		}
		var data map[string]any
		if err = json.Unmarshal(raw, &data); err != nil {
			return time.Time{}, false, err
		}
		if kind == "leased" {
			before, beforeKnown, haveLease = since, known, true
		}
		if kind == "lease_expired" && data["to"] == "queued" && haveLease {
			since, known, haveLease = before, beforeKnown, false
		}
		reset := kind == "requeued" || kind == "state" && data["to"] == "queued" || kind == "control" && (data["action"] == "retry" || data["action"] == "resume" || data["action"] == "approve_deletions" || data["action"] == "permission_approval")
		if reset && known && at.After(since) {
			since = at
		}
	}
	if err = rows.Err(); err != nil {
		return time.Time{}, false, err
	}
	return since, known, nil
}
