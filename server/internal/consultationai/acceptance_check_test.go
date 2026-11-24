package consultationai

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/completionplan"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
)

func TestAcceptanceItems(t *testing.T) {
	a := Record{"ready": true, "body": "## 受入条件\n- 確認: `go test ./...` → 期待: 終了コード0\n- 確認: `go test` → 期待: \n- 確認: → 期待: PASS\n- 検証する\n## マージ後の運用作業（自動化の対象外）\n- 検証する", "splitIssues": []any{Record{"acceptance": []any{"確認: GET /api/items → 期待: 200", "確認: 画面: 一覧を開く → 期待: 1件表示", "不明"}}}}
	p := acceptanceProblems(a)
	if len(p) != 4 || !strings.Contains(p[0], "3行") || !strings.Contains(p[3], "分割案1の受入条件3") {
		t.Fatal(p)
	}
	a["ready"] = false
	if len(acceptanceProblems(a)) != 0 {
		t.Fatal("ready=false")
	}
}

func TestAcceptancePlaceholderInPhrase(t *testing.T) {
	for _, tc := range []struct {
		result string
		want   int
	}{
		{"送信の成否が結果不明のとき記録が1件残る", 0},
		{"未確認対象への起票は0件", 0},
		{"不明な番号が1件残る", 0},
		{"ステータスに不明と表示される", 0},
		{"結果が不明、または失敗のとき記録が1件残る", 0},
		{"結果が未確認、もしくは失敗の場合は通知が1件届く", 0},
		{"不明、または未定の場合", 1},
		{"未確認、もしくはTBDのとき", 1},
		{"未定、あるいは不明の場合は通知が1件届く", 1},
		{"不明、またはの場合", 1},
		{"結果が不明、または失敗のとき記録が1件残る、終了状態は未定", 1},
		{"未定", 1},
		{"不明", 1},
		{"TBD", 1},
		{"未定、後で決める", 1},
		{"不明（要確認）", 1},
		{"結果は未定です、後で決める", 1},
		{"TBD (to be decided)", 1},
	} {
		t.Run(tc.result, func(t *testing.T) {
			item := "確認: TestFeature → 期待: " + tc.result
			answer := Record{"ready": true, "body": "## 受入条件\n- " + item, "splitIssues": []any{Record{"acceptance": []any{item}}}}
			problems := acceptanceProblems(answer)
			if len(problems) != 2*tc.want {
				t.Fatalf("problems = %v, want %d", problems, 2*tc.want)
			}
		})
	}
}

func TestAcceptanceUnlistedResultsTurn(t *testing.T) {
	calls := 0
	items := []any{"確認: 画面: 一覧を開く → 期待: 列が増えない", "確認: TestFeature → 期待: 設定が元のまま"}
	s, d := testService(t, func(_ context.Context, _ ai.Request) (ai.Answer, error) {
		calls++
		v := reply()
		v["ready"], v["title"], v["questions"] = true, "題名", []any{}
		v["body"] = "## 受入条件\n- 確認: 画面: 一覧を開く → 期待: 列が増えない\n- 確認: TestFeature → 期待: 設定が元のまま"
		v["splitIssues"] = []any{Record{"acceptance": items}}
		return measuredAnswer(v), nil
	})
	out, err := s.Turn(context.Background(), d)
	if err != nil || out["ready"] != true || calls != 2 {
		t.Fatalf("calls=%d result=%v err=%v", calls, out, err)
	}
}

