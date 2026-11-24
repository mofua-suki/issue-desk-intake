package consultationai

import (
	"context"
	"database/sql"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"os"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/usage"
)

func TestIntakeSizeCandidate(t *testing.T) {
	config := map[string]any{"assignments": Record{"intakeSplit": Record{"lines": 400, "files": 8}}}
	for _, tc := range []struct {
		name     string
		estimate any
		body     string
		split    []any
		blocked  bool
	}{
		{"boundary", Record{"lines": 400, "files": 8}, "", nil, false},
		{"lines", Record{"lines": 401, "files": 3}, "", nil, true},
		{"files", Record{"lines": 100, "files": 9}, "", nil, true},
		{"missing", nil, "", nil, true},
		{"partial", Record{"lines": 100}, "", nil, true},
		{"negative", Record{"lines": -1, "files": 1}, "", nil, true},
		{"fraction", Record{"lines": 1.5, "files": 1}, "", nil, true},
		{"string", Record{"lines": "1", "files": 1}, "", nil, true},
		{"split", Record{"lines": 401, "files": 3}, "", []any{Record{}}, false},
		{"supported", Record{"lines": 401, "files": 3}, "## 利用者の判断と根拠\n利用者原文「この依頼は分割しないでください」日時: 2026-11-07", nil, false},
		{"unquoted", Record{"lines": 401, "files": 3}, "## 利用者の判断と根拠\n分けないで。日時: 2026-11-07", nil, true},
		{"no date", Record{"lines": 401, "files": 3}, "## 利用者の判断と根拠\n「分けないで」", nil, true},
		{"unrelated", Record{"lines": 401, "files": 3}, "## 利用者の判断と根拠\n「急いでください」日時: 2026-11-07 分割禁止", nil, true},
		{"negated", Record{"lines": 401, "files": 3}, "## 利用者の判断と根拠\n「分割しないわけではない」日時: 2026-11-07", nil, true},
		{"later withdrawal", Record{"lines": 401, "files": 3}, "## 利用者の判断と根拠\n「この依頼は分割しないで」日時: 2026-11-07\n\nこの判断を撤回する。", nil, true},
		{"past", Record{"lines": 401, "files": 3}, "## 利用者の判断と根拠\n過去の依頼では「分割しない」日時: 2026-11-07", nil, true},
		{"rule", Record{"lines": 401, "files": 3}, "## 利用者の判断と根拠\n規則説明「分割しない」日時: 2026-11-07", nil, true},
		{"fence", Record{"lines": 401, "files": 3}, "## 利用者の判断と根拠\n```\n「分割しない」日時: 2026-11-07\n```", nil, true},
		{"long fence", Record{"lines": 401, "files": 3}, "## 利用者の判断と根拠\n````\n```\n「分割しない」日時: 2026-11-07\n````", nil, true},
		{"invalid date", Record{"lines": 401, "files": 3}, "## 利用者の判断と根拠\n「分割しない」日時: 2026-02-30", nil, true},
		{"denied outside quote", Record{"lines": 401, "files": 3}, "## 利用者の判断と根拠\n「分割しない」とは言っていない。日時: 2026-11-07", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := Record{"ready": true, "estimate": tc.estimate, "splitIssues": tc.split, "heavyReason": "分けない"}
			_, p := sizeCandidate(Record{"body": tc.body, "messages": []any{Record{"role": "user", "text": "この依頼は分割しないでください", "at": "2026-11-07T10:00:00Z"}}}, a, config)
			if (p != "") != tc.blocked {
				t.Fatalf("problem=%q", p)
			}
		})
	}
	config["assignments"] = Record{"intakeSplit": Record{"lines": 450, "files": 12}}
	_, p := sizeCandidate(nil, Record{"ready": true, "estimate": Record{"lines": 401, "files": 9}}, config)
	if p != "" {
		t.Fatal(p)
	}
}

