// Package filing persists submission intent before sending to Gitea and reconciles
// uncertain outcomes by the authenticated intake marker before considering a send.
package filing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strconv"
	"sync"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/pullcheck"
)

type Record = consultations.Record
type API interface {
	Issues(context.Context, string, string) ([]map[string]any, error)
	Issue(context.Context, string, int64) (map[string]any, error)
	CreateIssue(context.Context, string, any) (map[string]any, error)
}
type GroupIssueLinks interface {
	Get(context.Context, string, int64) (map[string]any, error)
	Update(context.Context, string, int64, string, any) error
}
type Prepared struct {
	Title  string
	Body   string
	Labels []int64
	Fields Record
	Job    jobs.NewJob
}
type Options struct {
	RepositoryArchived func(context.Context, string) (bool, error)
	GroupIssueLinks    GroupIssueLinks
	RepositoryGroups   func(context.Context) ([][]string, error)
	ValidateFollowup   func(context.Context, Record) error
	ValidateNewPost    func(context.Context, Record) error
	Acquire            func(context.Context, string) (context.Context, func(), error)
	IncidentLedger     func(context.Context) (any, error)
	EstimateScope      func(context.Context, string, map[string]any) (map[string]any, error)
	BackfillScope      func(context.Context, string, map[string]any, string) error
	SplitPlan          func(Record) ([]Record, error)
	BeforeSplitChild   func(context.Context, Record, Record, []any) (Record, error)
	Records            *consultations.Records
	API                API
	Jobs               *jobs.Store
	Now                func() time.Time
	Project            func(string) error
	Own                func(map[string]any) bool
	Build              func(context.Context, Record) (Prepared, error)
	Check              func(context.Context, Record) error
	Attach             func(context.Context, Record, map[string]any) error
	Link               func(string, int64) string
	Publish            func(map[string]any)
}
type Service struct {
	options    Options
	drafts     sync.Map
	repos      sync.Map
	filing     sync.Map
	estimating sync.Mutex
}

// ErrFilingReadUnavailable marks a read that can be retried from the saved,
// unposted draft.
var ErrFilingReadUnavailable = errors.New("起票に必要な情報を一時的に取得できません")

var marker = gitea.IssueMarker

