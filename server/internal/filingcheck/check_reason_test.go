package filingcheck

import (
	"context"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFilingCheckSummaryPersistsReasons(t *testing.T) {
	ctx := context.Background()
	calls := 0
	s, d, _ := fixture(t, func(context.Context, map[string]any) (map[string]any, error) {
		calls++
		out := allowed()
		out["proceed"], out["reason"] = false, fmt.Sprintf("候補と目的が重なるため第%d回は止めます", calls)
		return out, nil
	})
	read := func() Record {
		t.Helper()
		r, ok, err := s.options.Records.Get(ctx, "drafts", fleet.Text(d["id"]))
		if err != nil || !ok {
			t.Fatal(ok, err)
		}
		return r
	}
	for i := 0; i < 5; i++ {
		if err := s.Check(ctx, read(), "resubmit"); err == nil {
			t.Fatal("拒否されるはず")
		}
	}
	if err := s.Check(ctx, read(), "resubmit"); err == nil || !strings.Contains(err.Error(), "上限5回") || calls != 5 {
		t.Fatal(err, calls)
	}
	checks := fleet.Elements(obj(read()["filingCheckCalls"])["checks"])
	if len(checks) != 5 {
		t.Fatalf("呼び出し前の停止を履歴に足さず5件: %d", len(checks))
	}
	for i, item := range checks {
		row := obj(item)
		want := fmt.Sprintf("候補と目的が重なるため第%d回は止めます", i+1)
		if row["result"] != "refused" || row["reason"] != want {
			t.Fatalf("%d回目: %+v", i+1, row)
		}
	}
}

func TestFilingCheckReasonForEachResult(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		answer func() (map[string]any, error)
		result string
		reason string
	}{
		{"AI失敗", func() (map[string]any, error) { return nil, errors.New("token=abcdefghijkl 落ちた") }, "ai_failed", "受付AIの呼び出しに失敗しました。"},
		{"回答不正", func() (map[string]any, error) {
			out := allowed()
			out["reason"] = ""
			return out, nil
		}, "invalid_answer", checkReasonFallback["invalid_answer"]},
		{"許可", func() (map[string]any, error) {
			out := allowed()
			out["reason"] = "重なる依頼はありません"
			return out, nil
		}, "allowed", "重なる依頼はありません"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, d, _ := fixture(t, func(context.Context, map[string]any) (map[string]any, error) { return tc.answer() })
			_ = s.Check(ctx, d, "resubmit")
			saved, _, err := s.options.Records.Get(ctx, "drafts", fleet.Text(d["id"]))
			if err != nil {
				t.Fatal(err)
			}
			checks := fleet.Elements(obj(saved["filingCheckCalls"])["checks"])
			if len(checks) == 0 {
				t.Fatal("履歴がありません")
			}
			row := obj(checks[len(checks)-1])
			if row["result"] != tc.result || row["reason"] != tc.reason {
				t.Fatalf("%+v", row)
			}
		})
	}
}

func TestEvidenceRequestReasonNamesCandidatesOnly(t *testing.T) {
	got := evidenceCheckReason([]any{Record{"candidateId": "request:3", "kind": "body"}, Record{"candidateId": "秘密の本文 を含むID", "kind": "body"}})
	if got != "資料の追加を要求: request:3(body)" {
		t.Fatal(got)
	}
}

func TestSafeCheckReasonRemovesPrivateText(t *testing.T) {
	material := Record{
		"target":   Record{"body": "利用者が書いた長い依頼本文の一文がここにあります。\n短い本文の行"},
		"requests": []any{Record{"body": "比較候補の本文に書かれた目的の説明です。"}},
		"messages": []any{Record{"text": "会話の中で伝えた内部の事情を説明します。"}},
	}
	for _, leaked := range []string{"利用者が書いた長い依頼本文の一文", "短い本文の行", "比較候補の本文に書かれた目的", "会話の中で伝えた内部の事情"} {
		got := safeCheckReason("refused", "重複と判断: "+leaked+"が理由です server/internal/filingcheck/check.go", material)
		if strings.Contains(got, leaked) {
			t.Fatalf("%q が残りました: %s", leaked, got)
		}
		if !strings.Contains(got, "server/internal/filingcheck/check.go") {
			t.Fatalf("パスは残すはず: %s", got)
		}
	}
	if got := safeCheckReason("refused", "候補の目的は「内部の秘密の引用文」です", Record{}); strings.Contains(got, "内部の秘密") {
		t.Fatal(got)
	}
	for _, secret := range []string{"token: abcdefghijkl1234", "Bearer abcdefghijkl1234", "ghp_abcdefghijklmnop"} {
		if got := safeCheckReason("refused", "理由 "+secret, Record{}); got != checkReasonFallback["refused"] {
			t.Fatalf("資格情報は固定要約にする: %s", got)
		}
	}
	if got := safeCheckReason("refused", "「全部引用」", Record{}); got != checkReasonFallback["refused"] {
		t.Fatal(got)
	}
	long := safeCheckReason("refused", "一行目\n二行目"+strings.Repeat("長", 3000), Record{})
	if strings.Contains(long, "\n") || utf8.RuneCountInString(long) > 500 {
		t.Fatal(utf8.RuneCountInString(long))
	}
}