func TestIntakeSizeEvidenceDateUsesUTC(t *testing.T) {
	config := map[string]any{"assignments": Record{"intakeSplit": Record{"lines": 400, "files": 8}}}
	answer := Record{"ready": true, "estimate": Record{"lines": 401, "files": 3}}
	for _, tc := range []struct {
		name, at, date string
		blocked        bool
	}{
		{"utc date", "2026-11-07T20:00:00Z", "2026-11-07", false},
		{"japan date", "2026-11-07T20:00:00Z", "2026-11-08", true},
		{"offset utc date", "2026-11-08T05:00:00+09:00", "2026-11-07", false},
		{"offset local date", "2026-11-08T05:00:00+09:00", "2026-11-08", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Record{"body": "## 利用者の判断と根拠\n「この依頼は分割しないでください」日時: " + tc.date,
				"messages": []any{Record{"role": "user", "text": "この依頼は分割しないでください", "at": tc.at}}}
			_, problem := sizeCandidate(d, answer, config)
			if (problem != "") != tc.blocked {
				t.Fatalf("problem=%q, blocked=%v", problem, tc.blocked)
			}
		})
	}
}

func TestIntakeSizeTurnRetryAndSave(t *testing.T) {
	for _, resolved := range []bool{false, true} {
		t.Run(map[bool]string{false: "unresolved", true: "resolved"}[resolved], func(t *testing.T) {
			calls := 0
			s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
				calls++
				v := reply()
				v["ready"], v["title"], v["body"], v["questions"] = true, "題名", "目的と受入条件", []any{}
				v["estimate"] = Record{"lines": 401, "files": 3}
				if calls >= 3 && resolved {
					v["splitIssues"] = []any{Record{"title": "子1", "purpose": "目的", "acceptance": []any{"確認: `go test ./...` → 期待: 終了コード0"}, "dependsOn": []any{}}, Record{"title": "子2", "purpose": "検証", "acceptance": []any{"確認: `go test ./...` → 期待: 終了コード0"}, "dependsOn": []any{1}}}
				}
				if calls >= 3 && !strings.Contains(r.Input, "分割案") {
					t.Error("missing continuation request")
				}
				return measuredAnswer(v), nil
			})
			previous := s.options.Config
			s.options.Config = func(ctx context.Context) (map[string]any, error) {
				c, e := previous(ctx)
				c["assignments"] = Record{"intakeSplit": Record{"lines": 400, "files": 8}}
				return c, e
			}
			got, err := s.Turn(context.Background(), d)
			if err != nil {
				t.Fatal(err)
			}
			if got["ready"] != resolved || calls != 3 {
				t.Fatalf("calls=%d ready=%v", calls, got["ready"])
			}
			saved, _, err := s.options.Records.Get(context.Background(), "drafts", draftID)
			if err != nil {
				t.Fatal(err)
			}
			state := fleet.Object(fleet.Object(fleet.Object(saved["consultationBudget"])["contextRead"])["intakeSize"])
			if fleet.Number(state["requests"]) != 1 || fleet.Number(state["lines"]) != 401 || fleet.Number(fleet.Object(state["limits"])["lines"]) != 400 {
				t.Fatalf("state=%v", state)
			}
		})
	}
}

func TestIntakeSizeResumeDoesNotRepeat(t *testing.T) {
	calls := 0
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		calls++
		v := reply()
		v["ready"], v["title"], v["body"], v["questions"] = true, "題名", "目的と受入条件", []any{}
		v["estimate"] = Record{"lines": 401, "files": 3}
		return measuredAnswer(v), nil
	})
	previous := s.options.Config
	s.options.Config = func(ctx context.Context) (map[string]any, error) {
		c, e := previous(ctx)
		c["assignments"] = Record{"intakeSplit": Record{"lines": 400, "files": 8}}
		return c, e
	}
	first, err := s.Turn(context.Background(), d)
	if err != nil || first["ready"] != false {
		t.Fatalf("first=%v err=%v", first, err)
	}
	saved, _, err := s.options.Records.Get(context.Background(), "drafts", draftID)
	if err != nil {
		t.Fatal(err)
	}
	prior := calls
	got, err := s.Turn(context.Background(), saved)
	if err != nil || got["ready"] != false || calls != prior {
		t.Fatalf("ready=%v calls=%d before=%d err=%v", got["ready"], calls, prior, err)
	}
	saved, _, err = s.options.Records.Get(context.Background(), "drafts", draftID)
	if err != nil {
		t.Fatal(err)
	}
	if n := fleet.Number(fleet.Object(fleet.Object(fleet.Object(saved["consultationBudget"])["contextRead"])["intakeSize"])["requests"]); n != 1 {
		t.Fatalf("requests=%v", n)
	}
}

