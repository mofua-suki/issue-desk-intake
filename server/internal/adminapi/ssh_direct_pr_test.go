package adminapi

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestSSHDirectPRVerificationApproval(t *testing.T) {
	f := repairFixture(t)
	repo := "owner/repo"
	policy := map[string]any{
		"ttlMinutes": 5,
		"signer":     map[string]any{"token": "signer"},
		"hosts": map[string]any{
			"test-host": map[string]any{"principal": "issue-desk-repair-test-host", "user": "test", "machine": "test-host.example.net"},
			"second":    map[string]any{"principal": "issue-desk-repair-second", "user": "test", "machine": "second.example.net"},
		},
		"operations":           map[string]any{"verify": "/usr/bin/true", "verify-intake": "/usr/bin/true"},
		"directPRVerification": map[string]any{repo: map[string]any{"requester": "delegate", "host": "test-host"}},
	}
	save := func() {
		raw, err := json.Marshal(policy)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.policy, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	save()
	base := strings.Repeat("b", 40)
	rawCommit := "tree " + strings.Repeat("a", 40) + "\nparent " + base + "\n\nsubject\n"
	rawObject := fmt.Sprintf("commit %d\x00%s", len(rawCommit), rawCommit)
	head := fmt.Sprintf("%x", sha1.Sum([]byte(rawObject)))
	f.options.DirectPRBase = func(context.Context, string) (string, error) { return base, nil }
	f.restart()

	for _, tc := range []struct {
		name, credential, host, operation, repository, sha string
		code                                               int
		approved                                           bool
	}{
		{"direct-pr", "delegate", "test-host", "verify", repo, head, 201, true},
		{"no-purpose", "delegate", "test-host", "verify", "", "", 201, false},
		{"wrong-repository", "delegate", "test-host", "verify", "owner/other", head, 403, false},
		{"wrong-host", "delegate", "second", "verify", repo, head, 403, false},
		{"non-delegated", "worker", "test-host", "verify", repo, head, 403, false},
		{"temporary-key", "cli", "test-host", "verify", repo, head, 403, false},
		{"wrong-operation", "delegate", "test-host", "verify-intake", repo, head, 400, false},
		{"malformed-head", "delegate", "test-host", "verify", repo, "main", 400, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := ticketInput(tc.operation)
			input["host"] = tc.host
			if tc.repository != "" {
				input["directPR"] = map[string]any{"repo": tc.repository, "head": tc.sha, "branch": "chat/proof", "base": base, "commits": []any{base64.StdEncoding.EncodeToString([]byte(rawCommit))}}
			}
			code, value := f.call("POST", "/api/ssh-tickets", tc.credential, input)
			status(t, code, tc.code, value)
			if code != 201 {
				return
			}
			ticket := obj(value)
			if tc.approved {
				if ticket["status"] != "approved" || ticket["approver"] != "policy:direct-pr:"+repo || !strings.Contains(str(ticket["evidence"]), head) {
					t.Fatal(value)
				}
				id := str(ticket["id"])
				delete(policy, "directPRVerification")
				save()
				code, value = f.call("POST", "/api/ssh-tickets/"+id+"/issue", "signer", map[string]any{})
				status(t, code, 409, value)
				policy["directPRVerification"] = map[string]any{repo: map[string]any{"requester": "delegate", "host": "test-host"}}
				save()
				code, value = f.call("POST", "/api/ssh-tickets/"+id+"/issue", "signer", map[string]any{})
				status(t, code, 200, value)
				if obj(value)["forceCommand"] != "ISSUE_DESK_DIRECT_PR_HEAD="+head+" /usr/bin/true" {
					t.Fatal(value)
				}
				f.options.DirectPRBase = func(context.Context, string) (string, error) { return strings.Repeat("f", 40), nil }
				f.restart()
				code, value = f.call("POST", "/api/ssh-tickets", tc.credential, input)
				status(t, code, 409, value)
				f.options.DirectPRBase = func(context.Context, string) (string, error) { return base, nil }
				f.restart()
				code, value = f.call("POST", "/api/ssh-tickets", tc.credential, input)
				status(t, code, 201, value)
				id = str(obj(value)["id"])
				code, value = f.call("POST", "/api/tokens/bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb/delegate", "screen", map[string]any{"delegate": false})
				status(t, code, 200, value)
				code, value = f.call("POST", "/api/ssh-tickets/"+id+"/issue", "signer", map[string]any{})
				status(t, code, 409, value)
				code, value = f.call("POST", "/api/tokens/bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb/delegate", "screen", map[string]any{"delegate": true})
				status(t, code, 200, value)
			} else if ticket["status"] != "requested" || ticket["approver"] != nil {
				t.Fatal(value)
			}
		})
	}
}
