package consultationai

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/completionplan"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/filing"
)

func TestCompletionOtherQuestionAnswerThroughEngineAndFilingPostgres(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-plan", true: "new-premise"}[changed], func(t *testing.T) {
			ctx := context.Background()
			p := approvalFixture()
			turn := 0
			planQuestion := ""
			const otherQuestion = "実装資料はどこですか"
			s, _ := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
				value := reply()
				candidate, _ := completionplan.Parse(p.Record())
				value["ready"], value["title"], value["body"] = true, "接続設定を調整する", "固定した受入条件で検証する"
				value["questions"] = []any{}
				if turn < 3 {
					value["ready"], value["questions"] = false, []any{otherQuestion}
				}
				if turn == 2 {
					value["questions"] = []any{planQuestion, otherQuestion}
					candidate.Basis = "AIだけが書き直した候補の説明"
				}
				if turn == 3 && changed {
					candidate.Approvals[0].Condition = "本番環境で実行する"
				}
				value["completionPlan"] = candidate.Record()
				value["changeScope"] = Record{"known": true, "paths": []any{"src/connection.go"}, "reason": ""}
				return measuredAnswer(value), nil
			})
			s.options.Reference = nil
			principal := auth.Principal{Kind: auth.Screen}
			service, err := consultations.New(consultations.Options{Records: s.options.Records, Auth: &auth.Authenticator{}, Now: s.options.Now, Project: func(string) error { return nil }, Start: func(_ context.Context, _ string, d Record, _ auth.Principal) (Record, error) { return d, nil }})
			if err != nil {
				t.Fatal(err)
			}
			const id = "22222222-2222-4222-8222-222222222222"
			saved, err := service.CreateDraft(ctx, Record{"tier": "light", "id": id, "project": "owner/app", "hold": true}, principal, false)
			if err != nil {
				t.Fatal(err)
			}
			run := func(input string) {
				t.Helper()
				turn++
				queued, err := service.Chat(ctx, id, Record{"revision": saved["revision"], "text": input}, principal)
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 2)
				engine, err := consultations.NewEngine(ctx, consultations.EngineOptions{Service: service, Turn: s.Turn, Report: func(err error) { done <- err }, Publish: func(Record) {
					fresh, err := service.Draft(ctx, id, principal)
					if err != nil || fresh["status"] != "thinking" {
						done <- err
					}
				}})
				if err != nil {
					t.Fatal(err)
				}
				defer engine.Close()
				if _, err := engine.Start(ctx, id, queued, principal); err != nil {
					t.Fatal(err)
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				engine.Close()
				reopened, err := consultations.OpenRecords(ctx, s.options.Records.DB)
				if err != nil {
					t.Fatal(err)
				}
				var found bool
				saved, found, err = reopened.Get(ctx, "drafts", id)
				if err != nil || !found || saved["status"] != "draft" {
					t.Fatal(saved, err)
				}
			}
			run("接続設定を調整したい")
			for _, raw := range fleet.Elements(saved["questionReasons"]) {
				r := fleet.Object(raw)
				if r["controller"] == true && r["reason"] == completionQuestionReason {
					planQuestion = fleet.Text(r["question"])
				}
			}
			if planQuestion == "" {
				t.Fatal(saved)
			}
			run("A")
			receipt := fleet.Object(saved["completionApproval"])
			if saved["ready"] != false || saved["completionReconfirmation"] != nil || receipt["fingerprint"] != p.Fingerprint() || len(fleet.Elements(saved["questions"])) != 1 || fleet.Text(fleet.Elements(saved["questions"])[0]) != otherQuestion {
				t.Fatal("other question must survive plan approval", saved)
			}
			run("資料はdocs/x.mdです")
			_, filingErr := filing.Prepare(ctx, filing.BuildConfig{Config: map[string]any{"model": "gpt-6.1"}, Project: func(string) error { return nil }, Now: s.options.Now}, saved)
			comparison := fleet.Object(saved["completionComparison"])
			if comparison["rule"] != "completion-plan-instruction-v2" {
				t.Fatal("new user text used candidate rewrite rule", comparison)
			}
			if changed {
				if saved["ready"] != false || saved["completionApproval"] != nil || saved["completionReconfirmation"] == nil || filingErr == nil {
					t.Fatal("new premise bypassed strict comparison", saved, filingErr)
				}
			} else if saved["ready"] != true || saved["completionReconfirmation"] != nil || fleet.Object(saved["completionApproval"])["messageId"] != receipt["messageId"] || filingErr != nil {
				t.Fatal("unrelated answer revoked approval or blocked filing", saved, filingErr)
			}
		})
	}
}

