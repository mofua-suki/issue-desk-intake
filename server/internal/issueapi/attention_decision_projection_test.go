package issueapi

import (
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"strings"
	"testing"
)

func TestAttentionQuestionAndLimitListProjectionPostgres(t *testing.T) {
	for _, kind := range []string{"normal", "followup"} {
		for _, scenario := range []string{"fixed test", "unsubstantiated", "question", "approval", "deletion", "final approval", "dependency", "run limit", "review run limit", "usage limit", "question without body", "question empty", "resolved", "ordinary failure"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				f := usageHoldFixture(t, -1, 0, 100)
				if kind == "followup" {
					f.api.issue["body"] = "<!-- issue-desk:followup -->"
				}
				result := `{"status":"held","questions":[],"error":"固定テストが失敗しました。担当AIが原因を確認します","testFailures":{"complete":true,"names":["TestExample"]},"testTimeout":{"complete":true}}`
				view := `{}`
				hold, status := "question", "held"
				switch scenario {
				case "question":
					result = `{"status":"question","questions":["どちらにしますか"],"error":"固定テストが失敗"}`
				case "question without body":
					result = `{"status":"question","error":"固定テストが失敗"}`
				case "question empty":
					result = `{"status":"question","questions":[],"error":"質問の本文を確認できません"}`
				case "unsubstantiated":
					result = `{"status":"held","questions":[],"incomplete":{"reason":"実装が未完了です","remaining":["原因を確認して再開する"]}}`
				case "approval":
					view = `{"permissionHold":{"reason":"承認が必要"}}`
				case "deletion":
					hold = "deletions"
					view = `{"deletionApproval":{"head":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","items":[{"kind":"delete","path":"old.txt"}]}}`
				case "final approval":
					hold = "review_held"
					view = `{"finalApprovalHold":{"headSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","generation":0,"epoch":0,"approvals":["deploy"]},"completionPlan":{"version":1,"basis":"根拠","steps":[{"phase":"implementation","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"verification","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"merge","owner":"担当","prerequisite":"前提","evidence":"確認"},{"phase":"deployment","owner":"担当","prerequisite":"前提","evidence":"確認"}],"approvals":[{"category":"irreversible","id":"deploy","phase":"after_diff","target":"配置","operation":"配置","condition":"差分確認"}]}}`
					if _, err := f.pg.Exec(`UPDATE jobs SET head_sha=$2 WHERE id=$1`, f.job, strings.Repeat("a", 40)); err != nil {
						t.Fatal(err)
					}
				case "dependency":
					status, hold = "queued", ""
					view = `{"dependsOnIssues":[{"repo":"owner/repo","issue":2}]}`
					var predecessor int64
					if err := f.pg.QueryRow(`INSERT INTO jobs(repo,issue,status) VALUES('owner/repo',2,'queued') RETURNING id`).Scan(&predecessor); err != nil {
						t.Fatal(err)
					}
					if _, err := f.pg.Exec(`INSERT INTO job_deps(job_id,depends_on) VALUES($1,$2)`, f.job, predecessor); err != nil {
						t.Fatal(err)
					}
				case "run limit", "review run limit":
					view = `{"workerExecution":{"implementationRuns":10}}`
					if scenario == "review run limit" {
						hold = "review_held"
					}
				case "usage limit":
					if _, err := f.pg.Exec(`INSERT INTO usage_rows(key,at,host,repo,issue,json,sender) VALUES('usage-1',now(),'worker','owner/repo',1,'{"repo":"owner/repo","issue":1,"step":"実装","provider":"codex","inputTokens":101,"cacheReadTokens":0,"outputTokens":0}','worker')`); err != nil {
						t.Fatal(err)
					}
				case "resolved":
					status, hold = "merged", ""
				case "ordinary failure":
					status, hold = "failed", ""
				}
				if _, err := f.pg.Exec(`UPDATE jobs SET kind=$2,status=$3,hold_reason=NULLIF($4,''),result=$5::jsonb,view=$6::jsonb WHERE id=$1`, f.job, kind, status, hold, result, view); err != nil {
					t.Fatal(err)
				}
				rows, err := f.s.List(f.ctx)
				if err != nil || len(rows) != 1 {
					t.Fatal(rows, err)
				}
				entry, err := f.s.ListEntry(f.ctx, "owner/repo", 1)
				if err != nil {
					t.Fatal(err)
				}
				detail, err := f.s.Detail(f.ctx, "owner/repo", 1, false, false)
				if err != nil {
					t.Fatal(err)
				}
				summary, err := f.s.Detail(f.ctx, "owner/repo", 1, false, true)
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range []Object{rows[0], entry, detail, summary} {
					unknownQuestion := scenario == "question without body" || scenario == "question empty"
					want := scenario != "fixed test" && scenario != "unsubstantiated" && !unknownQuestion && scenario != "dependency" && scenario != "resolved" && scenario != "ordinary failure"
					if (row["requiresUser"] == true) != want {
						t.Fatalf("%s: %v", scenario, row)
					}
					wantStatus := "waiting"
					if scenario == "review run limit" || scenario == "deletion" || scenario == "final approval" {
						wantStatus = "review_held"
					}
					if scenario == "dependency" && (row["status"] != "dependency_waiting" || row["executionScheduled"] != true || !strings.Contains(fmt.Sprint(row["dependencyReason"]), "PRマージ待ち")) {
						t.Fatalf("dependency: %v", row)
					}
					pending, _ := row["pendingFinalApprovals"].([]jobs.PendingFinalApproval)
					if scenario == "deletion" && fleet.ObjectOrEmpty(row["deletionApproval"])["head"] != strings.Repeat("a", 40) || scenario == "final approval" && (len(pending) != 1 || pending[0].HeadSHA != strings.Repeat("a", 40)) {
						t.Fatalf("approval evidence: %v", row)
					}
					if want && row["status"] != wantStatus {
						t.Fatalf("decision status: %v", row)
					}
					if scenario == "fixed test" {
						if row["status"] != "review_held" || row["nextActor"] == "user" || row["nextAction"] != nil || !strings.Contains(fmt.Sprint(row["error"]), "担当AI") {
							t.Fatalf("fixed test hold: %v", row)
						}
					}
					if scenario == "unsubstantiated" || unknownQuestion {
						if row["nextActor"] == "user" || row["nextAction"] != nil || row["error"] == nil {
							t.Fatalf("hold evidence: %v", row)
						}
						if unknownQuestion && !strings.Contains(fmt.Sprint(row["error"]), "未確認") {
							t.Fatalf("missing question: %v", row)
						}
						if scenario == "unsubstantiated" && (!strings.Contains(fmt.Sprint(row["error"]), "未完了") || !strings.Contains(fmt.Sprint(row["error"]), "原因を確認して再開")) {
							t.Fatalf("missing hold cause or recovery: %v", row)
						}
					}
				}
				if scenario == "question without body" || scenario == "question empty" {
					_, err := f.s.SendRecoveryAction(f.ctx, "owner/repo", 1, "00000000-0000-4000-8000-000000000094", "reply", "続けてください")
					if err == nil || f.api.commentCalls != 0 {
						t.Fatalf("missing question allowed automatic reply: %v calls=%d", err, f.api.commentCalls)
					}
					_, err = f.options.Jobs.Control(f.ctx, f.job, jobs.ControlInput{ID: "00000000-0000-4000-8000-000000000093", Action: "retry", By: "recovery", Automatic: true}, jobs.ControlOptions{})
					if err == nil {
						t.Fatal("missing question allowed automatic retry")
					}
					job, err := f.options.Jobs.Get(f.ctx, f.job)
					if err != nil || job.Status != "held" {
						t.Fatalf("automatic retry changed job: %v %v", job, err)
					}
				}
				if scenario == "fixed test" && detail["retryable"] != true {
					t.Fatalf("detail: %v", detail)
				}
				if scenario == "fixed test" && (fleet.ObjectOrEmpty(detail["dispatchRecord"])["status"] != "held" || fleet.ObjectOrEmpty(detail["result"])["status"] != "held" || len(fleet.Items(fleet.ObjectOrEmpty(detail["result"])["questions"])) != 0 || summary["latestAsk"] != nil) {
					t.Fatalf("public result: %v %v", detail, summary)
				}
			})
		}
	}
}
