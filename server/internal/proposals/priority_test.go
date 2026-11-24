package proposals

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/filing"
)

func priorityParse(t *testing.T, p Object) (Object, string, error) {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return parse("<!-- issue-desk-proposal " + string(raw) + " -->")
}

func TestPriorityHighAndCompatibility(t *testing.T) {
	p := Object{"version": 3, "kind": "followup", "id": "followup-1", "source": "owner/repo#1", "title": "後続の案", "pr": "https://git.example/owner/repo/pulls/2", "items": []any{Object{"problem": "不具合", "expected": "正常", "test": "検証"}}, "dependsOn": []any{}, "provider": "codex", "changeScope": nil}
	plain, hash, err := priorityParse(t, p)
	if err != nil || hash != "c2b6b2112c04e77840e57f8c7a8fd7714f6888a0011ca1a2a0965f7494f610b2" {
		t.Fatalf("旧指紋 %s %v", hash, err)
	}
	check := func(p Object, want int) {
		marker, _ := json.Marshal(followupSettings(p, "codex", nil))
		job, err := filing.JobFromIssue("owner/repo", Object{"number": 1, "title": "題", "body": "<!-- issue-desk:11111111-1111-4111-8111-111111111111 -->\n<!-- issue-desk:settings " + string(marker) + " -->"}, "followup")
		if err != nil || job.Priority != want {
			t.Fatalf("設定=%s job=%+v err=%v", marker, job, err)
		}
	}
	check(plain, 0)
	p["priority"] = Object{"level": "high", "reason": "  早急に修正 😀  "}
	parsed, markedHash, err := priorityParse(t, p)
	if err != nil || markedHash == hash {
		t.Fatalf("印 %v %s", err, markedHash)
	}
	d := consultations.Record{}
	setFollowupPriority(d, parsed, "2026-01-01T00:00:00Z")
	if d["priority"] != "high" || obj(d["prioritySource"])["reason"] != "早急に修正 😀" {
		t.Fatalf("理由 %v", d)
	}
	check(parsed, 1)
}

func TestPriorityInvalid(t *testing.T) {
	cases := []any{nil, "high", Object{"level": "normal", "reason": "理由"}, Object{"level": "high", "reason": " "}, Object{"level": "high", "reason": strings.Repeat("あ", 501)}, Object{"level": "high", "reason": "理由", "extra": true}, Object{"level": "high"}}
	for _, value := range cases {
		p := v3()
		p["priority"] = value
		parsed, _, err := priorityParse(t, p)
		if err == nil || !strings.Contains(err.Error(), "priority") || parsed != nil {
			t.Fatalf("不正な印を拒否できません: %v %v", value, err)
		}
	}
	p := v3()
	p["priority"] = Object{"level": "high", "reason": strings.Repeat("😀", 500)}
	if _, _, err := priorityParse(t, p); err != nil {
		t.Fatal(err)
	}
}

func TestPriorityShippedWriterContract(t *testing.T) {
	module, err := filepath.Abs(filepath.Join("..", "..", "..", "shared", "src", "proposal-comment.ts"))
	if err != nil {
		t.Fatal(err)
	}
	p := v3()
	p["priority"] = Object{"reason": "\u0085 理由😀 \u0085", "level": "high"}
	raw, _ := json.Marshal(p)
	cmd := exec.Command("node", "--input-type=module", "-e", `import {pathToFileURL} from 'node:url'; const m=await import(pathToFileURL(process.argv[1]).href); const p=JSON.parse(process.argv[2]); process.stdout.write(JSON.stringify({body:m.formatProposal(p),hash:m.proposalFingerprint(p)}));`, module, string(raw))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("writer: %s %v", output, err)
	}
	var result Object
	if err = json.Unmarshal(output, &result); err != nil {
		t.Fatal(err)
	}
	parsed, hash, err := parse(str(result["body"]))
	if err != nil || hash != result["hash"] || obj(parsed["priority"])["reason"] != "理由😀" {
		t.Fatalf("契約 %v %s %v", parsed, hash, err)
	}
}
