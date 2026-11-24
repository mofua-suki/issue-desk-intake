package adminapi

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/contract"
)

func distributedReadOnlyPolicy(t *testing.T) (hostSSHPolicy, map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "host-ssh.json"))
	if err != nil {
		t.Fatal(err)
	}
	var p hostSSHPolicy
	var data map[string]any
	if err = json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	return p, data
}

func TestReadOnlyPolicyValidation(t *testing.T) {
	_, data := distributedReadOnlyPolicy(t)
	cases := []struct {
		name  string
		list  any
		valid bool
	}{
		{"normal", []string{"status", "tools", "worker-logs"}, true}, {"absent", nil, true},
		{"empty", []string{""}, false}, {"duplicate", []string{"status", "status"}, false}, {"unknown", []string{"missing"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.list == nil {
				delete(data, "readOnlyOperations")
			} else {
				data["readOnlyOperations"] = tc.list
			}
			raw, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(t.TempDir(), "policy.json")
			if err = os.WriteFile(file, raw, 0600); err != nil {
				t.Fatal(err)
			}
			s := &service{Options: Options{Config: map[string]any{"hostSshPolicyFile": file}}}
			_, err = s.repairPolicy()
			if (err == nil) != tc.valid {
				t.Fatalf("validation error: %v", err)
			}
			if err != nil {
				problem, ok := err.(*contract.Problem)
				if !ok || problem.Status != 503 || !strings.Contains(err.Error(), "設定が不正") {
					t.Fatalf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestDistributedReadOnlyCommands(t *testing.T) {
	p, _ := distributedReadOnlyPolicy(t)
	if !reflect.DeepEqual(p.ReadOnlyOperations, []string{"status", "tools", "worker-logs"}) {
		t.Fatal(p.ReadOnlyOperations)
	}
	forbidden := regexp.MustCompile(`(?i)(^|[^a-z0-9_-])(rm|mv|cp|touch|tee|sudo|reboot|restart|stop|start|exec|run|kill|chmod|chown|mkdir|truncate|install)([^a-z0-9_-]|$)|>{1,2}`)
	for _, name := range p.ReadOnlyOperations {
		cmd := strings.ReplaceAll(p.Operations[name], "2>&1", "")
		if forbidden.MatchString(cmd) {
			t.Fatalf("%s: forbidden operation: %s", name, forbidden.FindString(cmd))
		}
	}
	logs := p.Operations["worker-logs"]
	for _, part := range []string{"timeout 10s", "--since 1h", "--tail 500", "head -c 65536"} {
		if !strings.Contains(logs, part) {
			t.Fatalf("missing %s", part)
		}
	}
}

func TestDistributedReadOnlySecretFiltering(t *testing.T) {
	p, _ := distributedReadOnlyPolicy(t)
	dir := t.TempDir()
	script := "#!/bin/sh\n" + `if [ "$(basename "$0")" = docker ]; then
 [ "$1" = compose ] && [ "$2" = -f ] && [ "$3" = "$BOOTSTRAP_DIR/compose.yaml" ] && [ "$4" = logs ] && [ "$5" = --since ] && [ "$6" = 1h ] && [ "$7" = --tail ] && [ "$8" = 500 ] && [ "$9" = --no-color ] && [ "${10}" = issue-desk-client ] || exit 42
fi
printf 'safe stdout\nidk_value\nidh_value\nPASSWORD value\n-----BEGIN RSA PRIVATE KEY-----\nkey body\n-----END RSA PRIVATE KEY-----\nsafe after\n'
printf 'safe stderr\nsecret value\ntoken value\n-----BEGIN PRIVATE KEY-----\nstderr key body\n-----END PRIVATE KEY-----\n' >&2
`
	for _, name := range []string{"docker", "go", "node"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"tools", "worker-logs"} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command("/bin/sh", "-c", p.Operations[name])
			cmd.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin", "BOOTSTRAP_DIR="+dir)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			s := string(out)
			for _, safe := range []string{"safe stdout", "safe stderr", "safe after"} {
				if !strings.Contains(s, safe) {
					t.Fatalf("missing %s: %s", safe, s)
				}
			}
			for _, secret := range []string{"idk_", "idh_", "PASSWORD", "secret", "token", "PRIVATE KEY", "key body"} {
				if strings.Contains(s, secret) {
					t.Fatalf("leaked %s: %s", secret, s)
				}
			}
		})
	}
}

func TestWorkerLogsOutputLimit(t *testing.T) {
	p, _ := distributedReadOnlyPolicy(t)
	dir := t.TempDir()
	script := "#!/bin/sh\nyes 'ordinary log line' | head -c 200000\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", p.Operations["worker-logs"])
	cmd.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin", "BOOTSTRAP_DIR="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("worker logs: %v", err)
	}
	if len(out) != 65536 {
		t.Fatalf("output length = %d, want 65536", len(out))
	}
}
