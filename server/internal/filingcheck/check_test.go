package filingcheck

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/pgtest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, answer func(context.Context, map[string]any) (map[string]any, error)) (*Service, Record, *Record) {
	t.Helper()
	ctx := context.Background()
	records, err := consultations.OpenRecords(ctx, pgtest.New(t))
	if err != nil {
		t.Fatal(err)
	}
	d := Record{"id": "11111111-1111-4111-8111-111111111111", "project": "owner/app", "status": "submitting", "revision": 1, "title": "修正", "body": "目的。src/app.goを直す", "messages": []any{}, "dependsOnIssues": []any{Record{"source": "gitea", "number": 7}}}
	_, err = records.Change(ctx, "drafts", fleet.Text(d["id"]), func(*sql.Tx, Record) (Record, error) { return clone(d), nil })
	if err != nil {
		t.Fatal(err)
	}
	material := Record{"target": Record{"body": d["body"], "dependsOnIssues": []any{Record{"source": "gitea", "number": 7}}}, "requests": []any{Record{"number": 3, "body": "別の目的。src/other.goを直す", "title": "他の依頼", "acceptsInstructions": true}}, "consultations": []any{}, "jobs": []any{}}
	schema, _ := filepath.Abs(filepath.Join("..", "..", "schemas"))
	s, err := New(Options{Records: records, Config: func(context.Context) (map[string]any, error) { return map[string]any{}, nil }, Project: func(string) error { return nil }, Answer: answer, Material: func(context.Context, Record) (Record, error) { return clone(material), nil }, Revision: strings.Repeat("a", 40), SchemaDir: schema, Now: func() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	return s, d, &material
}

func TestNonOverlappingEvidenceAndInputSize(t *testing.T) {
	var inputs []string
	s, d, material := fixture(t, func(_ context.Context, in map[string]any) (map[string]any, error) {
		input := fleet.Text(in["input"])
		inputs = append(inputs, input)
		var sent Record
		if err := json.Unmarshal([]byte(strings.SplitN(input, "\n確認資料:\n", 2)[1]), &sent); err != nil {
			t.Fatal(err)
		}
		row := obj(fleet.Elements(sent["requests"])[0])
		if row["body"] != nil || row["nonOverlapping"] != true || row["acceptsInstructions"] != true || obj(row["job"])["status"] != "queued" {
			t.Fatal(row)
		}
		out := allowed()
		if len(inputs) == 1 {
			// 題名の近い候補の本文を要求し、共通対象と目的を照合する。
			out["proceed"] = false
			out["evidenceRequests"] = []any{Record{"candidateId": "request:3", "kind": "body"}}
		} else {
			if !strings.Contains(string(raw(sent["evidence"])), "共通目的 src/app.go") {
				t.Fatal(sent["evidence"])
			}
			out["proceed"], out["reason"] = false, "重複"
			out["duplicate"] = Record{"candidateId": "request:3", "targetPurposeQuote": "共通目的", "candidatePurposeQuote": "共通目的", "commonTargets": []any{Record{"kind": "path", "value": "src/app.go"}}}
		}
		return out, nil
	})
	body := "共通目的 src/app.go " + strings.Repeat("資料", 1000)
	(*material)["target"] = Record{"body": "共通目的 src/app.go", "changeScope": Record{"known": true, "paths": []any{"src/app.go"}}}
	(*material)["requests"] = []any{Record{"number": 3, "title": "共通目的", "body": body, "paths": []any{"other/file.go"}, "acceptsInstructions": true, "job": Record{"issue": 3, "status": "queued"}}}
	before := len(instructions + "\n確認資料:\n" + string(raw(*material)))
	if err := s.Check(context.Background(), d); err == nil || !strings.Contains(err.Error(), "重複") || len(inputs) != 2 {
		t.Fatal(err, len(inputs))
	}
	if len(inputs[0]) >= before-5000 || strings.Contains(inputs[0], body) || strings.Count(inputs[1], body) != 1 {
		t.Fatal(before, len(inputs[0]), len(inputs[1]))
	}
	if before != len(instructions)+6313 || len(inputs[0]) != len(instructions)+310 || len(inputs[1]) != len(instructions)+6596 {
		t.Fatalf("固定資料の送信量が変わりました: 縮小前=%d 初回=%d 追加取得後=%d", before, len(inputs[0]), len(inputs[1]))
	}
	t.Logf("縮小前=%d 初回=%d 追加取得後=%d", before, len(inputs[0]), len(inputs[1]))
}

func TestNonOverlappingPreservesDependencyAndInstructionDecision(t *testing.T) {
	material := Record{"target": Record{"changeScope": Record{"known": true, "paths": []any{"src/a.go"}}}, "requests": []any{Record{"number": 3, "paths": []any{"other/b.go"}, "acceptsInstructions": false, "instructionBlocks": []any{"送信中"}, "job": Record{"issue": 3, "status": "queued"}}}}
	short := reduceNonOverlapping(material)
	decision := Record{"proceed": false, "duplicate": Record{"candidateId": "request:3"}}
	if len(fleet.Elements(duplicateVerdict(decision, short)["evidenceRequests"])) != 1 || obj(fleet.Elements(short["requests"])[0])["instructionBlocks"] == nil {
		t.Fatal(short, decision)
	}
	short["evidenceFinal"] = true
	short["evidence"] = []any{Record{"candidateId": "request:3", "body": "本文", "acquiredRanges": Record{"body": "all"}}}
	if duplicateVerdict(Record{"proceed": false, "duplicate": Record{"candidateId": "request:3"}}, short)["proceed"] != true {
		t.Fatal("本文取得後に指示受付不可を判定できない")
	}
	deps, err := dependencies(Record{"dependsOnIssues": []any{Record{"source": "gitea", "number": 3}}, "dependencyEvidence": []any{Record{"candidateId": "job:3", "number": 3}}}, short)
	if err != nil || len(deps) != 1 {
		t.Fatal(deps, err)
	}
	obj(obj(material["target"])["changeScope"])["known"] = false
	if obj(fleet.Elements(reduceNonOverlapping(material)["requests"])[0])["nonOverlapping"] != nil {
		t.Fatal("known=false の範囲を縮めた")
	}
}

func TestReducedDuplicateAutomaticallyReadsEveryCandidateBody(t *testing.T) {
	for _, kind := range []string{"request", "job", "requestJob", "consultation"} {
		for _, missing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/missing=%t", kind, missing), func(t *testing.T) {
				calls := 0
				id := "request:3"
				if kind == "job" || kind == "requestJob" {
					id = "job:3"
				}
				if kind == "consultation" {
					id = "consultation:c1"
				}
				s, d, material := fixture(t, func(_ context.Context, in map[string]any) (map[string]any, error) {
					calls++
					var sent Record
					if err := json.Unmarshal([]byte(strings.SplitN(fleet.Text(in["input"]), "\n確認資料:\n", 2)[1]), &sent); err != nil {
						t.Fatal(err)
					}
					if candidates(sent)[id]["nonOverlapping"] != true || candidates(sent)[id]["body"] != nil {
						t.Fatal(sent)
					}
					if calls == 2 && !missing && !strings.Contains(string(raw(sent["evidence"])), "共通目的 src/app.go") {
						t.Fatal(sent)
					}
					out := allowed()
					if calls == 2 && missing {
						out["refusalCategory"] = "input_limit_only"
						return out, nil // 起票済み依頼は本文の照合を要する。
					}
					out["proceed"], out["reason"] = false, "重複"
					out["duplicate"] = Record{"candidateId": id, "targetPurposeQuote": "共通目的", "candidatePurposeQuote": "共通目的", "commonTargets": []any{Record{"kind": "path", "value": "src/app.go"}}}
					return out, nil
				})
				body := "共通目的 src/app.go"
				if missing {
					body += strings.Repeat("大", 25000)
				}
				row := Record{"number": 3, "issue": 3, "id": "c1", "revision": 1, "title": "共通目的", "body": body, "paths": []any{"other/file.go"}}
				(*material)["target"] = Record{"body": "共通目的 src/app.go", "changeScope": Record{"known": true, "paths": []any{"src/app.go"}}}
				(*material)["requests"], (*material)["jobs"], (*material)["consultations"] = []any{}, []any{}, []any{}
				switch kind {
				case "request":
					(*material)["requests"] = []any{row}
				case "requestJob":
					row["job"] = Record{"status": "queued"}
					(*material)["requests"] = []any{row}
				case "job":
					(*material)["jobs"] = []any{row}
				case "consultation":
					(*material)["consultations"] = []any{row}
					_, err := s.options.Records.Change(context.Background(), "drafts", "c1", func(*sql.Tx, Record) (Record, error) { return clone(row), nil })
					if err != nil {
						t.Fatal(err)
					}
				}
				err := s.Check(context.Background(), d)
				want := "重複"
				if missing {
					want = "資料不足"
				}
				if missing && kind == "consultation" {
					if err != nil || calls != 2 {
						t.Fatal(err, calls)
					}
				} else if missing {
					if err == nil || !strings.Contains(err.Error(), want) || calls != 2 {
						t.Fatal(err, calls)
					}
					saved, _, saveErr := s.options.Records.Get(context.Background(), "drafts", fleet.Text(d["id"]))
					checks := fleet.Elements(obj(saved["filingCheckCalls"])["checks"])
					if saveErr != nil || len(checks) != 2 || obj(checks[1])["refusalCategory"] != "other" || obj(checks[1])["reportedRefusalCategory"] != "input_limit_only" {
						t.Fatal(saved, saveErr)
					}
				} else if err == nil || !strings.Contains(err.Error(), want) || calls != 2 {
					t.Fatal(err, calls)
				}
			})
		}
	}
}
func allowed() map[string]any {
	return map[string]any{"proceed": true, "reason": "重複しません", "dependsOnIssues": []any{}, "dependencyEvidence": []any{}, "duplicate": nil, "evidenceRequests": []any{}}
}
func TestCheckKeepsExplicitDependenciesAndAddsOnlySnapshotProof(t *testing.T) {
	s, d, _ := fixture(t, func(context.Context, map[string]any) (map[string]any, error) {
		out := allowed()
		out["dependsOnIssues"] = []any{Record{"source": "gitea", "number": 3}}
		out["dependencyEvidence"] = []any{Record{"candidateId": "request:3", "number": 3}}
		return out, nil
	})
	if err := s.Check(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if len(fleet.Elements(d["dependsOnIssues"])) != 2 {
		t.Fatal(d)
	}
}
func TestDependencyRejectsCommonNumberTransferredAsGitea(t *testing.T) {
	material := Record{"target": Record{}, "requests": []any{Record{"number": 3}}}
	_, err := dependencies(Record{"dependsOnIssues": []any{Record{"source": "gitea", "number": 30}}, "dependencyEvidence": []any{Record{"candidateId": "request:3", "number": 30}}}, material)
	if err == nil {
		t.Fatal("異なる番号を採用した")
	}
}
func TestRefusalUnchangedSnapshotDoesNotCallAgain(t *testing.T) {
	calls := 0
	s, d, _ := fixture(t, func(context.Context, map[string]any) (map[string]any, error) {
		calls++
		out := allowed()
		out["proceed"], out["reason"] = false, "PRの資料がありません"
		return out, nil
	})
	if err := s.Check(context.Background(), d); err == nil {
		t.Fatal("資料不足を通した")
	}
	saved, _, _ := s.options.Records.Get(context.Background(), "drafts", fleet.Text(d["id"]))
	if err := s.Check(context.Background(), saved); err == nil || calls != 1 {
		t.Fatal(err, calls)
	}
}
func TestEvidenceGetsOneFinalDecisionAndNoThirdCall(t *testing.T) {
	calls := 0
	s, d, _ := fixture(t, func(context.Context, map[string]any) (map[string]any, error) {
		calls++
		out := allowed()
		out["evidenceRequests"] = []any{Record{"candidateId": "request:3", "kind": "body"}}
		out["proceed"] = false
		return out, nil
	})
	if err := s.Check(context.Background(), d); err == nil || calls != 2 {
		t.Fatal(err, calls)
	}
	saved, _, _ := s.options.Records.Get(context.Background(), "drafts", fleet.Text(d["id"]))
	if obj(saved["filingCheckCalls"])["stopKind"] != "evidence_missing" {
		t.Fatal(saved)
	}
}
func TestDuplicateNeedsEqualPurposeQuotesAndBothCommonTargets(t *testing.T) {
	material := Record{"target": Record{"body": "目的。src/app.goを直す"}, "requests": []any{Record{"number": 3, "body": "別の目的。src/app.goを直す"}}, "evidenceFinal": true}
	decision := Record{"proceed": false, "duplicate": Record{"candidateId": "request:3", "targetPurposeQuote": "目的。", "candidatePurposeQuote": "別の目的。", "commonTargets": []any{Record{"kind": "path", "value": "src/app.go"}}}}
	if got := duplicateVerdict(decision, material); got["proceed"] != true {
		t.Fatal(got)
	}
}
func TestSelectionProtectsAcceptanceAndRejectsFenceCut(t *testing.T) {
	source := "# 本文\n内容\n## 受入条件\n条件を守る\n## 補足\n別資料"
	selected, _, err := selectText(source, Record{"section": "補足"}, true)
	if err != nil || !strings.Contains(selected, "条件を守る") {
		t.Fatal(selected, err)
	}
	_, _, err = selectText("前\n```go\ncode\n```\n後", Record{"startLine": 3, "endLine": 4}, false)
	if err == nil {
		t.Fatal("コードフェンス途中の切断を許可した")
	}
}
func TestRepairGenerationDoesNotGrantOnNewCodeSHAAlone(t *testing.T) {
	calls := Record{"count": 5, "outcome": "refused", "codeRevision": "old"}
	if repairBaseline(calls) {
		t.Fatal("不明なbaselineで復活した")
	}
	if repairBaseline(calls) {
		t.Fatal("同じgenerationで復活した")
	}
	calls["recovery"] = Record{"baseline": Record{"refused": "0"}, "used": Record{"refused": []any{"0"}}}
	if !repairBaseline(calls) {
		t.Fatal("既知の修復generationを検出しなかった")
	}
	consumeRepair(calls)
	if repairBaseline(calls) {
		t.Fatal("同じ修復を二度使用した")
	}
}
func TestTypedScopeAndDependencySlicesRemainVisible(t *testing.T) {
	if len(fleet.Elements([]int64{1, 2})) != 2 || len(fleet.Elements([]string{"src/a.go"})) != 1 {
		t.Fatal("型付きの依存や範囲が消えた")
	}
}

func TestOversizedReferenceMessagesAreDroppedBeforeRefusing(t *testing.T) {
	calls := 0
	s, d, material := fixture(t, func(_ context.Context, in map[string]any) (map[string]any, error) {
		calls++
		if strings.Contains(fleet.Text(in["input"]), "祖先の長い発言") {
			t.Fatal("祖先の発言を省略せずに送った")
		}
		return allowed(), nil
	})
	long := strings.Repeat("祖先の長い発言", 12000)
	obj(*material)["target"].(Record)["decisionContext"] = Record{"messages": []any{}, "references": []any{Record{"draftId": "a", "title": "親", "body": "親の本文", "messages": []any{Record{"text": long}}}}, "omittedMessages": 0, "omittedReferences": 0}
	if err := s.Check(context.Background(), d); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
}
func TestOversizedInputFailureShowsBreakdownInBytesAndCharacters(t *testing.T) {
	s, d, material := fixture(t, func(context.Context, map[string]any) (map[string]any, error) { return allowed(), nil })
	obj(*material)["target"].(Record)["body"] = strings.Repeat("あ", 60000)
	err := s.Check(context.Background(), d)
	if err == nil || !strings.Contains(err.Error(), "入力上限") || !strings.Contains(err.Error(), "内訳(bytes/文字)") || !strings.Contains(err.Error(), "対象本文") {
		t.Fatal(err)
	}
	// 読み足し・明示依存・ファイル有無・省略の記録も内訳に出す（合計との差が分かるように）。
	for _, part := range []string{"読み足し", "明示依存", "ファイル有無", "読み足し省略", "上限省略"} {
		if !strings.Contains(err.Error(), "・"+part) {
			t.Fatal(part, err)
		}
	}
}

// overlappingCandidates は、確認対象と同じファイルに触れる開いている依頼を多数作る。
// 先頭の候補ほど確認対象と重なるパスが多い。
func overlappingCandidates(count, paths int) []any {
	list := []any{}
	for i := 0; i < count; i++ {
		scope := []any{"server/internal/shared/common.go"}
		if i < 3 {
			scope = append(scope, "server/internal/shared/second.go")
		}
		for j := 0; j < paths; j++ {
			scope = append(scope, fmt.Sprintf("server/internal/package%03d/source-file-%03d.go", i, j))
		}
		blocks := []any{}
		for j := 0; j < 6; j++ {
			blocks = append(blocks, Record{"reason": strings.Repeat("指示を受け付けない理由", 6), "at": "2026-11-07T00:00:00Z"})
		}
		list = append(list, Record{"number": 100 + i, "title": fmt.Sprintf("重なる候補%d", i), "body": strings.Repeat("候補の本文", 800), "paths": scope, "acceptsInstructions": true, "instructionBlocks": blocks,
			"job": Record{"id": 500 + i, "issue": 100 + i, "status": "queued", "scope": scope}})
	}
	return list
}
func TestOversizedOverlappingCandidatesAfterEvidenceStillReachAnswer(t *testing.T) {
	calls := 0
	var sent []string
	s, d, material := fixture(t, func(_ context.Context, in map[string]any) (map[string]any, error) {
		calls++
		sent = append(sent, fleet.Text(in["input"]))
		out := allowed()
		if calls == 1 {
			out["proceed"] = false
			out["evidenceRequests"] = []any{Record{"candidateId": "request:100", "kind": "body"}, Record{"candidateId": "request:101", "kind": "body"}, Record{"candidateId": "request:102", "kind": "body"}}
		}
		return out, nil
	})
	obj(*material)["requests"] = overlappingCandidates(700, 8)
	obj(*material)["target"].(Record)["changeScope"] = Record{"known": true, "paths": []any{"server/internal/shared/common.go", "server/internal/shared/second.go"}}
	if err := s.Check(context.Background(), d); err != nil || calls != 2 {
		t.Fatal("入力上限で止めずに判定へ進んでいません", err, calls)
	}
	for _, input := range sent {
		if len(input)+len(s.schema) > inputLimit {
			t.Fatal("入力上限を超えて送った", len(input))
		}
	}
	if !strings.Contains(sent[1], inputLimitOmittedKey) || !strings.Contains(sent[1], "重なる候補0") {
		t.Fatal("重なりの多い候補を残して省略を記録していません")
	}
	saved, _, err := s.options.Records.Get(context.Background(), "drafts", fleet.Text(d["id"]))
	if err != nil {
		t.Fatal(err)
	}
	checks := fleet.Elements(obj(saved["filingCheckCalls"])["checks"])
	omissions := fleet.Elements(obj(checks[len(checks)-1])["inputOmissions"])
	found := false
	for _, item := range omissions {
		if row := obj(item); row["reason"] == "入力上限" && row["kind"] == "候補一覧" && row["bodyRequiredForDuplicateCheck"] == true {
			found = true
		}
	}
	if !found {
		t.Fatal("入力上限の省略が記録されていません", omissions)
	}
}
func TestFilingCheckKeepsConsultationBodiesWithoutInputMetrics(t *testing.T) {
	const candidateCount = 22
	const candidateID = "22222222-2222-4222-8222-222222222222"
	var body string
	s, d, material := fixture(t, func(_ context.Context, in map[string]any) (map[string]any, error) {
		_, data, found := strings.Cut(fleet.Text(in["input"]), materialSeparator)
		var sent Record
		if !found || json.Unmarshal([]byte(data), &sent) != nil {
			t.Fatal("判定AIに資料が届いていません")
		}
		candidates := fleet.Elements(sent["consultations"])
		if len(candidates) != candidateCount || len(fleet.Elements(sent[inputLimitOmittedKey])) != 0 {
			t.Fatalf("候補の件数または省略が変わりました: %d", len(candidates))
		}
		first := obj(candidates[0])
		_, hasMetrics := first["inputBudget"]
		if hasMetrics || first["id"] != candidateID || first["body"] != body || first["status"] != "draft" || fleet.WholeNumber(first["messageCount"]) != 3 {
			t.Fatal("候補本文・状態・発言件数と計測明細の投影が不正です")
		}
		return allowed(), nil
	})
	rows := make([]any, candidateCount)
	for i := range rows {
		rows[i] = Record{"id": fmt.Sprintf("candidate-%d", i), "status": "draft", "title": "比較候補", "messageCount": 3}
	}
	obj(rows[0])["id"] = candidateID
	(*material)["consultations"] = rows
	body = "対象と同じファイルに関わる背景。" + strings.Repeat("x", (inputLimit-s.inputSize(*material)-3500)/candidateCount)
	for _, row := range rows {
		obj(row)["body"] = body
	}
	if s.inputSize(*material) >= inputLimit {
		t.Fatal("計測明細を除いても上限を超えている")
	}
	metrics := make([]any, 64)
	for i := range metrics {
		metrics[i] = Record{"at": "2026-11-01T00:00:00Z", "sha256": strings.Repeat("a", 64), "bytes": 4096}
	}
	obj(rows[0])["inputBudget"] = Record{"records": metrics}
	if s.inputSize(*material) <= inputLimit {
		t.Fatal("計測明細で入力上限を超える状況を作れていません")
	}
	ctx := t.Context()
	_, err := s.options.Records.Change(ctx, "drafts", candidateID, func(*sql.Tx, Record) (Record, error) {
		return clone(obj(rows[0])), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Check(ctx, d); err != nil {
		t.Fatal(err)
	}
	saved, _, err := s.options.Records.Get(ctx, "drafts", candidateID)
	if err != nil || len(fleet.Elements(obj(saved["inputBudget"])["records"])) != len(metrics) {
		t.Fatal("保存された計測明細が変わりました", err)
	}
}

func TestFitInputDropsSummaryDetailBeforeCandidates(t *testing.T) {
	material := Record{"target": Record{"changeScope": Record{"known": true, "paths": []any{"server/internal/shared/common.go"}}}, "requests": overlappingCandidates(3, 30)}
	material = reduceCandidates(material)
	detail := dropSummaryDetail(material)
	row := obj(fleet.Elements(detail["requests"])[0])
	if row["instructionBlocks"] != nil || row["job"] != nil || row["acceptsInstructions"] != true {
		t.Fatal("指示ブロック・ジョブを落とすか受け付け可否を残していません", row)
	}
	paths := keepOverlappingPaths(detail)
	row = obj(fleet.Elements(paths["requests"])[0])
	if len(fleet.Elements(row["paths"])) != 1 || fleet.WholeNumber(row["pathsTotal"]) != 32 {
		t.Fatal("重ならないパスを全件数付きで落としていません", row)
	}
	omissions := inputOmissions(paths)
	kinds := map[string]bool{}
	for _, item := range omissions {
		kinds[fleet.Text(obj(item)["kind"])] = true
	}
	if !kinds["要約候補の指示ブロック・ジョブ"] || !kinds["要約候補の重ならないパス"] {
		t.Fatal(omissions)
	}
}
func bigCandidates(count, paths int) []any {
	list := []any{}
	for i := 0; i < count; i++ {
		scope := []any{}
		for j := 0; j < paths; j++ {
			scope = append(scope, fmt.Sprintf("server/internal/package%03d/source-file-%03d.go", i, j))
		}
		list = append(list, Record{"number": 100 + i, "title": fmt.Sprintf("候補の題名%d", i), "body": strings.Repeat("候補の本文", 800), "paths": scope, "acceptsInstructions": true,
			"job": Record{"id": 500 + i, "issue": 100 + i, "status": "queued", "scope": scope, "body": strings.Repeat("ジョブの本文", 400), "view": Record{"log": strings.Repeat("実行記録", 400)}}})
	}
	return list
}
func TestCandidateSummaryDoesNotRepeatTitlesPathsOrJobRecords(t *testing.T) {
	out := reduceCandidates(Record{"requests": bigCandidates(2, 3)})
	for _, item := range fleet.Elements(out["omittedCandidateSummaries"]) {
		row := obj(item)
		if _, exists := row["title"]; exists {
			t.Fatal("題名を重ねて入れた", row)
		}
		if _, exists := row["paths"]; exists {
			t.Fatal("パスを重ねて入れた", row)
		}
		if row["candidateId"] == "" || fleet.WholeNumber(row["bytes"]) == 0 {
			t.Fatal(row)
		}
	}
	row := obj(fleet.Elements(out["requests"])[0])
	job := obj(row["job"])
	if row["title"] == nil || len(fleet.Elements(row["paths"])) != 3 || row["body"] != nil || job["status"] != "queued" || len(fleet.Elements(job["scope"])) != 3 || job["body"] != nil || job["view"] != nil {
		t.Fatal(row)
	}
}
func TestManyOpenRequestsDoNotBlockFilingWithAShortBody(t *testing.T) {
	var sent string
	s, d, material := fixture(t, func(_ context.Context, in map[string]any) (map[string]any, error) {
		sent = fleet.Text(in["input"])
		return allowed(), nil
	})
	obj(*material)["requests"] = bigCandidates(40, 25)
	if err := s.Check(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sent, "候補の本文") || strings.Contains(sent, "実行記録") || !strings.Contains(sent, "候補の題名39") {
		t.Fatal(len(sent))
	}
}
func TestCandidatePathsAreCutOnlyWithTheirTotalWhenStillOversized(t *testing.T) {
	var sent string
	s, d, material := fixture(t, func(_ context.Context, in map[string]any) (map[string]any, error) {
		sent = fleet.Text(in["input"])
		return allowed(), nil
	})
	obj(*material)["requests"] = bigCandidates(60, 60)
	if err := s.Check(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sent, "pathsTotal") || !strings.Contains(sent, "scopeTotal") || strings.Contains(sent, "package000/source-file-008.go") {
		t.Fatal(len(sent))
	}
	small := reduceCandidatePaths(Record{"requests": bigCandidates(1, summaryPathLimit)})
	if _, exists := obj(fleet.Elements(small["requests"])[0])["pathsTotal"]; exists {
		t.Fatal("上限以内のパスを切った")
	}
}
func TestOversizedBreakdownCountsOmittedCandidateList(t *testing.T) {
	message := sizeBreakdown(Record{"target": Record{}, "omittedCandidateSummaries": []any{Record{"candidateId": "request:1"}}}, 1, 1)
	if !strings.Contains(message, "省略候補") {
		t.Fatal(message)
	}
}
func TestJoinedJobBodyIsRemoved(t *testing.T) {
	same := withoutJobBody(Record{"issue": 3, "body": "同じ本文"})
	if _, exists := same["body"]; exists {
		t.Fatal(same)
	}
	different := withoutJobBody(Record{"issue": 3, "body": "ジョブ側の本文"})
	if _, exists := different["body"]; exists {
		t.Fatal(different)
	}
}

func TestNonOverlappingCandidatesKeepOnlyTitleAndStatusWhenStillOversized(t *testing.T) {
	scope := func(paths ...string) Record {
		list := []any{}
		for _, p := range paths {
			list = append(list, p)
		}
		return Record{"known": true, "paths": list}
	}
	material := Record{
		"target": Record{"changeScope": scope("server/internal/package001/source-file-001.go", "docs/")},
		"requests": []any{
			Record{"number": 1, "title": "重なる依頼", "status": "queued", "body": "本文", "paths": []any{"server/internal/package001/source-file-001.go"}},
			Record{"number": 2, "title": "別のディレクトリ", "status": "queued", "paths": []any{"client/src/app.ts"}},
			Record{"number": 3, "title": "ディレクトリの下", "status": "queued", "paths": []any{"docs/ai-usage.md"}},
			Record{"number": 4, "title": "パスを持たない依頼", "status": "queued"},
		},
		"jobs":          []any{Record{"issue": 5, "title": "ジョブ", "status": "running", "scope": []any{"client/src/app.ts"}}},
		"consultations": []any{Record{"id": "draft", "title": "相談", "body": "本文", "changeScope": scope("client/src/app.ts")}, Record{"id": "unknown", "body": "本文", "changeScope": Record{"known": false, "paths": []any{"client/src/app.ts"}}}},
	}
	out := reduceNonOverlapping(material)
	requests := fleet.Elements(out["requests"])
	if obj(requests[0])["nonOverlapping"] != nil || obj(requests[0])["paths"] == nil {
		t.Fatal("重なる依頼を省いた", requests[0])
	}
	if obj(requests[1])["nonOverlapping"] != true || obj(requests[1])["paths"] != nil || obj(requests[1])["title"] != "別のディレクトリ" {
		t.Fatal("重ならない依頼を題名だけにしなかった", requests[1])
	}
	if obj(requests[2])["nonOverlapping"] != nil {
		t.Fatal("ディレクトリの下のパスを重ならないとした", requests[2])
	}
	if obj(requests[3])["nonOverlapping"] != nil {
		t.Fatal("パスを持たない依頼を省いた（重なりを判断できない）", requests[3])
	}
	if obj(fleet.Elements(out["jobs"])[0])["nonOverlapping"] != true || obj(fleet.Elements(out["consultations"])[0])["nonOverlapping"] != true || obj(fleet.Elements(out["consultations"])[1])["body"] != "本文" || fleet.WholeNumber(out["omittedNonOverlapping"]) != 3 {
		t.Fatal("ジョブの省略または件数が不正です", out)
	}
	unknown := reduceNonOverlapping(Record{"target": Record{"changeScope": Record{"known": false}}, "requests": []any{Record{"number": 9, "title": "x", "paths": []any{"a.go"}}}})
	if obj(fleet.Elements(unknown["requests"])[0])["nonOverlapping"] != nil || unknown["omittedNonOverlapping"] != nil && fleet.WholeNumber(unknown["omittedNonOverlapping"]) != 0 {
		t.Fatal("対象の範囲が未確定なのに省いた", unknown)
	}
}
func TestHundredsOfOpenRequestsFitByDroppingOnlyNonOverlappingDetail(t *testing.T) {
	var sent string
	s, d, material := fixture(t, func(_ context.Context, in map[string]any) (map[string]any, error) {
		sent = fleet.Text(in["input"])
		return allowed(), nil
	})
	obj(*material)["requests"] = bigCandidates(900, 40)
	obj(*material)["target"].(Record)["changeScope"] = Record{"known": true, "paths": []any{"server/internal/package001/source-file-001.go"}}
	if err := s.Check(context.Background(), d); err != nil {
		t.Fatal("重ならない候補を省いても上限を超えました", err)
	}
	if !strings.Contains(sent, "nonOverlapping") || !strings.Contains(sent, "omittedNonOverlapping") || !strings.Contains(sent, "候補の題名1") {
		t.Fatal("題名を残して省略した形で送られていません", len(sent))
	}
	if !strings.Contains(sent, "package001/source-file-001.go") {
		t.Fatal("重なる候補のパスが消えた")
	}
}

func TestOutsideConsultationEvidence(t *testing.T) {
	ctx := context.Background()
	s, d, material := fixture(t, func(context.Context, map[string]any) (map[string]any, error) { return allowed(), nil })
	save := func(id, project, status, body string) {
		t.Helper()
		_, err := s.options.Records.Change(ctx, "drafts", id, func(*sql.Tx, Record) (Record, error) {
			record := Record{"id": id, "project": project, "status": status, "title": "題名", "body": body, "changeScope": Record{"known": true, "paths": []any{"a.go"}}, "issue": Record{"number": 7}, "issues": []any{Record{"number": 8}}, "messages": []any{"秘密"}}
			if id == "child" {
				record["parentSplit"] = Record{"draftId": "parent"}
			}
			return record, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	body := "## 目的\n目的本文\n## 調査メモ\n秘密の調査\n## 受入条件\n受入本文"
	save("filed", "OWNER/APP", "filed", body)
	save("child", "owner/app", "draft", body)
	save("other", "other/app", "filed", body)
	save("empty", "owner/app", "filed", "## 調査\n秘密")
	request := func(id string, sel any) Record {
		return Record{"candidateId": "consultation:" + id, "kind": "body", "selection": sel}
	}
	for _, id := range []string{"filed", "child"} {
		for _, sel := range []any{nil, Record{"body": nil, "reference": nil, "messages": nil}} {
			out, err := s.expand(ctx, d, *material, []any{request(id, sel)})
			if err != nil {
				t.Fatal(err)
			}
			e := obj(fleet.Elements(out["evidence"])[0])
			text := string(raw(e))
			for _, want := range []string{"題名", "目的本文", "受入本文", "a.go", "number"} {
				if !strings.Contains(text, want) {
					t.Fatal(text, want)
				}
			}
			if strings.Contains(text, "秘密") || obj(e["acquiredRanges"])["body"] == "all" || e["reference"] != nil || len(fleet.Elements(e["messages"])) != 0 {
				t.Fatal(e)
			}
		}
	}
	out, err := s.expand(ctx, d, *material, []any{request("empty", nil), request("other", nil), request("absent", nil), request("filed", Record{"messages": Record{"start": 1, "end": 1}})})
	if err != nil || len(fleet.Elements(out["evidenceMissing"])) != 2 || len(fleet.Elements(out["evidenceOutsideCandidates"])) != 1 || len(fleet.Elements(out["evidence"])) != 1 {
		t.Fatal(out, err)
	}
	bad, err := s.expand(ctx, d, *material, []any{request("absent", nil), Record{"candidateId": "request:3", "kind": "invalid"}})
	if err != nil || len(fleet.Elements(bad["evidenceMissing"])) != 2 {
		t.Fatal(bad, err)
	}
	for _, item := range fleet.Elements(bad["evidenceMissing"]) {
		if !strings.Contains(fleet.Text(obj(item)["next"]), "consultation:<相談ID>") {
			t.Fatal(item)
		}
	}
	selected, err := s.expand(ctx, d, *material, []any{request("filed", Record{"body": Record{"section": "調査メモ"}})})
	if err != nil || (!strings.Contains(string(raw(selected["evidence"])), "秘密の調査") || strings.Contains(string(raw(selected["evidence"])), "目的本文")) {
		t.Fatal(selected, err)
	}
	var wanted []any
	for i := 0; i < 13; i++ {
		id := fmt.Sprintf("limit%d", i)
		save(id, "owner/app", "filed", body)
		wanted = append(wanted, request(id, nil))
	}
	out, err = s.expand(ctx, d, *material, wanted)
	if err != nil || len(fleet.Elements(out["evidence"])) != 12 || len(fleet.Elements(out["evidenceOmitted"])) != 1 {
		t.Fatal(out, err)
	}
	save("large", "owner/app", "filed", "## 目的\n"+strings.Repeat("x", 60001))
	out, err = s.expand(ctx, d, *material, []any{request("large", nil)})
	if err != nil || len(fleet.Elements(out["evidenceOmitted"])) != 1 {
		t.Fatal(out, err)
	}
	wanted = nil
	longBody := "## 目的\n" + strings.Repeat("line\n", 30)
	save("many", "owner/app", "filed", longBody)
	for i := 1; i <= 25; i++ {
		wanted = append(wanted, request("many", Record{"body": Record{"startLine": i, "endLine": i}}))
	}
	out, err = s.expand(ctx, d, *material, wanted)
	if err != nil || len(fleet.Elements(out["evidence"])) != 24 || len(fleet.Elements(out["evidenceOmitted"])) != 1 {
		t.Fatal(out, err)
	}

}

func TestConsultationSummaryInputBytes(t *testing.T) {
	body := "## 目的\n目的本文\n## 調査メモ\n" + strings.Repeat("調", 2000) + "\n## 受入条件\n受入本文"
	summary := consultationSections(body)
	if strings.Contains(summary, "調査メモ") || !strings.Contains(summary, "受入本文") || len(summary) >= len(body) {
		t.Fatal(len(body), len(summary), summary)
	}
	t.Logf("全文=%d bytes 節抽出後=%d bytes", len(body), len(summary))
}

func TestOutsideConsultationDuplicateVerdict(t *testing.T) {
	material := Record{"target": Record{"body": "共通目的 a.go"}, "consultations": []any{}, "evidence": []any{Record{"candidateId": "consultation:outside", "body": "共通目的 a.go"}}, "evidenceFinal": true}
	result := duplicateVerdict(Record{"proceed": false, "duplicate": Record{"candidateId": "consultation:outside", "targetPurposeQuote": "共通目的", "candidatePurposeQuote": "共通目的", "commonTargets": []any{Record{"kind": "path", "value": "a.go"}}}}, material)
	if result["proceed"] != true {
		t.Fatal(result)
	}
}

func TestOutsideConsultationCheckProceeds(t *testing.T) {
	ctx := context.Background()
	calls := 0
	s, d, _ := fixture(t, func(_ context.Context, in map[string]any) (map[string]any, error) {
		calls++
		result := allowed()
		if calls == 1 {
			result["proceed"] = false
			result["evidenceRequests"] = []any{Record{"candidateId": "consultation:outside", "kind": "body"}}
		} else {
			if !strings.Contains(fleet.Text(in["input"]), "共通目的 a.go") {
				t.Fatal("候補外相談が届いていません")
			}
			result["proceed"] = false
			result["duplicate"] = Record{"candidateId": "consultation:outside", "targetPurposeQuote": "共通目的", "candidatePurposeQuote": "共通目的", "commonTargets": []any{Record{"kind": "path", "value": "a.go"}}}
		}
		return result, nil
	})
	_, err := s.options.Records.Change(ctx, "drafts", "outside", func(*sql.Tx, Record) (Record, error) {
		return Record{"id": "outside", "project": "owner/app", "status": "filed", "body": "## 目的\n共通目的 a.go"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Check(ctx, d); err != nil || calls != 2 {
		t.Fatal(err, calls)
	}
}

func TestEvidenceContinuesAcrossCandidates(t *testing.T) {
	for _, mode := range []string{"success", "mixed", "missing", "limit"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			s, d, material := fixture(t, func(_ context.Context, input map[string]any) (map[string]any, error) {
				calls++
				var sent Record
				if err := json.Unmarshal([]byte(strings.SplitN(fleet.Text(input["input"]), "\n確認資料:\n", 2)[1]), &sent); err != nil {
					t.Fatal(err)
				}
				out := allowed()
				if calls > 1 && len(fleet.Elements(sent["evidence"])) != calls-1 {
					t.Fatalf("call %d: %v", calls, sent["evidence"])
				}
				if (mode == "success" || mode == "mixed") && calls == 3 {
					return out, nil
				}
				id := fmt.Sprintf("request:%d", calls+2)
				if mode == "missing" {
					id = "consultation:absent"
				}
				out["proceed"] = false
				out["evidenceRequests"] = []any{Record{"candidateId": id, "kind": "body"}}
				if mode == "mixed" && calls == 2 {
					out["evidenceRequests"] = append(fleet.Elements(out["evidenceRequests"]), Record{"candidateId": "request:3", "kind": "body"})
				}
				return out, nil
			})
			rows := []any{}
			for i := 3; i <= 10; i++ {
				rows = append(rows, Record{"number": i, "body": "別の目的", "title": "候補"})
			}
			(*material)["requests"] = rows
			err := s.Check(context.Background(), d)
			if mode == "success" || mode == "mixed" {
				if err != nil || calls != 3 {
					t.Fatal(err, calls)
				}
				return
			}
			if err == nil {
				t.Fatal("停止しませんでした")
			}
			if mode == "limit" && (calls != 9 || !strings.Contains(err.Error(), "読み足し上限")) {
				t.Fatal(err, calls)
			}
			if mode == "missing" {
				saved, _, e := s.options.Records.Get(context.Background(), "drafts", fleet.Text(d["id"]))
				if e != nil {
					t.Fatal(e)
				}
				hold := obj(obj(saved["filingCheckCalls"])["evidenceHold"])
				if calls != 1 || !strings.Contains(string(raw(hold)), "相談が存在しません") {
					t.Fatal(calls, hold)
				}
			}
		})
	}
}

func TestEvidenceLimitsAcrossRounds(t *testing.T) {
	for _, mode := range []string{"candidates", "documents"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			s, d, material := fixture(t, func(_ context.Context, _ map[string]any) (map[string]any, error) {
				calls++
				out := allowed()
				out["proceed"] = false
				wanted := []any{}
				start, end := 3, 9
				if calls == 2 {
					start, end = 9, 16
				}
				for i := start; i < end; i++ {
					wanted = append(wanted, Record{"candidateId": fmt.Sprintf("request:%d", i), "kind": "body"})
					if mode == "documents" {
						wanted = append(wanted, Record{"candidateId": fmt.Sprintf("request:%d", i), "kind": "reference"})
					}
				}
				out["evidenceRequests"] = wanted
				return out, nil
			})
			rows := []any{}
			for i := 3; i < 16; i++ {
				rows = append(rows, Record{"number": i, "body": "本文"})
			}
			(*material)["requests"] = rows
			err := s.Check(context.Background(), d)
			expected := 2
			if mode == "limit" {
				expected = 9
			}
			if err == nil || calls != expected {
				t.Fatal(err, calls)
			}
			saved, _, e := s.options.Records.Get(context.Background(), "drafts", fleet.Text(d["id"]))
			if e != nil {
				t.Fatal(e)
			}
			hold := obj(obj(saved["filingCheckCalls"])["evidenceHold"])
			if !strings.Contains(string(raw(hold)), "件数上限") {
				t.Fatal(hold)
			}
		})
	}
}

func TestEvidenceInputBudgetAcrossRounds(t *testing.T) {
	calls := 0
	s, d, material := fixture(t, func(_ context.Context, input map[string]any) (map[string]any, error) {
		calls++
		var sent Record
		if err := json.Unmarshal([]byte(strings.SplitN(fleet.Text(input["input"]), "\n確認資料:\n", 2)[1]), &sent); err != nil {
			t.Fatal(err)
		}
		out := allowed()
		if calls == 3 {
			if len(fleet.Elements(sent["evidence"])) != 1 || !strings.Contains(string(raw(sent["evidenceOmitted"])), "資料入力上限") {
				t.Fatal(sent["evidence"], sent["evidenceOmitted"])
			}
			return out, nil
		}
		out["proceed"] = false
		out["evidenceRequests"] = []any{Record{"candidateId": fmt.Sprintf("request:%d", calls+2), "kind": "body"}}
		return out, nil
	})
	(*material)["requests"] = []any{Record{"number": 3, "body": strings.Repeat("a", 35000)}, Record{"number": 4, "body": strings.Repeat("b", 35000)}}
	if err := s.Check(context.Background(), d); err != nil || calls != 3 {
		t.Fatal(err, calls)
	}
}

func TestSelectionRecoveryAndUnresolvedHold(t *testing.T) {
	for _, mode := range []string{"recover", "repeat", "abandon", "limit"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			selection := Record{"body": Record{"section": "目的"}}
			s, d, material := fixture(t, func(_ context.Context, in map[string]any) (map[string]any, error) {
				calls++
				out := allowed()
				if calls == 1 {
					out["proceed"] = false
					out["evidenceRequests"] = []any{Record{"candidateId": "request:3", "kind": "body", "selection": selection}}
					return out, nil
				}
				var sent Record
				if err := json.Unmarshal([]byte(strings.SplitN(fleet.Text(in["input"]), "\n確認資料:\n", 2)[1]), &sent); err != nil {
					t.Fatal(err)
				}
				if calls == 2 || mode == "limit" {
					missing := obj(fleet.Elements(sent["evidenceMissing"])[0])
					if calls == 2 && (missing["candidateId"] != "request:3" || missing["kind"] != "body" || string(raw(missing["selection"])) != string(raw(selection)) || !strings.Contains(fleet.Text(missing["reason"]), "節が重複") || !strings.Contains(fleet.Text(missing["next"]), "startLine")) {
						t.Fatal(sent)
					}
					if mode == "abandon" {
						return out, nil
					}
					out["proceed"] = false
					next := selection
					if mode == "limit" {
						next = Record{"body": Record{"section": fmt.Sprintf("不存在%d", calls)}}
					}
					if mode == "recover" {
						next = Record{"body": Record{"startLine": 1, "endLine": 2}}
					}
					out["evidenceRequests"] = []any{Record{"candidateId": "request:3", "kind": "body", "selection": next}}
					return out, nil
				}
				if len(fleet.Elements(sent["evidenceMissing"])) != 0 || !strings.Contains(string(raw(sent["evidence"])), "最初") {
					t.Fatal(sent)
				}
				return out, nil
			})
			(*material)["requests"] = []any{Record{"number": 3, "body": "## 目的\n最初\n## 目的\n二番"}}
			err := s.Check(context.Background(), d)
			if mode == "recover" {
				if err != nil || calls != 3 {
					t.Fatal(err, calls)
				}
				return
			}
			expected := 2
			if mode == "limit" {
				expected = 9
			}
			if err == nil || calls != expected {
				t.Fatal(err, calls)
			}
			saved, _, e := s.options.Records.Get(context.Background(), "drafts", fleet.Text(d["id"]))
			if e != nil {
				t.Fatal(e)
			}
			hold := obj(obj(saved["filingCheckCalls"])["evidenceHold"])
			if !strings.Contains(string(raw(hold)), "節が重複") || !strings.Contains(string(raw(hold["requests"])), "目的") {
				t.Fatal(saved)
			}
		})
	}
}

func TestOutsideConsultationMissingSectionsRecovery(t *testing.T) {
	for _, mode := range []string{"recover", "repeat", "abandon", "limit"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			s, d, _ := fixture(t, func(_ context.Context, in map[string]any) (map[string]any, error) {
				calls++
				out := allowed()
				if calls == 1 {
					out["proceed"] = false
					out["evidenceRequests"] = []any{Record{"candidateId": "consultation:outside", "kind": "body"}}
					return out, nil
				}
				var sent Record
				if err := json.Unmarshal([]byte(strings.SplitN(fleet.Text(in["input"]), "\n確認資料:\n", 2)[1]), &sent); err != nil {
					t.Fatal(err)
				}
				if calls == 2 || mode == "limit" {
					missing := obj(fleet.Elements(sent["evidenceMissing"])[0])
					if calls == 2 && (missing["candidateId"] != "consultation:outside" || !strings.Contains(fleet.Text(missing["reason"]), "目的・受入の節がありません") || !strings.Contains(fleet.Text(missing["next"]), "selection")) {
						t.Fatal(sent)
					}
					if mode == "abandon" {
						return out, nil
					}
					request := Record{"candidateId": "consultation:outside", "kind": "body"}
					if mode == "recover" {
						request["selection"] = Record{"body": Record{"section": "調査"}}
					}
					if mode == "limit" {
						request["selection"] = Record{"body": Record{"section": fmt.Sprintf("不存在%d", calls)}}
					}
					out["proceed"] = false
					out["evidenceRequests"] = []any{request}
					return out, nil
				}
				if len(fleet.Elements(sent["evidenceMissing"])) != 0 || !strings.Contains(string(raw(sent["evidence"])), "調査本文") {
					t.Fatal(sent)
				}
				return out, nil
			})
			_, err := s.options.Records.Change(context.Background(), "drafts", "outside", func(*sql.Tx, Record) (Record, error) {
				return Record{"id": "outside", "project": d["project"], "body": "## 調査\n調査本文"}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			err = s.Check(context.Background(), d)
			if mode == "recover" {
				if err != nil || calls != 3 {
					t.Fatal(err, calls)
				}
				return
			}
			expected := 2
			if mode == "limit" {
				expected = 9
			}
			if err == nil || calls != expected {
				t.Fatal(err, calls)
			}
			saved, _, err := s.options.Records.Get(context.Background(), "drafts", fleet.Text(d["id"]))
			if err != nil {
				t.Fatal(err)
			}
			hold := obj(obj(saved["filingCheckCalls"])["evidenceHold"])
			if !strings.Contains(string(raw(hold["evidenceMissing"])), "目的・受入の節がありません") || !strings.Contains(string(raw(hold["requests"])), "consultation:outside") {
				t.Fatal(hold)
			}
		})
	}
}

func budgetCandidates(material *Record) {
	rows := []any{}
	for i := 1; i <= 8; i++ {
		rows = append(rows, Record{"number": i + 10, "body": fmt.Sprintf("候補%dの別の資料", i), "title": fmt.Sprintf("候補%d", i)})
	}
	(*material)["requests"] = rows
}

func TestEvidenceRoundsDoNotConsumeDecisionBudget(t *testing.T) {
	calls := 0
	s, d, material := fixture(t, func(context.Context, map[string]any) (map[string]any, error) {
		calls++
		out := allowed()
		if calls <= 4 {
			out["evidenceRequests"] = []any{Record{"candidateId": fmt.Sprintf("request:%d", 10+calls), "kind": "body"}}
		}
		return out, nil
	})
	budgetCandidates(material)
	for i := 0; i < 5; i++ {
		if err := s.Check(context.Background(), d, "explicit"); err != nil {
			t.Fatal(i, err)
		}
	}
	if calls != 9 {
		t.Fatal(calls)
	}
	if err := s.Check(context.Background(), d, "explicit"); err == nil || calls != 9 {
		t.Fatal(err, calls)
	}
}

func TestEvidenceBudgetRunsFinalDecision(t *testing.T) {
	for _, proceed := range []bool{true, false} {
		t.Run(fmt.Sprint(proceed), func(t *testing.T) {
			calls := 0
			s, d, material := fixture(t, func(_ context.Context, in map[string]any) (map[string]any, error) {
				calls++
				out := allowed()
				if calls <= 8 {
					out["evidenceRequests"] = []any{Record{"candidateId": fmt.Sprintf("request:%d", 10+calls), "kind": "body"}}
				} else {
					if !strings.Contains(fleet.Text(in["input"]), `"evidenceBudgetFinal":true`) {
						t.Fatal("final flag missing")
					}
					out["proceed"], out["reason"] = proceed, "判定理由"
				}
				return out, nil
			})
			budgetCandidates(material)
			err := s.Check(context.Background(), d, "explicit")
			if (err == nil) != proceed || calls != 9 {
				t.Fatal(err, calls)
			}
			saved, _, err := s.options.Records.Get(context.Background(), "drafts", fleet.Text(d["id"]))
			if err != nil {
				t.Fatal(err)
			}
			c := obj(saved["filingCheckCalls"])
			b := obj(c["budget"])
			if fleet.WholeNumber(b["evidence"]) != 8 || fleet.WholeNumber(b["decisions"]) != 1 {
				t.Fatal(c)
			}
			if !proceed && (c["outcome"] != "refused" || c["lastRefusal"] != "判定理由") {
				t.Fatal(c)
			}
		})
	}
}

func TestFinalDecisionCannotRestartEvidence(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(fmt.Sprint(duplicate), func(t *testing.T) {
			calls := 0
			s, d, material := fixture(t, func(context.Context, map[string]any) (map[string]any, error) {
				calls++
				out := allowed()
				if calls <= 8 {
					out["evidenceRequests"] = []any{Record{"candidateId": fmt.Sprintf("request:%d", 10+calls), "kind": "body"}}
				} else if duplicate {
					out["proceed"] = false
					out["duplicate"] = Record{"candidateId": "request:30"}
				} else {
					out["evidenceRequests"] = []any{Record{"candidateId": "request:30", "kind": "body", "selection": Record{"body": Record{"startLine": 1, "endLine": 2}}}}
				}
				return out, nil
			})
			budgetCandidates(material)
			(*material)["requests"] = append(fleet.Elements((*material)["requests"]), Record{"number": 30, "title": "候補", "paths": []any{"other/a.go"}, "body": "不足資料"})
			obj((*material)["target"])["changeScope"] = Record{"known": true, "paths": []any{"src/app.go"}}
			err := s.Check(context.Background(), d, "explicit")
			if err == nil || calls != 9 || !strings.Contains(err.Error(), "request:30") || !strings.Contains(err.Error(), "body") || !strings.Contains(err.Error(), "必要範囲") {
				t.Fatal(err, calls)
			}
			saved, _, e := s.options.Records.Get(context.Background(), "drafts", fleet.Text(d["id"]))
			if e != nil || obj(obj(saved["filingCheckCalls"])["evidenceHold"])["reason"] == nil {
				t.Fatal(saved, e)
			}
		})
	}
}
