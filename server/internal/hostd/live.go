package hostd

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"math"
	"regexp"
	"sort"
)

func (s *Service) LiveNext(ctx context.Context, host string) (any, error) {
	targets, _, err := s.Targets()
	if err != nil {
		return nil, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var token string
	if err = tx.QueryRowContext(ctx, "SELECT host FROM host_image_tokens WHERE host=$1 FOR UPDATE", host).Scan(&token); errors.Is(err, sql.ErrNoRows) {
		return map[string]any{"job": nil}, nil
	}
	if err != nil {
		return nil, err
	}
	var id int64
	var repo, commit string
	var config []byte
	err = tx.QueryRowContext(ctx, `SELECT c.id,c.repo,c.commit_sha,c.config FROM live_checks c JOIN jobs j ON j.id=c.job_id WHERE c.host=$1 AND c.status IN('queued','running') AND j.status='held' AND j.generation=c.generation AND(j.view->'liveCheck'->>'id')::bigint=c.id ORDER BY c.id FOR UPDATE OF c LIMIT 1`, host).Scan(&id, &repo, &commit, &config)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]any{"job": nil}, nil
	}
	if err != nil {
		return nil, err
	}
	var deployment string
	err = tx.QueryRowContext(ctx, "SELECT status FROM app_deployments WHERE repo=$1 AND commit_sha=$2", repo, commit).Scan(&deployment)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if deployment == "" && targets[repo] != "" || one(deployment, "queued", "running") {
		return map[string]any{"job": nil}, nil
	}
	if _, err = tx.ExecContext(ctx, "UPDATE live_checks SET status='running',started_at=coalesce(started_at,$2) WHERE id=$1", id, s.now()); err != nil {
		return nil, err
	}
	var job map[string]any
	if err = json.Unmarshal(config, &job); err != nil {
		return nil, err
	}
	job["id"] = id
	job["repo"] = repo
	job["commit"] = commit
	if deployment != "" && deployment != "deployed" {
		job["failure"] = "deployment_failed"
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{"job": job}, nil
}

var unsafeLive = regexp.MustCompile(`(?i)[\x00-\x1f\x7f-\x9f]|data:|base64|iVBORw0KGgo|/9j/|R0lGOD|<img|\b(?:bytes|thumbnail|password|authorization|credential|secret|token)\b|\b(?:idk_|idh_|sk-)|https?:|[?&][^\s=]+\s*=|[A-Za-z0-9+/=_-]{32,}|\\x[0-9a-f]{2}|\x{FFFD}`)

