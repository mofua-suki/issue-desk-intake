package proposals

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"issue-desk-intake/server/internal/dispatch"
	"issue-desk-intake/server/internal/fleet"
)

func proposalWriters(config Object) []string {
	intake := obj(config["intake"])
	if writers, has := intake["proposalWriters"]; has {
		return fleet.Strings(writers)
	}
	if writers, has := intake["workerAccounts"]; has {
		return fleet.Strings(writers)
	}
	out := []string{}
	for _, v := range obj(obj(config["assignments"])["workerAccounts"]) {
		out = append(out, str(v))
	}
	return out
}
func (s *Service) hostReady(ctx context.Context, host string) (bool, error) {
	var ready bool
	var heartbeat time.Time
	var raw []byte
	e := s.o.Postgres.QueryRowContext(ctx, `SELECT ready,heartbeat_at,capabilities FROM hosts WHERE host=$1`, host).Scan(&ready, &heartbeat, &raw)
	if e == sql.ErrNoRows {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	capabilities := Object{}
	if len(raw) > 0 && json.Unmarshal(raw, &capabilities) != nil {
		return false, fmt.Errorf("ホストの能力を読み取れません")
	}
	return ready && capabilities["workerTokenKind"] == "worker" && !heartbeat.Before(s.now().Add(-15*time.Minute)) && !heartbeat.After(s.now().Add(15*time.Minute)), nil
}
func (s *Service) repostHost(ctx context.Context, config Object, repo string) (string, error) {
	assignments := obj(config["assignments"])
	implementers := fleet.Strings(obj(assignments["implementers"])[repo])
	home := str(obj(assignments["repositories"])[repo])
	if len(implementers) == 0 && home != "" {
		implementers = []string{home}
	}
	ordered := []string{}
	for _, host := range implementers {
		if host == home {
			ordered = append(ordered, host)
		}
	}
	for _, host := range implementers {
		if host != home {
			ordered = append(ordered, host)
		}
	}
	for _, host := range ordered {
		if !fleet.ValidHost(host) {
			continue
		}
		ready, e := s.hostReady(ctx, host)
		if e != nil {
			return "", e
		}
		if ready {
			return host, nil
		}
	}
	return "", fail(409, "投稿し直せる、監視用認証で動作中の担当PCを確認できません。")
}
func (s *Service) command(ctx context.Context, id string) (*dispatch.Command, error) {
	var raw []byte
	e := s.o.Postgres.QueryRowContext(ctx, `SELECT row_to_json(c) FROM job_commands c WHERE id=$1`, id).Scan(&raw)
	if e == sql.ErrNoRows {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var row struct {
		ID         string `json:"id"`
		Repo       string `json:"repo"`
		Issue      int64  `json:"issue"`
		Kind       string `json:"kind"`
		Payload    Object `json:"payload"`
		TargetHost string `json:"target_host"`
		Status     string `json:"status"`
		Result     Object `json:"result"`
	}
	if e = json.Unmarshal(raw, &row); e != nil {
		return nil, e
	}
	return &dispatch.Command{ID: row.ID, Repo: row.Repo, Issue: row.Issue, Kind: row.Kind, Payload: row.Payload, TargetHost: row.TargetHost, Status: row.Status, Result: row.Result}, nil
}
func repostAnswer(data, r Object, command *dispatch.Command, created bool) Object {
	return Object{"commandId": command.ID, "targetHost": command.TargetHost, "status": command.Status, "created": created, "project": data["project"], "number": data["number"], "commentId": data["commentId"], "proposalId": r["id"], "result": command.Result}
}
func (s *Service) Repost(ctx context.Context, data Object, via string) (Object, error) {
	if !repoPattern.MatchString(str(data["project"])) || !validNumber(data["number"]) || !validNumber(data["commentId"]) {
		return nil, fail(400, "project・number・commentIdを指定してください。")
	}
	if e := s.o.Project(str(data["project"])); e != nil {
		return nil, e
	}
	if s.o.Postgres == nil {
		return nil, fail(503, "指示の共有保存先を利用できません。")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	config, e := s.o.Config()
	if e != nil {
		return nil, e
	}
	rejectionKey := fmt.Sprintf("rejection:%s#%d:%d", str(data["project"]), num(data["number"]), num(data["commentId"]))
	r, e := s.repostableRejection(ctx, rejectionKey, data, via, config)
	if e != nil {
		return nil, e
	}
	key := strings.Replace(rejectionKey, "rejection:", "repost:", 1)
	saved, e := s.read(ctx, key)
	if e != nil {
		return nil, e
	}
	queue := &dispatch.Queue{DB: s.o.Postgres}
	payload := Object{"v": 1, "commentId": data["commentId"], "proposalId": r["id"]}
	enqueue := func(id, host string) (*dispatch.Command, error) {
		return queue.EnqueueCommand(ctx, dispatch.Command{ID: id, Repo: str(data["project"]), Issue: num(data["number"]), Kind: "repost_proposal", Payload: payload, TargetHost: host})
	}
	target := ""
	if commandID := str(saved["commandId"]); commandID != "" {
		current, e := s.command(ctx, commandID)
		if e != nil {
			return nil, e
		}
		if current == nil {
			command, e := enqueue(commandID, str(saved["targetHost"]))
			if e != nil {
				return nil, e
			}
			return repostAnswer(data, r, command, true), nil
		}
		if current.Status != "done" && current.Status != "failed" {
			ready, e := s.hostReady(ctx, current.TargetHost)
			if e != nil {
				return nil, e
			}
			if ready {
				return repostAnswer(data, r, current, false), nil
			}
			target, e = s.repostHost(ctx, config, str(data["project"]))
			if e != nil {
				return nil, e
			}
			latest, e := s.cancelRepostCommand(ctx, current.ID)
			if e != nil {
				return nil, e
			}
			if latest != nil {
				return repostAnswer(data, r, latest, false), nil
			}
		}
	}
	if target == "" {
		target, e = s.repostHost(ctx, config, str(data["project"]))
		if e != nil {
			return nil, e
		}
	}
	commandID := id()
	previous := arr(saved["previous"])
	if previous == nil {
		previous = []any{}
	}
	if prior := str(saved["commandId"]); prior != "" {
		previous = append(previous, prior)
	}
	record := Object{"type": "repost", "key": key, "repo": data["project"], "number": data["number"], "commentId": data["commentId"], "id": r["id"], "commandId": commandID, "targetHost": target, "via": via, "requestedAt": s.at(), "previous": previous}
	if e = s.save(ctx, key, record); e != nil {
		return nil, e
	}
	command, e := enqueue(commandID, target)
	if e != nil {
		return nil, e
	}
	return repostAnswer(data, r, command, true), nil
}

// repostableRejection reads the rejected proposal and refuses a repost that is dismissed,
// adopted or rejected for a reason a repost does not fix.
func (s *Service) repostableRejection(ctx context.Context, rejectionKey string, data Object, via string, config Object) (Object, error) {
	r, e := s.read(ctx, rejectionKey)
	if e != nil {
		return nil, e
	}
	if r["type"] != "rejection" {
		return nil, fail(404, "拒否した案の記録がありません。")
	}
	if strings.HasPrefix(via, "token:") {
		created := r["checkedAt"]
		if created == nil {
			created = r["at"]
		}
		if e := s.instruction(data["instructionAt"], created); e != nil {
			return nil, e
		}
	}
	mark, e := s.read(ctx, fmt.Sprintf("dismissal:%s#%d:%s", str(r["repo"]), num(r["number"]), str(r["id"])))
	if e != nil {
		return nil, e
	}
	if mark["status"] == "dismissed" && str(r["fingerprint"]) != "" && mark["fingerprint"] == r["fingerprint"] {
		return nil, fail(409, "この案は終了しています。先に取り消してください。")
	}
	if r["status"] != "rejected" || str(r["id"]) == "" || str(config["gitUser"]) == "" || r["author"] != config["gitUser"] {
		return nil, fail(409, "現在の拒否理由では投稿し直せません。")
	}
	for _, writer := range proposalWriters(config) {
		if writer == r["author"] {
			return nil, fail(409, "この書き手は現在の設定で投稿できます。")
		}
	}
	adopted, e := s.read(ctx, fmt.Sprintf("%s#%d:%s", str(r["repo"]), num(r["number"]), str(r["id"])))
	if e != nil {
		return nil, e
	}
	if adopted["type"] == "proposal" {
		return nil, fail(409, "同じ案は採用済みです。")
	}
	return r, nil
}

// cancelRepostCommand cancels a pending command whose host no longer qualifies. When the
// command moved on first, it returns that command unless it failed.
func (s *Service) cancelRepostCommand(ctx context.Context, id string) (*dispatch.Command, error) {
	result, _ := json.Marshal(Object{"result": "cancelled", "reason": "宛先が条件を満たさなくなったため別の担当PCへ入れ替えます。"})
	changed, e := s.o.Postgres.ExecContext(ctx, `UPDATE job_commands SET status='failed',result=$2,acked_at=now() WHERE id=$1 AND status IN ('queued','acked')`, id, result)
	if e != nil {
		return nil, e
	}
	count, e := changed.RowsAffected()
	if e != nil {
		return nil, e
	}
	if count == 0 {
		latest, e := s.command(ctx, id)
		if e != nil {
			return nil, e
		}
		if latest != nil && latest.Status != "failed" {
			return latest, nil
		}
	}
	return nil, nil
}