func TestCompletionExplanationAndControllerNoticesSurviveTurnPostgres(t *testing.T) {
	for _, state := range []string{"waiting", "approved", "resolved", "non-exact"} {
		notices := []string{"none", "new-project", "cross-project"}
		if state == "resolved" {
			notices = append(notices, "empty-new-project")
		}
		for _, notice := range notices {
			t.Run(state+"/"+notice, func(t *testing.T) {
				ctx := context.Background()
				p := approvalFixture()
				const explanation = "隔離環境で結果を確かめ、担当者が配置版を照合する計画です。"
				var savedQuestions any
				s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
					value := reply()
					value["reply"], value["ready"], value["title"], value["body"] = explanation, true, "接続設定を調整する", "固定した受入条件で検証する"
					value["completionPlan"], value["questions"] = p.Record(), []any{}
					if state == "resolved" {
						value["questions"] = savedQuestions
						if notice == "new-project" {
							value["reply"] = "\n了承済みです。"
						}
					}
					if state == "non-exact" {
						value["reply"] = "保存発言の全文を了承済みとして記録しました。"
					}
					if notice == "empty-new-project" && state == "resolved" {
						value["reply"] = ""
					}
					if notice == "new-project" || notice == "empty-new-project" {
						value["newProject"] = Record{"needed": true}
					}
					if notice == "cross-project" {
						value["crossProject"] = Record{"reason": "両プロジェクトの接続境界をそろえる必要があります", "scopes": []any{Record{"project": "owner/app", "scope": "src/app.go"}, Record{"project": "owner/other", "scope": "src/other.go"}}}
					}
					return measuredAnswer(value), nil
				})
				s.options.Reference = nil
				config, _ := s.options.Config(ctx)
				config["repositories"] = append(fleet.Elements(config["repositories"]), Record{"name": "owner/other"})
				d["completionPlan"] = p.Record()
				d["messages"] = []any{presentation(p)}
				if state == "approved" || state == "resolved" {
					d["messages"] = append(fleet.Elements(d["messages"]), explicitReply("accepted", "A"))
				}
				if state == "approved" {
					d["completionApproval"], _ = completionplan.Confirmed(p, d["messages"])
					d["messages"] = append(fleet.Elements(d["messages"]), Record{"role": "assistant", "text": "計画への了承を確認しました"})
				}
				if state == "waiting" || state == "approved" {
					d["messages"] = append(fleet.Elements(d["messages"]), explicitReply("explain", "説明して"))
				}
				if state == "non-exact" {
					d["messages"] = append(fleet.Elements(d["messages"]), explicitReply("attempt", "A（補足）"))
				}
				if state == "resolved" {
					first := candidateAnswer(p)
					prepareCompletionPlan(Record{}, first)
					d["completionProposal"], d["questions"], d["questionReasons"] = p.Fingerprint(), first["questions"], first["questionReasons"]
				}
				savedQuestions = d["questions"]
				out, err := s.Turn(ctx, d)
				if err != nil {
					t.Fatal(err)
				}
				body := fleet.Text(out["reply"])
				if (state == "waiting" || state == "approved") && !strings.Contains(body, explanation) {
					t.Fatal("explanation disappeared", out)
				}
				if state == "non-exact" && (out["completionApproval"] != nil || strings.Contains(body, "保存発言") || strings.Contains(body, "了承済み") || !strings.Contains(body, "A / 1-A / 承認します / この計画を承認します")) {
					t.Fatal("invalid approval displayed as accepted", out)
				}
				if notice != "none" && out["ready"] != false {
					t.Fatal("controller hold lost", out)
				}
				if (notice == "new-project" || notice == "empty-new-project") && !strings.Contains(body, "新しいリポジトリが必要な場合は、チャットのAIに相談してください。") {
					t.Fatal("new project notice disappeared", out)
				}
				if state == "resolved" && notice == "new-project" && strings.Contains(body, "了承済みです。") {
					t.Fatal("original reply duplicated", out)
				}
				if notice == "cross-project" {
					for _, detail := range []string{"両プロジェクトの接続境界をそろえる必要があります", "owner/app: src/app.go", "owner/other: src/other.go"} {
						if !strings.Contains(body, detail) {
							t.Fatal("cross project terms disappeared", detail, out)
						}
					}
				}
			})
		}
	}
}