func TestIntakeSizeBudgetAndMissingShareContinuation(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name         string
		calls        int
		rounds       float64
		continuation bool
	}{
		{"available", 0, 0, true}, {"calls exhausted", 37, 0, false}, {"reads exhausted", 0, 12, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Record{"_ephemeral": true}
			s := &Service{options: Options{Now: func() time.Time { return time.Unix(0, 0) }}}
			b := &callBudget{service: s, draft: d, value: Record{"calls": tc.calls, "weighted": 0}, calls: 40, weighted: 1000}
			r := &readBudget{budget: b, value: Record{"rounds": tc.rounds}}
			a := Record{"ready": true, "estimate": Record{"lines": 401, "files": 3}}
			cfg := map[string]any{"assignments": Record{"intakeSplit": Record{"lines": 400, "files": 8}}}
			again, err := r.checkIntakeSize(ctx, d, a, cfg, false, 0)
			if err != nil || again != tc.continuation {
				t.Fatalf("again=%v err=%v", again, err)
			}
			if !again && a["ready"] != false {
				t.Fatal(a)
			}
			if again {
				second := Record{"ready": true, "estimate": Record{"lines": 401}}
				again, err = r.checkIntakeSize(ctx, d, second, cfg, false, 0)
				if err != nil || again || second["ready"] != false || fleet.Number(fleet.Object(r.value["intakeSize"])["requests"]) != 1 {
					t.Fatalf("again=%v second=%v state=%v err=%v", again, second, r.value, err)
				}
				if fleet.Number(fleet.Object(fleet.Object(r.value["intakeSize"])["lastRequest"])["lines"]) != 401 {
					t.Fatalf("request evidence lost: %v", r.value)
				}
			}
		})
	}
}

func TestIntakeSizeAfterReadAndRepair(t *testing.T) {
	for _, kind := range []string{"read", "repair"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
				calls++
				v := reply()
				v["ready"], v["title"], v["body"], v["questions"] = true, "題名", "目的と受入条件", []any{}
				v["estimate"] = Record{"lines": 401, "files": 3}
				if calls == 1 {
					if kind == "read" {
						v["readFiles"] = []any{Record{"file": "README.md", "mode": "lines", "start": 1, "end": 1}}
					} else {
						v["readFiles"] = []any{42}
					}
				}
				return measuredAnswer(v), nil
			})
			s.options.Reference = nil
			old := s.options.Config
			s.options.Config = func(ctx context.Context) (map[string]any, error) {
				c, e := old(ctx)
				c["assignments"] = Record{"intakeSplit": Record{"lines": 400, "files": 8}}
				return c, e
			}
			got, err := s.Turn(context.Background(), d)
			if err != nil || got["ready"] != false || calls != 3 {
				t.Fatalf("calls=%d result=%v err=%v", calls, got, err)
			}
		})
	}
}

