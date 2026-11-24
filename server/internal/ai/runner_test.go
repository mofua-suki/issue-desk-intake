package ai

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testRequest(t *testing.T, provider string) Request {
	t.Helper()
	dir := t.TempDir()
	schema := filepath.Join(dir, "schema.json")
	if err := os.WriteFile(schema, []byte(`{"type":"object","required":["ok"],"properties":{"ok":{"type":"boolean"}},"additionalProperties":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	return Request{Provider: provider, Model: "fixture", Effort: "low", Workspace: dir, SchemaFile: schema, Input: "機密の本文"}
}

func TestSchemaNamesCacheReusesAndSeparatesDocuments(t *testing.T) {
	// This signature prevents callers from changing the cached property set.
	var lookup func([]byte) func(string) bool = schemaNames
	first := []byte(`{"properties":{"title":{"type":"string"}}}`)
	second := []byte(`{"properties":{"body":{"type":"string"}}}`)
	a := lookup(first)
	b := lookup(first)
	if a == nil || b == nil || !a("title") || !b("title") || a("body") || b("body") {
		t.Fatal("same schema returned incorrect properties")
	}
	if c := lookup(second); c == nil || !c("body") || c("title") {
		t.Fatal("different schema reused the wrong properties")
	}
	if lookup([]byte(`{`)) != nil {
		t.Fatal("invalid schema must not produce names")
	}
}
func TestCodexAnswerAndUsage(t *testing.T) {
	request := testRequest(t, "codex")
	observed := 0
	runner := Runner{Observe: func(r Request, u Usage, d time.Duration, ok bool) {
		observed++
		if r.Input != "" || !ok || u.InputTokens == nil || *u.InputTokens != 5 {
			t.Fatalf("invalid observation: %+v %+v %v", r, u, ok)
		}
	}, Execute: func(ctx context.Context, r ProcessRequest) (ProcessResult, error) {
		if r.Arguments[0] == "login" {
			return ProcessResult{Stdout: []byte("Logged in using ChatGPT")}, nil
		}
		output := ""
		for i, arg := range r.Arguments {
			if arg == "-o" {
				output = r.Arguments[i+1]
			}
		}
		if !strings.Contains(strings.Join(r.Arguments, " "), "--ignore-user-config") || output == "" {
			t.Fatal("missing controller options")
		}
		if err := os.WriteFile(output, []byte(`{"ok":true}`), 0600); err != nil {
			t.Fatal(err)
		}
		return ProcessResult{Stdout: []byte(`{"type":"turn.completed","usage":{"input_tokens":5,"cached_input_tokens":2,"output_tokens":1}}`)}, nil
	}}
	answer, err := runner.Call(context.Background(), request)
	if err != nil || answer.Value["ok"] != true || observed != 1 {
		t.Fatalf("%+v %v %d", answer, err, observed)
	}
	entries, _ := os.ReadDir(request.Workspace)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".answer-") {
			t.Fatal("temporary output remains")
		}
	}
}
func TestInvalidAnswerStillRecordsUsage(t *testing.T) {
	request := testRequest(t, "claude")
	observed := 0
	calls := 0
	runner := Runner{Observe: func(r Request, u Usage, d time.Duration, ok bool) {
		observed++
		if ok || u.CostUSD == nil || *u.CostUSD != 0.2 {
			t.Fatal("invalid failure measurement")
		}
	}, Execute: func(ctx context.Context, r ProcessRequest) (ProcessResult, error) {
		calls++
		if r.Arguments[0] == "auth" {
			return ProcessResult{Stdout: []byte(`{"loggedIn":true,"apiProvider":"firstParty","authMethod":"oauth_token"}`)}, nil
		}
		return ProcessResult{Stdout: []byte(`{"type":"result","structured_output":{"ok":"wrong"},"usage":{"input_tokens":1},"total_cost_usd":0.2}`)}, nil
	}}
	_, err := runner.Call(context.Background(), request)
	var failure *Failure
	if !errors.As(err, &failure) || failure.Field != "ok" || observed != 1 || calls != 2 {
		t.Fatalf("validation/observation: %v %d %d", err, observed, calls)
	}
}
func TestClaudeRejectsNonSubscriptionBeforeSending(t *testing.T) {
	request := testRequest(t, "claude")
	calls := 0
	runner := Runner{Execute: func(ctx context.Context, r ProcessRequest) (ProcessResult, error) {
		calls++
		if r.Input != "" {
			t.Fatal("input sent before auth")
		}
		return ProcessResult{Stdout: []byte(`{"loggedIn":true,"apiProvider":"firstParty","authMethod":"API_KEY"}`)}, nil
	}}
	if _, err := runner.Call(context.Background(), request); err == nil || calls != 1 {
		t.Fatal("non-subscription accepted")
	}
	request.Tools = "Bash"
	if _, err := runner.Call(context.Background(), request); err == nil || calls != 1 {
		t.Fatal("write tool accepted")
	}
}
func TestUsageCountsMissingAndRepeatedMessages(t *testing.T) {
	data := []byte("{\"type\":\"assistant\",\"message\":{\"id\":\"same\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n{\"type\":\"assistant\",\"message\":{\"id\":\"same\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n")
	u := Measure("claude", data)
	if u.InputTokens == nil || *u.InputTokens != 3 || u.CacheReadTokens != nil || u.Partial == nil || !*u.Partial {
		t.Fatalf("%+v", u)
	}
	u = Measure("codex", []byte("{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"cached_input_tokens\":2}}\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":4,\"output_tokens\":2}}"))
	if *u.InputTokens != 7 || u.CacheReadTokens != nil {
		t.Fatalf("%+v", u)
	}
	if Measure("codex", []byte("no events")).InputTokens != nil {
		t.Fatal("missing sample became zero")
	}
}
func TestRunCancellationAndEnvironment(t *testing.T) {
	t.Setenv("GITEA_TOKEN", "secret")
	t.Setenv("ANTHROPIC_API_KEY", "secret")
	for _, entry := range CleanEnvironment() {
		if strings.Contains(entry, "secret") {
			t.Fatal("secret environment inherited")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	result, err := Run(ctx, ProcessRequest{Executable: "/bin/sh", Arguments: []string{"-c", "sleep 30 & wait"}, Directory: t.TempDir()})
	if !errors.Is(err, context.DeadlineExceeded) || result.Code == 0 {
		t.Fatalf("cancel did not terminate: %+v %v", result, err)
	}
}
func TestSchemaRejectsExternalReference(t *testing.T) {
	file := filepath.Join(t.TempDir(), "schema.json")
	data, _ := json.Marshal(map[string]string{"$ref": "https://example.invalid/private.json"})
	os.WriteFile(file, data, 0600)
	if _, err := Compile(file); err == nil {
		t.Fatal("remote schema accepted")
	}
}

func TestClaudeNativeContractAuthenticationShapes(t *testing.T) {
	for _, method := range []string{"claude.ai", "oauth_token", "API_KEY", "unknown", ""} {
		t.Run(method, func(t *testing.T) {
			runner := Runner{}
			check := func(context.Context, ProcessRequest) (ProcessResult, error) {
				raw, _ := json.Marshal(map[string]any{"loggedIn": true, "apiProvider": "firstParty", "authMethod": method})
				return ProcessResult{Stdout: raw}, nil
			}
			err := runner.claudeReady(context.Background(), check, "claude", t.TempDir())
			allowed := method == "claude.ai" || method == "oauth_token"
			if (err == nil) != allowed {
				t.Fatal("契約アカウントの認証形式を誤判定しました")
			}
		})
	}
}

func TestValidationFieldShippedIntakeSchema(t *testing.T) {
	path := filepath.Join("..", "..", "schemas", "intake.schema.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := Compile(path)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"executionPlan":null,"completionPlan":null,"reply":"","basisConflicts":[],"title":123,"body":"","ready":false,"readFiles":[],"reviewedFiles":[],"questions":[],"imageReviews":[],"splitIssues":[],"crossProject":{"reason":"","scopes":[],"approvalMessageId":""},"changeScope":{"known":false,"paths":[],"reason":""},"dependsOnIssues":[],"kind":"implementation","escalate":false,"escalateReason":"","heavyReason":"","workType":"bugfix","estimate":{"lines":0,"files":0}}`)
	if got := validationField(schema, raw, data); got != "title" {
		t.Fatalf("field = %q", got)
	}
}