func DraftID(body string) string { return gitea.DraftID(body) }
func New(ctx context.Context, options Options) (*Service, error) {
	if options.Records == nil || options.API == nil || options.Jobs == nil || options.Project == nil || options.Own == nil || options.Build == nil || options.Check == nil || options.Link == nil {
		return nil, errors.New("起票の保存先・検証・Gitea・配布先が必要です")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if _, err := options.Records.DB.ExecContext(ctx, bindSQL("CREATE TABLE IF NOT EXISTS job_filings(key TEXT PRIMARY KEY,json TEXT NOT NULL)")); err != nil {
		return nil, err
	}
	if _, err := options.Records.DB.ExecContext(ctx, bindSQL("CREATE TABLE IF NOT EXISTS slow_incident_filings(id TEXT PRIMARY KEY,json TEXT NOT NULL)")); err != nil {
		return nil, err
	}
	return &Service{options: options}, nil
}
func row(value any) Record { return fleet.Object(value) }
func fail(status int, message string) *contract.Problem {
	return contract.NewProblem(status, message, "")
}
func clone(d Record) Record {
	r := Record{}
	for k, v := range d {
		r[k] = v
	}
	return r
}
func iso(t time.Time) string { return contract.ISO(t) }
func lock(locks *sync.Map, key string) func() {
	value, _ := locks.LoadOrStore(key, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}
func (s *Service) Submit(ctx context.Context, id string, data Record, p auth.Principal) (Record, error) {
	return s.submit(ctx, id, data, p, false)
}

// SubmitAccepted resumes controller-owned work after its sender was revalidated.
// Interactive token requests retain the additional hold-mode check in Submit.
func (s *Service) SubmitAccepted(ctx context.Context, id string, data Record, p auth.Principal) (Record, error) {
	d, _, err := s.options.Records.Get(ctx, "drafts", id)
	if err != nil {
		return nil, err
	}
	if row(d["submissionRequest"]) != nil {
		ctx = context.WithValue(ctx, checkTriggerKey{}, "explicit_resend")
	}
	return s.submit(ctx, id, data, p, true)
}
func (s *Service) submit(ctx context.Context, id string, data Record, p auth.Principal, accepted bool) (Record, error) {
	unlock := lock(&s.drafts, id)
	defer unlock()
	d, found, err := s.options.Records.Get(ctx, "drafts", id)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fail(404, "下書きが見つかりません。")
	}
	if p.Kind == auth.PC && !p.Delegated && (fleet.Text(d["owner"]) != "token:"+p.TokenID || !accepted && d["hold"] != true) {
		return nil, fail(403, "このトークンから確定できる相談ではありません。")
	}
	if err = s.policy(ctx, d); err != nil {
		return nil, err
	}
	if fleet.Text(d["status"]) == "submitted" && row(d["issue"])["number"] != nil {
		issue := row(d["issue"])
		number, _ := integer(issue["number"])
		repo := fleet.Text(d["project"])
		return d, s.filePendingKey(ctx, repo, repo+"#"+strconv.FormatInt(number, 10))
	}
	if fleet.Text(d["status"]) == "submitted" && len(fleet.Items(d["issues"])) > 0 {
		return d, nil
	}
	reconcile := fleet.Text(d["status"]) == "submitting" || fleet.Text(d["status"]) == "submit_unknown"
	revision, valid := integer(data["revision"])
	actual, _ := integer(d["revision"])
	if !reconcile && (!valid || revision != actual || !allowed(d)) {
		return nil, fail(409, "下書きを確認してから送信してください。")
	}
	if len(fleet.Items(d["splitIssues"])) > 0 {
		return s.split(ctx, d)
	}
	if d["unassigned"] == true {
		return nil, fail(409, "プロジェクト未定の相談は分割案で対象を決めてください。")
	}
	return s.normal(ctx, d, reconcile)
}
func allowed(d Record) bool {
	return (fleet.Text(d["status"]) == "draft" || fleet.Text(d["status"]) == "auto_ready" || fleet.Text(d["status"]) == "submit_error") && d["ready"] == true && len(fleet.Items(d["questions"])) == 0
}
func (s *Service) normal(ctx context.Context, d Record, reconcile bool) (Record, error) {
	repo := fleet.Text(d["project"])
	unlock := lock(&s.repos, repo)
	defer unlock()
	if s.options.Acquire != nil {
		next, release, err := s.options.Acquire(ctx, "filing")
		if err != nil {
			return nil, err
		}
		ctx = next
		defer release()
	}
	if err := s.options.Project(repo); err != nil {
		return nil, err
	}
	d, err := s.change(ctx, d, func(next Record) { next["status"] = "submitting"; consultations.SetDraftError(next, nil, nil) })
	if err != nil {
		return nil, err
	}
	var history []map[string]any
	if indexed, ok := s.options.API.(interface {
		IssuesByMarker(context.Context, string, string) ([]map[string]any, error)
	}); ok {
		history, err = indexed.IssuesByMarker(ctx, repo, fleet.Text(d["id"]))
	} else {
		history, err = s.options.API.Issues(ctx, repo, "all")
	}
	if err != nil {
		if d["attempted"] != true && TemporaryGiteaRead(err) {
			err = fmt.Errorf("%w: %w", ErrFilingReadUnavailable, err)
		}
		return s.failure(ctx, d, err)
	}
	issue, err := s.markedIssue(history, d)
	if err != nil {
		return s.failure(ctx, d, err)
	}
	if issue == nil && reconcile && d["attempted"] == true {
		return s.failure(ctx, d, fail(409, "前回の送信結果を照合中です。Issueが見つからないため再投稿していません。"))
	}
	var prepared Prepared
	if issue != nil {
		if d["attempted"] != true {
			d, err = s.change(ctx, d, func(next Record) { next["attempted"] = true })
			if err != nil {
				return nil, err
			}
		}
		// Reconciliation must not rerun AI or apply today's provider/scope choices.
		// Prefer the exact job persisted before POST; old records recover from the Issue.
		prepared.Job, err = reconciledJob(repo, d, issue)
		if err != nil {
			return s.failure(ctx, d, err)
		}
	} else {
		if cause := repositoryStop(d); cause != nil {
			return s.failure(ctx, d, cause)
		}
		if err = s.checkNewPost(ctx, d); err != nil {
			return s.failure(ctx, d, err)
		}
		prepared, err = s.options.Build(ctx, d)
		if err != nil {
			return s.failure(ctx, d, err)
		}
		// Check immutable policy and replacement preconditions again after AI work.
		if err = s.checkNewPost(ctx, d); err != nil {
			return s.failure(ctx, d, err)
		}
		d, err = s.change(ctx, d, func(next Record) { rememberPreparedPost(next, prepared) })
		if err != nil {
			return nil, err
		}
		issue, err = s.options.API.CreateIssue(ctx, repo, issuePayload(prepared))
		if err != nil {
			return s.postFailure(ctx, d, err)
		}
	}
	number, ok := integer(issue["number"])
	if !ok || number < 1 {
		return s.failure(ctx, d, &gitea.Error{UnknownOutcome: true})
	}
	if hasImages(d) {
		if issue, err = s.attachImages(ctx, d, repo, number, issue); err != nil {
			return s.failure(ctx, d, err)
		}
	}
	return s.complete(ctx, d, issue, prepared.Job)
}

// TemporaryGiteaRead classifies errors from read-only Gitea calls.
func TemporaryGiteaRead(err error) bool {
	var failure *gitea.Error
	if !errors.As(err, &failure) || failure.UnknownOutcome {
		return false
	}
	return failure.Status == 0 || failure.Status == 408 || failure.Status == 429 || failure.Status >= 500
}

// markedIssue finds the issue this service posted for the draft, refusing more than one.
func (s *Service) markedIssue(history []map[string]any, d Record) (map[string]any, error) {
	var issue map[string]any
	for _, candidate := range history {
		if candidate["pull_request"] == nil && DraftID(fleet.Text(candidate["body"])) == fleet.Text(d["id"]) && s.options.Own(row(candidate["user"])) {
			if issue != nil {
				return nil, fail(409, "同じ起票マーカーのIssueが複数あり、照合を停止しました。")
			}
			issue = candidate
		}
	}
	return issue, nil
}

// reconciledJob returns the job saved before the earlier POST, or recovers it from the issue
// for records saved before the job was kept.
func reconciledJob(repo string, d Record, issue map[string]any) (jobs.NewJob, error) {
	var job jobs.NewJob
	if saved := d["preparedJob"]; saved != nil {
		encoded, encodeErr := json.Marshal(saved)
		if encodeErr != nil {
			return job, encodeErr
		}
		if err := json.Unmarshal(encoded, &job); err != nil {
			return job, err
		}
		return job, nil
	}
	return JobFromIssue(repo, issue, "")
}

// checkNewPost checks the filing policy and, for proposals, the new-post preconditions.
func (s *Service) checkNewPost(ctx context.Context, d Record) error {
	boundary := pullcheck.Boundary(ctx)
	if err := s.options.Check(boundary, d); err != nil {
		return err
	}
	if truth(d["proposal"]) && s.options.ValidateNewPost != nil {
		if err := s.options.ValidateNewPost(boundary, d); err != nil {
			return err
		}
	}
	return nil
}

// rememberPreparedPost saves the prepared body and job before the POST so that a retry
// reconciles the same issue instead of preparing it again.
func rememberPreparedPost(next Record, prepared Prepared) {
	next["postBody"] = prepared.Body
	next["preparedJob"] = prepared.Job
	for key, value := range prepared.Fields {
		if key == "implementationTier" || key == "precedingIssues" || key == "changeScope" {
			next[key] = value
		}
	}
	next["attempted"] = true
}

// issuePayload is the issue creation request for the prepared issue.
func issuePayload(prepared Prepared) map[string]any {
	payload := map[string]any{"title": prepared.Title, "body": prepared.Body}
	if len(prepared.Labels) > 0 {
		payload["labels"] = prepared.Labels
	}
	return payload
}

// attachImages publishes the draft's images to the issue and rereads the published issue.
func (s *Service) attachImages(ctx context.Context, d Record, repo string, number int64, issue map[string]any) (map[string]any, error) {
	if s.options.Attach == nil {
		return nil, fail(503, "画像の転送を利用できません。")
	}
	if err := s.options.Attach(ctx, d, issue); err != nil {
		return nil, err
	}
	// The attachment PATCH changes the execution body. Project the published
	// body, not the temporary pending marker, into the initial acceptance input.
	issue, err := s.options.API.Issue(ctx, repo, number)
	if err != nil {
		return nil, err
	}
	if DraftID(fleet.Text(issue["body"])) != fleet.Text(d["id"]) || !s.options.Own(row(issue["user"])) {
		return nil, fail(409, "添付後の起票マーカーと投稿者を確認できません。")
	}
	return issue, nil
}

func (s *Service) change(ctx context.Context, d Record, apply func(Record)) (Record, error) {
	return s.options.Records.Change(ctx, "drafts", fleet.Text(d["id"]), func(_ *sql.Tx, current Record) (Record, error) {
		actual, _ := integer(current["revision"])
		expected, _ := integer(d["revision"])
		if current == nil || actual != expected || fleet.Text(current["status"]) != fleet.Text(d["status"]) {
			return nil, fail(409, "下書きが変わったため送信を止めました。")
		}
		next := clone(current)
		apply(next)
		next["updatedAt"] = iso(s.options.Now())
		return next, nil
	})
}

// Only issue creation can prove a new POST was rejected or never connected.
// Failures while reading history or publishing attachments retain send intent.
func (s *Service) postFailure(ctx context.Context, d Record, cause error) (Record, error) {
	next := clone(d)
	var response *gitea.Error
	if errors.As(cause, &response) && (response.DialRefused || response.Status >= 400 && response.Status < 500 && !response.UnknownOutcome) {
		next["attempted"] = false
		if response.Status == 423 {
			cause = s.repositoryRejection(ctx, next)
		}
	}
	return s.failure(ctx, next, cause)
}
func (s *Service) failure(ctx context.Context, d Record, cause error) (Record, error) {
	unknown := d["attempted"] == true
	if !unknown && errors.Is(cause, ErrFilingReadUnavailable) {
		// The recovery loop rechecks the saved draft and its marker before
		// posting. This branch is only reached for a marked read failure.
		return d, cause
	}
	saved, err := s.change(ctx, d, func(next Record) {
		if stops, ok := d["repositoryStops"]; ok {
			next["repositoryStops"] = stops
		}
		failed := consultations.AtStage("起票", cause)
		next["status"] = "submit_error"
		consultations.SetDraftError(next, consultations.PublicFailure(d, failed, "起票を完了できませんでした。保存済みの相談で状況を確認してください。"), nil)
		next["lastFailure"] = consultations.FailureRecord(iso(s.options.Now()), d, failed, fleet.Text(next["error"]))
		if unknown {
			next["status"] = "submit_unknown"
		} else {
			next["attempted"] = false
		}
		var p *contract.Problem
		if errors.As(cause, &p) {
			consultations.SetDraftError(next, p.Message, p.Code)
		}
	})
	contract.LogFailure("起票 "+fleet.Text(d["id"]), consultations.AtStage("起票", cause))
	if err != nil {
		return nil, err
	}
	s.emit(saved)
	return saved, cause
}
func (s *Service) emit(d Record) {
	if s.options.Publish != nil {
		s.options.Publish(map[string]any{"type": "draft", "id": d["id"]})
	}
}