func TestIntakeSizeAfterProxyContinuation(t *testing.T) {
	calls := 0
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		if r.Step == usage.ProxyConfirmationStep {
			return measuredAnswer(map[string]any{"answers": []any{Record{"index": 1, "decision": "answer", "category": "fact", "answer": "資料です", "evidence": []any{Record{"kind": "document", "source": "README.md", "location": "App"}}}}}), nil
		}
		calls++
		if calls == 1 {
			v := reply()
			v["body"] = "## 利用者の判断と根拠\n利用者の原文「分割しないで」日時: 2026-11-07"
			return measuredAnswer(v), nil
		}
		v := reply()
		v["ready"], v["title"], v["body"], v["questions"] = true, "題名", "目的と受入条件", []any{}
		v["estimate"] = Record{"lines": 401, "files": 3}
		return measuredAnswer(v), nil
	})
	cfg, _ := s.options.Config(context.Background())
	fleet.Object(fleet.Object(cfg["intake"])["proxy"])["enabled"] = true
	cfg["assignments"] = Record{"intakeSplit": Record{"lines": 400, "files": 8}}
	s.options.Reference = nil
	first, err := s.Turn(context.Background(), d)
	if err != nil || first["_continue"] != true {
		t.Fatalf("first=%v err=%v", first, err)
	}
	saved, _, err := s.options.Records.Get(context.Background(), "drafts", draftID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Turn(context.Background(), saved)
	if err != nil || got["ready"] != false || calls != 3 {
		t.Fatalf("calls=%d result=%v err=%v", calls, got, err)
	}
}

func TestIntakeSizeUsesDeployedAssignments(t *testing.T) {
	raw, err := os.ReadFile("../../../assignments.json")
	if err != nil {
		t.Fatal(err)
	}
	var assignments map[string]any
	if err = json.Unmarshal(raw, &assignments); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{"assignments": assignments}
	for _, tc := range []struct {
		lines, files int
		blocked      bool
	}{{400, 8, false}, {401, 3, true}, {100, 9, true}} {
		_, problem := sizeCandidate(nil, Record{"ready": true, "estimate": Record{"lines": tc.lines, "files": tc.files}}, cfg)
		if (problem != "") != tc.blocked {
			t.Fatalf("%d/%d: %q", tc.lines, tc.files, problem)
		}
	}
}

func TestIntakeSizeSaveFailureStops(t *testing.T) {
	s, d := testService(t, nil)
	d["run"] = "lost"
	b, err := s.budget(d, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	reads, err := b.reads()
	if err != nil {
		t.Fatal(err)
	}
	a := Record{"ready": true, "estimate": Record{"lines": 401, "files": 3}}
	_, err = reads.checkIntakeSize(context.Background(), d, a, map[string]any{"assignments": Record{"intakeSplit": Record{"lines": 400, "files": 8}}}, false, 0)
	if err == nil {
		t.Fatal("save failure must abort Turn")
	}
}

func TestIntakeSizeLegacyPendingAnswerRequiresEstimate(t *testing.T) {
	calls := 0
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		calls++
		if !strings.Contains(r.Input, "見込み") {
			t.Error("estimate request missing")
		}
		v := reply()
		v["ready"], v["title"], v["body"], v["questions"] = true, "題名", "目的と受入条件", []any{}
		delete(v, "estimate")
		return measuredAnswer(v), nil
	})
	s.options.Reference = nil
	old := reply()
	old["ready"], old["title"], old["body"], old["questions"] = true, "題名", "目的と受入条件", []any{}
	delete(old, "estimate")
	d["consultationBudget"] = Record{"calls": 1, "weighted": 0, "unknownCalls": 0, "contextRead": Record{"rounds": 0, "answerPending": true, "lastReply": old}}
	got, err := s.Turn(context.Background(), d)
	if err != nil || got["ready"] != false || calls != 1 {
		t.Fatalf("calls=%d result=%v err=%v", calls, got, err)
	}
}

func TestIntakeSizeGeneratedQuoteIsNotEvidence(t *testing.T) {
	cfg := map[string]any{"assignments": Record{"intakeSplit": Record{"lines": 400, "files": 8}}}
	answer := Record{"ready": true, "estimate": Record{"lines": 401, "files": 3}, "body": "## 利用者の判断と根拠\n利用者の原文「この依頼は分割しないで」日時: 2026-11-07"}
	_, problem := sizeCandidate(Record{"body": "依頼"}, answer, cfg)
	if problem == "" {
		t.Fatal("newly generated quote accepted")
	}
}

