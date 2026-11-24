package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"time"
	"unicode/utf16"

	"issue-desk-intake/server/internal/contract"
)

type NewJob struct {
	Repo        string
	Issue       int64
	Title       string
	Kind        string
	Priority    int
	FiledAt     *time.Time
	Scope       []string
	ScopeSource string
	Provider    *string
	Tier        *string
	Status      string
	HoldReason  *string
	Branch      *string
	PR          *int64
	View        map[string]any
	DependsOn   []int64
}
type CreatedJob struct {
	Job     *Job
	Created bool
}

var repositoryName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func (s *Store) ByIssue(ctx context.Context, repo string, issue int64) (*Job, error) {
	job, err := scanJob(s.db.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE repo=$1 AND issue=$2", repo, issue))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return job, err
}
func (s *Store) Create(ctx context.Context, v NewJob) (CreatedJob, error) {
	var result CreatedJob
	v, deps, raw, err := normalizeNewJob(v)
	if err != nil {
		return result, err
	}
	invalid := func(message string) error { return contract.NewProblem(400, message, "") }
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,title,kind,priority,filed_at,scope,scope_source,provider,tier,status,hold_reason,branch,pr,view,updated_at) VALUES($1,$2,$3,$4,$5,coalesce($6::timestamptz,$16::timestamptz),$7::text[],$8,$9,$10,$11,$12,$13,$14,$15::jsonb,$16) ON CONFLICT(repo,issue) DO NOTHING RETURNING id`, v.Repo, v.Issue, v.Title, v.Kind, v.Priority, v.FiledAt, v.Scope, v.ScopeSource, v.Provider, v.Tier, v.Status, v.HoldReason, v.Branch, v.PR, string(raw), s.options.Now()).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			job, err := scanJob(tx.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE repo=$1 AND issue=$2", v.Repo, v.Issue))
			result.Job = job
			return err
		}
		if err != nil {
			return err
		}
		for _, dep := range deps {
			var exists bool
			if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM jobs WHERE id=$1)", dep).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return invalid("指定した先行ジョブがありません。")
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO job_deps(job_id,depends_on) VALUES($1,$2)", id, dep); err != nil {
				return err
			}
		}
		if err := addEvent(ctx, tx, id, "", "state", map[string]any{"to": v.Status, "created": true}, s.options.Now()); err != nil {
			return err
		}
		result.Job, err = readJob(ctx, tx, id, false)
		result.Created = err == nil
		return err
	})
	return result, err
}

// normalizeNewJob は新しいジョブの指定を検査して既定値を補い、重複を除いた先行ジョブと保存する表示データを返す。
func normalizeNewJob(v NewJob) (NewJob, []int64, []byte, error) {
	invalid := func(message string) error { return contract.NewProblem(400, message, "") }
	if !repositoryName.MatchString(v.Repo) || v.Issue < 1 || v.Issue > maxSafeInteger {
		return v, nil, nil, invalid("ジョブのリポジトリとIssue番号が不正です。")
	}
	if v.Kind == "" {
		v.Kind = "normal"
	}
	if v.Kind == "investigation" || v.View["workType"] == "investigation" {
		return v, nil, nil, contract.NewProblem(409, "調査Issueの実行はできません。", "investigation_disabled")
	}
	if !oneOf(v.Kind, "normal", "followup", "stall_fix") {
		return v, nil, nil, invalid("ジョブの種類が不正です。")
	}
	if v.Priority != 0 && v.Priority != 1 {
		return v, nil, nil, invalid("priorityは0か1で指定してください。")
	}
	if v.ScopeSource == "" {
		if v.Scope == nil {
			v.ScopeSource = "estimated"
		} else {
			v.ScopeSource = "declared"
		}
	}
	if !oneOf(v.ScopeSource, "declared", "estimated", "widened") {
		return v, nil, nil, invalid("範囲の記録元が不正です。")
	}
	if v.Scope == nil {
		v.Scope = []string{"*"}
	}
	if len(v.Scope) > 200 {
		return v, nil, nil, invalid("範囲は200件以内で指定してください。")
	}
	if len(v.Scope) > 0 {
		var err error
		v.Scope, err = NormalizeScope(v.Scope)
		if err != nil {
			return v, nil, nil, err
		}
	}
	if v.Status == "" {
		v.Status = "queued"
	}
	if !oneOf(v.Status, "queued", "held", "failed", "stopped") {
		return v, nil, nil, invalid("新しいジョブの状態が不正です。")
	}
	if v.Status == "held" {
		if v.HoldReason == nil || !oneOf(*v.HoldReason, "review_held", "question", "deletions", "scope_wait") {
			return v, nil, nil, invalid("保留理由が不正です。")
		}
	} else {
		v.HoldReason = nil
	}
	deps := []int64{}
	seen := map[int64]bool{}
	for _, id := range v.DependsOn {
		if id < 1 || id > maxSafeInteger {
			return v, nil, nil, invalid("先行ジョブのIDが不正です。")
		}
		if !seen[id] {
			deps = append(deps, id)
			seen[id] = true
		}
	}
	view := map[string]any{"reviewHolds": 0}
	for key, value := range v.View {
		view[key] = value
	}
	raw, err := json.Marshal(view)
	if err != nil {
		return v, nil, nil, invalid("ジョブの表示データが不正です。")
	}
	if len(utf16.Encode([]rune(string(raw)))) > 32*1024 {
		return v, nil, nil, invalid("ジョブの表示データが上限を超えています。")
	}
	title := []rune(v.Title)
	if len(title) > 500 {
		v.Title = string(title[:500])
	}
	return v, deps, raw, nil
}

func (s *Store) AddDependencies(ctx context.Context, id int64, deps []int64) (int, error) {
	added := 0
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if err := lockDependencyGraph(ctx, tx); err != nil {
			return err
		}
		job, err := readJob(ctx, tx, id, true)
		if err != nil {
			return err
		}
		explicit := []int64{}
		for _, dep := range deps {
			if dep != id && dep > 0 {
				explicit = append(explicit, dep)
			}
		}
		added, err = addExplicitDependencies(ctx, tx, job, explicit)
		return err
	})
	return added, err
}
