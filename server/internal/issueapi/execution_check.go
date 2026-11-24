package issueapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/usage"
	"issue-desk-intake/server/internal/workplan"
	"strings"
)

func (s *Service) recordExecutionCheck(ctx context.Context, repo string, n int64, id string, data Object, p auth.Principal) (Object, error) {
	if p.Kind == auth.PC && !p.Delegated {
		return nil, fail(403, "運用の代行を許したトークンが必要です。")
	}
	check, e := s.decodeExecutionCheck(data, id)
	if e != nil {
		return nil, e
	}
	job, e := s.o.Jobs.ByIssue(ctx, repo, n)
	if e != nil {
		return nil, e
	}
	if job == nil {
		return nil, fail(404, "対象の仕事がありません。")
	}
	plan, e := workplan.Parse(job.View["executionPlan"])
	if e != nil || plan == nil || plan.Fingerprint() != check.PlanSHA || job.HeadSHA == nil || *job.HeadSHA != check.Head || job.Generation != check.Generation {
		return nil, fail(409, "確認対象の計画・世代・SHAが変わりました。")
	}
	record := ""
	for _, r := range plan.Requirements {
		if r.ID == check.Requirement && r.EvidenceKind == "report_comment" {
			if check.Status != "passed" || strings.TrimSpace(check.Detail) == "" || len(check.Detail) > 8000 {
				return nil, fail(400, "訂正報告を入力してください。")
			}
			if record, e = s.postCheckReport(ctx, repo, n, id, check, p, record); e != nil {
				return nil, e
			}
		}
	}
	saved, e := s.o.Jobs.RecordExecutionCheck(ctx, job.ID, check, via(p), record)
	if e != nil {
		return nil, e
	}
	return Object{"id": id, "executionProgress": executionPlanProjection(saved)}, nil
}

// decodeExecutionCheck は確認記録を読み、送信IDとの一致と内容を検査して、根拠と詳細を伏せ字にする。
func (s *Service) decodeExecutionCheck(data Object, id string) (jobs.ExecutionCheck, error) {
	var check jobs.ExecutionCheck
	raw, e := json.Marshal(data["executionCheck"])
	if e != nil {
		return check, fail(400, "確認記録を読めません。")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&check) != nil || check.ID != id {
		return check, fail(400, "送信IDと確認記録が一致しません。")
	}
	if e := jobs.ValidateExecutionCheck(check, s.o.Now()); e != nil {
		return check, e
	}
	check.Source = usage.Redact(check.Source)
	check.Detail = usage.Redact(check.Detail)
	return check, nil
}

// postCheckReport は訂正報告を Issue へ1回だけ投稿し、記録に使うコメントを返す。投稿の前に意図を保存し、結果を照合できない再投稿は断る。
func (s *Service) postCheckReport(ctx context.Context, repo string, n int64, id string, check jobs.ExecutionCheck, p auth.Principal, record string) (string, error) {
	marker := "<!-- issue-desk-check:" + id + " -->"
	body := check.Detail + "\n根拠: " + check.Source + "\n対象: " + check.Head + "\n" + marker
	comments, e := s.o.Gitea.Comments(ctx, repo, n)
	if e != nil {
		return "", e
	}
	for _, comment := range comments {
		if fleet.Text(comment["body"]) == body && s.o.Own(fleet.ObjectOrEmpty(comment["user"])) {
			record = fmt.Sprintf("comment:%v", comment["id"])
			break
		}
	}
	if record == "" {
		// Save the immutable intent before posting, and reconcile the marker on replay.
		saved, e := s.read(ctx, "actions", id)
		if e != nil {
			return "", e
		}
		if saved != nil && (fleet.Text(saved["body"]) != body || fleet.Text(saved["repo"]) != repo || saved["number"] != float64(n)) {
			return "", fail(409, "送信IDは別の報告で使用済みです。")
		}
		if saved != nil && saved["attempted"] == true {
			return "", fail(409, "訂正報告の投稿結果を照合中です。同じIDで再読取してください。")
		}
		intent := Object{"repo": repo, "number": n, "body": body, "attempted": true, "actor": via(p)}
		if e = s.save(ctx, "actions", id, intent); e != nil {
			return "", e
		}
		comment, e := s.o.Gitea.Comment(ctx, repo, n, body)
		if e != nil {
			return "", e
		}
		record = fmt.Sprintf("comment:%v", comment["id"])
	}
	return record, nil
}
