package fleet

import "testing"

func nativeAssignments(t *testing.T) *Assignments {
	t.Helper()
	raw := assignmentsBody(t)
	Object(raw["hosts"])["mac-test"] = map[string]any{"role": "test-only", "maxImplementations": float64(1), "repositories": map[string]any{}, "testRepositories": []any{"owner/repo"}}
	assignments, err := NormalizeAssignments(raw)
	if err != nil {
		t.Fatal(err)
	}
	return assignments
}

func TestTestOnlyRoleCannotBecomeImplementer(t *testing.T) {
	assignments := nativeAssignments(t)
	if !assignments.CanTest("mac-test", "owner/repo") || assignments.CanImplement("mac-test", "owner/repo") || !assignments.CanImplement("nas", "owner/repo") {
		t.Fatal("役割が混ざりました")
	}
	for _, invalidRole := range []string{"both", "tester"} {
		raw := assignmentsBody(t)
		Object(Object(raw["hosts"])["nas"])["role"] = invalidRole
		if _, err := NormalizeAssignments(raw); err == nil {
			t.Fatal("未知の役割を受け入れました")
		}
	}
	raw := assignmentsBody(t)
	Object(Object(raw["hosts"])["nas"])["role"] = "test-only"
	Object(Object(raw["hosts"])["nas"])["testRepositories"] = []any{"owner/repo"}
	if _, err := NormalizeAssignments(raw); err == nil {
		t.Fatal("テスト専用に実装担当を登録できました")
	}
}

func TestNativeTestTargetRequiresRegisteredCommandAndMacEnvironment(t *testing.T) {
	assignments := nativeAssignments(t)
	document := Document{Defaults: map[string]any{}, Hosts: map[string]map[string]any{
		"nas":      {"repositories": []any{map[string]any{"name": "owner/repo", "nativeTestHost": "mac-test"}}},
		"other":    {"repositories": []any{map[string]any{"name": "owner/repo", "nativeTestHost": "mac-test"}}},
		"mac-test": {"environment": map[string]any{"platform": "darwin"}, "repositories": []any{map[string]any{"name": "owner/repo", "testCommand": []any{"sh", "scripts/verify-apple.sh", "macos"}}}},
	}}
	if err := validateNativeTests(document, assignments); err != nil {
		t.Fatal(err)
	}
	if NativeTestTarget(document, assignments, "nas", "owner/repo") != "mac-test" || NativeTestTarget(document, assignments, "mac-test", "owner/repo") != "" {
		t.Fatal("検証先が不正です")
	}
	Object(List(document.Hosts["other"]["repositories"])[0])["nativeTestHost"] = ""
	if err := validateNativeTests(document, assignments); err == nil {
		t.Fatal("代替実装PCがMac検証を迂回できました")
	}
	Object(List(document.Hosts["other"]["repositories"])[0])["nativeTestHost"] = "mac-test"
	Object(document.Hosts["mac-test"]["environment"])["platform"] = "linux"
	if err := validateNativeTests(document, assignments); err == nil {
		t.Fatal("LinuxにAppleネイティブ検証を配布できました")
	}
	Object(document.Hosts["mac-test"]["environment"])["platform"] = "darwin"
	Object(List(document.Hosts["mac-test"]["repositories"])[0])["testCommand"] = []any{}
	if err := validateNativeTests(document, assignments); err == nil {
		t.Fatal("固定コマンドなしの担当を受け入れました")
	}
}
