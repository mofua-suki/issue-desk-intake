package admincli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/claudeapikey"
)

func keyCLI(t *testing.T) ([]string, *claudeapikey.Store) {
	t.Helper()
	args, _ := fixture(t)
	dir := filepath.Join(filepath.Dir(args[1]), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return args, claudeapikey.New(filepath.Join(dir, "gitea-intake-token"))
}
func runKey(args []string, action, input string) (string, string, error) {
	var out, notice bytes.Buffer
	err := Run(context.Background(), append(append([]string{}, args...), "claude-api-key", action), strings.NewReader(input), &out, &notice)
	return out.String(), notice.String(), err
}
func TestClaudeAPIKeyAdminLifecycle(t *testing.T) {
	args, s := keyCLI(t)
	key := "sk-ant-" + strings.Repeat("a", 24)
	r, err := s.Read()
	if err != nil || r.State != "unregistered" {
		t.Fatal(r, err)
	}
	for i := 0; i < 2; i++ {
		_, _, err = runKey(args, "set", key+"\n")
		if err != nil {
			t.Fatal(err)
		}
		next, e := s.Read()
		if e != nil || next.State != "registered" || *next.Key != key || next.Version == nil || r.Version != nil && *r.Version == *next.Version {
			t.Fatal(next, e)
		}
		r = next
	}
	replacement := "sk-ant-" + strings.Repeat("b", 24)
	if _, _, err = runKey(args, "set", replacement); err != nil {
		t.Fatal(err)
	}
	changed, err := s.Read()
	if err != nil || changed.Key == nil || *changed.Key != replacement || *changed.Version == *r.Version {
		t.Fatal("replacement failed", err)
	}
	r = changed
	key = replacement
	info, err := os.Stat(filepath.Join(s.Dir, claudeapikey.Filename))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	for _, invalid := range []string{"bad", strings.Repeat("x", 4097)} {
		_, _, err = runKey(args, "set", invalid)
		if err == nil {
			t.Fatal("accepted invalid input")
		}
		next, e := s.Read()
		if e != nil || *next.Version != *r.Version || *next.Key != key {
			t.Fatal(next, e)
		}
	}
	_, _, err = runKey(args, "clear", "")
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.Read()
	if err != nil || next.State != "cleared" || next.Key != nil || *next.Version == *r.Version {
		t.Fatal(next, err)
	}
}
func TestClaudeAPIKeyAdminNoLeak(t *testing.T) {
	args, s := keyCLI(t)
	key := "sk-ant-" + strings.Repeat("z", 24)
	for _, tc := range []struct{ action, input string }{{"set", key}, {"set", key + "!"}, {"clear", ""}} {
		out, notice, err := runKey(args, tc.action, tc.input)
		message := out + notice
		if err != nil {
			message += err.Error()
		}
		if strings.Contains(message, key) {
			t.Fatal("key leaked")
		}
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, claudeapikey.Filename))
	if err != nil || bytes.Contains(data, []byte(key)) {
		t.Fatal("clear retained key", err)
	}
}
