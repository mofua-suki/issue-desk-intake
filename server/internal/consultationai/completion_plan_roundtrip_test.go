package consultationai

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/completionplan"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/filing"
)

// Exercise the shipped Turn -> Engine -> PostgreSQL -> Chat -> Turn -> filing
// path, including the controller's actual presentation writer.
func TestCompletionRewriteThroughEngineChatAndFilingPostgres(t *testing.T) {
	testCompletionRewriteRoundtrip(t, false, false, false)
}

func TestCompletionProxyRewriteThroughEngineChatAndFilingPostgres(t *testing.T) {
	testCompletionRewriteRoundtrip(t, true, false, false)
}

func TestCompletionAutoReadyRoundtripPostgres(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(fmt.Sprint("split=", split), func(t *testing.T) { testCompletionRewriteRoundtrip(t, false, true, split) })
	}
}

func testCompletionRewriteRoundtrip(t *testing.T, proxy, auto, split bool) {
	t.Helper()
	for _, variant := range []string{"ref", "wording", "both", "free-text"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			original := completionFixture()
			original.Basis = "認証と接続経路を維持する。根拠は固定ref " + strings.Repeat("a", 40) + "の確認済みコードと試験本文"
			original.Approvals = []completionplan.Approval{{ID: "p", Category: "permission", Phase: "before_implementation", Target: "対象設定", Operation: "変更", Condition: "指定した経路で確認"}}
			candidate, _ := completionplan.Parse(original.Record())
			turn := 0
			proxyCalls := 0
			carriedQuestion := ""
			s, _ := testService(t, func(_ context.Context, request ai.Request) (ai.Answer, error) {
				if filepath.Base(request.SchemaFile) == "proxy.schema.json" {
					proxyCalls++
					// The controller's approval question must never reach the proxy.
					return measuredAnswer(Record{"answers": []any{Record{"index": 1, "decision": "answer", "category": "fact", "answer": "README.mdに記載", "evidence": []any{Record{"kind": "document", "source": "README.md", "location": "1行目"}}}}, "readFiles": []any{}}), nil
				}
				value := reply()
				value["ready"], value["title"], value["body"], value["questions"] = true, "接続の設定を変更する", "固定した受入条件で検証する", []any{}
				if turn == 2 {
					value["ready"], value["questions"], value["reply"] = false, []any{carriedQuestion}, "質問が残ります。起票案を確定しました。"
				}
				if proxy && turn == 1 {
					value["questions"] = []any{"実装資料はどこですか"}
				}
				if split {
					value["splitIssues"] = []any{Record{"title": "子1", "purpose": "実装", "acceptance": []any{"確認: `go test ./...` → 期待: 終了コード0"}}, Record{"title": "子2", "purpose": "検証", "acceptance": []any{"確認: `go test ./...` → 期待: 終了コード0"}}}
				}
				value["completionPlan"] = candidate.Record()
				value["changeScope"] = Record{"known": true, "paths": []any{"src/connection.go"}, "reason": ""}
				return measuredAnswer(value), nil
			})
			s.options.Reference = nil
			config, _ := s.options.Config(ctx)
			fleet.Object(fleet.Object(config["intake"])["proxy"])["enabled"] = proxy
			principal := auth.Principal{Kind: auth.Screen}
			service, err := consultations.New(consultations.Options{Records: s.options.Records, Auth: &auth.Authenticator{}, Now: s.options.Now, Project: func(string) error { return nil }, Start: func(_ context.Context, _ string, d Record, _ auth.Principal) (Record, error) { return d, nil }})
			if err != nil {
				t.Fatal(err)
			}
			const id = "22222222-2222-4222-8222-222222222222"
			saved, err := service.CreateDraft(ctx, Record{"tier": "light", "id": id, "project": "owner/app", "hold": !auto}, principal, false)
			if err != nil {
				t.Fatal(err)
			}
			submissions := make(chan Record, 1)
			run := func(input string) Record {
				t.Helper()
				turn++
				queued, err := service.Chat(ctx, id, Record{"revision": saved["revision"], "text": input}, principal)
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 2)
				engine, err := consultations.NewEngine(ctx, consultations.EngineOptions{Service: service, Turn: s.Turn, Submit: func(_ context.Context, draft Record) (Record, error) { submissions <- draft; return nil, nil }, Report: func(err error) { done <- err }, Publish: func(Record) {
					fresh, err := service.Draft(ctx, id, principal)
					if err != nil || fresh["status"] != "thinking" {
						done <- err
					}
				}})
				if err != nil {
					t.Fatal(err)
				}
				defer engine.Close()
				if _, err = engine.Start(ctx, id, queued, principal); err != nil {
					t.Fatal(err)
				}
				if err = <-done; err != nil {
					t.Fatal(err)
				}
				engine.Close()
				fresh, err := service.Draft(ctx, id, principal)
				if err != nil || (fresh["status"] != "draft" && !(auto && turn == 2 && fresh["status"] == "auto_ready")) {
					t.Fatal(fresh, err)
				}
				reopened, err := consultations.OpenRecords(ctx, s.options.Records.DB)
				if err != nil {
					t.Fatal(err)
				}
				restored, found, err := reopened.Get(ctx, "drafts", id)
				if err != nil || !found {
					t.Fatal(restored, err)
				}
				return restored
			}
			saved = run("設定の変更を進めたい")
			if saved["ready"] != false || saved["completionProposal"] != original.Fingerprint() {
				t.Fatal(saved)
			}
			presentation := fleet.Object(fleet.Elements(saved["messages"])[1])
			if _, err := completionplan.Parse(presentation["completionPlan"]); err != nil {
				t.Fatal("engine did not save its structured presentation", err)
			}
			userIndex := 2
			if proxy {
				userIndex = 3
				messages := fleet.Elements(saved["messages"])
				if proxyCalls != 1 || len(messages) != 3 || fleet.Object(messages[2])["role"] != "proxy" || len(fleet.Elements(saved["questions"])) != 1 {
					t.Fatal("proxy SaveProxy path did not keep the controller question", saved, proxyCalls)
				}
				var count int
				if err := s.options.Records.DB.QueryRowContext(ctx, "SELECT count(*) FROM proxy_answers WHERE draft=?", id).Scan(&count); err != nil || count != 1 {
					t.Fatal("proxy answer was not persisted", count, err)
				}
			}
			carriedQuestion = fleet.Text(fleet.Elements(saved["questions"])[0])
			if variant != "wording" {
				candidate.Basis = strings.ReplaceAll(candidate.Basis, strings.Repeat("a", 40), strings.Repeat("b", 40))
			}
			if variant != "ref" {
				candidate.Steps[0].Evidence = "隔離検証の結果と版を照合。"
			}
			if variant == "free-text" {
				candidate.Basis = "受付AIが生成し直した候補の判断根拠"
				for i := range candidate.Steps {
					candidate.Steps[i].Prerequisite = "候補が書き直した環境と前提"
					candidate.Steps[i].Evidence = "候補が書き直した確認方法"
				}
			}
			saved = run("A")
			if auto {
				submitted := <-submissions
				if submitted["status"] != "auto_ready" || saved["status"] != "auto_ready" || saved["ready"] != true || len(fleet.Elements(saved["questions"])) != 0 {
					t.Fatal(saved)
				}
				return
			}
			for i := 0; i < 2; i++ {
				if saved["ready"] != true || len(fleet.Elements(saved["questions"])) != 0 || saved["completionProposal"] != nil || saved["completionReconfirmation"] != nil {
					t.Fatal("cosmetic rewrite asked again", saved)
				}
				p, err := completionplan.Check(saved["completionPlan"], saved["messages"], saved["completionApproval"])
				if err != nil || p.Fingerprint() != original.Fingerprint() {
					t.Fatal(p, err)
				}
				r := fleet.Object(saved["completionApproval"])
				u := fleet.Object(fleet.Elements(saved["messages"])[userIndex])
				if r["fingerprint"] != original.Fingerprint() || r["messageId"] != u["id"] || r["quote"] != "A" || r["at"] != u["at"] || r["source"] != "screen-session" {
					t.Fatal(r, u)
				}
				comparison := fleet.Object(saved["completionComparison"])
				if comparison["presentedReplyCompatible"] != true || comparison["equivalent"] != true || comparison["presented"] != original.Fingerprint() || comparison["candidate"] != candidate.Fingerprint() {
					t.Fatal(comparison)
				}
				if variant == "free-text" && (len(fleet.Elements(comparison["changes"])) != 9 || fleet.Object(comparison["candidatePlan"])["basis"] != candidate.Basis || fleet.Object(comparison["presentedPlan"])["basis"] != original.Basis) {
					t.Fatal("literal prose differences were lost after reload", comparison)
				}
				prepared, err := filing.Prepare(ctx, filing.BuildConfig{Config: map[string]any{"model": "gpt-6.1"}, Project: func(string) error { return nil }, Now: s.options.Now}, saved)
				if err != nil || fleet.Object(prepared.Job.View["completionApproval"])["fingerprint"] != original.Fingerprint() || !strings.Contains(prepared.Body, original.Basis) {
					t.Fatal(fmt.Sprint(prepared), err)
				}
				if i == 0 {
					saved = run("説明して")
				}
			}
			if turn != 3 {
				t.Fatal(turn)
			}
			if proxy && proxyCalls != 1 {
				t.Fatal("approval question was sent to the proxy", proxyCalls)
			}
		})
	}
}

