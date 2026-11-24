package startup

import (
	"context"
	"io"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/hostd"
	"issue-desk-intake/server/internal/jobs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type deployPollTransport func(*http.Request) (*http.Response, error)

func (f deployPollTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestPostgresDefaultBranchPollAndMergeShareIdempotentDeploymentPath(t *testing.T) {
	ctx := context.Background()
	db := appPostgres(t)
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if e := os.WriteFile(token, []byte("fixture-token"), 0600); e != nil {
		t.Fatal(e)
	}
	commit := strings.Repeat("a", 40)
	branch := "main"
	calls := []string{}
	api, e := gitea.New("http://gitea.invalid", token, deployPollTransport(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.Method+" "+r.URL.EscapedPath())
		body := `{"default_branch":"` + branch + `"}`
		if strings.Contains(r.URL.Path, "/branches/") {
			body = `{"commit":{"id":"` + commit + `"}}`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}))
	if e != nil {
		t.Fatal(e)
	}
	repos := []any{}
	deploy := map[string]any{}
	for _, repo := range []string{"owner/app", "mofua-suki/issue-desk-worker", "mofua-suki/issue-desk-intake"} {
		repos = append(repos, map[string]any{"name": repo, "autoDeploy": true})
		deploy[repo] = map[string]any{"deployHost": "nas"}
	}
	repos = append(repos, map[string]any{"name": "owner/no-auto", "autoDeploy": false})
	deploy["owner/no-auto"] = map[string]any{"deployHost": "nas"}
	fleetFile, assignFile := filepath.Join(dir, "fleet.json"), filepath.Join(dir, "assignments.json")
	writeConfig(t, fleetFile, map[string]any{"version": 1, "defaults": map[string]any{"repositories": repos}, "hosts": map[string]any{"nas": map[string]any{}}})
	ranked := map[string]any{}
	for repo := range deploy {
		ranked[repo] = map[string]any{"rank": 1}
	}
	writeConfig(t, assignFile, map[string]any{"pollSeconds": 300, "hosts": map[string]any{"nas": map[string]any{"maxImplementations": 1, "repositories": ranked}}, "repositories": deploy})
	a := App{DB: db, Gitea: api, Fleet: &fleet.Service{DB: db, FleetPath: fleetFile, AssignmentsPath: assignFile}}
	for i := 0; i < 2; i++ {
		if e = a.pollAppDeployments(ctx); e != nil {
			t.Fatal(e)
		}
	}
	var count int
	if e = db.QueryRowContext(ctx, "SELECT count(*)FROM app_deployments").Scan(&count); e != nil || count != 1 {
		t.Fatal(count, e)
	}
	for _, call := range calls {
		if !strings.HasPrefix(call, "GET /api/v1/repos/owner/app") {
			t.Fatal("Self更新または許可外アプリを監視", calls)
		}
	}
	// A manual merge arrives only through the default-branch observation.
	commit = strings.Repeat("b", 40)
	branch = "release/ready"
	if e = a.pollAppDeployments(ctx); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(calls[len(calls)-1], "release%2Fready") {
		t.Fatal("ブランチを正しく符号化していない", calls)
	}
	var status string
	if e = db.QueryRowContext(ctx, "SELECT status FROM app_deployments WHERE commit_sha=$1", strings.Repeat("a", 40)).Scan(&status); e != nil || status != "failed" {
		t.Fatal(status, e)
	}
	// Auto-deploy must also enqueue when no liveCheck is configured.
	commit = strings.Repeat("c", 40)
	service := hostd.Service{DB: db, Targets: a.Fleet.Targets}
	tx, e := db.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if e = service.Merged(ctx, tx, &jobs.Job{Repo: "owner/app"}, map[string]any{"mergeCommit": commit}); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	if e = db.QueryRowContext(ctx, "SELECT count(*)FROM app_deployments WHERE repo='owner/app'AND commit_sha=$1 AND status='queued'", commit).Scan(&count); e != nil || count != 1 {
		t.Fatal(count, e)
	}
}