func TestAcceptanceSpecificMethodsAndResults(t *testing.T) {
	answer := Record{"ready": true, "body": "## 受入条件\n- 確認: 画面: 一覧 → 期待: 表示\n- 確認: sh scripts/verify.sh → 期待: 実行する\n- 確認: sh scripts/verify.sh → 期待: 終了コード0\n- 確認: 画面: 一覧を開く → 期待: 1件表示", "splitIssues": []any{Record{"acceptance": []any{"確認: 画面: 詳細 → 期待: 表示", "確認: bash scripts/check.sh → 期待: PASS"}}}}
	problems := acceptanceProblems(answer)
	if len(problems) != 3 || !strings.Contains(problems[0], "2行：具体的な確認手段・具体的な期待結果") || !strings.Contains(problems[1], "3行：具体的な期待結果") || !strings.Contains(problems[2], "分割案1の受入条件1：具体的な確認手段・具体的な期待結果") {
		t.Fatal(problems)
	}
}
func TestAcceptanceConcreteResultAndStandaloneCommand(t *testing.T) {
	answer := Record{"ready": true, "body": "## 受入条件\n- 確認: TestFeature → 期待: 画面に表示される\n- 確認: ./scripts/verify.sh → 期待: 終了コード0\n- 確認: TestFeature → 期待: 一覧に受付番号が表示される", "splitIssues": []any{Record{"acceptance": []any{"確認: TestFeature → 期待: 結果が表示される", "確認: ../bin/check → 期待: PASS"}}}}
	problems := acceptanceProblems(answer)
	if len(problems) != 2 || !strings.Contains(problems[0], "本文 2行：具体的な期待結果") || !strings.Contains(problems[1], "分割案1の受入条件1：具体的な期待結果") {
		t.Fatal(problems)
	}
}

func TestAcceptanceQualifiedVagueAndErrorFreeResults(t *testing.T) {
	answer := Record{"ready": true, "body": "## 受入条件\n- 確認: TestFeature → 期待: 正常に表示される\n- 確認: TestFeature → 期待: エラーにならない\n- 確認: TestFeature → 期待: 一覧に受付番号が正常に表示される", "splitIssues": []any{Record{"acceptance": []any{"確認: TestFeature → 期待: 正常に表示される", "確認: TestFeature → 期待: エラーにならない"}}}}
	problems := acceptanceProblems(answer)
	if len(problems) != 2 || !strings.Contains(problems[0], "本文 2行：具体的な期待結果") || !strings.Contains(problems[1], "分割案1の受入条件1：具体的な期待結果") {
		t.Fatal(problems)
	}
}

func TestAcceptanceScreenStateAndVagueSuffix(t *testing.T) {
	answer := Record{"ready": true, "body": "## 受入条件\n- 確認: TestFeature → 期待: 正常に表示されることを確認する\n- 確認: 画面: ログインボタンを押す → 期待: ホーム画面に切り替わる", "splitIssues": []any{Record{"acceptance": []any{"確認: TestFeature → 期待: 正常に表示されることを確認する。", "確認: 画面: ログインボタンを押す → 期待: ホーム画面に切り替わる"}}}}
	problems := acceptanceProblems(answer)
	if len(problems) != 2 || !strings.Contains(problems[0], "本文 2行：具体的な期待結果") || !strings.Contains(problems[1], "分割案1の受入条件1：具体的な期待結果") {
		t.Fatal(problems)
	}
}

func TestAcceptanceVagueCompoundAndBareCommand(t *testing.T) {
	answer := Record{"ready": true, "body": "## 受入条件\n- 確認: TestFeature → 期待: 正常に動作して状態が正しい\n- 確認: pwd → 期待: 終了コード0\n- 確認: TestFeature → 期待: 状態が完了に更新される", "splitIssues": []any{Record{"acceptance": []any{"確認: TestFeature → 期待: 正常に動作して状態が正しい", "確認: pwd → 期待: 終了コード0"}}}}
	problems := acceptanceProblems(answer)
	if len(problems) != 2 || !strings.Contains(problems[0], "本文 2行：具体的な期待結果") || !strings.Contains(problems[1], "分割案1の受入条件1：具体的な期待結果") {
		t.Fatal(problems)
	}
}