func TestCompletionNonExactReplyThroughTurnPostgres(t *testing.T) {
	for _, input := range []string{"A（補足）", "A: 続けて", "1-A、2-A"} {
		t.Run(input, func(t *testing.T) {
			p := approvalFixture()
			s, d := testService(t, func(_ context.Context, _ ai.Request) (ai.Answer, error) {
				value := reply()
				value["ready"], value["title"], value["body"], value["questions"], value["completionPlan"] = true, "接続の設定を変更する", "固定した受入条件で検証する", []any{}, p.Record()
				value["reply"] = "保存発言の全文「" + input + "」を了承済みとして記録しました。"
				return measuredAnswer(value), nil
			})
			s.options.Reference = nil
			d["completionPlan"], d["completionProposal"] = p.Record(), p.Fingerprint()
			d["messages"] = []any{presentation(p), explicitReply("answer", input)}
			out, err := s.Turn(context.Background(), d)
			if err != nil {
				t.Fatal(err)
			}
			output := fleet.Text(out["reply"])
			if out["ready"] != false || out["completionApproval"] != nil || !strings.Contains(output, "A / 1-A / 承認します / この計画を承認します") || strings.Contains(output, "保存発言") || strings.Contains(output, "了承済み") {
				t.Fatal(out)
			}
		})
	}
}