func TestCompletionReplacedReplyPreservesNotice(t *testing.T) {
	answer := Record{"reply": "元の返答", "ready": false, "questions": []any{"確認事項"}}
	finish := completionReplyFinalizer(answer, true, fleet.Elements(answer["questions"]))
	answer["reply"] = "後続処理の案内"
	finish()
	body := fleet.Text(answer["reply"])
	if !strings.Contains(body, "後続処理の案内") || strings.Contains(body, "元の返答") || !strings.Contains(body, "確認事項") {
		t.Fatal(answer)
	}
}

func TestCompletionResolvedReplyFiltersFinalityClaims(t *testing.T) {
	for _, tc := range []struct {
		name, sentence string
		keep           bool
	}{
		{"completed", "起票案を確定しました。", false},
		{"state", "計画は確定しています。", false},
		{"final", "計画は確定です。", false},
		{"tax", "確定申告の設定を確認します。", true},
		{"future", "確認後に計画を確定します。", true},
		{"negative", "計画は確定していません。", true},
		{"polite negative", "計画は確定しておりません。", true},
		{"negative continuation", "計画は確定しておらず、確認が必要です。", true},
		{"passive", "計画が確定されました。", false},
		{"passive state", "計画が確定されています。", false},
		{"becomes final", "計画が確定となります。", false},
		{"passive negative", "計画は確定されておりません。", true},
		{"passive negative continuation", "計画は確定されておらず、確認が必要です。", true},
		{"passive denial", "計画が確定されたわけではありません。", true},
		{"mixed denial and claim", "計画は確定しておりませんが、別の計画は確定されました。", false},
		{"denial", "計画が確定したわけではありません。", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer := Record{"reply": tc.sentence, "ready": false, "questions": []any{"別の確認事項"}}
			finish := completionReplyFinalizer(answer, true, nil)
			finish()
			body := fleet.Text(answer["reply"])
			if strings.Contains(body, tc.sentence) != tc.keep || !strings.Contains(body, "別の確認事項") {
				t.Fatal(body)
			}
		})
	}
}

func TestCompletionReviewedProblemFinalityThroughTurnPostgres(t *testing.T) {
	p := approvalFixture()
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
		value := reply()
		value["reply"], value["ready"], value["title"], value["body"] = "確認を終えました。", true, "接続設定", "固定した受入条件"
		value["completionPlan"], value["questions"], value["reviewedFiles"] = p.Record(), []any{}, []any{"missing.go"}
		return measuredAnswer(value), nil
	})
	s.options.Reference = nil
	d["completionPlan"] = p.Record()
	d["messages"] = []any{presentation(p), explicitReply("accepted", "A")}
	out, err := s.Turn(context.Background(), d)
	if err != nil || out["ready"] != false || !strings.Contains(fleet.Text(out["reply"]), "未確認の質問が残っています") || !strings.Contains(fleet.Text(out["reply"]), "実装とテストの該当箇所") {
		t.Fatal(out, err)
	}
}