func TestIntakeSizeProxyGeneratedQuoteIsNotEvidence(t *testing.T) {
	cfg := map[string]any{"assignments": Record{"intakeSplit": Record{"lines": 400, "files": 8}}}
	d := Record{"body": "## 利用者の判断と根拠\n利用者の原文「分割しないで」日時: 2026-11-07", "intakeSizeProxyEvidence": "元の依頼", "messages": []any{Record{"role": "proxy"}}}
	_, problem := sizeCandidate(d, Record{"ready": true, "estimate": Record{"lines": 401, "files": 3}}, cfg)
	if problem == "" {
		t.Fatal("proxy-generated quote accepted")
	}
}

func TestIntakeSizeTurnQuoteDecision(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		ready      bool
		calls      int
	}{
		{"opposed", "## 利用者の判断と根拠\n利用者の原文「分割しないという案に賛成できません」日時: 2026-11-07", false, 2},
		{"opposed variant", "## 利用者の判断と根拠\n利用者の原文「分割しないという案に賛同できません」日時: 2026-11-07", false, 2},
		{"supported", "## 利用者の判断と根拠\n利用者の原文「この依頼は分割しないでください」日時: 2026-11-07", true, 1},
		{"opposition before", "## 利用者の判断と根拠\nこの判断には賛成できません。\n\n利用者の原文「この依頼は分割しないでください」日時: 2026-11-07", false, 2},
		{"opposition after", "## 利用者の判断と根拠\n利用者の原文「この依頼は分割しないでください」日時: 2026-11-07\n\nこの判断には賛成できません。", false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
				calls++
				if calls > 1 && !strings.Contains(r.Input, "分割案") {
					t.Error("分割案の再要求がありません")
				}
				v := reply()
				v["ready"], v["title"], v["body"], v["questions"] = true, "題名", "目的と受入条件", []any{}
				v["estimate"] = Record{"lines": 401, "files": 3}
				return measuredAnswer(v), nil
			})
			s.options.Reference = nil
			d["body"] = tc.body
			d["messages"] = []any{Record{"role": "user", "text": "この依頼は分割しないでください", "at": "2026-11-07T10:00:00Z"}}
			if _, err := s.options.Records.Change(context.Background(), "drafts", draftID, func(_ *sql.Tx, current Record) (Record, error) {
				current["body"] = tc.body
				current["messages"] = d["messages"]
				return current, nil
			}); err != nil {
				t.Fatal(err)
			}
			got, err := s.Turn(context.Background(), d)
			if err != nil || got["ready"] != tc.ready || calls != tc.calls {
				t.Fatalf("calls=%d ready=%v err=%v", calls, got["ready"], err)
			}
			saved, _, err := s.options.Records.Get(context.Background(), "drafts", draftID)
			if err != nil {
				t.Fatal(err)
			}
			state := fleet.Object(fleet.Object(fleet.Object(saved["consultationBudget"])["contextRead"])["intakeSize"])
			want := float64(0)
			if !tc.ready {
				want = 1
			}
			if fleet.Number(state["requests"]) != want {
				t.Fatalf("state=%v", state)
			}
		})
	}
}

