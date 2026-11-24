package consultationai

import (
	"context"
	"database/sql"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/ai"
)

const heavyTwoCriteria = "## 受入条件\n- 確認: `go test ./...` → 期待: 終了コード0\n- 確認: `go vet ./...` → 期待: 終了コード0"

func TestUnsupportedHeavyReasons(t *testing.T) {
	quoted := Record{"body": "## 利用者の判断と根拠\n利用者原文「この依頼は分割しないでください」日時: 2026-11-07", "messages": []any{Record{"role": "user", "text": "この依頼は分割しないでください", "at": "2026-11-07T10:00:00Z"}}}
	unquoted := Record{"body": "## 利用者の判断と根拠\n分けないで。日時: 2026-11-07", "messages": []any{Record{"role": "user", "text": "分けないで", "at": "2026-11-07T10:00:00Z"}}}
	split := func(acceptance ...int) []any {
		out := []any{}
		for _, n := range acceptance {
			items := []any{}
			for i := 0; i < n; i++ {
				items = append(items, "確認: `go test ./...` → 期待: 終了コード0")
			}
			out = append(out, Record{"title": "案", "acceptance": items, "heavyReason": "同じ状態遷移を扱う"})
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		draft  Record
		answer Record
		want   []string
	}{
		{"top level without quote", unquoted, Record{"ready": true, "body": heavyTwoCriteria, "heavyReason": "同じ状態遷移を扱う"}, []string{"本文"}},
		{"top level with quote", quoted, Record{"ready": true, "body": heavyTwoCriteria, "heavyReason": "同じ状態遷移を扱う"}, nil},
		{"single criterion cannot be split", unquoted, Record{"ready": true, "body": "## 受入条件\n- 確認: `go test ./...` → 期待: 終了コード0", "heavyReason": "同じ状態遷移を扱う"}, nil},
		{"fenced bullets are examples", unquoted, Record{"ready": true, "body": "## 受入条件\n- 確認: `go test ./...` → 期待: 終了コード0\n```\n- 例\n```", "heavyReason": "同じ状態遷移を扱う"}, nil},
		{"hotfix reason", unquoted, Record{"ready": true, "body": heavyTwoCriteria, "heavyReason": "止血: 本番の停止を先に止める"}, nil},
		{"stall fix", Record{"body": unquoted["body"], "messages": unquoted["messages"], "stallFix": true}, Record{"ready": true, "body": heavyTwoCriteria, "heavyReason": "同じ状態遷移を扱う"}, nil},
		{"heading variants", unquoted, Record{"ready": true, "body": "##受入条件（確認方法）\n- 確認: `go test ./...` → 期待: 終了コード0\n### 補足\n- 確認: `go vet ./...` → 期待: 終了コード0\n## 確認済み事項\n- 対象外", "heavyReason": "同じ状態遷移を扱う"}, []string{"本文"}},
		{"empty reason", unquoted, Record{"ready": true, "body": heavyTwoCriteria, "heavyReason": " "}, nil},
		{"not ready", unquoted, Record{"ready": false, "body": heavyTwoCriteria, "heavyReason": "同じ状態遷移を扱う"}, nil},
		{"split proposals", unquoted, Record{"ready": true, "heavyReason": "無視される", "splitIssues": split(1, 3)}, []string{"分割案2"}},
		{"split proposals with quote", quoted, Record{"ready": true, "splitIssues": split(2, 3)}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := unsupportedHeavyReasons(tc.draft, tc.answer)
			if !reflect.DeepEqual(got, tc.want) && !(len(got) == 0 && len(tc.want) == 0) {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
		})
	}
	answer := Record{"ready": true, "heavyReason": "x", "splitIssues": split(2, 2)}
	clearHeavyReasons(answer, []string{"本文", "分割案2"})
	if answer["heavyReason"] != "" || fleet.Object(fleet.Elements(answer["splitIssues"])[0])["heavyReason"] == "" || fleet.Object(fleet.Elements(answer["splitIssues"])[1])["heavyReason"] != "" {
		t.Fatalf("answer=%v", answer)
	}
}

func TestHeavyReasonTurnRequestsSplitOnce(t *testing.T) {
	for _, resolved := range []bool{false, true} {
		t.Run(map[bool]string{false: "kept whole", true: "split"}[resolved], func(t *testing.T) {
			calls, notices := 0, 0
			s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
				calls++
				if strings.Contains(r.Input, "heavyReason）が、利用者の分割禁止の発言") {
					notices++
				}
				v := reply()
				v["ready"], v["title"], v["body"], v["questions"] = true, "題名", heavyTwoCriteria, []any{}
				v["heavyReason"] = "同じ状態遷移を扱うため分けられない"
				if notices > 0 && resolved {
					v["heavyReason"] = ""
					v["splitIssues"] = []any{Record{"title": "子1", "purpose": "目的", "acceptance": []any{"確認: `go test ./...` → 期待: 終了コード0"}, "dependsOn": []any{}, "heavyReason": ""}, Record{"title": "子2", "purpose": "検証", "acceptance": []any{"確認: `go vet ./...` → 期待: 終了コード0"}, "dependsOn": []any{1}, "heavyReason": ""}}
				}
				return measuredAnswer(v), nil
			})
			got, err := s.Turn(context.Background(), d)
			if err != nil {
				t.Fatal(err)
			}
			if got["ready"] != true || notices != 1 {
				t.Fatalf("calls=%d notices=%d ready=%v", calls, notices, got["ready"])
			}
			saved, _, err := s.options.Records.Get(context.Background(), "drafts", draftID)
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(fleet.Text(got["heavyReason"])) != "" {
				t.Fatalf("unsupported heavyReason was kept: %q", got["heavyReason"])
			}
			if resolved != (len(fleet.Elements(got["splitIssues"])) == 2) {
				t.Fatalf("splitIssues=%v", got["splitIssues"])
			}
			state := fleet.Object(fleet.Object(fleet.Object(saved["consultationBudget"])["contextRead"])["heavyReason"])
			if fleet.Number(state["requests"]) != 1 || state["pending"] != false {
				t.Fatalf("state=%v", state)
			}
			if !resolved && !strings.Contains(fleet.Text(got["reply"]), "外しました") {
				t.Fatalf("reply=%q", got["reply"])
			}
		})
	}
}

