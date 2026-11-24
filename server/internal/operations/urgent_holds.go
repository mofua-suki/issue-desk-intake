package operations

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"strings"
	"time"
)

func (s *Service) urgentHolds(ctx context.Context) ([]any, error) {
	out := []any{}
	if s.PostgreSQL == nil {
		return out, nil
	}
	rows, e := s.PostgreSQL.QueryContext(ctx, "SELECT row_to_json(j)FROM jobs j WHERE(priority=1 OR kind='stall_fix')AND((status='held' AND hold_reason='review_held' AND j.view->'finalApprovalHold' IS NULL)OR status='failed')AND updated_at<=$1 ORDER BY updated_at,id", s.now().Add(-30*time.Minute))
	if e != nil {
		return nil, e
	}
	list, _, e := appsql.JSONObjects[[]byte](rows, appsql.JSONRowsOptions{})
	if e != nil {
		return nil, e
	}
	// Bound each external lookup phase across the whole alert request.
	lookupTimeout := s.UrgentLookupTimeout
	if lookupTimeout <= 0 {
		lookupTimeout = time.Second
	}
	lookupCtx, cancel := context.WithTimeout(ctx, lookupTimeout)
	closedIssues := s.lookupUrgentIssues(lookupCtx, list)
	cancel()
	commentsCtx, cancelComments := context.WithTimeout(ctx, time.Second)
	defer cancelComments()
	for _, r := range list {
		repo := fleet.Text(r["repo"])
		issue := int64(fleet.Number(r["issue"]))
		key := urgentIssueKey(repo, issue)
		if closedIssues[key] {
			continue
		}
		waiting, e := s.humanMergeWait(ctx, commentsCtx, repo, issue)
		if e != nil {
			return nil, e
		}
		if waiting {
			continue
		}
		var unchanged bool
		e = s.PostgreSQL.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM jobs WHERE id=$1 AND status=$2 AND updated_at=$3)", int64(fleet.Number(r["id"])), r["status"], r["updated_at"]).Scan(&unchanged)
		if e != nil {
			return nil, e
		}
		if !unchanged {
			continue
		}
		updated, e := time.Parse(time.RFC3339Nano, fleet.Text(r["updated_at"]))
		if e != nil {
			continue
		}
		reasons := []string{}
		if r["priority"] == float64(1) {
			reasons = append(reasons, "割り込み")
		}
		if r["kind"] == "stall_fix" {
			reasons = append(reasons, "障害の直し")
		}
		state := "失敗（failed）"
		if r["status"] == "held" {
			state = "保留（review_held）"
		}
		reason := "理由の記録なし"
		for _, field := range []string{"error", "summary"} {
			v, ok := fleet.Object(r["result"])[field].(string)
			if !ok || strings.TrimSpace(v) == "" {
				continue
			}
			safe := []rune(usage.Redact(v))
			if len(safe) > 300 {
				safe = safe[:300]
			}
			reason = string(safe)
			break
		}
		detail := []string{fmt.Sprintf("依頼: %s#%d %s", usage.Redact(repo), issue, usage.Redact(fleet.Text(r["title"]))), "対象の理由: " + strings.Join(reasons, "／"), fmt.Sprintf("状態: %s / %d分停止", state, int(s.now().Sub(updated).Minutes())), "保留・失敗の理由: " + reason, "保留の指摘: 分類は未確認（jobs の報告に指摘の種類・互換性の記録がありません）", "次に取れる手: 修正指示.md に原因と修正指示を書き、次の雛形で返信できます。", fmt.Sprintf("~/.issue-desk/desk reply-issue.ts --project %s --number %d --file <修正指示.md>", repo, issue), "直接PRを利用する場合は、利用者の明示許可を得て専用ブランチで修正・検証します。push済みなら --branch、パッチなら --patch と --base（40桁SHA）を使います。", "PR作成は merge-manual-pr Skillの汎用 operation 手順（POST /api/v1/requests）で、branch方式またはpatch方式を指定します。"}
		out = append(out, map[string]any{"id": fmt.Sprintf("urgent-fix-hold-%.0f", fleet.Number(r["id"])), "kind": "urgent_fix_hold", "status": "stall_alert", "project": repo, "title": "急ぎの直しが止まっています", "detectedAt": iso(updated.Add(30 * time.Minute)), "detail": strings.Join(detail, "\n")})
		if len(out) >= 50 {
			break
		}
	}
	return out, nil
}
