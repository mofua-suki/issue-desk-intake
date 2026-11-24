package consultationai

import (
	"context"
	"database/sql"
	"issue-desk-intake/server/internal/fleet"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/completionplan"
	"issue-desk-intake/server/internal/consultations"
)

func TestSplitAnswerThroughTurnAndEngine(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		for _, mode := range []string{"empty", "ambiguous", "cancel", "cancel_notice", "replace", "empty_completion", "cancel_completion"} {
			if proxy && mode == "cancel_notice" {
				continue
			}
			t.Run(map[bool]string{false: "normal", true: "proxy"}[proxy]+"/"+mode, func(t *testing.T) {
				ctx := context.Background()
				calls := 0
				s, _ := testService(t, func(_ context.Context, req ai.Request) (ai.Answer, error) {
					if filepath.Base(req.SchemaFile) == "proxy.schema.json" {
						calls++
						return measuredAnswer(Record{"answers": []any{Record{"index": 1, "decision": "answer", "category": "fact", "answer": "README.mdに記載", "evidence": []any{Record{"kind": "document", "source": "README.md", "location": "1行目"}}}}, "readFiles": []any{}}), nil
					}
					a := reply()
					a["reply"] = "確認しました"
					if mode == "ambiguous" {
						a["reply"] = "分割案を取り消すか確認します。"
					}
					if mode == "cancel" || mode == "cancel_notice" || mode == "cancel_completion" {
						a["reply"] = "分割案をすべて取り消します。"
					}
					if mode == "cancel_notice" {
						a["newProject"] = Record{"needed": true}
					}
					a["splitIssues"] = []any{}
					if mode == "replace" {
						a["splitIssues"] = []any{Record{"title": "新", "body": "新しい内容"}}
					}
					a["completionPlan"] = completionFixture().Record()
					if mode == "empty_completion" || mode == "cancel_completion" {
						a["completionPlan"] = approvalFixture().Record()
					}
					if proxy {
						a["questions"] = []any{"実装資料はどこですか"}
					}
					return measuredAnswer(a), nil
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
				_, err = service.CreateDraft(ctx, Record{"tier": "light", "id": id, "project": "owner/app", "hold": true}, principal, false)
				if err != nil {
					t.Fatal(err)
				}
				saved, err := s.options.Records.Change(ctx, "drafts", id, func(_ *sql.Tx, d Record) (Record, error) {
					d["splitIssues"] = []any{Record{"title": "元", "body": "元の内容"}}
					return d, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				queued, err := service.Chat(ctx, id, Record{"revision": saved["revision"], "text": "相談"}, principal)
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 2)
				engine, err := consultations.NewEngine(ctx, consultations.EngineOptions{Service: service, Turn: s.Turn, Report: func(err error) { done <- err }, Publish: func(consultations.Record) {
					d, e := service.Draft(ctx, id, principal)
					if e != nil || d["status"] != "thinking" {
						done <- e
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
				saved, err = service.Draft(ctx, id, principal)
				if err != nil {
					t.Fatal(err)
				}
				plans := fleet.Elements(saved["splitIssues"])
				if mode == "cancel" || mode == "cancel_notice" || mode == "cancel_completion" {
					if len(plans) != 0 {
						t.Fatal(plans)
					}
				} else {
					want := "元"
					if mode == "replace" {
						want = "新"
					}
					if len(plans) != 1 || fleet.Object(plans[0])["title"] != want {
						t.Fatal(plans)
					}
				}
				if mode == "empty_completion" || mode == "cancel_completion" {
					if saved["ready"] != false || saved["completionApproval"] != nil || saved["completionProposal"] != approvalFixture().Fingerprint() {
						t.Fatal("split handling bypassed completion confirmation", saved)
					}
					pending, err := completionplan.Parse(saved["completionPlan"])
					if err != nil || pending.Fingerprint() != approvalFixture().Fingerprint() {
						t.Fatal("completion presentation was lost while saving splits", pending, err)
					}
				}
				messages := fleet.Elements(saved["messages"])
				found := false
				for _, m := range messages {
					if strings.Contains(fleet.Text(fleet.Object(m)["text"]), "完了") {
						found = true
					}
				}
				if mode == "cancel_notice" {
					notice := false
					for _, m := range messages {
						notice = notice || strings.Contains(fleet.Text(fleet.Object(m)["text"]), "新しいリポジトリが必要な場合")
					}
					if !notice {
						t.Fatal("new project notice missing", messages)
					}
				}
				if !found {
					t.Fatal("completion plan was not appended", messages)
				}
				if proxy && calls == 0 {
					t.Fatal("proxy was not called")
				}
			})
		}
	}
}
