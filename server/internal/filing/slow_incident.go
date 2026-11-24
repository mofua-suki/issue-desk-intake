package filing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"math"
	"strings"

	"issue-desk-intake/server/internal/operations"
	"issue-desk-intake/server/internal/pullcheck"
)

// slowIncidentIntent is an internal handoff, not an authorization to file.
// The caller must supply a validator that checks the persisted judgement and target.
type slowIncidentIntent struct {
	IncidentID string
	Cause      string
	Project    string
	Prepared   Prepared
}

func slowIncidentID(incident, cause string) string {
	return operations.SlowIncidentSubmissionID(incident, cause)
}

// fileSlowIncidentMode is intentionally not exposed as a filing policy exception.
// A durable attempted record is created atomically before any POST; a crash in
// that interval is treated as ambiguous and never causes an automatic resend.
func (s *Service) fileSlowIncidentMode(ctx context.Context, in slowIncidentIntent, validate func(context.Context, slowIncidentIntent) error, recoveryOnly bool) (map[string]any, error) {
	if validate == nil {
		return nil, errors.New("遅延有事の検証先が必要です")
	}
	if strings.TrimSpace(in.IncidentID) == "" || strings.TrimSpace(in.Cause) == "" || strings.TrimSpace(in.Project) == "" {
		return nil, errors.New("遅延有事の識別情報が必要です")
	}
	id := slowIncidentID(in.IncidentID, in.Cause)
	unlock := lock(&s.drafts, id)
	defer unlock()
	unlockRepo := lock(&s.repos, in.Project)
	defer unlockRepo()
	if s.options.Acquire != nil {
		next, release, err := s.options.Acquire(ctx, "filing")
		if err != nil {
			return nil, err
		}
		ctx = next
		defer release()
	}
	if err := s.options.Project(in.Project); err != nil {
		return nil, err
	}
	saved, found, err := s.slowIntentGet(ctx, id)
	if err != nil {
		return nil, err
	}
	if found && (saved["slowIncidentIntent"] != true || fleet.Text(saved["incidentID"]) != in.IncidentID || fleet.Text(saved["cause"]) != in.Cause || fleet.Text(saved["project"]) != in.Project) {
		return nil, errors.New("遅延有事の送信記録が一致しません")
	}
	history, err := s.options.API.Issues(ctx, in.Project, "all")
	if err != nil {
		return nil, err
	}
	issue, err := s.slowIncidentIssue(history, id)
	if err != nil {
		return nil, err
	}
	if issue != nil {
		if !found {
			return nil, errors.New("送信記録のないIssueを照合できません")
		}
		number, ok := integer(issue["number"])
		if !ok || number < 1 {
			return nil, errors.New("Issue番号を確認できません")
		}
		return issue, s.rememberSlowIssue(ctx, id, number)
	}
	if found {
		return nil, errors.New("送信結果を確認できないため再投稿しません")
	}
	if recoveryOnly {
		return nil, errors.New("送信意図がなく結果を確認できません。再投稿しません")
	}
	if err = validate(pullcheck.Boundary(ctx), in); err != nil {
		return nil, err
	}
	if err = markSlowIncidentBody(&in.Prepared, id); err != nil {
		return nil, err
	}
	if err = validate(pullcheck.Boundary(ctx), in); err != nil {
		return nil, err
	}
	_, err = s.slowIntentChange(ctx, id, func(_ *sql.Tx, current Record) (Record, error) {
		if current != nil {
			return nil, errors.New("送信意図が既に存在します")
		}
		return Record{"id": id, "slowIncidentIntent": true, "incidentID": in.IncidentID, "cause": in.Cause, "project": in.Project, "prepared": in.Prepared, "attempted": true}, nil
	})
	if err != nil {
		return nil, err
	}
	if err = validate(pullcheck.Boundary(ctx), in); err != nil {
		return nil, err
	}
	issue, err = s.options.API.CreateIssue(ctx, in.Project, issuePayload(in.Prepared))
	if err != nil {
		return nil, err
	}
	number, ok := integer(issue["number"])
	if !ok || number < 1 {
		return nil, fmt.Errorf("起票結果の番号を確認できません")
	}
	return issue, s.rememberSlowIssue(ctx, id, number)
}

