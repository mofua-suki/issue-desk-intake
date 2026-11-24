package consultationai

import (
	"context"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/fleet"
)

// 確認枠が尽きた回は、読めなかった資料を質問に残し、質問の無い下書きで相談を止めない。
func TestReadEndKeepsUnreadMaterialsAsQuestion(t *testing.T) {
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		return measuredAnswer(map[string]any{"response": map[string]any{"readFiles": []any{Record{"file": "README.md", "mode": "lines", "start": 1, "end": 1}}}}), nil
	})
	out, err := s.Turn(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	questions := fleet.Elements(out["questions"])
	if out["ready"] == true || len(questions) != 1 || !strings.Contains(fleet.Text(questions[0]), "README.md:1-1") {
		t.Fatalf("確認枠の終わりの回答: %v", out)
	}
}

// endAnswer はどの呼び出し元からでも、質問の無い回答に依頼者が答えられる質問を1つ足す。
func TestEndAnswerAddsQuestionWhenNoneRemain(t *testing.T) {
	cases := []struct {
		name   string
		answer Record
		want   []string
	}{
		{"資料の名前", Record{"readFiles": []any{"docs/a.md", Record{"file": "src/b.go", "mode": "keyword", "keyword": "Turn"}}}, []string{"docs/a.md", "src/b.go（検索語: Turn）"}},
		{"資料の要求なし", Record{"questions": []any{"  "}}, []string{"補う資料（パスと行範囲）"}},
		{"回答なし", nil, []string{"補う資料（パスと行範囲）"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := endAnswer(Record{"title": "題", "body": "本文"}, tc.answer)
			questions := fleet.Elements(got["questions"])
			if got["ready"] != false || len(questions) != 1 || len(fleet.Elements(got["readFiles"])) != 0 {
				t.Fatalf("回答: %v", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(fleet.Text(questions[0]), w) {
					t.Fatalf("質問に %q がありません: %v", w, questions[0])
				}
			}
		})
	}
}

// 依頼者へ既に質問があるときは、そのまま残して足さない。
func TestEndAnswerKeepsExistingQuestions(t *testing.T) {
	got := endAnswer(Record{}, Record{"questions": []any{"どの版ですか"}, "readFiles": []any{"README.md"}})
	questions := fleet.Elements(got["questions"])
	if len(questions) != 1 || questions[0] != "どの版ですか" {
		t.Fatalf("質問: %v", questions)
	}
}

// 読取枠の終わりを通らない回でも、起票可でも質問でもない回答には質問を1つ足す。
func TestTurnAddsQuestionToQuestionlessAnswer(t *testing.T) {
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		return measuredAnswer(map[string]any{"response": map[string]any{"ready": false, "reply": "追加読取枠が終了しており、起票可否は未確定です。", "questions": []any{}}}), nil
	})
	out, err := s.Turn(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	questions := fleet.Elements(out["questions"])
	reasons := fleet.Elements(out["questionReasons"])
	if out["ready"] == true || len(questions) != 1 || questions[0] != questionlessQuestion || len(reasons) != 1 || Record(fleet.Object(reasons[0]))["controller"] != true {
		t.Fatalf("質問の無い回答: %v", out)
	}
}

// 追加した質問の理由はこの回のものだけにし、前の回の理由を残さない。
func TestEnsureQuestionReplacesStaleReasons(t *testing.T) {
	got := endAnswer(Record{}, Record{"questionReasons": []any{Record{"question": "前の質問", "reason": "前の理由"}}})
	reasons := fleet.Elements(got["questionReasons"])
	if len(reasons) != 1 || Record(fleet.Object(reasons[0]))["question"] != fleet.Elements(got["questions"])[0] {
		t.Fatalf("質問の理由: %v", got)
	}
}

// 保留中・分割の子・新しいプロジェクトの下書きは、相談の止め方が別なので質問を足さない。
func TestEnsureQuestionSkipsDraftsTheEngineKeeps(t *testing.T) {
	cases := []struct {
		name   string
		d      Record
		answer Record
	}{
		{"保留", Record{"hold": true}, Record{}},
		{"分割の子", Record{"parentSplit": "parent"}, Record{}},
		{"新しいプロジェクト", Record{}, Record{"newProject": Record{"needed": true}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := endAnswer(tc.d, tc.answer)
			if hasQuestion(got["questions"]) {
				t.Fatalf("質問を足しました: %v", got)
			}
		})
	}
}
