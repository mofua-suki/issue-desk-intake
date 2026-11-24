package dispatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"issue-desk-intake/server/internal/fleet"
)

type Command struct {
	ID         string         `json:"id"`
	Seq        int64          `json:"seq"`
	Repo       string         `json:"repo"`
	Issue      int64          `json:"issue"`
	Kind       string         `json:"kind"`
	Payload    map[string]any `json:"payload"`
	TargetHost string         `json:"targetHost"`
	Status     string         `json:"status"`
	CreatedAt  string         `json:"createdAt"`
	AckedAt    *string        `json:"ackedAt"`
	Result     map[string]any `json:"result"`
}

const commandColumns = "id,seq,repo,issue,kind,payload,target_host,status,created_at,acked_at,result"

func scanCommand(row scanner) (*Command, error) {
	command := &Command{}
	var payload, result []byte
	var created time.Time
	var acked sql.NullTime
	err := row.Scan(&command.ID, &command.Seq, &command.Repo, &command.Issue, &command.Kind, &payload, &command.TargetHost, &command.Status, &created, &acked, &result)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(payload, &command.Payload); err != nil {
		return nil, err
	}
	if len(result) > 0 {
		if err = json.Unmarshal(result, &command.Result); err != nil {
			return nil, err
		}
	}
	command.CreatedAt, command.AckedAt = iso(created), nullISO(acked)
	return command, nil
}

func (q *Queue) EnqueueCommand(ctx context.Context, command Command) (*Command, error) {
	if !commandID.MatchString(command.ID) || !fleet.ValidRepo(command.Repo) || command.Issue < 1 || !fleet.ValidHost(command.TargetHost) {
		return nil, problem(400, "指示の対象・ID・送り先が不正です。")
	}
	valid := false
	for _, kind := range []string{"close", "stop", "instruction", "repost_proposal"} {
		if command.Kind == kind {
			valid = true
		}
	}
	if !valid {
		return nil, problem(400, "指示の種類が不正です。")
	}
	if command.Payload == nil {
		command.Payload = map[string]any{}
	}
	if _, err := q.DB.ExecContext(ctx, "INSERT INTO job_commands(id,repo,issue,kind,payload,target_host) VALUES($1,$2,$3,$4,$5::jsonb,$6) ON CONFLICT(id) DO NOTHING", command.ID, command.Repo, command.Issue, command.Kind, encode(command.Payload), command.TargetHost); err != nil {
		return nil, err
	}
	saved, err := scanCommand(q.DB.QueryRowContext(ctx, "SELECT "+commandColumns+" FROM job_commands WHERE id=$1", command.ID))
	if err != nil {
		return nil, err
	}
	if saved.Repo != command.Repo || saved.Issue != command.Issue || saved.Kind != command.Kind || saved.TargetHost != command.TargetHost {
		return nil, problem(409, "指示IDが別の指示で使われています。")
	}
	return saved, nil
}

func (q *Queue) Commands(ctx context.Context, host string, after int64) ([]*Command, error) {
	rows, err := q.DB.QueryContext(ctx, "SELECT "+commandColumns+" FROM job_commands WHERE target_host=$1 AND status IN('queued','acked') AND seq>$2 ORDER BY seq LIMIT 50", host, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []*Command{}
	for rows.Next() {
		command, err := scanCommand(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, command)
	}
	return list, rows.Err()
}

func (q *Queue) AckCommand(ctx context.Context, id, host, status string, result map[string]any) (*Command, error) {
	if status != "acked" && status != "done" && status != "failed" || status != "acked" && result == nil {
		return nil, problem(400, "statusとresultが不正です。")
	}
	if !commandID.MatchString(id) {
		return nil, problem(404, "指示が見つかりません。")
	}
	tx, err := q.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	current, err := scanCommand(tx.QueryRowContext(ctx, "SELECT "+commandColumns+" FROM job_commands WHERE id=$1 FOR UPDATE", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, problem(404, "指示が見つかりません。")
	}
	if err != nil {
		return nil, err
	}
	if current.TargetHost != host {
		return nil, problem(404, "指示が見つかりません。")
	}
	if current.Status == status && (status == "acked" || same(current.Result, result)) {
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return current, nil
	}
	if current.Status == "done" || current.Status == "failed" || status == "acked" && current.Status != "queued" {
		return nil, problem(409, "この指示は既に終了しています。")
	}
	if status == "acked" {
		result = nil
	}
	updated, err := scanCommand(tx.QueryRowContext(ctx, "UPDATE job_commands SET status=$2,result=$3::jsonb,acked_at=coalesce(acked_at,now()) WHERE id=$1 RETURNING "+commandColumns, id, status, encode(result)))
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}
