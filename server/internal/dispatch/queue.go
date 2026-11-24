package dispatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"issue-desk-intake/server/internal/contract"
)

type Queue struct{ DB *sql.DB }

func iso(value time.Time) string { return contract.ISO(value) }
func nullISO(value sql.NullTime) *string {
	if !value.Valid {
		return nil
	}
	text := iso(value.Time)
	return &text
}
func encode(value any) any {
	if value == nil {
		return nil
	}
	raw, _ := json.Marshal(value)
	return raw
}
func problem(status int, message string) error { return contract.NewProblem(status, message, "") }
func same(a, b any) bool {
	normalize := func(value any) any {
		raw, _ := json.Marshal(value)
		var result any
		_ = json.Unmarshal(raw, &result)
		return result
	}
	return reflect.DeepEqual(normalize(a), normalize(b))
}

type Request struct {
	ID         string         `json:"id"`
	Kind       string         `json:"kind"`
	Repo       string         `json:"repo"`
	Payload    map[string]any `json:"payload"`
	Sender     string         `json:"sender"`
	Status     string         `json:"status"`
	ClaimedBy  *string        `json:"claimedBy"`
	ClaimedAt  *string        `json:"claimedAt"`
	Result     map[string]any `json:"result"`
	CreatedAt  string         `json:"createdAt"`
	FinishedAt *string        `json:"finishedAt"`
	Resumed    *bool          `json:"resumed,omitempty"`
}

const requestColumns = "id,kind,repo,payload,sender,status,claimed_by,claimed_at,result,created_at,finished_at"

func requestTable(kind string) (string, error) {
	switch kind {
	case "pr_create", "merge_manual":
		return "requests", nil
	case "pr_close":
		return "pr_close_requests", nil
	case "full_test":
		return "main_test_requests", nil
	case "native_test":
		return "native_test_requests", nil
	}
	return "", problem(400, "依頼の種類が不正です。")
}