func TestCompletionAcceptedReplyUsesPresentedPlanThroughTurnPostgres(t *testing.T) {
	p := approvalFixture()
	candidate, _ := completionplan.Parse(p.Record())
	candidate.Basis = "候補だけの判断根拠"
	candidate.Steps[0].Prerequisite = "候補だけの前提"
	candidate.Steps[0].Evidence = "候補だけの確認方法"
	s, d := testService(t, func(_ context.Context, _ ai.Request) (ai.Answer, error) {
		value := reply()
		value["ready"], value["title"], value["body"], value["questions"], value["completionPlan"] = true, "接続の設定を変更する", "固定した受入条件で検証する", []any{}, candidate.Record()
		value["reply"] = "この候補計画を了承済みです。\n" + candidate.Section()
		return measuredAnswer(value), nil
	})
	s.options.Reference = nil
	d["completionPlan"], d["completionProposal"] = p.Record(), p.Fingerprint()
	d["messages"] = []any{presentation(p), explicitReply("answer", "A")}
	out, err := s.Turn(context.Background(), d)
	if err != nil || out["ready"] != true || out["completionApproval"] == nil || strings.Contains(fleet.Text(out["reply"]), "候補だけ") || !strings.Contains(fleet.Text(out["reply"]), p.Section()) {
		t.Fatal(out, err)
	}
}
