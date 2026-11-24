package jobs

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/testcause"
	"issue-desk-intake/server/internal/workplan"
	"strings"
	"time"
)

type ControllerTest struct {
	PlanSHA string         `json:"planSHA"`
	Test    map[string]any `json:"test"`
}
type ControllerReport struct {
	PlanSHA string `json:"planSHA"`
	Head    string `json:"head"`
	Marker  string `json:"marker"`
	BodySHA string `json:"bodySHA"`
}

func receiptHead(current *Job, report Report) string {
	if report.HeadSHA.Value != nil {
		return *report.HeadSHA.Value
	}
	if current.HeadSHA != nil {
		return *current.HeadSHA
	}
	return ""
}

// Only dedicated controller events can create receipts. Free model Result
// data cannot supply an owner, host, generation or successful receipt.
func (s *Store) bindExecutionReceipts(ctx context.Context, tx *sql.Tx, current *Job, report Report, view map[string]any, host string, at time.Time) error {
	if report.ControllerTest == nil && report.ControllerReport == nil {
		return nil
	}
	plan, err := workplan.Parse(current.View["executionPlan"])
	if err != nil || plan == nil {
		return contract.NewProblem(409, "受入条件の実行計画がありません。", "execution_plan_mismatch")
	}
	evidence, err := workplan.DecodeEvidence(current.View["executionReceipts"])
	if err != nil {
		return contract.NewProblem(409, "保存済みの確認証拠を読めません。", "execution_evidence_invalid")
	}
	head := receiptHead(current, report)
	if !shaPattern.MatchString(head) {
		return contract.NewProblem(409, "確認証拠の対象SHAがありません。", "execution_head_mismatch")
	}
	add := func(kind, record string) {
		for _, r := range plan.Requirements {
			if r.EvidenceKind != kind {
				continue
			}
			next := workplan.Evidence{Requirement: r.ID, PlanSHA: plan.Fingerprint(), Head: head, Generation: current.Generation, RecordID: record, Status: "passed", Source: r.Owner, Detail: record}
			kept := evidence[:0]
			for _, old := range evidence {
				if old.Requirement != r.ID {
					kept = append(kept, old)
				}
			}
			evidence = append(kept, next)
		}
	}
	if proof := report.ControllerTest; proof != nil {
		observed, target, err := s.verifyControllerTest(ctx, current, report, proof, plan, head)
		if err != nil {
			return err
		}
		view["executionTestRecord"] = map[string]any{"host": host, "generation": current.Generation, "epoch": report.Epoch, "at": iso(at), "test": proof.Test, "reportedHead": head}
		add("fixed_test", fmt.Sprintf("fixed-test:%s:%d:%v:%s", host, report.Epoch, target["at"], observed))
	}
	if proof := report.ControllerReport; proof != nil {
		legacy := fmt.Sprintf("<!-- issue-desk-execution-report:%d:%s:%s -->", current.ID, head, plan.Fingerprint())
		modern := fmt.Sprintf("<!-- issue-desk-execution-report:%d:g%d:%s:%s -->", current.ID, current.Generation, head, plan.Fingerprint())
		if proof.PlanSHA != plan.Fingerprint() || proof.Head != head || (proof.Marker != legacy && proof.Marker != modern) || s.options.ReadComments == nil || s.options.ReportAuthor == nil {
			return contract.NewProblem(409, "訂正報告の対象を照合できません。", "execution_report_invalid")
		}
		authors := map[string]bool{}
		if author := s.options.ReportAuthor(host); author != "" {
			authors[author] = true
		}
		if proof.Marker == modern {
			rows, err := tx.QueryContext(ctx, `SELECT host, data FROM job_events WHERE job_id=$1 AND kind='leased'`, current.ID)
			if err != nil {
				return err
			}
			for rows.Next() {
				var prior sql.NullString
				var raw []byte
				if err := rows.Scan(&prior, &raw); err != nil {
					rows.Close()
					return err
				}
				var data map[string]any
				if json.Unmarshal(raw, &data) != nil {
					continue
				}
				generation, genOK := count(data["generation"])
				epoch, epochOK := count(data["epoch"])
				if prior.Valid && genOK && epochOK && generation == current.Generation && epoch > 0 && epoch < current.LeaseEpoch {
					if author := s.options.ReportAuthor(prior.String); author != "" {
						authors[author] = true
					}
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
		}
		found, e := s.findControllerReportComment(ctx, current, proof, proof.Marker, authors)
		if e != nil {
			return e
		}
		add("report_comment", found)
	}
	raw, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	var rows []any
	if err = json.Unmarshal(raw, &rows); err != nil {
		return err
	}
	view["executionReceipts"] = rows
	view["executionProofFingerprint"] = executionProofFingerprint(report)
	return nil
}

// verifyControllerTest は固定検証の証拠が今の計画で対象版（マージ済みならその元の版）を完全に通したものかを照合し、観測した版と対象を返す。
func (s *Store) verifyControllerTest(ctx context.Context, current *Job, report Report, proof *ControllerTest, plan *workplan.Plan, head string) (string, map[string]any, error) {
	raw, _ := json.Marshal(proof.Test)
	cause := testcause.Classify(proof.Test["causeObservation"], nil)
	target, _ := cause["target"].(map[string]any)
	observed, _ := target["sha"].(string)
	bound := observed == head
	if !bound && (report.Status == "merged" || report.Status == "held" && report.View["codeMerged"] == true) {
		var err error
		bound, err = s.executionMergedHead(ctx, current, report, observed, head)
		if err != nil {
			return "", nil, err
		}
	}
	diagnostic, _ := proof.Test["diagnostic"].(map[string]any)
	if len(raw) > 64*1024 || proof.PlanSHA != plan.Fingerprint() || !bound || target["complete"] != true || target["passed"] != true || target["source"] != "fixed-test" || diagnostic["reason"] != "passed" || diagnostic["head"] != observed {
		return "", nil, contract.NewProblem(409, "対象版の完全な固定検証証拠ではありません。", "execution_test_invalid")
	}
	return observed, target, nil
}

// findControllerReportComment は報告者のアカウントが投稿した、印と本文の指紋が一致する訂正報告のコメントを探す。
func (s *Store) findControllerReportComment(ctx context.Context, current *Job, proof *ControllerReport, marker string, authors map[string]bool) (string, error) {
	comments, e := s.options.ReadComments(ctx, current.Repo, current.Issue)
	if e != nil {
		return "", e
	}
	found := ""
	for _, comment := range comments {
		body, _ := comment["body"].(string)
		user, _ := comment["user"].(map[string]any)
		sum := sha256.Sum256([]byte(body))
		if authors[stringLogin(user)] && strings.Contains(body, marker) && hex.EncodeToString(sum[:]) == proof.BodySHA {
			found = fmt.Sprintf("comment:%v", comment["id"])
			break
		}
	}
	if found == "" {
		return "", contract.NewProblem(409, "投稿済みの訂正報告を確認できません。", "execution_report_missing")
	}
	return found, nil
}

func executionProofFingerprint(report Report) string {
	raw, _ := json.Marshal([]any{report.ControllerTest, report.ControllerReport})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// A report-waiting hold may follow a successful merge. Bind different SHAs
// through the actual pull rather than treating a model's merged flag as proof.
func (s *Store) executionMergedHead(ctx context.Context, current *Job, report Report, tested, head string) (bool, error) {
	number := current.PR
	if report.PR.Present && report.PR.Value == nil {
		// The report names no PR (or detached the previous one).
		return false, nil
	}
	if report.PR.Value != nil {
		if number != nil && *number != *report.PR.Value {
			return false, nil
		}
		number = report.PR.Value
	}
	if number == nil || s.options.ReadPull == nil {
		return false, nil
	}
	pull, err := s.options.ReadPull(ctx, current.Repo, *number)
	if err != nil {
		return false, err
	}
	object := func(value any) map[string]any { v, _ := value.(map[string]any); return v }
	source, target := object(pull["head"]), object(pull["base"])
	branchMatches := current.Branch == nil || source["ref"] == *current.Branch
	return pull["merged"] == true && pull["merge_commit_sha"] == head && source["sha"] == tested && branchMatches && object(source["repo"])["full_name"] == current.Repo && object(target["repo"])["full_name"] == current.Repo, nil
}

func stringLogin(user map[string]any) string { login, _ := user["login"].(string); return login }