type scanner interface{ Scan(...any) error }
type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func scanRequest(row scanner) (*Request, error) {
	request := &Request{}
	var payload, result []byte
	var claimed, finished sql.NullTime
	var created time.Time
	err := row.Scan(&request.ID, &request.Kind, &request.Repo, &payload, &request.Sender, &request.Status, &request.ClaimedBy, &claimed, &result, &created, &finished)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(payload, &request.Payload); err != nil {
		return nil, err
	}
	if len(result) > 0 {
		if err = json.Unmarshal(result, &request.Result); err != nil {
			return nil, err
		}
	}
	request.ClaimedAt, request.FinishedAt, request.CreatedAt = nullISO(claimed), nullISO(finished), iso(created)
	return request, nil
}
func readRequest(ctx context.Context, q queryer, id string) (*Request, error) {
	query := "SELECT " + requestColumns + " FROM requests WHERE id=$1 UNION ALL SELECT " + requestColumns + " FROM main_test_requests WHERE id=$1 UNION ALL SELECT " + requestColumns + " FROM pr_close_requests WHERE id=$1 UNION ALL SELECT " + requestColumns + " FROM native_test_requests WHERE id=$1"
	request, err := scanRequest(q.QueryRowContext(ctx, query, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return request, err
}
func (q *Queue) GetRequest(ctx context.Context, id string) (*Request, error) {
	return readRequest(ctx, q.DB, id)
}

func (q *Queue) Submit(ctx context.Context, request Request) (*Request, bool, error) {
	if !RequestID.MatchString(request.ID) || request.Sender == "" {
		return nil, false, problem(400, "依頼IDと送り手が不正です。")
	}
	payload, err := ValidatePayload(request.Kind, request.Repo, request.Payload)
	if err != nil {
		return nil, false, err
	}
	request.Payload = payload
	table, err := requestTable(request.Kind)
	if err != nil {
		return nil, false, err
	}
	tx, err := q.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "issue-desk:request:"+request.ID); err != nil {
		return nil, false, err
	}
	if err = applyPreapproval(ctx, tx, &request); err != nil {
		return nil, false, err
	}
	previous, err := readRequest(ctx, tx, request.ID)
	if err != nil {
		return nil, false, err
	}
	if previous != nil {
		if previous.Kind != request.Kind || previous.Repo != request.Repo || previous.Sender != request.Sender || !same(previous.Payload, request.Payload) {
			return nil, false, problem(409, "同じ依頼IDが別の内容で使われています。")
		}
		if err = tx.Commit(); err != nil {
			return nil, false, err
		}
		return previous, false, nil
	}
	created, err := scanRequest(tx.QueryRowContext(ctx, "INSERT INTO "+table+"(id,kind,repo,payload,sender) VALUES($1,$2,$3,$4::jsonb,$5) RETURNING "+requestColumns, request.ID, request.Kind, request.Repo, encode(request.Payload), request.Sender))
	if err != nil {
		return nil, false, err
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	return created, true, nil
}

func (q *Queue) Next(ctx context.Context, host, kind string, homeRepos, implRepos []string) (*Request, error) {
	return q.next(ctx, host, kind, homeRepos, implRepos, nil)
}

func (q *Queue) NextNative(ctx context.Context, host string, repos []string, senders map[string][]string) (*Request, error) {
	return q.next(ctx, host, "native_test", repos, []string{}, senders)
}

func (q *Queue) next(ctx context.Context, host, kind string, homeRepos, implRepos []string, nativeSenders map[string][]string) (*Request, error) {
	table, err := requestTable(kind)
	if err != nil {
		return nil, err
	}
	tx, err := q.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "issue-desk:request-host:"+host+":"+kind); err != nil {
		return nil, err
	}
	scope := "kind=$2 AND (repo=ANY($3::text[]) OR (repo=ANY($4::text[]) AND kind<>'full_test'))"
	args := []any{host, kind, homeRepos, implRepos}
	if kind == "native_test" {
		scope += " AND payload->>'host'=$1 AND COALESCE(($5::jsonb->repo) ? sender,false)"
		if nativeSenders == nil {
			nativeSenders = map[string][]string{}
		}
		args = append(args, encode(nativeSenders))
	}
	mine, err := scanRequest(tx.QueryRowContext(ctx, "SELECT "+requestColumns+" FROM "+table+" WHERE status='claimed' AND claimed_by=$1 AND "+scope+" ORDER BY created_at,id LIMIT 1", args...))
	if err == nil {
		resumed := true
		mine.Resumed = &resumed
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return mine, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	request, err := scanRequest(tx.QueryRowContext(ctx, "UPDATE "+table+" SET status='claimed',claimed_by=$1,claimed_at=now() WHERE id=(SELECT id FROM "+table+" WHERE status='queued' AND "+scope+" ORDER BY created_at,id LIMIT 1 FOR UPDATE SKIP LOCKED) AND status='queued' RETURNING "+requestColumns, args...))
	if errors.Is(err, sql.ErrNoRows) {
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	resumed := false
	request.Resumed = &resumed
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return request, nil
}

// actionUpdate validates the action on the request and builds the update that
// applies it only while the request is still in the expected state.
func actionUpdate(current *Request, table, id, actor, action string, result map[string]any) (string, []any, error) {
	var err error
	query := ""
	args := []any{id, actor}
	switch action {
	case "result":
		if current.Kind == "native_test" {
			for _, key := range []string{"head", "base", "tree"} {
				if result[key] != current.Payload[key] {
					return "", nil, problem(409, "Mac検証結果の対象SHAが一致しません。")
				}
			}
			if current.Status == "done" && !same(current.Result, result) {
				return "", nil, problem(409, "確定済みのMac検証結果は変更できません。")
			}
		}
		if err = ValidateResult(current.Kind, result); err != nil {
			return "", nil, err
		}
		query = "UPDATE " + table + " SET status='done',result=$3::jsonb,finished_at=now() WHERE id=$1 AND status='claimed' AND claimed_by=$2 RETURNING " + requestColumns
		args = append(args, encode(result))
	case "release":
		query = "UPDATE " + table + " SET status='queued',claimed_by=NULL,claimed_at=NULL WHERE id=$1 AND status='claimed' AND claimed_by=$2 RETURNING " + requestColumns
	case "cancel":
		query = "UPDATE " + table + " SET status='done',result='{\"status\":\"cancelled\"}'::jsonb,finished_at=now() WHERE id=$1 AND status='queued' AND sender=$2 RETURNING " + requestColumns
	default:
		return "", nil, problem(404, "操作が見つかりません。")
	}
	return query, args, nil
}

func (q *Queue) RequestAction(ctx context.Context, id, actor, action string, result map[string]any) (*Request, error) {
	current, err := q.GetRequest(ctx, id)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, problem(404, "依頼が見つかりません。")
	}
	table, err := requestTable(current.Kind)
	if err != nil {
		return nil, err
	}
	query, args, err := actionUpdate(current, table, id, actor, action, result)
	if err != nil {
		return nil, err
	}
	updated, err := scanRequest(q.DB.QueryRowContext(ctx, query, args...))
	if err == nil {
		return updated, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if action == "cancel" {
		return nil, nil
	}
	latest, err := q.GetRequest(ctx, id)
	if err != nil {
		return nil, err
	}
	if latest != nil && current.Kind == "native_test" && action == "result" && latest.Status == "done" && !same(latest.Result, result) {
		return nil, problem(409, "確定済みのMac検証結果は変更できません。")
	}
	if latest != nil && (action == "result" && latest.Status == "done" && latest.ClaimedBy != nil && *latest.ClaimedBy == actor || action == "release" && latest.Status == "queued") {
		return latest, nil
	}
	return nil, problem(409, "この監視が引き取っている依頼ではありません。")
}