func TestAcceptanceUnspecifiedMethodAndConjunctiveVagueResult(t *testing.T) {
	answer := Record{"ready": true, "body": "## 受入条件\n- 確認: TestFeature → 期待: 正常に動作し状態が正しい\n- 確認: TBD → 期待: 終了コード0\n- 確認: pwd → 期待: 終了コード0\n- 確認: TestFeature → 期待: 状態が完了に更新される", "splitIssues": []any{Record{"acceptance": []any{"確認: TestFeature → 期待: 正常に動作し状態が正しい", "確認: TBD → 期待: 終了コード0", "確認: `TBD` → 期待: 終了コード0", "確認: pwd → 期待: 終了コード0"}}}}
	problems := acceptanceProblems(answer)
	if len(problems) != 5 || !strings.Contains(problems[0], "本文 2行：具体的な期待結果") || !strings.Contains(problems[1], "本文 3行：具体的な確認手段") || !strings.Contains(problems[2], "分割案1の受入条件1：具体的な期待結果") || !strings.Contains(problems[3], "分割案1の受入条件2：具体的な確認手段") || !strings.Contains(problems[4], "分割案1の受入条件3：具体的な確認手段") {
		t.Fatal(problems)
	}
}

func TestAcceptancePlaceholderExplanationAndVagueState(t *testing.T) {
	items := []string{"確認: TBD later → 期待: 終了コード0", "確認: `TODO later` → 期待: PASS", "確認: TestFeature → 期待: 状態が正しく表示される", "確認: TestFeature → 期待: 状態が完了と表示される", "確認: TestFeature → 期待: 一覧に受付番号が正しく表示される"}
	answer := Record{"ready": true, "body": "## 受入条件\n- " + strings.Join(items, "\n- "), "splitIssues": []any{Record{"acceptance": items}}}
	problems := acceptanceProblems(answer)
	if len(problems) != 6 {
		t.Fatal(problems)
	}
	for i, location := range []string{"本文 2行", "本文 3行", "本文 4行", "分割案1の受入条件1", "分割案1の受入条件2", "分割案1の受入条件3"} {
		if !strings.Contains(problems[i], location) || !strings.Contains(problems[i], []string{"確認手段", "確認手段", "期待結果"}[i%3]) {
			t.Fatal(problems)
		}
	}
}

func TestAcceptanceEmbeddedPlaceholderAndBareCommand(t *testing.T) {
	items := []string{"確認: 手段は TBD later → 期待: 終了コード0", "確認: TestFeature → 期待: 終了コードは未定", "確認: echo → 期待: 終了コード0", "確認: TestFeature → 期待: 未設定と表示される"}
	answer := Record{"ready": true, "body": "## 受入条件\n- " + strings.Join(items, "\n- "), "splitIssues": []any{Record{"acceptance": items}}}
	problems := acceptanceProblems(answer)
	want := []string{"本文 2行：具体的な確認手段", "本文 3行：具体的な期待結果", "分割案1の受入条件1：具体的な確認手段", "分割案1の受入条件2：具体的な期待結果"}
	if len(problems) != len(want) {
		t.Fatal(problems)
	}
	for i := range want {
		if !strings.Contains(problems[i], want[i]) {
			t.Fatal(problems)
		}
	}
}

func TestAcceptanceExpectedExitCodeAndLiteralDisplay(t *testing.T) {
	items := []string{
		"確認: TestFeature → 期待: 終了コード",
		"確認: TestFeature → 期待: 終了コードは未定",
		"確認: TestFeature → 期待: ステータスに不明と表示される",
		"確認: TestFeature → 期待: ステータスに未定と表示されるが終了コードは未定",
		"確認: TestFeature → 期待: 終了コード0",
	}
	answer := Record{"ready": true, "body": "## 受入条件\n- " + strings.Join(items, "\n- "), "splitIssues": []any{Record{"acceptance": items}}}
	problems := acceptanceProblems(answer)
	want := []string{"本文 2行", "本文 3行", "本文 5行", "分割案1の受入条件1", "分割案1の受入条件2", "分割案1の受入条件4"}
	if len(problems) != len(want) {
		t.Fatal(problems)
	}
	for i, location := range want {
		if !strings.Contains(problems[i], location+"：具体的な期待結果") {
			t.Fatal(problems)
		}
	}
}

