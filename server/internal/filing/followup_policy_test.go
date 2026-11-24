package filing

import (
	"context"
	"errors"
	"testing"
)

func TestFollowupPolicyRequiresControllerValidationAndKeepsOtherRestrictions(t *testing.T) {
	ctx := context.Background()
	records := recordsFixture(t)
	d := Record{"id": "review-followup", "proposal": map[string]any{"fingerprint": "test"}}
	service := &Service{options: Options{Records: records}}
	if service.policy(ctx, d) == nil {
		t.Fatal("検証器なしで案を起票できます")
	}
	refusal := errors.New("マージ未確認")
	service.options.ValidateFollowup = func(context.Context, Record) error { return refusal }
	if err := service.policy(ctx, d); !errors.Is(err, refusal) {
		t.Fatal("拒否を無視しました", err)
	}
	service.options.ValidateFollowup = func(context.Context, Record) error { return nil }
	if err := service.policy(ctx, d); err != nil {
		t.Fatal(err)
	}
	if d["proposal"] == nil {
		t.Fatal("保存する来歴を削除しました")
	}
	for _, key := range []string{"attention", "stallFix", "fullTestFix", "appHealth"} {
		d[key] = true
		if service.policy(ctx, d) == nil {
			t.Fatal("別の自動起票制限を解除しました", key)
		}
		delete(d, key)
	}
	d["workType"] = "investigation"
	if service.policy(ctx, d) == nil {
		t.Fatal("廃止した調査を許可しました")
	}
	delete(d, "workType")
	d["parentSplit"] = "missing"
	if service.policy(ctx, d) == nil {
		t.Fatal("親の来歴不明を許可しました")
	}
	delete(d, "parentSplit")
	if CheckPolicy(ctx, records, d) == nil {
		t.Fatal("一般の手動経路からも案を起票できます")
	}
}
