package startup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func guardGit(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	command := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	command.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	output, e := command.CombinedOutput()
	return string(output), e
}

func mustGuardGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	output, e := guardGit(t, dir, args...)
	if e != nil {
		t.Fatal(args, e, output)
	}
	return strings.TrimSpace(output)
}

// A real clone with the real hooks: the fake git of the other NAS tests cannot show refusal.
func guardedCheckout(t *testing.T) string {
	t.Helper()
	repo, e := filepath.Abs(filepath.Join("..", "..", ".."))
	if e != nil {
		t.Fatal(e)
	}
	root := t.TempDir()
	origin, dir := filepath.Join(root, "origin.git"), filepath.Join(root, "deploy")
	mustGuardGit(t, root, "init", "-q", "--bare", "-b", "trunk", origin)
	mustGuardGit(t, root, "clone", "-q", origin, dir)
	mustGuardGit(t, dir, "switch", "-q", "-c", "trunk")
	for _, file := range []string{"update-checkout-guard.sh", "git-hooks/reference-transaction", "git-hooks/post-checkout"} {
		raw, e := os.ReadFile(filepath.Join(repo, "deploy", "nas", file))
		if e != nil {
			t.Fatal(e)
		}
		target := filepath.Join(dir, "deploy", "nas", file)
		if e := os.MkdirAll(filepath.Dir(target), 0o755); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(target, raw, 0o644); e != nil {
			t.Fatal(e)
		}
	}
	mustGuardGit(t, dir, "add", ".")
	mustGuardGit(t, dir, "commit", "-q", "-m", "base")
	mustGuardGit(t, dir, "push", "-q", "origin", "trunk")
	if body, code := runGuard(t, dir, "checkout_guard_install"); code != nil {
		t.Fatal(code, body)
	}
	return dir
}

func runGuard(t *testing.T, dir, function string) (string, error) {
	t.Helper()
	script := `DIR=$1; say() { printf '%s\n' "$*"; }; . "$DIR/deploy/nas/update-checkout-guard.sh"; ` + function
	output, e := exec.CommandContext(t.Context(), "sh", "-c", script, "sh", dir).CombinedOutput()
	return string(output), e
}

func TestDeployCheckoutRefusesLocalCommitsAndBranches(t *testing.T) {
	dir := guardedCheckout(t)
	head := mustGuardGit(t, dir, "rev-parse", "HEAD")
	for name, args := range map[string][]string{
		"commit":      {"commit", "-q", "--allow-empty", "-m", "local"},
		"amend":       {"commit", "-q", "--amend", "--allow-empty", "-m", "local"},
		"new branch":  {"switch", "-q", "-c", "chat/local"},
		"branch only": {"branch", "chat/local"},
		"detach":      {"switch", "-q", "--detach"},
	} {
		output, e := guardGit(t, dir, args...)
		if e == nil {
			t.Fatal(name, "断られない", output)
		}
		if !strings.Contains(output, "NASの配布用") {
			t.Fatal(name, "理由が出ない", output)
		}
	}
	if got := mustGuardGit(t, dir, "rev-parse", "HEAD"); got != head {
		t.Fatal("HEADが動いた", got)
	}
	if got := mustGuardGit(t, dir, "branch", "--list", "chat/local"); got != "" {
		t.Fatal("枝ができた", got)
	}
	// The update's own work must keep passing through the same hooks.
	mustGuardGit(t, dir, "fetch", "-q", "origin", "trunk")
	mustGuardGit(t, dir, "gc", "-q")
}

func TestDeployCheckoutUpdateMayFastForward(t *testing.T) {
	dir := guardedCheckout(t)
	other := filepath.Join(t.TempDir(), "other")
	origin := mustGuardGit(t, dir, "remote", "get-url", "origin")
	mustGuardGit(t, filepath.Dir(other), "clone", "-q", origin, other)
	mustGuardGit(t, other, "commit", "-q", "--allow-empty", "-m", "next")
	mustGuardGit(t, other, "push", "-q", "origin", "trunk")
	want := mustGuardGit(t, other, "rev-parse", "HEAD")
	mustGuardGit(t, dir, "fetch", "-q", "origin", "trunk")
	if _, e := guardGit(t, dir, "merge", "-q", "--ff-only", "origin/trunk"); e == nil {
		t.Fatal("更新の印なしで進んだ")
	}
	t.Setenv("ISSUE_DESK_DEPLOY_UPDATE", "1")
	mustGuardGit(t, dir, "merge", "-q", "--ff-only", "origin/trunk")
	if got := mustGuardGit(t, dir, "rev-parse", "HEAD"); got != want {
		t.Fatal("進まない", got)
	}
}