func TestIntakeSizeTurnEvidenceFromConversation(t *testing.T) {
	for _, tc := range []struct {
		name, quote, user, at, later string
		ready                        bool
	}{
		{"verified", "この依頼は分割しないでください", "この依頼は分割しないでください", "2026-11-07T10:00:00Z", "", true},
		{"fabricated", "この依頼は分割しないでください", "通常の依頼です", "2026-11-07T10:00:00Z", "", false},
		{"other target", "マイグレーションは分割しない", "マイグレーションは分割しない", "2026-11-07T10:00:00Z", "", false},
		{"wrong date", "この依頼は分割しないでください", "この依頼は分割しないでください", "2026-11-06T10:00:00Z", "", false},
		{"withdrawn later", "この依頼は分割しないでください", "この依頼は分割しないでください", "2026-11-07T10:00:00Z", "その判断を撤回します", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
				calls++
				if calls > 1 && !strings.Contains(r.Input, "分割案") {
					t.Error("分割案の再要求がありません")
				}
				v := reply()
				v["ready"], v["title"], v["body"], v["questions"] = true, "題名", "目的と受入条件", []any{}
				v["estimate"] = Record{"lines": 401, "files": 3}
				return measuredAnswer(v), nil
			})
			s.options.Reference = nil
			d["body"] = "## 利用者の判断と根拠\n利用者の原文「" + tc.quote + "」日時: 2026-11-07"
			d["messages"] = []any{Record{"role": "user", "text": tc.user, "at": tc.at}, Record{"role": "assistant", "text": d["body"]}}
			if tc.later != "" {
				d["messages"] = append(fleet.Elements(d["messages"]), Record{"role": "user", "text": tc.later})
			}
			if _, err := s.options.Records.Change(context.Background(), "drafts", draftID, func(_ *sql.Tx, current Record) (Record, error) {
				current["body"], current["messages"] = d["body"], d["messages"]
				return current, nil
			}); err != nil {
				t.Fatal(err)
			}
			got, err := s.Turn(context.Background(), d)
			if err != nil || got["ready"] != tc.ready || calls != map[bool]int{true: 1, false: 2}[tc.ready] {
				t.Fatalf("ready=%v calls=%d err=%v", got["ready"], calls, err)
			}
		})
	}
}

func TestIntakeSizeSyntheticChildMessageIsNotEvidence(t *testing.T) {
	d := Record{"parentConsultation": "parent", "body": "## 利用者の判断と根拠\n「この依頼は分割しないでください」日時: 2026-11-07", "messages": []any{Record{"role": "user", "text": "この依頼は分割しないでください", "at": "2026-11-07T10:00:00Z"}}}
	cfg := map[string]any{"assignments": Record{"intakeSplit": Record{"lines": 400, "files": 8}}}
	_, problem := sizeCandidate(d, Record{"ready": true, "estimate": Record{"lines": 401, "files": 3}}, cfg)
	if problem == "" {
		t.Fatal("生成された子相談の本文を利用者発言として扱いました")
	}
}

func TestIntakeSizeAfterProxySplitCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		remaining   int
	}{
		{"explicit cancellation", "分割案をすべて取り消します。", 0},
		{"implicit empty answer", "確認しました", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			draftCalls := 0
			s, d := testService(t, func(_ context.Context, req ai.Request) (ai.Answer, error) {
				if req.Step == usage.ProxyConfirmationStep {
					return measuredAnswer(map[string]any{"answers": []any{Record{"index": 1, "decision": "answer", "category": "fact", "answer": "資料です", "evidence": []any{Record{"kind": "document", "source": "README.md", "location": "App"}}}}}), nil
				}
				draftCalls++
				v := reply()
				if draftCalls == 1 {
					v["reply"], v["splitIssues"] = tc.reply, []any{}
					return measuredAnswer(v), nil
				}
				v["ready"], v["title"], v["body"], v["questions"] = true, "題名", "目的と受入条件", []any{}
				v["estimate"] = Record{"lines": 401, "files": 3}
				return measuredAnswer(v), nil
			})
			config, err := s.options.Config(ctx)
			if err != nil {
				t.Fatal(err)
			}
			fleet.Object(fleet.Object(config["intake"])["proxy"])["enabled"] = true
			s.options.Reference = nil
			initial := []any{Record{"title": "保存済みの分割案"}}
			d["splitIssues"] = initial
			if _, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, current Record) (Record, error) { current["splitIssues"] = initial; return current, nil }); err != nil {
				t.Fatal(err)
			}
			first, err := s.Turn(ctx, d)
			if err != nil || first["_continue"] != true {
				t.Fatalf("first=%v err=%v", first, err)
			}
			saved, _, err := s.options.Records.Get(ctx, "drafts", draftID)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(fleet.Elements(saved["splitIssues"])); got != tc.remaining {
				t.Fatalf("saved split count=%d want=%d", got, tc.remaining)
			}
			got, err := s.Turn(ctx, saved)
			if err != nil || got["ready"] != false || draftCalls != 3 {
				t.Fatalf("calls=%d ready=%v err=%v", draftCalls, got["ready"], err)
			}
			saved, _, err = s.options.Records.Get(ctx, "drafts", draftID)
			if err != nil {
				t.Fatal(err)
			}
			state := fleet.Object(fleet.Object(fleet.Object(saved["consultationBudget"])["contextRead"])["intakeSize"])
			if fleet.Number(state["requests"]) != 1 {
				t.Fatalf("state=%v", state)
			}
		})
	}
}

