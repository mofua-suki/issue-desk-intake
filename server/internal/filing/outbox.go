package filing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"strconv"
	"strings"
	"time"

	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/jobs"
)

func integer(value any) (int64, bool) {
	switch n := value.(type) {
	case json.Number:
		v, e := n.Int64()
		return v, e == nil
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), n == float64(int64(n))
	}
	return 0, false
}
func hasImages(d Record) bool {
	for _, entry := range fleet.Items(d["messages"]) {
		if len(fleet.Items(row(entry)["images"])) > 0 {
			return true
		}
	}
	return false
}

type pending struct {
	raw        map[string]json.RawMessage
	Estimating bool         `json:"estimating,omitempty"`
	Estimate   Record       `json:"estimate,omitempty"`
	Repo       string       `json:"repo"`
	Number     int64        `json:"number"`
	DraftID    string       `json:"draftId"`
	At         string       `json:"at"`
	Job        *jobs.NewJob `json:"preparedJob,omitempty"`
}

func (s *Service) complete(ctx context.Context, d Record, issue map[string]any, job jobs.NewJob) (Record, error) {
	number, _ := integer(issue["number"])
	repo := fleet.Text(d["project"])
	job.Repo, job.Issue, job.Title = repo, number, fleet.Text(issue["title"])
	if job.View == nil {
		job.View = map[string]any{}
	}
	job.View["acceptanceTransferInput"] = transferInput(issue)
	if group, err := s.filingGroup(ctx, d); err != nil {
		return nil, err
	} else if group != nil {
		job.View["filingGroup"] = group
	}
	if created, err := time.Parse(time.RFC3339Nano, fleet.Text(issue["created_at"])); err == nil {
		job.FiledAt = &created
	}
	next, err := s.options.Records.Change(ctx, "drafts", fleet.Text(d["id"]), func(tx *sql.Tx, current Record) (Record, error) {
		revision, _ := integer(current["revision"])
		expected, _ := integer(d["revision"])
		if current == nil || revision != expected || fleet.Text(current["status"]) != "submitting" {
			return nil, fail(409, "起票結果の保存前に下書きが変わりました。")
		}
		next := clone(current)
		next["issue"] = Record{"project": repo, "number": number, "url": s.options.Link(repo, number)}
		next["status"], next["updatedAt"] = "submitted", iso(s.options.Now())
		consultations.SetDraftError(next, nil, nil)
		entry := pending{Repo: repo, Number: number, DraftID: fleet.Text(d["id"]), At: iso(s.options.Now()), Job: &job}
		raw, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, insertIgnoreSQL("job_filings", []string{"key", "json"}, "VALUES(?,?)"), repo+"#"+strconv.FormatInt(number, 10), string(raw)); err != nil {
			return nil, err
		}
		return next, nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(next)
	if err = s.filePendingKey(ctx, repo, repo+"#"+strconv.FormatInt(number, 10)); err != nil {
		return next, err
	}
	return next, nil
}
func (s *Service) FilePending(ctx context.Context) error {
	return s.FilePendingWithLifetime(ctx, ctx)
}

func (s *Service) filePendingKey(ctx context.Context, repo, key string) error {
	return s.filePendingKeyWithLifetime(ctx, ctx, repo, key)
}

func (s *Service) filePendingKeyWithLifetime(ctx, businessCtx context.Context, repo, key string) error {
	unlock := lock(&s.filing, repo)
	defer unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	var raw string
	err := s.options.Records.DB.QueryRowContext(ctx, bindSQL("SELECT json FROM job_filings WHERE key=?"), key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var entry pending
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		return errors.New("起票後の配布記録を読み取れません")
	}
	if entry.Repo != repo {
		return errors.New("起票後の配布記録のリポジトリが一致しません")
	}
	return s.file(businessCtx, key, entry)
}

// FilePendingWithLifetime stops between records while allowing a started record to finish.
func (s *Service) FilePendingWithLifetime(ctx, businessCtx context.Context) error {
	rows, err := s.options.Records.DB.QueryContext(ctx, bindSQL("SELECT key,json FROM job_filings ORDER BY "+rowOrder(false)))
	if err != nil {
		return err
	}
	type item struct {
		key  string
		repo string
	}
	var entries, deferred []item
	waitingRepo := map[string]bool{}
	failures := []error{}
	for rows.Next() {
		var key, raw string
		if err = rows.Scan(&key, &raw); err != nil {
			rows.Close()
			return err
		}
		var value pending
		if err = json.Unmarshal([]byte(raw), &value); err != nil {
			failures = append(failures, errors.New("起票後の配布記録を読み取れません: "+key))
			continue
		}
		// Keep each repository in order, while letting work that already
		// declares a scope leave the outbox before other repositories estimate.
		if waitingRepo[value.Repo] || value.Job == nil || value.Job.Scope == nil {
			waitingRepo[value.Repo] = true
			deferred = append(deferred, item{key, value.Repo})
		} else {
			entries = append(entries, item{key, value.Repo})
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, entry := range append(entries, deferred...) {
		if ctx.Err() != nil {
			return errors.Join(append(failures, ctx.Err())...)
		}
		if err = s.filePendingKeyWithLifetime(ctx, businessCtx, entry.repo, entry.key); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
func (s *Service) file(ctx context.Context, key string, entry pending) error {
	waiting, err := s.groupLinkWaiting(ctx, entry.DraftID)
	if err != nil {
		return err
	}
	if waiting {
		return nil
	}
	if err := s.options.Project(entry.Repo); err != nil {
		return err
	}
	existing, err := s.options.Jobs.ByIssue(ctx, entry.Repo, entry.Number)
	if err != nil {
		return err
	}
	if existing != nil {
		_, err = s.options.Records.DB.ExecContext(ctx, "DELETE FROM job_filings WHERE key=?", key)
		return err
	}
	issue, err := s.options.API.Issue(ctx, entry.Repo, entry.Number)
	if err != nil {
		return err
	}
	switch fleet.Text(issue["state"]) {
	case "closed":
		_, err = s.options.Records.DB.ExecContext(ctx, "DELETE FROM job_filings WHERE key=?", key)
		return err
	case "open":
	default:
		return fail(503, "起票済みIssueの現在の状態を確認できません。")
	}
	job := entry.Job
	if job == nil {
		kind := ""
		if raw := entry.raw["kind"]; len(raw) > 0 {
			_ = json.Unmarshal(raw, &kind)
		}
		parsed, err := JobFromIssue(entry.Repo, issue, kind)
		if err != nil {
			return err
		}
		job = &parsed
		if raw := entry.raw["filingGroup"]; len(raw) > 0 {
			var group Record
			if json.Unmarshal(raw, &group) == nil {
				job.View["filingGroup"] = group
			}
		}
	}
	deps := []int64{}
	for _, raw := range fleet.Items(job.View["dependsOnIssues"]) {
		dependency := row(raw)
		repo := fleet.Text(dependency["repo"])
		if repo == "" {
			repo = entry.Repo
		}
		number, ok := integer(dependency["issue"])
		if !ok || number < 1 {
			return errors.New("保存済みの先行Issueの指定を読み取れません")
		}
		predecessor, err := s.options.Jobs.ByIssue(ctx, repo, number)
		if err != nil {
			return err
		}
		if predecessor == nil {
			return nil
		}
		deps = append(deps, predecessor.ID)
	}
	job.DependsOn = deps
	if job.Scope == nil {
		estimated, err := s.estimateOnce(ctx, key, &entry, job, issue)
		if err != nil {
			return err
		}
		job = estimated
	}
	if _, err := s.options.Jobs.Create(ctx, *job); err != nil {
		return err
	}
	if _, err := s.options.Records.DB.ExecContext(ctx, "DELETE FROM job_filings WHERE key=?", key); err != nil {
		return err
	}
	return nil
}

func (e *pending) UnmarshalJSON(data []byte) error {
	type plain pending
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if err := json.Unmarshal(data, &value.raw); err != nil {
		return err
	}
	*e = pending(value)
	return nil
}
func (e pending) MarshalJSON() ([]byte, error) {
	type plain pending
	data, err := json.Marshal(plain(e))
	if err != nil {
		return nil, err
	}
	own := map[string]json.RawMessage{}
	if err = json.Unmarshal(data, &own); err != nil {
		return nil, err
	}
	merged := map[string]json.RawMessage{}
	for key, value := range e.raw {
		merged[key] = value
	}
	for key, value := range own {
		merged[key] = value
	}
	return json.Marshal(merged)
}
func (s *Service) savePending(ctx context.Context, key string, entry *pending) error {
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = s.options.Records.DB.ExecContext(ctx, bindSQL("UPDATE job_filings SET json=? WHERE key=?"), string(raw), key)
	return err
}
func (s *Service) estimateOnce(ctx context.Context, key string, entry *pending, job *jobs.NewJob, issue map[string]any) (*jobs.NewJob, error) {
	if applied, err := applySavedEstimate(entry, job); err != nil || applied {
		if err != nil {
			return nil, err
		}
		return job, nil
	}
	whole := func(note string) (*jobs.NewJob, error) {
		job.Scope, job.ScopeSource = []string{"*"}, "estimated"
		entry.Estimate = Record{"value": Record{"scope": job.Scope, "scopeSource": job.ScopeSource}, "note": note}
		if err := s.savePending(ctx, key, entry); err != nil {
			return nil, err
		}
		return job, nil
	}
	if entry.Estimating {
		return whole("前回の見積もりが終わらなかったため全体")
	}
	if s.options.EstimateScope == nil {
		return nil, fail(503, "変更範囲の見積もりを利用できません。")
	}
	// Keep the AI load at one filing estimate even when independent
	// repositories can deliver jobs concurrently.
	s.estimating.Lock()
	defer s.estimating.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entry.Estimating = true
	if err := s.savePending(ctx, key, entry); err != nil {
		return nil, err
	}
	estimated, err := s.options.EstimateScope(ctx, entry.Repo, issue)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return whole("見積もりを完了できなかったため全体")
	}
	scope := estimatedScope(estimated)
	if scope["known"] != true {
		return whole("範囲を特定できなかったため全体")
	}
	paths := scopePaths(scope)
	if len(paths) == 0 {
		return whole("範囲の宣言を確認できなかったため全体")
	}
	job.Scope, job.ScopeSource = paths, "estimated"
	entry.Estimate = Record{"value": Record{"scope": paths, "scopeSource": "estimated"}, "note": "見積もり"}
	if err = s.savePending(ctx, key, entry); err != nil {
		return nil, err
	}
	if s.options.BackfillScope != nil && s.options.Own(row(issue["user"])) {
		if body := BackfilledBody(fleet.Text(issue["body"]), scope); body != "" {
			contract.LogFailure("変更範囲の本文への補記 "+entry.Repo, s.options.BackfillScope(ctx, entry.Repo, issue, body))
		}
	}
	return job, nil
}

func (s *Service) filingGroup(ctx context.Context, d Record) (Record, error) {
	current := d
	owner := fleet.Text(d["owner"])
	seen := map[string]bool{}
	for depth := 0; depth < 64; depth++ {
		id := fleet.Text(current["id"])
		if seen[id] {
			return nil, fail(409, "相談の祖先が循環しています。")
		}
		seen[id] = true
		parent := fleet.Text(current["parentSplit"])
		if parent == "" {
			parent = fleet.Text(current["parentConsultation"])
		}
		if parent == "" {
			break
		}
		ancestor, found, err := s.options.Records.Get(ctx, "drafts", parent)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fail(409, "相談の祖先がありません。")
		}
		current = ancestor
		if owner == "" {
			owner = fleet.Text(current["owner"])
		}
	}
	if strings.HasPrefix(owner, "token:") {
		return Record{"family": current["id"], "owner": owner}, nil
	}
	return nil, nil
}

// applySavedEstimate applies the estimate saved by an earlier attempt to the job.
func applySavedEstimate(entry *pending, job *jobs.NewJob) (bool, error) {
	if value := row(entry.Estimate["value"]); value != nil {
		var paths []string
		for _, raw := range fleet.Items(value["scope"]) {
			if path, ok := raw.(string); ok {
				paths = append(paths, path)
			}
		}
		if len(paths) > 0 {
			job.Scope, job.ScopeSource = paths, "estimated"
			if provider := fleet.Text(value["provider"]); provider != "" {
				if provider != "codex" && provider != "claude" && provider != "antigravity" {
					return false, fail(503, "保存済みの実行先を確認できません。")
				}
				job.Provider = &provider
			}
			return true, nil
		}
	}
	return false, nil
}

// estimatedScope normalizes the estimator's answer to a scope declaration.
func estimatedScope(estimated map[string]any) Record {
	scope := row(estimated["scope"])
	if scope == nil {
		scope = Record(estimated)
	}
	return scopeDeclaration(scope)
}

// scopePaths lists the declared paths.
func scopePaths(scope Record) []string {
	paths := []string{}
	for _, raw := range fleet.Items(scope["paths"]) {
		if path, ok := raw.(string); ok {
			paths = append(paths, path)
		}
	}
	return paths
}