func TestAcceptanceExitCodeOutputAndQuotedDisplay(t *testing.T) {
	items := []string{
		"確認: TestFeature → 期待: 終了コードを出力する",
		"確認: TestFeature → 期待: exit statusが表示される",
		"確認: TestFeature → 期待: ステータスに『不明』が表示される",
		"確認: TestFeature → 期待: ステータスに「未定」が表示されるが終了コードは未定",
		"確認: TestFeature → 期待: 終了コード0を出力する",
	}
	answer := Record{"ready": true, "body": "## 受入条件\n- " + strings.Join(items, "\n- "), "splitIssues": []any{Record{"acceptance": items}}}
	problems := acceptanceProblems(answer)
	want := []string{"本文 2行", "本文 3行", "本文 5行", "分割案1の受入条件1", "分割案1の受入条件2", "分割案1の受入条件4"}
	if len(problems) != len(want) {
		t.Fatal(problems)
	}
	for i, location := range want {
		if !strings.Contains(problems[i], location+"：具体的な期待結果") {
			t.Fatal(problems)
		}
	}
}

func TestAcceptanceReviewExpressions(t *testing.T) {
	items := []string{
		"確認: TestFeature → 期待: 終了コードを標準出力に表示する",
		"確認: TestFeature → 期待: ステータスに『不明』という文字列が表示される",
		"確認: node scripts/lib/todo-cli.ts → 期待: 終了コード0",
		"確認: TestFeature → 期待: 『不明』という文字列が表示されるが終了コードは未定",
	}
	answer := Record{"ready": true, "body": "## 受入条件\n- " + strings.Join(items, "\n- "), "splitIssues": []any{Record{"acceptance": items}}}
	problems := acceptanceProblems(answer)
	want := []string{"本文 2行", "本文 5行", "分割案1の受入条件1", "分割案1の受入条件4"}
	if len(problems) != len(want) {
		t.Fatal(problems)
	}
	for i, location := range want {
		if !strings.Contains(problems[i], location+"：具体的な期待結果") {
			t.Fatal(problems)
		}
	}
}

func TestAcceptanceExitCodeReviewValues(t *testing.T) {
	items := []string{
		"確認: TestFeature → 期待: 終了コードが正常に表示される",
		"確認: TestFeature → 期待: 終了コードは、0である",
		"確認: TestFeature → 期待: exit status is displayed normally",
	}
	answer := Record{"ready": true, "body": "## 受入条件\n- " + strings.Join(items, "\n- "), "splitIssues": []any{Record{"acceptance": items}}}
	problems := acceptanceProblems(answer)
	want := []string{"本文 2行", "本文 4行", "分割案1の受入条件1", "分割案1の受入条件3"}
	if len(problems) != len(want) {
		t.Fatal(problems)
	}
	for i, location := range want {
		if !strings.Contains(problems[i], location+"：具体的な期待結果") {
			t.Fatal(problems)
		}
	}
}

func TestAcceptanceExitCodeConditions(t *testing.T) {
	items := []string{
		"確認: TestFeature → 期待: exit code is 0",
		"確認: TestFeature → 期待: 終了コードは成功を示す0",
		"確認: TestFeature → 期待: 終了コードが0か確認する",
		"確認: TestFeature → 期待: 終了コードが0かどうか確認する",
		"確認: TestFeature → 期待: 終了コードが0かを確認する",
	}
	answer := Record{"ready": true, "body": "## 受入条件\n- " + strings.Join(items, "\n- "), "splitIssues": []any{Record{"acceptance": items}}}
	problems := acceptanceProblems(answer)
	want := []string{"本文 4行", "本文 5行", "本文 6行", "分割案1の受入条件3", "分割案1の受入条件4", "分割案1の受入条件5"}
	if len(problems) != len(want) {
		t.Fatal(problems)
	}
	for i, location := range want {
		if !strings.Contains(problems[i], location+"：具体的な期待結果") {
			t.Fatal(problems)
		}
	}
}

