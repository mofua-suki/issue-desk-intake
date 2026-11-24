package operations

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/usage"
	"sort"
	"strings"
	"time"
)

func (s *Service) jobAttention(ctx context.Context, configured map[string]bool) ([]any, error) {
	out := []any{}
	if s.PostgreSQL == nil {
		return out, nil
	}
	rows, e := s.PostgreSQL.QueryContext(ctx, "SELECT id,repo,issue,title,status,hold_reason,view,result,updated_at FROM jobs WHERE (status='held' AND (hold_reason='prerequisite_missing' OR (hold_reason='question' AND view->'prerequisiteMissingGenerations' @> jsonb_build_array(generation)))) OR (status='queued' AND (view->'acceptanceTransferWait'->>'blocking'='true' OR jsonb_array_length(CASE WHEN jsonb_typeof(view->'acceptanceTransferWait'->'orphanedWaits')='array' THEN view->'acceptanceTransferWait'->'orphanedWaits' ELSE '[]'::jsonb END)>0)) ORDER BY id")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	for rows.Next() {
		var id, issue int64
		var repo, title, status string
		var reason *string
		var viewRaw, resultRaw []byte
		var updated any
		if e = rows.Scan(&id, &repo, &issue, &title, &status, &reason, &viewRaw, &resultRaw, &updated); e != nil {
			return nil, e
		}
		var view map[string]any
		if e = json.Unmarshal(viewRaw, &view); e != nil {
			return nil, e
		}
		result := jobs.StoredResult(id, resultRaw)
		if status == "held" {
			if !configured[repo] {
				continue
			}
			if item, ok := s.prerequisiteAttentionItem(repo, issue, title, view, result, updated); ok {
				out = append(out, item)
			}
		} else {
			out = append(out, acceptanceTransferAttentionItems(id, repo, issue, title, view, updated)...)
		}
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	rows.Close()
	stalls, e := s.dependencyStallAttention(ctx, configured)
	return append(out, stalls...), e
}

// prerequisiteAttentionItem は前提の不足で止まった依頼を、不足した成果物と自動処理の経過とともに要対応の項目にする。不足の記録が正しくなければ ok=false を返す。
func (s *Service) prerequisiteAttentionItem(repo string, issue int64, title string, view, result map[string]any, updated any) (map[string]any, bool) {
	missing := fleet.Object(result["prerequisiteMissing"])
	artifacts := fleet.List(missing["artifacts"])
	if missing == nil || len(artifacts) == 0 {
		return nil, false
	}
	details := []string{}
	safeArtifacts := []any{}
	valid := result["status"] == "failed" && result["incomplete"] == nil && len(fleet.List(result["questions"])) == 0
	for _, raw := range artifacts {
		a := fleet.Object(raw)
		if !fleet.ValidRepo(fleet.Text(a["repo"])) || fleet.Number(a["issue"]) < 1 || fleet.Text(a["artifact"]) == "" {
			valid = false
			break
		}
		copy := map[string]any{}
		for k, v := range a {
			copy[k] = v
		}
		copy["artifact"] = usage.Redact(fleet.Text(a["artifact"]))
		safeArtifacts = append(safeArtifacts, copy)
		details = append(details, fmt.Sprintf("%s（%s#%v）", copy["artifact"], a["repo"], a["issue"]))
	}
	if !valid {
		return nil, false
	}
	progress := []string{}
	started := updated
	for _, raw := range fleet.Object(view["prerequisiteResolution"]) {
		r := fleet.Object(raw)
		artifact := fleet.Object(r["artifact"])
		state := fleet.Text(r["state"])
		reason := usage.Redact(fleet.Text(r["reason"]))
		switch state {
		case "connected":
			reason = "依存を接続しました。 " + reason
		case "retried":
			reason = "再試行しました。 " + reason
		case "rejected":
			reason = "再試行が拒否されました。 " + reason
		case "limit_reached":
			reason = "自動処理の上限（3回）に達しました。 " + reason
		case "cycle":
			reason = "依存関係が循環します。 " + reason
		case "out_of_scope":
			reason = "対象外のリポジトリ: " + usage.Redact(fleet.Text(artifact["repo"])) + "。 " + reason
		}
		line := fmt.Sprintf("%s: %s %s", usage.Redact(fleet.Text(artifact["artifact"])), state, reason)
		if issue := fleet.Object(r["issue"]); issue != nil {
			line += fmt.Sprintf(" %v#%v", issue["project"], issue["number"])
		}
		for key, prefix := range map[string]string{"consultationId": " 相談 #/drafts/", "searchId": " 照合相談 #/drafts/"} {
			if id := fleet.Text(r[key]); id != "" {
				line += prefix + id
			}
		}
		progress = append(progress, line)
		if fleet.Text(r["startedAt"]) != "" && (started == updated || fleet.Text(r["startedAt"]) < fmt.Sprint(started)) {
			started = r["startedAt"]
		}
	}
	sort.Strings(progress)
	var desk any
	if s.Desk != nil {
		desk = s.Desk(repo, issue)
	}
	return map[string]any{"key": fmt.Sprintf("prerequisite:%s#%d", repo, issue), "kind": "prerequisite_missing", "kindName": "前提成果物不足", "state": "waiting", "stateLabel": "前提待ち（実装回数外）", "project": repo, "number": issue, "desk": desk, "title": "前提待ち: " + title + " — " + strings.Join(details, "／"), "reason": usage.Redact(fleet.Text(missing["reason"])) + "。" + strings.Join(progress, "／") + " 成果物を確認してから受付画面または issue-desk-issue-control Skillの再試行手順 で再試行してください。", "artifacts": safeArtifacts, "updatedAt": updated, "startedAt": started}, true
}

func acceptanceTransferAttentionItems(id int64, repo string, issue int64, title string, view map[string]any, updated any) []any {
	wait := fleet.Object(view["acceptanceTransferWait"])
	out := []any{}
	if wait["blocking"] == true {
		out = append(out, acceptanceTransferAttentionItem(id, repo, issue, title, view, updated))
	}
	if len(fleet.List(wait["orphanedWaits"])) > 0 {
		out = append(out, acceptanceTransferOrphanItem(id, repo, issue, title, view, updated))
	}
	return out
}

// acceptanceTransferAttentionItem は移管成果物を待つ依頼を、待っている成果物と移管先とともに要対応の項目にする。
func acceptanceTransferAttentionItem(id int64, repo string, issue int64, title string, view map[string]any, updated any) map[string]any {
	wait := fleet.Object(view["acceptanceTransferWait"])
	details := []string{}
	for _, raw := range fleet.List(wait["waits"]) {
		w := fleet.Object(raw)
		destination := "未起票の案 " + fleet.Text(w["proposalId"])
		if d := fleet.Object(w["destination"]); d != nil {
			destination = fmt.Sprintf("%v#%v", d["project"], d["number"])
		}
		line := fmt.Sprintf("%s（移管元 %v／%s）: %v", strings.Join(fleet.Strings(w["artifacts"]), "、"), w["source"], destination, w["reason"])
		if id := fleet.Text(w["consultationId"]); id != "" {
			line += " 相談: #/drafts/" + id
		}
		details = append(details, line)
	}
	return map[string]any{"key": fmt.Sprintf("acceptance-transfer:%d", id), "kind": "acceptance_transfer", "kindName": "移管成果物待ち", "state": "dependency_waiting", "stateLabel": "移管成果物待ち", "project": repo, "number": issue, "title": title, "reason": strings.Join(details, "\n") + "\n移管先の成果物を完成させPRをマージしてください。関係不明は表示した相談で根拠を照合してください。受付画面または運用代行トークンの相談返信・再送で解消し、再試行・再開だけでは解除されません。", "startedAt": wait["since"], "updatedAt": updated}
}

func acceptanceTransferOrphanItem(id int64, repo string, issue int64, title string, view map[string]any, updated any) map[string]any {
	wait := fleet.Object(view["acceptanceTransferWait"])
	details := []string{}
	for _, raw := range fleet.List(wait["orphanedWaits"]) {
		w := fleet.Object(raw)
		details = append(details, fmt.Sprintf("%s（移管元 %v／案 %s）: %s", strings.Join(fleet.Strings(w["artifacts"]), "、"), w["source"], fleet.Text(w["proposalId"]), fleet.Text(w["reason"])))
	}
	return map[string]any{"key": fmt.Sprintf("acceptance-transfer-orphan:%d", id), "kind": "acceptance_transfer_orphan", "kindName": "移管先の案記録なし", "state": "queued", "stateLabel": "要確認", "project": repo, "number": issue, "title": title, "reason": strings.Join(details, "\n"), "updatedAt": updated}
}

// dependencyStallAttention reads the persisted edge markers, which are cleared
// by the dependency state transitions. Grouping is by predecessor job.
func (s *Service) dependencyStallAttention(ctx context.Context, configured map[string]bool) ([]any, error) {
	rows, err := s.PostgreSQL.QueryContext(ctx, `SELECT p.id,p.repo,p.issue,p.title,p.status,p.updated_at,
 j.repo,j.issue,d.wait_started_at,d.wait_unknown
 FROM job_deps d JOIN jobs p ON p.id=d.depends_on JOIN jobs j ON j.id=d.job_id
 WHERE d.wait_unknown OR d.wait_started_at < $1
 ORDER BY p.id,j.repo,j.issue,j.id`, s.now().Add(-24*time.Hour))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []any
	var current int64 = -1
	var row map[string]any
	var followers []string
	var unknown bool
	var earliest *time.Time
	flush := func() {
		if row == nil {
			return
		}
		start := "待ち始め不明"
		if !unknown && earliest != nil {
			start = "待ち始め: " + earliest.Format(time.RFC3339)
			row["startedAt"] = *earliest
		}
		row["reason"] = fmt.Sprintf("先行の状態: %v。後続%d件: %s。%s。先行Issueの次の担当と対処を確認してください。", row["state"], len(followers), strings.Join(followers, "、"), start)
		out = append(out, row)
	}
	for rows.Next() {
		var id, issue, childIssue int64
		var repo, title, status, childRepo string
		var updated time.Time
		var started *time.Time
		var edgeUnknown bool
		if err = rows.Scan(&id, &repo, &issue, &title, &status, &updated, &childRepo, &childIssue, &started, &edgeUnknown); err != nil {
			return nil, err
		}
		if !configured[repo] || !configured[childRepo] {
			continue
		}
		if id != current {
			flush()
			current = id
			followers = nil
			unknown = false
			earliest = nil
			var desk any
			if s.Desk != nil {
				desk = s.Desk(repo, issue)
			}
			row = map[string]any{"key": fmt.Sprintf("dependency-stall:%d", id), "kind": "dependency_stall", "kindName": "先行PRのマージ待ち", "state": status, "stateLabel": status, "project": repo, "number": issue, "desk": desk, "title": title, "updatedAt": updated}
		}
		child := fmt.Sprintf("%s#%d", childRepo, childIssue)
		if s.Desk != nil {
			if desk := s.Desk(childRepo, childIssue); desk != nil {
				child = fmt.Sprintf("%v (%s)", desk, child)
			}
		}
		followers = append(followers, child)
		unknown = unknown || edgeUnknown
		if started != nil && (earliest == nil || started.Before(*earliest)) {
			earliest = started
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	flush()
	return out, nil
}