func TestCompletionPendingQuestionPreservesExplanationThroughTurnPostgres(t *testing.T) {
	p := approvalFixture()
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
		value := reply()
		value["reply"], value["ready"], value["title"], value["body"] = "APIの扱いを説明します。", true, "接続設定", "固定した受入条件"
		value["completionPlan"], value["questions"] = p.Record(), []any{}
		return measuredAnswer(value), nil
	})
	s.options.Reference = nil
	d["completionPlan"], d["completionProposal"] = p.Record(), p.Fingerprint()
	d["messages"] = []any{presentation(p), explicitReply("question", "APIの扱いを説明してください")}
	out, err := s.Turn(context.Background(), d)
	if err != nil || !strings.Contains(fleet.Text(out["reply"]), "APIの扱いを説明します。") || !strings.Contains(fleet.Text(out["reply"]), "A / 1-A / 承認します / この計画を承認します") {
		t.Fatal(out, err)
	}
}

func TestCompletionFinalityClaimWithRemainingQuestionThroughTurnPostgres(t *testing.T) {
	p := approvalFixture()
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
		value := reply()
		value["reply"], value["ready"], value["title"], value["body"] = "接続方法を説明します。確認を終えました。", true, "接続設定", "固定した受入条件"
		value["completionPlan"], value["questions"] = p.Record(), []any{"実装資料はどこですか"}
		return measuredAnswer(value), nil
	})
	s.options.Reference = nil
	d["completionPlan"] = p.Record()
	d["messages"] = []any{presentation(p), explicitReply("accepted", "A")}
	out, err := s.Turn(context.Background(), d)
	body := fleet.Text(out["reply"])
	if err != nil || out["ready"] != false || !strings.Contains(body, "接続方法を説明します。") || !strings.Contains(body, "未確認の質問が残っています") || !strings.Contains(body, "実装資料はどこですか") {
		t.Fatal(out, err)
	}
}