func TestAcceptanceNumericQuestions(t *testing.T) {
	items := []string{
		"確認: TestFeature → 期待: 終了コードが0であるか確認する",
		"確認: TestFeature → 期待: 件数が3か確認する",
		"確認: TestFeature → 期待: 件数が3件か確認する",
		"確認: TestFeature → 期待: 件数を確認する",
		"確認: TestFeature → 期待: 件数が正しいことを確認する",
		"確認: TestFeature → 期待: 件数が正しい",
		"確認: TestFeature → 期待: 終了コードが0であることを確認する",
		"確認: TestFeature → 期待: 件数が3件表示される",
		"確認: TestFeature → 期待: 件数が3件であることを確認する",
	}
	answer := Record{"ready": true, "body": "## 受入条件\n- " + strings.Join(items, "\n- "), "splitIssues": []any{Record{"acceptance": items}}}
	problems := acceptanceProblems(answer)
	want := []string{"本文 2行", "本文 3行", "本文 4行", "本文 5行", "本文 6行", "本文 7行", "分割案1の受入条件1", "分割案1の受入条件2", "分割案1の受入条件3", "分割案1の受入条件4", "分割案1の受入条件5", "分割案1の受入条件6"}
	if len(problems) != len(want) {
		t.Fatal(problems)
	}
	for i, location := range want {
		if !strings.Contains(problems[i], location+"：具体的な期待結果") {
			t.Fatal(problems)
		}
	}
}

func TestAcceptanceExitCodeQuestionStopsTurn(t *testing.T) {
	calls := 0
	s, d := testService(t, func(_ context.Context, _ ai.Request) (ai.Answer, error) {
		calls++
		v := reply()
		v["ready"], v["title"], v["questions"] = true, "題名", []any{}
		v["body"] = "## 受入条件\n- 確認: TestFeature → 期待: 件数が正しい"
		v["splitIssues"] = []any{Record{"acceptance": []any{"確認: TestFeature → 期待: 件数が正しい"}}}
		return measuredAnswer(v), nil
	})
	result, err := s.Turn(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	questions := fleet.Elements(result["questions"])
	reasons := fleet.Elements(result["questionReasons"])
	if calls != 3 || result["ready"] == true || len(questions) == 0 || len(reasons) == 0 || !strings.Contains(fleet.Text(fleet.Object(reasons[0])["reason"]), "本文 2行") || !strings.Contains(fleet.Text(fleet.Object(reasons[0])["reason"]), "分割案1の受入条件1") || !strings.Contains(fleet.Text(fleet.Object(reasons[0])["reason"]), "期待結果") {
		t.Fatalf("calls=%d result=%v", calls, result)
	}
}

func TestAcceptanceFences(t *testing.T) {
	a := Record{"ready": true, "body": "節外\n- 不足\n## 受入条件\n説明\n```go\n- 不足\n```\n- 不足\n~~~\n## 別節\n- 不足\n"}
	p := acceptanceProblems(a)
	if len(p) != 1 || !strings.Contains(p[0], "8行") {
		t.Fatal(p)
	}
	a["body"] = "受入条件なし\n- 不足"
	if len(acceptanceProblems(a)) != 0 {
		t.Fatal(acceptanceProblems(a))
	}
}
func TestAcceptanceTurnRepair(t *testing.T) {
	for _, tc := range []struct {
		name         string
		fixed, limit bool
	}{{"成功", true, false}, {"未補完", false, false}, {"上限", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			saw := false
			s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
				calls++
				if strings.Contains(r.Input, "受入条件の確認手段と期待結果を補完") {
					saw = true
				}
				v := reply()
				v["ready"], v["title"], v["questions"] = true, "題名", []any{}
				v["body"] = "## 受入条件\n- 確認: 手段は TBD later → 期待: 終了コード0\n- 確認: TestFeature → 期待: 終了コードは未定"
				if tc.fixed && saw {
					v["body"] = "## 受入条件\n- 確認: echo → 期待: 終了コード0"
				}
				return measuredAnswer(v), nil
			})
			if tc.limit {
				d["formatRepairs"] = Record{"total": 8}
			}
			result, err := s.Turn(context.Background(), d)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.limit {
				saved, found, err := s.options.Records.Get(context.Background(), "drafts", fleet.Text(d["id"]))
				if err != nil || !found || fleet.Number(fleet.Object(fleet.Object(saved["formatRepairs"])["kinds"])["acceptance"]) != 1 {
					t.Fatal(saved, err)
				}
			}
			if !tc.fixed && len(fleet.Elements(result["questions"])) == 0 {
				t.Fatal(result)
			}
			if tc.limit {
				if saw || calls != 2 || result["ready"] != false {
					t.Fatal(calls, result)
				}
			} else if !saw || calls != 3 || (result["ready"] == true) != tc.fixed {
				t.Fatal(calls, result)
			}
		})
	}
}