// slowIncidentIssue finds the issue this service posted with the submission marker.
func (s *Service) slowIncidentIssue(history []map[string]any, id string) (map[string]any, error) {
	var issue map[string]any
	for _, candidate := range history {
		if candidate["pull_request"] == nil && DraftID(fleet.Text(candidate["body"])) == id && s.options.Own(row(candidate["user"])) {
			if issue != nil {
				return nil, errors.New("同じ起票マーカーのIssueが複数あります")
			}
			issue = candidate
		}
	}
	return issue, nil
}

// rememberSlowIssue records the posted issue number in the submission intent.
func (s *Service) rememberSlowIssue(ctx context.Context, id string, number int64) error {
	_, err := s.slowIntentChange(ctx, id, func(_ *sql.Tx, current Record) (Record, error) {
		if current == nil || current["slowIncidentIntent"] != true {
			return nil, errors.New("送信記録がありません")
		}
		next := clone(current)
		next["issueNumber"] = number
		return next, nil
	})
	return err
}

// markSlowIncidentBody adds the submission marker and settings to the prepared body.
func markSlowIncidentBody(prepared *Prepared, id string) error {
	// Reject an existing marker so the persisted and posted identity is unambiguous.
	if marker.MatchString(prepared.Body) {
		return errors.New("本文に既存の起票マーカーがあります")
	}
	prepared.Body += "\n<!-- issue-desk:" + id + " -->"
	if len(prepared.Fields) > 0 {
		settings, err := json.Marshal(prepared.Fields)
		if err != nil {
			return err
		}
		prepared.Body += "\n<!-- issue-desk:settings " + string(settings) + " -->"
	}
	return nil
}