// Reported loop: after "A" the consultation AI repeated its own approval
// question; answering it with a symbol must not reopen the confirmation.
func TestCompletionModelApprovalQuestionAndSymbolFollowupThroughEngineAndFilingPostgres(t *testing.T) {
	for _, symbol := range []string{"A", "1-A"} {
		for _, other := range []bool{false, true} {
			t.Run(symbol+map[bool]string{false: "/plan-only", true: "/with-other"}[other], func(t *testing.T) {
				ctx := context.Background()
				p := approvalFixture()
				turn := 0
				const otherQuestion = "実装資料はどこですか"
				s, _ := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
					value := reply()
					value["ready"], value["title"], value["body"] = false, "接続設定を調整する", "固定した受入条件で検証する"
					value["questions"] = []any{modelPlanQuestion}
					if other {
						value["questions"] = append(fleet.Elements(value["questions"]), otherQuestion)
					}
					value["completionPlan"] = p.Record()
					value["changeScope"] = Record{"known": true, "paths": []any{"src/connection.go"}, "reason": ""}
					return measuredAnswer(value), nil
				})
				s.options.Reference = nil
				principal := auth.Principal{Kind: auth.Screen}
				service, err := consultations.New(consultations.Options{Records: s.options.Records, Auth: &auth.Authenticator{}, Now: s.options.Now, Project: func(string) error { return nil }, Start: func(_ context.Context, _ string, d Record, _ auth.Principal) (Record, error) { return d, nil }})
				if err != nil {
					t.Fatal(err)
				}
				const id = "33333333-3333-4333-8333-333333333333"
				saved, err := service.CreateDraft(ctx, Record{"tier": "light", "id": id, "project": "owner/app", "hold": true}, principal, false)
				if err != nil {
					t.Fatal(err)
				}
				run := func(input string) {
					t.Helper()
					turn++
					queued, err := service.Chat(ctx, id, Record{"revision": saved["revision"], "text": input}, principal)
					if err != nil {
						t.Fatal(err)
					}
					done := make(chan error, 2)
					engine, err := consultations.NewEngine(ctx, consultations.EngineOptions{Service: service, Turn: s.Turn, Report: func(err error) { done <- err }, Publish: func(Record) {
						fresh, err := service.Draft(ctx, id, principal)
						if err != nil || fresh["status"] != "thinking" {
							done <- err
						}
					}})
					if err != nil {
						t.Fatal(err)
					}
					defer engine.Close()
					if _, err := engine.Start(ctx, id, queued, principal); err != nil {
						t.Fatal(err)
					}
					if err := <-done; err != nil {
						t.Fatal(err)
					}
					engine.Close()
					reopened, err := consultations.OpenRecords(ctx, s.options.Records.DB)
					if err != nil {
						t.Fatal(err)
					}
					var found bool
					saved, found, err = reopened.Get(ctx, "drafts", id)
					if err != nil || !found || saved["status"] != "draft" {
						t.Fatal(saved, err)
					}
				}
				planQuestions := func() int {
					count := 0
					for _, raw := range fleet.Elements(saved["questions"]) {
						if completionApprovalQuestion(fleet.Text(raw)) {
							count++
						}
						if fleet.Text(raw) == modelPlanQuestion {
							t.Fatal("model plan question was saved", saved)
						}
					}
					return count
				}
				run("接続設定を調整したい")
				if planQuestions() != 1 || saved["completionProposal"] != p.Fingerprint() {
					t.Fatal("plan confirmation must be one controller question", saved)
				}
				run("A")
				receipt := fleet.Object(saved["completionApproval"])
				if receipt["fingerprint"] != p.Fingerprint() || saved["completionReconfirmation"] != nil || planQuestions() != 0 || saved["ready"] != !other || len(fleet.Elements(saved["questions"])) != btoi(other) {
					t.Fatal("verified approval kept the model approval question", saved)
				}
				// The last assistant row has no presentation; answer it with a symbol.
				run(symbol)
				if saved["ready"] != !other || saved["completionReconfirmation"] != nil || fleet.Object(saved["completionApproval"])["messageId"] != receipt["messageId"] || planQuestions() != 0 || len(fleet.Elements(saved["questions"])) != btoi(other) {
					t.Fatal("symbol reply to an unpresented row reopened confirmation", saved)
				}
				// The filing-time provenance check accepts the same reread history.
				if _, err := completionplan.Check(saved["completionPlan"], saved["messages"], saved["completionApproval"]); err != nil {
					t.Fatal("filing provenance rejected the preserved approval", err)
				}
				if !other {
					if _, err := filing.Prepare(ctx, filing.BuildConfig{Config: map[string]any{"model": "gpt-6.1"}, Project: func(string) error { return nil }, Now: s.options.Now}, saved); err != nil {
						t.Fatal("filing rejected the preserved approval", err)
					}
				}
			})
		}
	}
}

