package dispatch

import (
	"testing"
	"time"
)

const testHead = "0123456789abcdef0123456789abcdef01234567"
const testInstructionAt = "2026-11-11T11:58:00+09:00"

func init() { now = func() time.Time { return time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC) } }

func TestMergeManualKeepsApprovalOnlyWhenGiven(t *testing.T) {
	out, err := ValidatePayload("merge_manual", "o/r", map[string]any{"number": float64(30), "head": testHead, "approval": " 30はマージして ", "instructionAt": testInstructionAt})
	if err != nil || out["approval"] != "30はマージして" || out["instructionAt"] != testInstructionAt {
		t.Fatalf("承認発言は前後の空白を除いて残します: %v %v", out, err)
	}
	out, err = ValidatePayload("merge_manual", "o/r", map[string]any{"number": float64(30), "head": testHead})
	if err != nil {
		t.Fatal(err)
	}
	if _, has := out["approval"]; has {
		t.Fatalf("承認発言が無いときは付けません: %v", out)
	}
}

func TestMergeManualRejectsBadApproval(t *testing.T) {
	if _, err := ValidatePayload("merge_manual", "o/r", map[string]any{"number": float64(30), "head": testHead, "approval": float64(3)}); err == nil {
		t.Fatal("文字列でない承認発言は断ります")
	}
	long := make([]rune, 501)
	for i := range long {
		long[i] = 'あ'
	}
	if _, err := ValidatePayload("merge_manual", "o/r", map[string]any{"number": float64(30), "head": testHead, "approval": string(long)}); err == nil {
		t.Fatal("501文字の承認発言は断ります")
	}
}

// 承認発言には利用者がチャットで指示した日時が要る。無い・オフセットが無い・未来・7日より前・発言の無い日時は断る。
func TestMergeManualApprovalNeedsInstructionAt(t *testing.T) {
	for _, at := range []any{nil, "", "2026-11-11T11:58:00", "2026-11-11T12:01:00+09:00", "2026-11-04T11:59:00+09:00", float64(1)} {
		payload := map[string]any{"number": float64(30), "head": testHead, "approval": "30はマージして"}
		if at != nil {
			payload["instructionAt"] = at
		}
		if _, err := ValidatePayload("merge_manual", "o/r", payload); err == nil {
			t.Fatalf("instructionAt %v accepted", at)
		}
	}
	if _, err := ValidatePayload("merge_manual", "o/r", map[string]any{"number": float64(30), "head": testHead, "instructionAt": testInstructionAt}); err == nil {
		t.Fatal("承認発言の無い指示日時は断ります")
	}
}

func TestMergeManualKeepsDelegationMarkOnlyWithApproval(t *testing.T) {
	out, err := ValidatePayload("merge_manual", "o/r", map[string]any{"number": float64(30), "head": testHead, "approval": "30はマージして", "approvalDelegated": true, "instructionAt": testInstructionAt})
	if err != nil || out["approvalDelegated"] != true {
		t.Fatalf("承認発言があれば印を残します: %v %v", out, err)
	}
	out, err = ValidatePayload("merge_manual", "o/r", map[string]any{"number": float64(30), "head": testHead, "approvalDelegated": true})
	if err != nil {
		t.Fatal(err)
	}
	if _, has := out["approvalDelegated"]; has {
		t.Fatalf("承認発言が無いときは印を付けません: %v", out)
	}
}

func TestFullTestAcceptsAnyValidRepoWithEmptyPayload(t *testing.T) {
	if _, err := ValidatePayload("full_test", "mofua-suki/issue-desk-intake", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidatePayload("full_test", "o/r", map[string]any{"extra": true}); err == nil {
		t.Fatal("空でない payload は断ります")
	}
	if _, err := ValidatePayload("full_test", "not-a-repo", map[string]any{}); err == nil {
		t.Fatal("不正なリポジトリ名は断ります")
	}
}

func TestNativeTestRequiresExactTargetAndBoundedResult(t *testing.T) {
	payload := map[string]any{"host": "mac-test", "head": testHead, "base": testHead, "tree": testHead}
	if _, err := ValidatePayload("native_test", "owner/repo", payload); err != nil {
		t.Fatal(err)
	}
	payload["command"] = []any{"arbitrary"}
	if _, err := ValidatePayload("native_test", "owner/repo", payload); err == nil {
		t.Fatal("任意コマンドを受け入れました")
	}
	for _, state := range []string{"passed", "failed", "unrunnable", "interrupted"} {
		result := map[string]any{"status": state, "head": testHead, "base": testHead, "tree": testHead, "message": "result"}
		if err := ValidateResult("native_test", result); err != nil {
			t.Fatal(err)
		}
		delete(result, "tree")
		if err := ValidateResult("native_test", result); err == nil {
			t.Fatal("内容のSHAなしの結果を受け入れました")
		}
	}
}

func TestPullCreateKeepsPreapproval(t *testing.T) {
	for _, payload := range []map[string]any{
		{"branch": "chat/x", "title": "t", "body": "b"},
		{"title": "t", "body": "b", "patch": "p", "base": testHead, "patchSha256": "148de9c5a7a44d19e56cd9ae1a554bf67847afb0c58f6e12fa29ac7ddfca9940"},
	} {
		payload["approval"], payload["approvalDelegated"], payload["instructionAt"] = " マージまで進めて ", true, testInstructionAt
		out, err := ValidatePayload("pr_create", "o/r", payload)
		if err != nil || out["approval"] != "マージまで進めて" || out["approvalDelegated"] != true || out["instructionAt"] != testInstructionAt {
			t.Fatalf("作成依頼の事前承認を残しません: %v %v", out, err)
		}
		delete(payload, "instructionAt")
		if _, err := ValidatePayload("pr_create", "o/r", payload); err == nil {
			t.Fatal("指示日時の無い事前承認を受け入れました")
		}
	}
}

func TestMergeManualApprovalRequestShape(t *testing.T) {
	out, err := ValidatePayload("merge_manual", "o/r", map[string]any{"number": float64(30), "head": testHead, "approvalRequest": "123-aabbccdd"})
	if err != nil || out["approvalRequest"] != "123-aabbccdd" {
		t.Fatalf("approvalRequest を残しません: %v %v", out, err)
	}
	for _, id := range []any{"bad", float64(1), "123-AABBCCDD"} {
		if _, err := ValidatePayload("merge_manual", "o/r", map[string]any{"number": float64(30), "head": testHead, "approvalRequest": id}); err == nil {
			t.Fatalf("approvalRequest %v を受け入れました", id)
		}
	}
	if _, err := ValidatePayload("merge_manual", "o/r", map[string]any{"number": float64(30), "head": testHead, "approval": "30はマージして", "instructionAt": testInstructionAt, "approvalRequest": "123-aabbccdd"}); err == nil {
		t.Fatal("承認発言と approvalRequest の同時指定を受け入れました")
	}
}
