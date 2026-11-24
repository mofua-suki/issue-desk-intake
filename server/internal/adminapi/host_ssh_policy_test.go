package adminapi

import (
	"path/filepath"
	"testing"
)

// 配布物の host-ssh.json が、サーバーの検査を通ること（DBなしで動く）。
func TestDistributedHostSSHPolicyIsValid(t *testing.T) {
	file, e := filepath.Abs(filepath.Join("..", "..", "..", "host-ssh.json"))
	if e != nil {
		t.Fatal(e)
	}
	s := &service{Options: Options{Config: map[string]any{"hostSshPolicyFile": file}}}
	policy, e := s.repairPolicy()
	if e != nil {
		t.Fatalf("配布物の host-ssh.json が検査を通りません: %v", e)
	}
	for _, host := range []string{"gt12-pro", "a9-max", "x1-pro1", "x1-pro2", "nab-9s"} {
		entry, ok := policy.Hosts[host]
		if !ok || entry.Machine != host+".example.ts.net" || entry.Principal != "issue-desk-repair-"+host {
			t.Fatalf("%s: %+v", host, entry)
		}
	}
	for _, operation := range []string{"status", "tools", "verify", "verify-intake", "verify-sample-expense-server", "verify-sample-expense-client"} {
		if policy.Operations[operation] == "" {
			t.Fatalf("操作がありません: %s", operation)
		}
	}
	for _, operation := range []string{"alarm-checklist-native-log", "alarm-checklist-release-ruby"} {
		for host := range policy.Hosts {
			if policy.allows(host, operation) != (host == "macbook-pro") {
				t.Errorf("%s: %s must be limited to macbook-pro", host, operation)
			}
		}
		if contains(policy.ReadOnlyOperations, operation) {
			t.Errorf("%s must require explicit approval", operation)
		}
	}
}

func TestDBReadGrantKind(t *testing.T) {
	f := repairFixture(t)
	for i, target := range []string{"shared", "app"} {
		t.Run(target, func(t *testing.T) {
			id := []string{"11111111111111111111111111111111", "22222222222222222222222222222222"}[i]
			input := grantInput(f, id, "db-read", target)
			input["ttlSeconds"] = 600
			code, value := f.call("POST", "/api/permission-grants", "delegate", input)
			status(t, code, 201, value)
			if target == "shared" {
				code, value = f.call("GET", "/api/permission-grants?kind=db-read", "screen", nil)
				status(t, code, 200, value)
				grants := obj(value)["grants"].([]any)
				if len(grants) != 1 || obj(grants[0])["target"] != "shared" {
					t.Fatalf("unexpected grants: %v", grants)
				}
			}
		})
	}
	t.Run("invalid target and permanent", func(t *testing.T) {
		input := grantInput(f, "33333333333333333333333333333333", "db-read", "other")
		input["ttlSeconds"] = 600
		code, value := f.call("POST", "/api/permission-grants", "delegate", input)
		status(t, code, 400, value)
		input["target"] = "shared"
		delete(input, "ttlSeconds")
		input["permanent"] = true
		code, value = f.call("POST", "/api/permission-grants", "delegate", input)
		status(t, code, 400, value)
	})
}