func TestAcceptanceRepairResume(t *testing.T) {
	calls := 0
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		calls++
		if r.OnSession != nil && r.Session == "" {
			if err := r.OnSession("11111111-1111-4111-8111-111111111112"); err != nil {
				t.Fatal(err)
			}
		}
		v := reply()
		v["ready"], v["title"], v["body"], v["questions"] = true, "題名", "## 受入条件\n- 不足", []any{}
		if calls == 3 {
			if !r.Resume || !strings.Contains(r.Input, "受入条件の確認手段と期待結果を補完") || r.CallTrigger != "format_repair" || r.RepairKind != "acceptance" {
				t.Fatalf("repair: %+v", r)
			}
			v["readFiles"] = []any{Record{"file": "invalid"}}
		}
		if calls == 4 {
			if strings.Contains(r.Input, "受入条件の確認手段と期待結果を補完") {
				t.Fatal("repair instruction leaked")
			}
			v["ready"] = false
		}
		return measuredAnswer(v), nil
	})
	result, err := s.Turn(context.Background(), d)
	if err != nil || calls != 4 || result["ready"] != false {
		t.Fatalf("calls=%d result=%v err=%v", calls, result, err)
	}
}

func TestAcceptanceNumberedContinuationAndIsolation(t *testing.T) {
	answer := Record{"ready": true, "body": "## 受入条件\n1. 動作\n   確認: TestFeature → 期待: PASS\n2. 確認: `go test ./...`\n3. → 期待: 0件\n4. 確認: 画面: 一覧を開く → 期待: 3件表示\n5. 確認: GET /items → 期待: 応答200\n6. 確認: `curl /metrics` → 期待: liveCheck 99が95以上"}
	problems := acceptanceProblems(answer)
	if len(problems) != 2 || !strings.Contains(problems[0], "4行") || !strings.Contains(problems[0], "期待結果") || !strings.Contains(problems[1], "5行") || !strings.Contains(problems[1], "確認手段") {
		t.Fatal(problems)
	}
}

func TestAcceptanceAfterCompletionApproval(t *testing.T) {
	p := completionFixture()
	p.Approvals = []completionplan.Approval{{ID: "p", Category: "permission", Phase: "before_implementation", Target: "設定", Operation: "変更", Condition: "確認"}}
	first := Record{"ready": true, "completionPlan": p.Record(), "questions": []any{}}
	prepareCompletionPlan(Record{}, first)
	q := fleet.Text(fleet.Elements(first["questions"])[0])
	calls := 0
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		calls++
		v := reply()
		v["ready"], v["title"], v["body"], v["questions"], v["completionPlan"] = false, "題名", "## 受入条件\n- 不足", []any{q}, p.Record()
		return measuredAnswer(v), nil
	})
	s.options.Reference = nil
	d["completionPlan"], d["completionProposal"], d["questions"], d["questionReasons"] = p.Record(), p.Fingerprint(), first["questions"], first["questionReasons"]
	d["messages"] = []any{Record{"role": "assistant", "completionProposal": p.Fingerprint(), "completionPlan": p.Record()}, Record{"id": "u", "role": "user", "text": "1-A", "at": "2026-11-07T10:00:00+09:00", "source": "screen-session"}}
	out, err := s.Turn(context.Background(), d)
	if err != nil || out["ready"] != false || len(fleet.Elements(out["questionReasons"])) == 0 || calls != 2 || !strings.Contains(fleet.Text(fleet.Object(fleet.Elements(out["questionReasons"])[0])["reason"]), "本文 2行") {
		t.Fatal(calls, out, err)
	}
}