func liveText(v any, max int) bool {
	s, ok := v.(string)
	return ok && contract.UTF16Length(s) > 0 && contract.UTF16Length(s) <= max && !unsafeLive.MatchString(s)
}
func validLive(v map[string]any) bool {
	if len(v) != 4 || !only(v, "pass", "summary", "metrics", "failures") || !liveText(v["summary"], 500) {
		return false
	}
	pass, ok := v["pass"].(bool)
	if !ok {
		return false
	}
	metrics := fleet.Object(v["metrics"])
	if metrics == nil || len(metrics) > 100 {
		return false
	}
	for name, value := range metrics {
		n, ok := value.(float64)
		if !regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`).MatchString(name) || !ok || math.IsNaN(n) || math.IsInf(n, 0) {
			return false
		}
	}
	failures, ok := v["failures"].([]any)
	if !ok || len(failures) > 20 || pass != (len(failures) == 0) {
		return false
	}
	for _, raw := range failures {
		f := fleet.Object(raw)
		if len(f) != 2 || !only(f, "target", "reason") || !liveText(f["target"], 200) || !liveText(f["reason"], 500) {
			return false
		}
	}
	return len([]byte(encode(v))) <= 16384
}
func canonicalHash(v any) string {
	sum := sha256.Sum256([]byte(encode(v)))
	return hex.EncodeToString(sum[:])
}
func (s *Service) LiveResult(ctx context.Context, host string, body map[string]any) (any, error) {
	id, result, err := parseLiveResult(body)
	if err != nil {
		return nil, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var jobID int64
	if err = tx.QueryRowContext(ctx, "SELECT job_id FROM live_checks WHERE id=$1 AND host=$2", id, host).Scan(&jobID); errors.Is(err, sql.ErrNoRows) {
		return nil, problem(409, "自分の実地確認だけ報告できます。")
	}
	if err != nil {
		return nil, err
	}
	var repo, status string
	var issue, generation int64
	var viewJSON []byte
	if err = tx.QueryRowContext(ctx, "SELECT repo,issue,status,generation,view FROM jobs WHERE id=$1 FOR UPDATE", jobID).Scan(&repo, &issue, &status, &generation, &viewJSON); err != nil {
		return nil, err
	}
	var checkStatus, commit string
	var checkGeneration int64
	var previousResult []byte
	if err = tx.QueryRowContext(ctx, "SELECT status,generation,commit_sha,result FROM live_checks WHERE id=$1 FOR UPDATE", id).Scan(&checkStatus, &checkGeneration, &commit, &previousResult); err != nil {
		return nil, err
	}
	if checkStatus == "done" {
		var previous any
		if err = json.Unmarshal(previousResult, &previous); err != nil {
			return nil, err
		}
		if canonicalHash(previous) != canonicalHash(result) {
			return nil, problem(409, "報告済みの結果と異なります。")
		}
		return map[string]any{"ok": true}, nil
	}
	var view map[string]any
	if err = json.Unmarshal(viewJSON, &view); err != nil {
		return nil, err
	}
	if checkStatus != "running" || status != "held" || generation != checkGeneration || int64(fleet.Number(fleet.Object(view["liveCheck"])["id"])) != id {
		return nil, problem(409, "実地確認の世代が変わりました。")
	}
	pass := result["pass"] == true
	var cause any
	times := 0
	if !pass {
		cause, times, err = liveFailureStreak(ctx, tx, jobID, result)
		if err != nil {
			return nil, err
		}
	}
	state := "merged"
	checkView := "passed"
	held := times >= 2
	increment := 0
	if !pass {
		state = "queued"
		checkView = "failed"
		increment = 1
		if held {
			state = "failed"
			checkView = "attention"
		}
		at := iso(s.now())
		view["retryFromBase"] = map[string]any{"generation": generation + 1, "by": "live_check", "at": at}
		view["instructions"] = append(fleet.List(view["instructions"]), map[string]any{"text": "実地確認の不合格を修正してください。観察結果: " + encode(result), "by": "live_check", "at": at})
	}
	if s.IssueState == nil {
		return nil, problem(503, "実地確認のIssue更新先が設定されていません。")
	}
	issueState := "open"
	if pass {
		issueState = "closed"
	}
	if err = s.IssueState(ctx, repo, issue, issueState); err != nil {
		return nil, err
	}
	view["liveCheck"] = map[string]any{"id": id, "status": checkView, "result": result, "times": times}
	outcome := map[string]any{"mergeCommit": commit, "liveCheck": result}
	if held {
		outcome["automaticRetryStopped"] = true
		outcome["reason"] = "live_check_repeated"
		outcome["error"] = "実地確認が同じ原因で2回続けて不合格です。観察結果: " + encode(result)
	}
	now := s.now()
	if _, err = tx.ExecContext(ctx, "UPDATE live_checks SET status='done',result=$2::jsonb,cause=$3,failures_in_row=$4,finished_at=$5 WHERE id=$1", id, encode(result), cause, times, now); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE jobs SET status=$2,hold_reason=NULL,generation=generation+$3,branch=CASE WHEN $4 THEN branch ELSE NULL END,pr=CASE WHEN $4 THEN pr ELSE NULL END,head_sha=CASE WHEN $4 THEN head_sha ELSE NULL END,view=$5::jsonb,result=$6::jsonb,updated_at=$7 WHERE id=$1", jobID, state, increment, pass, encode(view), encode(outcome), now); err != nil {
		return nil, err
	}
	for kind, data := range map[string]any{"live_check": map[string]any{"pass": pass, "times": times}, "state": map[string]any{"to": state, "holdReason": nil}} {
		if _, err = tx.ExecContext(ctx, "INSERT INTO job_events(job_id,host,kind,data,at)VALUES($1,$2,$3,$4::jsonb,$5)", jobID, host, kind, encode(data), now); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// parseLiveResult は実地確認の報告の形を検査する。結果の形が崩れていれば、実行できなかった不合格の結果に置き換える。
func parseLiveResult(body map[string]any) (int64, map[string]any, error) {
	if len(body) != 2 || !only(body, "id", "result") || !integer(body["id"], 1, 9007199254740991) {
		return 0, nil, problem(400, "実地確認の報告の形が違います。")
	}
	id := int64(fleet.Number(body["id"]))
	result := fleet.Object(body["result"])
	if !validLive(result) {
		result = map[string]any{"pass": false, "summary": "実地確認を実行できませんでした", "metrics": map[string]any{}, "failures": []any{map[string]any{"target": "live_check", "reason": "invalid_result"}}}
	}
	return id, result, nil
}

// liveFailureStreak は不合格の原因を失敗の並びの指紋にし、前回の実地確認と同じ原因なら続けて不合格の回数を数える。
func liveFailureStreak(ctx context.Context, tx *sql.Tx, jobID int64, result map[string]any) (any, int, error) {
	failures := append([]any{}, fleet.List(result["failures"])...)
	sort.Slice(failures, func(i, j int) bool {
		a, b := fleet.Object(failures[i]), fleet.Object(failures[j])
		if a["target"] != b["target"] {
			return fleet.Text(a["target"]) < fleet.Text(b["target"])
		}
		return fleet.Text(a["reason"]) < fleet.Text(b["reason"])
	})
	cause := canonicalHash(failures)
	var previousCause sql.NullString
	var previousTimes int
	err := tx.QueryRowContext(ctx, "SELECT cause,failures_in_row FROM live_checks WHERE job_id=$1 AND status='done' ORDER BY id DESC LIMIT 1", jobID).Scan(&previousCause, &previousTimes)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, 0, err
	}
	times := 1
	if previousCause.String == cause {
		times = previousTimes + 1
	}
	return cause, times, nil
}
