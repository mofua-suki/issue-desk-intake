package hostd

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"time"
)

type rollout struct {
	ID                               int64
	Revision, Canary, Status, Flavor string
	Previous                         []byte
	Cycles                           int
	Last                             sql.NullString
	Started                          time.Time
	Reason, Rollback                 sql.NullString
}

// selectedRelease is the effective registration, before global rollout gating.
func selectedRelease(ctx context.Context, tx *sql.Tx, host, flavor string) (*Release, error) {
	r := &Release{}
	err := tx.QueryRowContext(ctx, "SELECT host,flavor,registry,tag,digest,revision,kind,created_at FROM releases WHERE host IN($1,'*') AND flavor=$2 ORDER BY(host=$1)DESC LIMIT 1", host, flavor).Scan(&r.Host, &r.Flavor, &r.Registry, &r.Tag, &r.Digest, &r.Revision, &r.Kind, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

const rolloutColumns = "id,revision,canary,status,previous,success_cycles,last_cycle_id,started_at,reason,rollback_command_id,flavor"

func readRollout(row *sql.Row) (rollout, error) {
	r := rollout{}
	err := row.Scan(&r.ID, &r.Revision, &r.Canary, &r.Status, &r.Previous, &r.Cycles, &r.Last, &r.Started, &r.Reason, &r.Rollback, &r.Flavor)
	return r, err
}

func (s *Service) tickRollout(ctx context.Context, id int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Register and Desired take the same locks: releases, rollout, then host.
	if _, err = tx.ExecContext(ctx, "LOCK TABLE releases IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return err
	}
	r, err := readRollout(tx.QueryRowContext(ctx, "SELECT "+rolloutColumns+" FROM release_rollouts WHERE id=$1 FOR UPDATE", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// A registration may have ended/replaced this row while Tick waited.
	if r.Status == "canary" {
		if err = s.evaluateRollout(ctx, tx, &r); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Service) rolloutDesired(ctx context.Context, tx *sql.Tx, release *Release, host string) (*Release, error) {
	r, err := readRollout(tx.QueryRowContext(ctx, "SELECT "+rolloutColumns+" FROM release_rollouts WHERE host='*' AND flavor=$1 ORDER BY id DESC LIMIT 1 FOR UPDATE", release.Flavor))
	if errors.Is(err, sql.ErrNoRows) {
		return release, nil
	}
	if err != nil {
		return nil, err
	}
	// Distribution reads the saved decision; only Tick advances evaluation.
	if release.Host != "*" || r.Revision != release.Revision {
		return release, nil
	}
	if r.Status == "failed" || r.Status == "canary" && host != r.Canary {
		var previous Release
		if err = json.Unmarshal(r.Previous, &previous); err != nil {
			return nil, err
		}
		return &previous, nil
	}
	return release, nil
}

// evaluateRollout requires a locked canary row and the releases table lock.
// started_at is immutable; expiry wins even over a final successful cycle.
func (s *Service) evaluateRollout(ctx context.Context, tx *sql.Tx, r *rollout) error {
	now := s.now()
	reason := ""
	if now.Sub(r.Started) >= 180*time.Minute {
		reason = "timeout"
	}
	release, err := selectedRelease(ctx, tx, r.Canary, r.Flavor)
	if err != nil {
		return err
	}
	if reason == "" && (release == nil || release.Revision != r.Revision) {
		reason = "desired_revision_mismatch"
	}
	host, err := readCanaryHost(ctx, tx, r.Canary)
	if err != nil {
		return err
	}
	cycle := fleet.Object(host.state["cycle"])
	versions := fleet.Object(cycle["versions"])
	tag := r.Revision[:12]
	if r.Flavor == "-python" {
		tag += "-client-python"
	}
	current := versions["current"] == tag
	if reason == "" {
		var failed bool
		if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM host_image_results WHERE host=$1 AND flavor=$2 AND tag=$3 AND status='failed')", r.Canary, r.Flavor, tag).Scan(&failed); err != nil {
			return err
		}
		if failed {
			reason = "image_failed"
		} else if current && cycle["result"] == "failed" && cycle["phase"] != "wait" && fleet.Number(cycle["failures"]) >= 2 {
			reason = "cycle_failed"
		}
		success := r.countCycle(cycle, current)
		if reason == "" && success && r.Cycles >= 3 && host.verified(r.Revision, now) {
			r.Status = "passed"
			reason = "verified"
		}
	}
	if reason != "" && r.Status != "passed" {
		r.Status = "failed"
	}
	var decided any
	if r.Status != "canary" {
		decided = now
	}
	if r.Status == "failed" {
		skip, err := rollbackCanary(ctx, tx, r, release, host.state, current, now)
		if err != nil {
			return err
		}
		if skip != "" {
			reason += ":" + skip
		}
	}
	var savedReason any
	if reason != "" {
		savedReason = reason
	}
	_, err = tx.ExecContext(ctx, "UPDATE release_rollouts SET status=$2,success_cycles=$3,last_cycle_id=$4,decided_at=$5,reason=$6,rollback_command_id=$7 WHERE id=$1", r.ID, r.Status, r.Cycles, r.Last, decided, savedReason, r.Rollback)
	return err
}

// canaryHost は先行PCの行から判定に使う健康状態・心拍・準備・版・退避の印を読んだもの。
type canaryHost struct {
	state     map[string]any
	mark      map[string]any
	heartbeat sql.NullTime
	ready     sql.NullBool
	image     sql.NullString
}

// readCanaryHost は先行PCの行を更新用にロックして読む。行が無ければ空の状態を返す。
func readCanaryHost(ctx context.Context, tx *sql.Tx, canary string) (canaryHost, error) {
	var health, drain []byte
	var h canaryHost
	err := tx.QueryRowContext(ctx, "SELECT hostd_health,heartbeat_at,ready,image_revision,drain FROM hosts WHERE host=$1 FOR UPDATE", canary).Scan(&health, &h.heartbeat, &h.ready, &h.image, &drain)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return h, err
	}
	_ = json.Unmarshal(health, &h.state)
	_ = json.Unmarshal(drain, &h.mark)
	return h, nil
}

// verified は先行PCが新しい版で稼働を続けている（心拍が10分以内・準備完了・版が一致・hostdの退避なし）かを返す。
func (h canaryHost) verified(revision string, now time.Time) bool {
	drained := h.mark["hostd"] != nil || strings.HasPrefix(fleet.Text(h.mark["by"]), "hostd:")
	return h.heartbeat.Valid && !h.heartbeat.Time.After(now) && now.Sub(h.heartbeat.Time) <= 10*time.Minute && h.ready.Bool && strings.HasPrefix(h.image.String, revision[:12]) && !drained
}

// countCycle は新しい版での完了した周期を数え、失敗した周期で数え直す。今回の周期が成功の完了かを返す。
func (r *rollout) countCycle(cycle map[string]any, current bool) bool {
	success := current && cycle["result"] == "success" && cycle["stage"] == "complete" && fleet.Text(cycle["id"]) != ""
	if success && fleet.Text(cycle["id"]) != r.Last.String {
		r.Cycles++
		r.Last = sql.NullString{String: fleet.Text(cycle["id"]), Valid: true}
	} else if current && cycle["result"] == "failed" && cycle["phase"] != "wait" && cycle["phase"] != "start" {
		r.Cycles = 0
	}
	return success
}

// rollbackCanary は失敗した先行PCへ前の版に戻す指示を1回だけ出す。出さないときはその理由を返す。
func rollbackCanary(ctx context.Context, tx *sql.Tx, r *rollout, release *Release, state map[string]any, current bool, now time.Time) (string, error) {
	commands := fleet.Object(state["commands"])
	pending := fleet.Object(commands["command"])
	var globalRevision string
	err := tx.QueryRowContext(ctx, "SELECT revision FROM releases WHERE host='*' AND flavor=$1", r.Flavor).Scan(&globalRevision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	skip := ""
	switch {
	case r.Rollback.Valid:
		skip = "already_issued"
	case release != nil && release.Host != "*":
		skip = "host_override"
	case globalRevision != r.Revision:
		skip = "global_revision_changed"
	case !current:
		skip = "not_switched"
	case commands["pin"] != nil:
		skip = "pinned"
	case fleet.Number(pending["expiresAt"]) > float64(now.UnixMilli()):
		skip = "command_pending"
	}
	if skip == "" {
		sum := sha256.Sum256([]byte("release-canary:" + intString(r.ID)))
		id := hex.EncodeToString(sum[:])[:32]
		if commands == nil {
			commands = map[string]any{"pin": nil, "results": []any{}}
		}
		commands["command"] = map[string]any{"id": id, "kind": "rollback", "tag": "previous", "issuedAt": now.UnixMilli(), "expiresAt": now.Add(time.Hour).UnixMilli(), "by": "automatic:release-canary"}
		if _, err := tx.ExecContext(ctx, "UPDATE hosts SET hostd_health=coalesce(hostd_health,'{}'::jsonb)||jsonb_build_object('commands',$2::jsonb) WHERE host=$1", r.Canary, encode(commands)); err != nil {
			return "", err
		}
		r.Rollback = sql.NullString{String: id, Valid: true}
	}
	return skip, nil
}

func commandView(state map[string]any, now time.Time) (any, any) {
	var command, pin any
	c := fleet.Object(state["command"])
	if fleet.Number(c["expiresAt"]) > float64(now.UnixMilli()) {
		copy := fleet.Object(fleet.Clone(c))
		copy["issuedAt"] = iso(time.UnixMilli(int64(fleet.Number(c["issuedAt"]))))
		copy["expiresAt"] = iso(time.UnixMilli(int64(fleet.Number(c["expiresAt"]))))
		command = copy
	}
	p := fleet.Object(state["pin"])
	if p != nil {
		pin = map[string]any{"id": p["id"], "tag": p["tag"], "by": p["by"], "since": iso(time.UnixMilli(int64(fleet.Number(p["at"]))))}
	}
	return command, pin
}