// Separate storage keeps internal intents out of interactive draft listings.
func (s *Service) slowIntentGet(ctx context.Context, id string) (Record, bool, error) {
	var raw string
	err := s.options.Records.DB.QueryRowContext(ctx, bindSQL("SELECT json FROM slow_incident_filings WHERE id=?"), id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var record Record
	if err = json.Unmarshal([]byte(raw), &record); err != nil {
		return nil, false, err
	}
	return record, true, nil
}
func (s *Service) slowIntentChange(ctx context.Context, id string, change func(*sql.Tx, Record) (Record, error)) (Record, error) {
	tx, err := s.options.Records.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var raw string
	err = tx.QueryRowContext(ctx, bindSQL("SELECT json FROM slow_incident_filings WHERE id=?"), id).Scan(&raw)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var current Record
	if found {
		if err = json.Unmarshal([]byte(raw), &current); err != nil {
			return nil, err
		}
	}
	next, err := change(tx, current)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	if found {
		result, err := tx.ExecContext(ctx, bindSQL("UPDATE slow_incident_filings SET json=? WHERE id=? AND json=?"), string(encoded), id, raw)
		if err != nil {
			return nil, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n != 1 {
			return nil, errors.New("送信記録が変更されました")
		}
	} else {
		if _, err = tx.ExecContext(ctx, bindSQL("INSERT INTO slow_incident_filings(id,json) VALUES(?,?)"), id, string(encoded)); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return next, nil
}

// FileSlowIncident accepts only a persisted problem judgement. validate must
// re-read the live target and its explicit stop immediately before sending.
func (s *Service) FileSlowIncident(ctx context.Context, f *operations.SlowIncidentFiling, validate func(context.Context, *operations.SlowIncidentFiling) error) (map[string]any, error) {
	return s.slowIncident(ctx, f, validate, false)
}

// RecoverSlowIncident only looks up a previously persisted intent and marker.
// It cannot create an intent or submit an issue, including after a crash.
func (s *Service) RecoverSlowIncident(ctx context.Context, f *operations.SlowIncidentFiling) (map[string]any, error) {
	return s.slowIncident(ctx, f, nil, true)
}

func (s *Service) slowIncident(ctx context.Context, f *operations.SlowIncidentFiling, validate func(context.Context, *operations.SlowIncidentFiling) error, recoveryOnly bool) (map[string]any, error) {
	if f == nil || (!recoveryOnly && (validate == nil || s.options.IncidentLedger == nil)) {
		return nil, errors.New("遅延有事の照合先が必要です")
	}
	snapshot := *f
	check := func(ctx context.Context, _ slowIncidentIntent) error {
		ledger, err := s.options.IncidentLedger(ctx)
		if err != nil {
			return err
		}
		if err := matchSlowIncidentLedger(ledger, snapshot); err != nil {
			return err
		}
		copy := snapshot
		return validate(ctx, &copy)
	}
	return s.fileSlowIncidentMode(ctx, slowIncidentIssueIntent(snapshot), check, recoveryOnly)
}

// matchSlowIncidentLedger confirms the ledger holds exactly one expired slow incident that
// matches the filing.
func matchSlowIncidentLedger(ledger any, snapshot operations.SlowIncidentFiling) error {
	matches := 0
	for _, v := range fleet.Items(ledger) {
		r := row(v)
		if fleet.Text(r["id"]) != snapshot.IncidentID {
			continue
		}
		matches++
		target := row(r["target"])
		n, ok := integer(target["number"])
		elapsed, okElapsed := target["elapsedSeconds"].(float64)
		if !okElapsed {
			switch x := target["elapsedSeconds"].(type) {
			case int:
				elapsed = float64(x)
			case int64:
				elapsed = float64(x)
			case json.Number:
				elapsed, _ = x.Float64()
			}
		}
		if r["kind"] != "slow" || r["state"] != "expired" || r["endedAt"] != nil || r["slowJudgement"] != "problem" || r["slowCause"] != snapshot.Cause || r["slowDirectPR"] != snapshot.DirectPR || fleet.Text(r["key"]) != snapshot.IncidentKey || snapshot.IncidentKey != "slow-"+snapshot.WorkKey || snapshot.WorkKey == "" || fleet.Text(target["workKey"]) != snapshot.WorkKey || fleet.Text(target["project"]) != snapshot.Project || snapshot.Project == "" || !ok || n != snapshot.Number || n <= 0 || elapsed != snapshot.ElapsedSeconds || elapsed <= 0 || math.IsNaN(elapsed) || math.IsInf(elapsed, 0) || strings.TrimSpace(snapshot.Cause) == "" {
			return errors.New("保存済み遅延有事と一致しません")
		}
	}
	if matches != 1 {
		return errors.New("保存済み遅延有事を一意に確認できません")
	}
	return nil
}

// slowIncidentIssueIntent builds the high-priority issue for the slow incident.
func slowIncidentIssueIntent(snapshot operations.SlowIncidentFiling) slowIncidentIntent {
	reason := fmt.Sprintf("遅延有事の作業 %s、経過時間 %g秒", snapshot.WorkKey, snapshot.ElapsedSeconds)
	body := fmt.Sprintf("## 遅延有事の不具合\n\n対象: %s #%d\n\n原因: %s\n\n優先度理由: %s\n", inert(snapshot.Project), snapshot.Number, inert(snapshot.Cause), inert(reason))
	if snapshot.DirectPR {
		body += "\n直接PRが要る\n"
	}
	in := slowIncidentIntent{IncidentID: snapshot.IncidentID, Cause: snapshot.Cause, Project: snapshot.Project, Prepared: Prepared{Title: "遅延有事の不具合: " + snapshot.WorkKey, Body: body, Fields: Record{"priority": "high", "priorityReason": reason}}}
	return in
}