func TestIntakeSizeContinuationKeepsExplicitSplitCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		want        int
		omit        bool
	}{
		{"explicit", "分割案をすべて取り消します。", 0, false},
		{"ambiguous", "分割案を取り消すか確認します。", 1, false},
		{"missing split field", "分割案をすべて取り消します。", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			calls := 0
			var s *Service
			var d Record
			s, d = testService(t, func(_ context.Context, req ai.Request) (ai.Answer, error) {
				calls++
				if calls == 2 {
					saved, _, err := s.options.Records.Get(ctx, "drafts", draftID)
					if err != nil {
						t.Fatal(err)
					}
					if got := len(fleet.Elements(saved["splitIssues"])); got != tc.want {
						t.Fatalf("before continuation split count=%d want=%d", got, tc.want)
					}
				}
				v := reply()
				v["ready"], v["title"], v["body"], v["questions"] = true, "題名", "目的と受入条件", []any{}
				v["estimate"], v["splitIssues"] = Record{"lines": 401, "files": 3}, []any{}
				if calls == 1 {
					if tc.omit {
						delete(v, "splitIssues")
					}
					v["reply"] = tc.reply
				} else {
					v["reply"] = "確認しました"
				}
				return measuredAnswer(v), nil
			})
			s.options.Reference = nil
			prior := []any{Record{"title": "保存済みの分割案"}}
			d["splitIssues"] = prior
			if _, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, current Record) (Record, error) { current["splitIssues"] = prior; return current, nil }); err != nil {
				t.Fatal(err)
			}
			got, err := s.Turn(ctx, d)
			if err != nil || got["ready"] != false || calls != 2 {
				t.Fatalf("calls=%d result=%v err=%v", calls, got, err)
			}
			saved, _, err := s.options.Records.Get(ctx, "drafts", draftID)
			if err != nil {
				t.Fatal(err)
			}
			if n := len(fleet.Elements(saved["splitIssues"])); n != tc.want {
				t.Fatalf("saved split count=%d want=%d", n, tc.want)
			}
			state := fleet.Object(fleet.Object(fleet.Object(saved["consultationBudget"])["contextRead"])["intakeSize"])
			if fleet.Number(state["requests"]) != 1 {
				t.Fatalf("state=%v", state)
			}
		})
	}
}