// The consultation for draft f14da707 stopped repeatedly on the bare question
// "受入条件の確認手段または期待結果を確定できませんでした。" while the requester could
// not tell which criterion failed. Through Turn, the question now names each failed
// condition by its plan requirement (the body is rebuilt from the execution plan),
// what it lacks and its clipped content, and keeps the reason short.
func TestTurnAcceptanceQuestionNamesFailedPlanRequirements(t *testing.T) {
	long := strings.Repeat("長い条件の説明", 30)
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		reply := gateReply()
		plan := fleet.Object(reply["executionPlan"])
		first := fleet.Object(fleet.Elements(plan["requirements"])[0])
		bad := func(id, criterion string) map[string]any {
			return map[string]any{"id": id, "criterion": criterion, "owner": "implementer", "phase": "verification", "method": "目で見て確認する", "expected": "問題ない", "evidenceKind": "fixed_test", "capabilities": []any{}, "inputs": []any{}, "children": []any{}}
		}
		longer := bad("req-b", "長い条件")
		longer["method"] = "目で見て" + long
		plan["requirements"] = []any{first, bad("req-a", "承認の対象が出る"), longer}
		return measuredAnswer(map[string]any{"response": reply}), nil
	})
	out, err := s.Turn(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	var question, reason string
	for i, q := range fleet.Elements(out["questions"]) {
		if strings.HasPrefix(fleet.Text(q), "受入条件の確認手段または期待結果を確定できませんでした。") {
			question = fleet.Text(q)
			reason = fleet.Text(Record(fleet.Object(fleet.Elements(out["questionReasons"])[i]))["reason"])
		}
	}
	for _, want := range []string{"実行計画の要件 req-a", "実行計画の要件 req-b", "条件の内容：", "…"} {
		if !strings.Contains(question, want) {
			t.Fatalf("question lacks %q: %q", want, question)
		}
	}
	if strings.Contains(question, "本文 ") || out["ready"] == true || strings.Contains(reason, "条件の内容") || !strings.Contains(reason, "受入条件2件が不足") || !strings.Contains(reason, "実行計画の要件 req-b") {
		t.Fatalf("question %q reason %q ready %v", question, reason, out["ready"])
	}
}

// Without an execution plan the body is the requester's, so the line number stays.
// Only the first five failed conditions are listed; the rest are counted.
func TestAcceptanceQuestionListsFirstConditions(t *testing.T) {
	body := "## 受入条件\n"
	for i := 0; i < 7; i++ {
		body += fmt.Sprintf("- 条件%dを確かめる\n", i+1)
	}
	problems := acceptanceProblems(Record{"ready": true, "body": body})
	if len(problems) != 7 || !strings.Contains(problems[0], "本文 2行") || !strings.Contains(problems[0], "条件の内容：「条件1を確かめる」") {
		t.Fatalf("problems %v", problems)
	}
	question := acceptanceQuestion(problems)
	if !strings.Contains(question, "条件5を確かめる") || strings.Contains(question, "条件6を確かめる") || !strings.Contains(question, "ほか2件") {
		t.Fatalf("question %q", question)
	}
}

// Conditions are checked with their own plan requirement, not by re-reading the body rebuilt from
// the plan: a requirement that is not a fixed test in between, a condition whose text contains line
// breaks that look like list items or headings, and split children each keep the right label.
func TestAcceptancePlanLabelsSurviveMultilineAndSplits(t *testing.T) {
	v := gateReply()
	plan := fleet.Object(v["executionPlan"])
	first := fleet.Object(fleet.Elements(plan["requirements"])[0])
	req := func(id, kind, method string, children ...any) map[string]any {
		return map[string]any{"id": id, "criterion": id, "owner": "implementer", "phase": "verification", "method": method, "expected": "問題ない", "evidenceKind": kind, "capabilities": []any{}, "inputs": []any{}, "children": append([]any{}, children...)}
	}
	plan["requirements"] = []any{
		first,
		func() map[string]any {
			r := req("ops", "external_check", "運用担当が確かめる")
			r["owner"] = "controller"
			return r
		}(),
		req("multi", "fixed_test", "目で見て確認する\n- 別の箇条に見える行\n## 見出しに見える行"),
		req("child", "fixed_test", "目で見て確認する", 1),
		req("last", "fixed_test", "目で見て確認する"),
	}
	v["splitIssues"] = []any{Record{"title": "子1", "acceptance": []any{}}}
	problems := acceptanceProblems(v)
	want := []string{"実行計画の要件 multi：", "実行計画の要件 child：", "実行計画の要件 last：", "分割案1の実行計画の要件 multi：", "分割案1の実行計画の要件 child：", "分割案1の実行計画の要件 last："}
	if len(problems) != len(want) {
		t.Fatalf("problems %v", problems)
	}
	for i, w := range want {
		if !strings.HasPrefix(problems[i], w) {
			t.Fatalf("problem %d = %q, want prefix %q", i, problems[i], w)
		}
	}
	for _, p := range problems {
		if strings.Contains(p, "本文 ") || strings.Contains(p, "ops") {
			t.Fatalf("label from the rebuilt body or a non-fixed-test requirement: %q", p)
		}
	}
	reason := acceptanceReason(append(problems, "本文 9行：具体的な期待結果が不足（条件の内容：「x」）"))
	if !strings.HasPrefix(reason, "受入条件7件が不足：") || !strings.Contains(reason, "ほか2件") || strings.Contains(reason, "条件の内容") {
		t.Fatalf("reason %q", reason)
	}
}