func TestCompletionFactualAcceptanceFollowupThroughEngineAndFilingPostgres(t *testing.T) {
	for _, symbol := range []string{"終了済みのIssue検索・一覧の一括読取相談は、受入条件で詳細専用の読取削減を対象外にしています。"} {
		for _, other := range []bool{false, true} {
			t.Run(symbol+map[bool]string{false: "/plan-only", true: "/with-other"}[other], func(t *testing.T) {
				ctx := context.Background()
				p := approvalFixture()
				turn := 0
				const otherQuestion = "実装資料はどこですか"
				s, _ := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
					value := reply()
					value["ready"], value["title"], value["body"] = false, "接続設定を調整する", "固定した受入条件で検証する"
					value["questions"] = []any{modelPlanQuestion}
					if other {
						value["questions"] = append(fleet.Elements(value["questions"]), otherQuestion)
					}
					value["completionPlan"] = p.Record()
					value["changeScope"] = Record{"known": true, "paths": []any{"src/connection.go"}, "reason": ""}
					return measuredAnswer(value), nil
				})
				s.options.Reference = nil
				principal := auth.Principal{Kind: auth.Screen}
				service, err := consultations.New(consultations.Options{Records: s.options.Records, Auth: &auth.Authenticator{}, Now: s.options.Now, Project: func(string) error { return nil }, Start: func(_ context.Context, _ string, d Record, _ auth.Principal) (Record, error) { return d, nil }})
				if err != nil {
					t.Fatal(err)
				}
				const id = "33333333-3333-4333-8333-333333333333"
				saved, err := service.CreateDraft(ctx, Record{"tier": "light", "id": id, "project": "owner/app", "hold": true}, principal, false)
				if err != nil {
					t.Fatal(err)
				}
				run := func(input string) {
					t.Helper()
					turn++
					queued, err := service.Chat(ctx, id, Record{"revision": saved["revision"], "text": input}, principal)
					if err != nil {
						t.Fatal(err)
					}
					done := make(chan error, 2)
					engine, err := consultations.NewEngine(ctx, consultations.EngineOptions{Service: service, Turn: s.Turn, Report: func(err error) { done <- err }, Publish: func(Record) {
						fresh, err := service.Draft(ctx, id, principal)
						if err != nil || fresh["status"] != "thinking" {
							done <- err
						}
					}})
					if err != nil {
						t.Fatal(err)
					}
					defer engine.Close()
					if _, err := engine.Start(ctx, id, queued, principal); err != nil {
						t.Fatal(err)
					}
					if err := <-done; err != nil {
						t.Fatal(err)
					}
					engine.Close()
					reopened, err := consultations.OpenRecords(ctx, s.options.Records.DB)
					if err != nil {
						t.Fatal(err)
					}
					var found bool
					saved, found, err = reopened.Get(ctx, "drafts", id)
					if err != nil || !found || saved["status"] != "draft" {
						t.Fatal(saved, err)
					}
				}
				planQuestions := func() int {
					count := 0
					for _, raw := range fleet.Elements(saved["questions"]) {
						if completionApprovalQuestion(fleet.Text(raw)) {
							count++
						}
						if fleet.Text(raw) == modelPlanQuestion {
							t.Fatal("model plan question was saved", saved)
						}
					}
					return count
				}
				run("接続設定を調整したい")
				if planQuestions() != 1 || saved["completionProposal"] != p.Fingerprint() {
					t.Fatal("plan confirmation must be one controller question", saved)
				}
				run("A")
				receipt := fleet.Object(saved["completionApproval"])
				if receipt["fingerprint"] != p.Fingerprint() || saved["completionReconfirmation"] != nil || planQuestions() != 0 || saved["ready"] != !other || len(fleet.Elements(saved["questions"])) != btoi(other) {
					t.Fatal("verified approval kept the model approval question", saved)
				}
				after := len(fleet.Elements(saved["messages"]))
				run(symbol)
				if saved["ready"] != !other || saved["completionReconfirmation"] != nil || !reflect.DeepEqual(fleet.Object(saved["completionApproval"]), receipt) || planQuestions() != 0 || len(fleet.Elements(saved["questions"])) != btoi(other) {
					t.Fatal("symbol reply to an unpresented row reopened confirmation", saved)
				}
				if other && fleet.Text(fleet.Elements(saved["questions"])[0]) != otherQuestion {
					t.Fatal("other question changed", saved)
				}
				// Reconfirmation begins after the original answer; filing must reject it.
				pending := Record{}
				for k, v := range saved {
					pending[k] = v
				}
				pending["completionReconfirmation"] = p.Fingerprint()
				pending["completionReconfirmationAfter"] = after
				if _, err := filing.Prepare(ctx, filing.BuildConfig{Config: map[string]any{"model": "gpt-6.1"}, Project: func(string) error { return nil }, Now: s.options.Now}, pending); err == nil || !strings.Contains(err.Error(), "変更した前提への明示回答と保存した了承記録を確認していません") {
					t.Fatal("old receipt passed reconfirmation", err)
				}
				// The filing-time provenance check accepts the same reread history.
				if _, err := completionplan.Check(saved["completionPlan"], saved["messages"], saved["completionApproval"]); err != nil {
					t.Fatal("filing provenance rejected the preserved approval", err)
				}
				if !other {
					if _, err := filing.Prepare(ctx, filing.BuildConfig{Config: map[string]any{"model": "gpt-6.1"}, Project: func(string) error { return nil }, Now: s.options.Now}, saved); err != nil {
						t.Fatal("filing rejected the preserved approval", err)
					}
				}
			})
		}
	}
}
