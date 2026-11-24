package hostd

import (
	"context"
	"database/sql"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"strconv"
	"strings"
)

func intString(v int64) string { return strconv.FormatInt(v, 10) }
func (s *Service) DeployNext(ctx context.Context, host string) (any, error) {
	targets, _, err := s.Targets()
	if err != nil {
		return nil, err
	}
	allowed := []string{}
	for repo, target := range targets {
		if target == host {
			allowed = append(allowed, repo)
		}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SELECT host FROM host_image_tokens WHERE host=$1 FOR UPDATE", host); err != nil {
		return nil, err
	}
	var id int64
	var repo, commit string
	err = tx.QueryRowContext(ctx, "SELECT id,repo,commit_sha FROM app_deployments WHERE deploy_host=$1 AND status='running' ORDER BY id LIMIT 1", host).Scan(&id, &repo, &commit)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, "UPDATE app_deployments SET status='running',started_at=now() WHERE id=(SELECT id FROM app_deployments WHERE deploy_host=$1 AND status='queued' AND repo=ANY($2::text[]) ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED) RETURNING id,repo,commit_sha", host, allowed).Scan(&id, &repo, &commit)
	}
	var job any
	if err == nil {
		job = map[string]any{"id": id, "repo": repo, "commit": commit}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{"job": job}, nil
}
func (s *Service) DeployResult(ctx context.Context, host string, body map[string]any) (any, error) {
	id := fleet.Text(body["id"])
	if v, ok := body["id"].(float64); ok && v > 0 && v == float64(int64(v)) {
		id = intString(int64(v))
	}
	if !regexpID(id) || !only(body, "id", "status", "reason", "previous") || !one(body["status"], "deployed", "rolled_back", "failed") || !one(body["reason"], "healthy", "rollback_healthy", "rollback_failed", "not_allowed", "preflight_failed", "interrupted") {
		return nil, problem(400, "配備結果の形が違います。")
	}
	result := map[string]any{"reason": body["reason"]}
	if previous, exists := body["previous"]; exists {
		if !shaPattern.MatchString(fleet.Text(previous)) {
			return nil, problem(400, "以前のコミットの形が違います。")
		}
		result["previous"] = previous
	}
	var savedID int64
	var status string
	err := s.DB.QueryRowContext(ctx, "UPDATE app_deployments SET status=$3,result=$4::jsonb,finished_at=coalesce(finished_at,now()) WHERE id=$1 AND deploy_host=$2 AND(status='running' OR(status=$3 AND result=$4::jsonb)) RETURNING id,status", id, host, body["status"], encode(result)).Scan(&savedID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, problem(409, "配備ジョブがないか結果が一致しません。")
	}
	return map[string]any{"id": savedID, "status": status}, err
}
func regexpID(v string) bool {
	if v == "" || v[0] == '0' {
		return false
	}
	for _, r := range v {
		if r < '0' || r > '9' {
			return false
		}
	}
	_, err := strconv.ParseInt(v, 10, 64)
	return err == nil
}
func (s *Service) readerRepos(host string) ([]string, error) {
	a, err := s.Assignments()
	if err != nil {
		return nil, err
	}
	out := []string{}
	for repo, hosts := range fleet.Object(a.Output["readerHosts"]) {
		for _, name := range fleet.Strings(fleet.Clone(hosts)) {
			if name == host {
				out = append(out, repo)
			}
		}
	}
	return out, nil
}
func (s *Service) ReaderNext(ctx context.Context, host string) (any, error) {
	allowed, err := s.readerRepos(host)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT DISTINCT ON(repo)repo,commit_sha FROM app_deployments WHERE status='deployed' AND repo=ANY($1::text[]) ORDER BY repo,id DESC", allowed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []any{}
	for rows.Next() {
		var repo, commit string
		if err = rows.Scan(&repo, &commit); err != nil {
			return nil, err
		}
		jobs = append(jobs, map[string]any{"repo": repo, "commit": commit})
	}
	return map[string]any{"jobs": jobs}, rows.Err()
}
func (s *Service) ReaderResult(ctx context.Context, host string, body map[string]any) (any, error) {
	reasons := map[string][]string{"synced": {"healthy"}, "skipped": {"dirty", "detached", "diverged", "ahead", "not_on_default", "not_allowed"}, "rolled_back": {"rollback_healthy", "rollback_check_failed"}, "failed": {"rollback_failed", "preflight_failed", "interrupted"}}
	status := fleet.Text(body["status"])
	commit := strings.ToLower(fleet.Text(body["commit"]))
	repo := fleet.Text(body["repo"])
	if !only(body, "repo", "commit", "status", "reason", "previous") || !fleet.ValidRepo(repo) || !shaPattern.MatchString(commit) || !one(body["reason"], reasons[status]...) {
		return nil, problem(400, "読み取り結果の形が違います。")
	}
	result := map[string]any{"reason": body["reason"]}
	if previous, exists := body["previous"]; exists {
		if !shaPattern.MatchString(strings.ToLower(fleet.Text(previous))) {
			return nil, problem(400, "以前のコミットの形が違います。")
		}
		result["previous"] = previous
	}
	allowed, err := s.readerRepos(host)
	if err != nil {
		return nil, err
	}
	if !one(repo, allowed...) {
		return nil, problem(409, "読み取り担当ではありません。")
	}
	var savedHost, savedRepo, savedStatus string
	err = s.DB.QueryRowContext(ctx, `INSERT INTO app_reader_syncs(host,repo,commit_sha,status,result,reported_at) SELECT $1,$2,$3,$4,$5::jsonb,now() WHERE EXISTS(SELECT 1 FROM app_deployments WHERE repo=$2 AND commit_sha=$3 AND status='deployed') ON CONFLICT(host,repo)DO UPDATE SET commit_sha=excluded.commit_sha,status=excluded.status,result=excluded.result,reported_at=excluded.reported_at RETURNING host,repo,status`, host, repo, commit, status, encode(result)).Scan(&savedHost, &savedRepo, &savedStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, problem(409, "配備済みのコミットではありません。")
	}
	return map[string]any{"host": savedHost, "repo": savedRepo, "status": savedStatus}, err
}
