package consultationai

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/ai"
)

func TestInvalidReadCorrectionWithValidBatch(t *testing.T) {
	calls := 0
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		calls++
		v := reply()
		if calls == 1 {
			v["readFiles"] = []any{"repo-search", map[string]any{"file": "repo-search", "mode": "outline"}, map[string]any{"file": "repo-search", "mode": "keyword"}, map[string]any{"file": "repo-search", "mode": "keyword", "keyword": "実装"}}
		} else if !strings.Contains(r.Input, "資料要求が不正") {
			t.Error("訂正理由が届いていない")
		}
		return measuredAnswer(v), nil
	})
	result, err := s.Turn(context.Background(), d)
	if err != nil || calls != 2 || result == nil {
		t.Fatalf("calls=%d result=%v err=%v", calls, result, err)
	}
	saved, _, _ := s.options.Records.Get(context.Background(), "drafts", draftID)
	p := fleet.Object(fleet.Object(saved["progress"])["projects"])["owner/app"]
	found := 0
	for _, entry := range fleet.Elements(fleet.Object(p)["reads"]) {
		if fleet.Text(fleet.Object(entry)["path"]) == "repo-search" {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("正当な検索のみ保存されるべき: %v", p)
	}
}

func TestSavedInvalidPendingResumesValidRequests(t *testing.T) {
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		if !strings.Contains(r.Input, "repo-searchはkeyword") {
			t.Error("復旧理由が届いていない")
		}
		return measuredAnswer(reply()), nil
	})
	ctx := context.Background()
	config, _ := s.options.Config(ctx)
	b, _ := s.budget(d, config)
	r, _ := b.reads()
	if err := r.begin(ctx, []any{"repo-search", map[string]any{"file": "repo-search", "mode": "keyword", "keyword": "実装"}}); err != nil {
		t.Fatal(err)
	}
	saved, _, _ := s.options.Records.Get(ctx, "drafts", draftID)
	if _, err := s.Turn(ctx, saved); err != nil {
		t.Fatal(err)
	}
	saved, _, _ = s.options.Records.Get(ctx, "drafts", draftID)
	cr := fleet.Object(fleet.Object(saved["consultationBudget"])["contextRead"])
	if fleet.Number(cr["rounds"]) != 1 || len(fleet.Elements(fleet.Object(cr["active"])["pending"])) != 0 {
		t.Fatalf("枠または残件が不正: %v", cr)
	}
}

func TestInvalidReadCorrectionIsBounded(t *testing.T) {
	calls := 0
	s, d := testService(t, func(context.Context, ai.Request) (ai.Answer, error) {
		calls++
		v := reply()
		v["ready"] = true
		v["readFiles"] = []any{"repo-search"}
		return measuredAnswer(v), nil
	})
	result, err := s.Turn(context.Background(), d)
	// 訂正の直し1回の後に、既読の根拠で回答させる最終回を1回だけ呼び、それでも資料を求めれば終える。
	if err != nil || calls != 3 || result["ready"] != false || len(fleet.Elements(result["readFiles"])) != 0 {
		t.Fatalf("calls=%d result=%v err=%v", calls, result, err)
	}
}