func TestHeavyReasonTurnKeepsQuotedDecision(t *testing.T) {
	calls := 0
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		calls++
		v := reply()
		v["ready"], v["title"], v["body"], v["questions"] = true, "題名", heavyTwoCriteria, []any{}
		v["heavyReason"] = "同じ状態遷移を扱うため分けられない"
		return measuredAnswer(v), nil
	})
	s.options.Reference = nil
	d["body"] = "## 利用者の判断と根拠\n利用者原文「この依頼は分割しないでください」日時: 2026-11-01"
	d["messages"] = []any{Record{"role": "user", "id": "u", "text": "この依頼は分割しないでください", "at": "2026-11-01T00:00:00Z"}}
	if _, err := s.options.Records.Change(context.Background(), "drafts", draftID, func(_ *sql.Tx, current Record) (Record, error) {
		current["body"], current["messages"] = d["body"], d["messages"]
		return current, nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Turn(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if got["ready"] != true || calls != 1 {
		t.Fatalf("calls=%d ready=%v", calls, got["ready"])
	}
	if fleet.Text(got["heavyReason"]) == "" {
		t.Fatal("quoted no-split decision lost its heavyReason")
	}
}

func TestHeavyReasonPendingNoticeSurvivesResume(t *testing.T) {
	seen := false
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		seen = seen || strings.Contains(r.Input, "heavyReason）が、利用者の分割禁止の発言")
		v := reply()
		v["ready"], v["title"], v["body"], v["questions"] = true, "題名", heavyTwoCriteria, []any{}
		return measuredAnswer(v), nil
	})
	budget := Record{"contextRead": Record{"heavyReason": Record{"continuations": 1, "requests": 1, "pending": true, "notice": heavyReasonNotice([]string{"本文"})}}}
	d["consultationBudget"] = budget
	if _, err := s.options.Records.Change(context.Background(), "drafts", draftID, func(_ *sql.Tx, current Record) (Record, error) {
		current["consultationBudget"] = budget
		return current, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Turn(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("pending split request was not delivered after resume")
	}
}