// Reproduces 2026-11-03: a commit on a chat branch left in the deploy checkout before the hooks existed.
func TestDeployCheckoutReturnsToTrunkAndKeepsTheBranch(t *testing.T) {
	dir := guardedCheckout(t)
	t.Setenv("ISSUE_DESK_DEPLOY_UPDATE", "1")
	mustGuardGit(t, dir, "switch", "-q", "-c", "chat/local")
	mustGuardGit(t, dir, "commit", "-q", "--allow-empty", "-m", "local")
	kept := mustGuardGit(t, dir, "rev-parse", "HEAD")
	os.Unsetenv("ISSUE_DESK_DEPLOY_UPDATE")
	body, e := runGuard(t, dir, "checkout_guard_restore")
	if e != nil || !strings.Contains(body, "[update:recovered]") {
		t.Fatal(e, body)
	}
	if got := mustGuardGit(t, dir, "symbolic-ref", "--short", "HEAD"); got != "trunk" {
		t.Fatal("trunkへ戻らない", got)
	}
	if got := mustGuardGit(t, dir, "rev-parse", "chat/local"); got != kept {
		t.Fatal("枝のコミットを失った", got)
	}
}

func TestDeployCheckoutKeepsUnsavedChangesAndStops(t *testing.T) {
	dir := guardedCheckout(t)
	t.Setenv("ISSUE_DESK_DEPLOY_UPDATE", "1")
	mustGuardGit(t, dir, "switch", "-q", "-c", "chat/local")
	os.Unsetenv("ISSUE_DESK_DEPLOY_UPDATE")
	file := filepath.Join(dir, "deploy", "nas", "update-checkout-guard.sh")
	raw, e := os.ReadFile(file)
	if e != nil {
		t.Fatal(e)
	}
	edited := append(raw, []byte("# unsaved\n")...)
	if e := os.WriteFile(file, edited, 0o644); e != nil {
		t.Fatal(e)
	}
	body, failure := runGuard(t, dir, "checkout_guard_restore")
	if failure == nil || !strings.Contains(body, "未保存の変更") {
		t.Fatal(failure, body)
	}
	if got, _ := os.ReadFile(file); string(got) != string(edited) {
		t.Fatal("未保存の変更を失った")
	}
	if got := mustGuardGit(t, dir, "symbolic-ref", "--short", "HEAD"); got != "chat/local" {
		t.Fatal("枝が変わった", got)
	}
}

func TestDeployCheckoutDropsLocalTrunkCommitsOnlyAfterPreserving(t *testing.T) {
	dir := guardedCheckout(t)
	t.Setenv("ISSUE_DESK_DEPLOY_UPDATE", "1")
	mustGuardGit(t, dir, "commit", "-q", "--allow-empty", "-m", "local")
	kept := mustGuardGit(t, dir, "rev-parse", "HEAD")
	mustGuardGit(t, dir, "fetch", "-q", "origin", "trunk")
	os.Unsetenv("ISSUE_DESK_DEPLOY_UPDATE")
	if body, e := runGuard(t, dir, "checkout_guard_drop_local_commits"); e != nil {
		t.Fatal(e, body)
	}
	if got, want := mustGuardGit(t, dir, "rev-parse", "HEAD"), mustGuardGit(t, dir, "rev-parse", "origin/trunk"); got != want {
		t.Fatal("配布元に合わない", got, want)
	}
	if !strings.Contains(mustGuardGit(t, dir, "for-each-ref", "--format=%(objectname)", "refs/preserved/"), kept) {
		t.Fatal("手元のコミットを失った")
	}
}

func TestUpdateScriptGuardsBeforeCheckingTheBranch(t *testing.T) {
	raw, e := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "nas", "update.sh"))
	if e != nil {
		t.Fatal(e)
	}
	text := string(raw)
	guard, branch := strings.Index(text, "checkout_guard_restore"), strings.Index(text, "current_branch=")
	if guard < 0 || branch < 0 || guard > branch {
		t.Fatal("trunkの確認より前に戻していない")
	}
	if !strings.Contains(text, `ISSUE_DESK_DEPLOY_UPDATE=1 git -C "$DIR" merge --ff-only origin/trunk`) {
		t.Fatal("更新の早送りがhookに断られる")
	}
}