func TestIntakeSizeSplitReview(t *testing.T) {
	ctx := context.Background()
	calls := 0
	children := []any{
		Record{"title": "基盤", "purpose": "準備", "acceptance": []any{"確認: `go test ./...` → 期待: 終了コード0"}, "dependsOn": []any{}, "estimate": Record{"lines": 20, "files": 1}},
		Record{"title": "処理", "purpose": "実装", "acceptance": []any{"確認: `go test ./a` → 期待: 終了コード0", "確認: `go test ./b` → 期待: 終了コード0"}, "dependsOn": []any{1}, "estimate": Record{"lines": 401, "files": 3}},
	}
	s, d := testService(t, func(_ context.Context, req ai.Request) (ai.Answer, error) {
		calls++
		if calls == 2 && (!strings.Contains(req.Input, "分割案2") || !strings.Contains(req.Input, "依存・利用者の判断・受入条件") || !strings.Contains(req.Input, "go test ./a")) {
			t.Errorf("対象と保持すべき内容が不足: %s", req.Input)
		}
		v := reply()
		v["ready"], v["title"], v["body"], v["questions"] = true, "題名", "## 利用者の判断と根拠\n利用者の判断を保持", []any{}
		v["splitIssues"] = children
		return measuredAnswer(v), nil
	})
	s.options.Reference = nil
	got, err := s.Turn(ctx, d)
	if err != nil || calls != 2 || got["ready"] != true {
		t.Fatalf("calls=%d answer=%v err=%v", calls, got, err)
	}
	saved, _, err := s.options.Records.Get(ctx, "drafts", draftID)
	if err != nil {
		t.Fatal(err)
	}
	state := fleet.Object(fleet.Object(fleet.Object(saved["consultationBudget"])["contextRead"])["intakeSize"])
	if fleet.Number(state["continuations"]) != 1 || state["pending"] == true {
		t.Fatalf("state=%v", state)
	}
	gotChildren := fleet.Elements(got["splitIssues"])
	if len(gotChildren) != 2 || len(fleet.Elements(fleet.Object(gotChildren[1])["acceptance"])) != 2 || len(fleet.Elements(fleet.Object(gotChildren[1])["dependsOn"])) != 1 || fleet.Text(fleet.Elements(fleet.Object(gotChildren[1])["acceptance"])[0]) != "確認: `go test ./a` → 期待: 終了コード0" || fleet.Text(fleet.Elements(fleet.Object(gotChildren[1])["acceptance"])[1]) != "確認: `go test ./b` → 期待: 終了コード0" || fleet.Number(fleet.Elements(fleet.Object(gotChildren[1])["dependsOn"])[0]) != 1 || !strings.Contains(fleet.Text(got["body"]), "利用者の判断を保持") {
		t.Fatalf("answer=%v", got)
	}
}

func TestIntakeSizeSplitBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name                                  string
		lines, files                          int
		criteria                              int
		consumed, exhausted, prohibited, want bool
	}{
		{"lines", 401, 8, 2, false, false, false, true},
		{"files", 400, 9, 2, false, false, false, true},
		{"boundary", 400, 8, 2, false, false, false, false},
		{"single criterion", 401, 9, 1, false, false, false, false},
		{"consumed", 401, 9, 2, true, false, false, false},
		{"budget", 401, 9, 2, false, true, false, false},
		{"prohibited", 401, 9, 2, false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Record{"_ephemeral": true}
			if tc.prohibited {
				d["body"] = "## 利用者の判断と根拠\n「この依頼は分割しないでください」日時: 2026-11-07"
				d["messages"] = []any{Record{"role": "user", "text": "この依頼は分割しないでください", "at": "2026-11-07T10:00:00Z"}}
			}
			s := &Service{options: Options{Now: func() time.Time { return time.Unix(0, 0) }}}
			calls := 0
			if tc.exhausted {
				calls = 37
			}
			b := &callBudget{service: s, draft: d, value: Record{"calls": calls, "weighted": 0}, calls: 40, weighted: 1000}
			r := &readBudget{budget: b, value: Record{"rounds": 0}}
			if tc.consumed {
				r.value["intakeSize"] = Record{"continuations": 1}
			}
			criteria := []any{"A"}
			if tc.criteria == 2 {
				criteria = append(criteria, "B")
			}
			a := Record{"ready": true, "splitIssues": []any{Record{"acceptance": []any{"A"}}, Record{"acceptance": criteria, "estimate": Record{"lines": tc.lines, "files": tc.files}}}}
			cfg := map[string]any{"assignments": Record{"intakeSplit": Record{"lines": 400, "files": 8}}}
			again, err := r.checkIntakeSize(context.Background(), d, a, cfg, false, 0)
			if err != nil || again != tc.want || a["ready"] != true {
				t.Fatalf("again=%v answer=%v err=%v", again, a, err)
			}
			if again {
				again, err = r.checkIntakeSize(context.Background(), d, a, cfg, false, 0)
				if err != nil || again || a["ready"] != true {
					t.Fatalf("repeat=%v answer=%v err=%v", again, a, err)
				}
			}
		})
	}
}
