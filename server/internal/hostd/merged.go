package hostd

import (
	"context"
	"database/sql"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
)

func (s *Service) EnqueueDeployment(ctx context.Context, tx *sql.Tx, repo, commit, host string) error {
	if !fleet.ValidRepo(repo) || !shaPattern.MatchString(commit) || !fleet.ValidHost(host) {
		return problem(400, "配備ジョブの形が違います。")
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO app_deploy_heads(repo,commit_sha,deploy_host)VALUES($1,$2,$3)ON CONFLICT(repo)DO NOTHING", repo, commit, host); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "SELECT repo FROM app_deploy_heads WHERE repo=$1 FOR UPDATE", repo); err != nil {
		return err
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM app_deployments WHERE repo=$1 AND commit_sha=$2)", repo, commit).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE app_deployments SET status='failed',result='{"reason":"superseded"}'::jsonb,finished_at=now() WHERE repo=$1 AND status='queued'`, repo); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE app_deploy_heads SET commit_sha=$2,deploy_host=$3 WHERE repo=$1", repo, commit, host); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO app_deployments(repo,commit_sha,deploy_host)VALUES($1,$2,$3)", repo, commit, host)
	return err
}
func (s *Service) Merged(ctx context.Context, tx *sql.Tx, job *jobs.Job, result map[string]any) error {
	targets, checks, err := s.Targets()
	if err != nil {
		return err
	}
	config := checks[job.Repo]
	if config == nil && targets[job.Repo] == "" {
		return nil
	}
	commit := fleet.Text(result["mergeCommit"])
	if !shaPattern.MatchString(commit) {
		return problem(400, "実地確認にはマージしたコミットが必要です。")
	}
	if host := targets[job.Repo]; host != "" {
		if err = s.EnqueueDeployment(ctx, tx, job.Repo, commit, host); err != nil {
			return err
		}
	}
	if config == nil {
		return nil
	}
	var id int64
	if err = tx.QueryRowContext(ctx, "INSERT INTO live_checks(job_id,generation,repo,commit_sha,host,config)VALUES($1,$2,$3,$4,$5,$6::jsonb)ON CONFLICT(job_id,generation)DO UPDATE SET job_id=excluded.job_id RETURNING id", job.ID, job.Generation, job.Repo, commit, config["host"], encode(config)).Scan(&id); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE jobs SET status='held',hold_reason='question',view=view||$2::jsonb WHERE id=$1", job.ID, encode(map[string]any{"liveCheck": map[string]any{"id": id, "status": "pending", "commit": commit}}))
	return err
}
