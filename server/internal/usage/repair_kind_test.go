package usage

import "testing"

// 受付の相談が送る形式の直しの種類は、すべて使用量の行に残り、欄は種類ごとの許可に従う。
func TestFormatRepairKindsAreRecorded(t *testing.T) {
	base := func(kind string) map[string]any {
		return map[string]any{"at": "2026-11-01T00:00:00.000Z", "source": "intake", "host": "nas", "provider": "codex", "step": DraftStep, "callTrigger": "format_repair", "repairKind": kind}
	}
	for _, kind := range []string{"draft_format", "split_over_limit", "repeated_read", "reviewed_unconfirmed", "no_reviewed_body", "implementation_unchecked", "execution_plan", "completion_plan", "acceptance", "change_scope"} {
		if !ValidUsage(base(kind)) || !RecordedRepairKind(kind) {
			t.Fatalf("kind %s is not recorded", kind)
		}
	}
	if ValidUsage(base("unknown_kind")) || RecordedRepairKind("unknown_kind") {
		t.Fatal("unknown kind accepted")
	}
	if len(RecordedRepairKinds()) != 10 {
		t.Fatal(RecordedRepairKinds())
	}
	planField := base("change_scope")
	planField["repairField"] = "changeScope"
	if ValidUsage(planField) {
		t.Fatal("field accepted for a kind without fields")
	}
	initial := base("acceptance")
	initial["callTrigger"] = "initial"
	if ValidUsage(initial) {
		t.Fatal("repair kind accepted without format_repair")
	}
}
