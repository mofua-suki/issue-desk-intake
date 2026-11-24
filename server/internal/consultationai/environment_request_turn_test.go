package consultationai

import (
	"context"
	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
)

func TestEnvironmentRequestTurnRepair(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		child, fixed, clean, limit bool
	}{
		{name: "修復成功", fixed: true}, {name: "修復失敗"}, {name: "分割案", child: true, fixed: true}, {name: "違反なし", clean: true}, {name: "修復上限", limit: true}, {name: "分割案修復失敗", child: true}, {name: "分割案修復上限", child: true, limit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			sawInstruction := false
			s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
				calls++
				// 最初の呼び出しは予約資料の読み込みにより繰り返される。
				if strings.Contains(r.Input, "実装環境で実行できない要求があります") {
					sawInstruction = true
				}
				value := reply()
				value["ready"] = true
				value["title"] = "題名"
				value["body"] = "実装内容"
				if tc.fixed && sawInstruction {
					value["body"] = "本番ログの測定はコントローラーの担当とする。既定ブランチへの追従はコントローラーが行う"
				}
				value["questions"] = []any{}
				if !tc.clean && !(tc.fixed && sawInstruction) {
					if tc.child {
						value["splitIssues"] = []any{Record{"title": "git fetch を実行する", "purpose": "変更", "acceptance": []any{"確認: `go test ./...` → 期待: 終了コード0"}}}
					} else {
						value["body"] = "着手時に最新の main を取り込む"
					}
				}
				return measuredAnswer(value), nil
			})
			if tc.limit {
				d["formatRepairs"] = Record{"total": 8}
			}
			result, err := s.Turn(context.Background(), d)
			if err != nil {
				t.Fatal(err)
			}
			if tc.clean {
				if calls != 2 || result["ready"] != true {
					t.Fatalf("calls=%d result=%v", calls, result)
				}
				return
			}
			if tc.limit {
				if result["ready"] != false || len(fleet.Elements(result["questions"])) == 0 || sawInstruction {
					t.Fatalf("calls=%d result=%v", calls, result)
				}
				return
			}
			if !sawInstruction || calls != 3 {
				t.Fatalf("calls=%d instruction=%v result=%v", calls, sawInstruction, result)
			}
			if tc.fixed && (result["ready"] != true || len(fleet.Elements(result["questions"])) != 0) {
				t.Fatal(result)
			}
			if !tc.fixed && (result["ready"] != false || len(fleet.Elements(result["questions"])) == 0) {
				t.Fatal(result)
			}
		})
	}
}

func TestEnvironmentRepairNoticeOnlyOnNextResume(t *testing.T) {
	calls := 0
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		calls++
		if r.OnSession != nil && r.Session == "" {
			if err := r.OnSession("11111111-1111-4111-8111-111111111112"); err != nil {
				t.Fatal(err)
			}
		}
		value := reply()
		value["ready"], value["title"], value["body"], value["questions"] = true, "題名", "実装内容", []any{}
		switch calls {
		case 1, 2:
			value["body"] = "git fetch を実行する"
		case 3:
			if !r.Resume || !strings.Contains(r.Input, "実装環境で実行できない要求があります") {
				t.Fatalf("repair input: %s", r.Input)
			}
			value["readFiles"] = []any{Record{"file": "invalid"}}
		case 4:
			if !r.Resume || !strings.Contains(r.Input, "資料要求が不正です") || strings.Contains(r.Input, "実装環境で実行できない要求があります") {
				t.Fatalf("later input: %s", r.Input)
			}
			value["ready"] = false
		}
		return measuredAnswer(value), nil
	})
	result, err := s.Turn(context.Background(), d)
	if err != nil || calls != 4 || result["ready"] != false {
		t.Fatalf("calls=%d result=%v err=%v", calls, result, err)
	}
}

func TestEnvironmentCapabilityTurnRepair(t *testing.T) {
	for _, child := range []bool{false, true} {
		t.Run(map[bool]string{false: "body", true: "split"}[child], func(t *testing.T) {
			calls := 0
			notice := false
			s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
				calls++
				if strings.Contains(r.Input, "実装環境で実行できない要求があります") {
					notice = strings.Contains(r.Input, "gcc") && strings.Contains(r.Input, "fleet.json") && strings.Contains(r.Input, "go test -race")
				}
				value := reply()
				value["ready"], value["title"], value["questions"] = true, "題名", []any{}
				value["body"] = "実装内容"
				if !notice {
					if child {
						value["splitIssues"] = []any{Record{"project": "owner/app", "title": "変更", "purpose": "改善", "acceptance": []any{"go test -race ./... を実行する"}}}
					} else {
						value["body"] = "go test -race ./... を実行する"
					}
				} else {
					value["body"] = "実装内容\n## マージ後の運用作業（自動化の対象外）\n担当: 運用者。手順: gccを用意してgo test -race ./...を実行する。確認: テスト成功。理由: 実装PCにgccがない。"
				}
				return measuredAnswer(value), nil
			})
			s.options.Environment = func(context.Context, string) ([]EnvironmentHost, error) {
				return []EnvironmentHost{{"a", map[string]any{"gcc": false, "cgo": true, "race": true}}}, nil
			}
			result, err := s.Turn(context.Background(), d)
			if err != nil || !notice || calls != 3 || result["ready"] != true {
				t.Fatalf("calls=%d notice=%v result=%v err=%v", calls, notice, result, err)
			}
		})
	}
}

func TestEnvironmentCapabilityCoreConditionQuestion(t *testing.T) {
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		value := reply()
		value["title"] = "題名"
		if strings.Contains(r.Input, "実装環境で実行できない要求があります") {
			value["ready"] = false
			value["questions"] = []any{"race検証を完成物に残して実行環境を変更しますか、それとも検証を運用作業に移して範囲を縮小しますか？"}
		} else {
			value["ready"], value["body"], value["questions"] = true, "race検証を必須とする", []any{}
		}
		return measuredAnswer(value), nil
	})
	s.options.Environment = func(context.Context, string) ([]EnvironmentHost, error) {
		return []EnvironmentHost{{"a", map[string]any{"gcc": false, "cgo": true, "race": true}}}, nil
	}
	result, err := s.Turn(context.Background(), d)
	if err != nil || result["ready"] != false || len(fleet.Elements(result["questions"])) == 0 {
		t.Fatalf("result=%v err=%v", result, err)
	}
}