// A plan condition whose vague expected result gains a second line is still rejected, as the same
// condition written on one line is.
func TestAcceptancePlanMultilineVagueResultRejected(t *testing.T) {
	v := gateReply()
	plan := fleet.Object(v["executionPlan"])
	first := fleet.Object(fleet.Elements(plan["requirements"])[0])
	first["method"], first["criterion"] = "go test ./...", "確認: `go test ./...` → 期待: 問題なく動作すること"
	first["expected"] = "問題なく動作すること"
	if problems := acceptanceProblems(v); len(problems) != 1 || !strings.Contains(problems[0], "具体的な期待結果") {
		t.Fatalf("one-line vague result: %v", problems)
	}
	first["expected"] = "問題なく動作すること\n（補足）"
	first["criterion"] = "確認: `go test ./...` → 期待: 問題なく動作すること\n（補足）"
	if problems := acceptanceProblems(v); len(problems) != 1 || !strings.HasPrefix(problems[0], "実行計画の要件 fixture：") || !strings.Contains(problems[0], "具体的な期待結果") {
		t.Fatalf("a second line passed a vague result: %v", problems)
	}
}

// A correct condition whose method spans lines passes, and a placeholder left on an indented
// continuation line of the expected result still fails.
func TestAcceptancePlanMultilineMethodAndIndentedPlaceholder(t *testing.T) {
	v := gateReply()
	plan := fleet.Object(v["executionPlan"])
	first := fleet.Object(fleet.Elements(plan["requirements"])[0])
	first["method"], first["expected"] = "cd server\ngo test ./...", "終了コード0で完了する"
	first["criterion"] = "確認: cd server\ngo test ./... → 期待: 終了コード0で完了する"
	if problems := acceptanceProblems(v); len(problems) != 0 {
		t.Fatalf("a multi-line method was rejected: %v", problems)
	}
	first["method"], first["expected"] = "go test ./...", "終了コード0で完了する\n  件数は未定"
	first["criterion"] = "確認: `go test ./...` → 期待: 終了コード0で完了する\n  件数は未定"
	if problems := acceptanceProblems(v); len(problems) != 1 || !strings.Contains(problems[0], "具体的な期待結果") {
		t.Fatalf("a placeholder on an indented line passed: %v", problems)
	}
}

// A placeholder on a continuation line of the expected result fails whether the line is written as
// a list item or holds only the placeholder word.
func TestAcceptancePlanContinuationPlaceholderRejected(t *testing.T) {
	for _, expected := range []string{"終了コード0で完了する\n- 未定", "終了コード0で完了する\n1. TBD", "終了コード0で完了する\n未定"} {
		v := gateReply()
		first := fleet.Object(fleet.Elements(fleet.Object(v["executionPlan"])["requirements"])[0])
		first["method"], first["expected"] = "go test ./...", expected
		first["criterion"] = "確認: `go test ./...` → 期待: " + expected
		if problems := acceptanceProblems(v); len(problems) != 1 || !strings.Contains(problems[0], "具体的な期待結果") {
			t.Fatalf("%q passed: %v", expected, problems)
		}
	}
}
