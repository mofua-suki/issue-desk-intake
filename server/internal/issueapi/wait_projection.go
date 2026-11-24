package issueapi

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"strings"
)

func transferReason(job *jobs.Job) string {
	wait := fleet.ObjectOrEmpty(job.View["acceptanceTransferWait"])
	if wait["blocking"] != true {
		return ""
	}
	lines := []string{}
	for _, raw := range fleet.Items(wait["waits"]) {
		w := fleet.ObjectOrEmpty(raw)
		artifacts := []string{}
		for _, v := range fleet.Items(w["artifacts"]) {
			if text, ok := v.(string); ok {
				artifacts = append(artifacts, text)
			}
		}
		destination := ""
		d := fleet.ObjectOrEmpty(w["destination"])
		if len(d) > 0 {
			destination = fmt.Sprintf("%s#%d", fleet.Text(d["project"]), int64Number(d["number"]))
		} else {
			proposal := fleet.Text(w["proposalId"])
			if proposal == "" {
				proposal = "未確認"
			}
			destination = "未起票の案 " + proposal
		}
		line := fmt.Sprintf("%s（移管元 %s／%s）: %s", strings.Join(artifacts, "、"), fleet.Text(w["source"]), destination, fleet.Text(w["reason"]))
		if id := fleet.Text(w["consultationId"]); id != "" {
			line += " 相談: #/drafts/" + id
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n") + "\n移管先の成果物を完成させPRをマージしてください。関係不明は表示した相談で根拠を照合してください。受付画面または運用代行トークンの相談返信・再送で解消し、再試行・再開だけでは解除されません。"
}
func (s *Service) scopeWait(ctx context.Context, repo string, job *jobs.Job) (Object, error) {
	if job.Status != "held" || job.HoldReason == nil || *job.HoldReason != "scope_wait" {
		return nil, nil
	}
	if s.o.Postgres == nil {
		return nil, fail(503, "変更範囲の待ち先を確認できません。")
	}
	rows, e := s.o.Postgres.QueryContext(ctx, `SELECT issue,array_to_json(scope),view->>'phase' FROM jobs WHERE id<>$1 AND lower(repo)=lower($2) AND status='running' ORDER BY id`, job.ID, repo)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	waitFor := []any{}
	names := []string{}
	lookup := s.numberLookup(ctx, repo)
	for rows.Next() {
		var n int64
		var raw []byte
		var phase *string
		if e = rows.Scan(&n, &raw, &phase); e != nil {
			return nil, e
		}
		paths := []string{}
		if e = json.Unmarshal(raw, &paths); e != nil {
			return nil, e
		}
		if !jobs.ScopeOverlap(job.Scope, paths, nil) || len(waitFor) >= 10 {
			continue
		}
		status := "running"
		if phase != nil && oneOf(*phase, "running", "publishing", "releasing", "deploying") {
			status = *phase
		}
		waitFor = append(waitFor, Object{"issue": n, "status": status})
		names = append(names, shownNumber(fmt.Sprint(n), lookup))
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	who := strings.Join(names, "、")
	if who == "" {
		who = "（今は無し）"
	}
	since := job.UpdatedAt
	if since == "" {
		since = "不明"
	}
	return Object{"since": job.UpdatedAt, "waitFor": waitFor, "note": fmt.Sprintf("変更範囲が重なる実行中の依頼 %s が終わるのを待っています（待ちの開始 %s）。相手が終わると、受付が自動で実行待ちへ戻します。", who, since)}, nil
}
