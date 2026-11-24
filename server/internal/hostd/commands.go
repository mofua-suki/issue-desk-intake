package hostd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"strings"
	"time"
)

func (s *Service) Commands(ctx context.Context, host, by string, body map[string]any, write bool) (any, error) {
	if !fleet.ValidHost(host) {
		return nil, problem(400, "hostの形が違います。")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var raw []byte
	if err = tx.QueryRowContext(ctx, "SELECT hostd_health FROM hosts WHERE host=$1 FOR UPDATE", host).Scan(&raw); errors.Is(err, sql.ErrNoRows) {
		return nil, problem(404, "PCが未確認です。")
	}
	if err != nil {
		return nil, err
	}
	health := map[string]any{}
	_ = json.Unmarshal(raw, &health)
	if health == nil {
		health = map[string]any{}
	}
	state := fleet.Object(health["commands"])
	if state == nil {
		state = map[string]any{"command": nil, "pin": nil, "results": []any{}}
	}
	created := false
	if write {
		kind, tag, ttl, err := validHostdCommand(body)
		if err != nil {
			return nil, err
		}
		created, err = s.issueHostdCommand(ctx, tx, host, by, body, kind, tag, ttl, health, state)
		if err != nil {
			return nil, err
		}
	}
	out := hostdCommandsView(state, s.now(), write, created)
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// validHostdCommand は hostd への指示の項目・種類・tag・期限の形を検査する。
func validHostdCommand(body map[string]any) (string, any, float64, error) {
	if !only(body, "id", "kind", "tag", "ttlSeconds") || !idPattern.MatchString(fleet.Text(body["id"])) || !one(body["kind"], "install", "rollback", "pin", "follow") {
		return "", nil, 0, problem(400, "hostd指示の形が違います。")
	}
	kind := fleet.Text(body["kind"])
	tag := body["tag"]
	switch kind {
	case "follow":
		if tag != nil {
			return "", nil, 0, problem(400, "followにtagは指定できません。")
		}
	case "rollback":
		if tag == nil {
			tag = "previous"
		}
		if !one(tag, "previous", "last-good") {
			return "", nil, 0, problem(400, "切り戻し先の形が違います。")
		}
	default:
		if !regexp.MustCompile(`^[0-9a-f]{12}(?:-client-python|-python)?$`).MatchString(fleet.Text(tag)) {
			return "", nil, 0, problem(400, "tagの形が違います。")
		}
	}
	ttl := 3600.0
	if value, exists := body["ttlSeconds"]; exists {
		if !integer(value, 60, 86400) {
			return "", nil, 0, problem(400, "指示の期限は60〜86400秒の整数です。")
		}
		ttl = fleet.Number(value)
	}
	return kind, tag, ttl, nil
}

// issueHostdCommand は発行履歴にある版への指示を、未実行の指示が無いときだけ記録する。同じIDの指示の送り直しは記録しない。
func (s *Service) issueHostdCommand(ctx context.Context, tx *sql.Tx, host, by string, body map[string]any, kind string, tag any, ttl float64, health, state map[string]any) (bool, error) {
	if kind == "install" || kind == "pin" {
		if err := checkReleasedTag(ctx, tx, host, tag); err != nil {
			return false, err
		}
	}
	pending := fleet.Object(state["command"])
	if previous := previousHostdCommand(state, body["id"]); previous != nil {
		if previous["kind"] != kind || previous["tag"] != tag {
			return false, problem(409, "同じIDの指示内容が違います。")
		}
		return false, nil
	}
	now := s.now()
	if fleet.Number(pending["expiresAt"]) > float64(now.UnixMilli()) {
		return false, problem(409, "未実行の指示があります。")
	}
	if kind == "install" {
		if err := clearFailedImage(ctx, tx, host, tag); err != nil {
			return false, err
		}
	}
	state["command"] = map[string]any{"id": body["id"], "kind": kind, "tag": tag, "issuedAt": now.UnixMilli(), "expiresAt": now.Add(time.Duration(ttl) * time.Second).UnixMilli(), "by": by}
	health["commands"] = state
	if _, err := tx.ExecContext(ctx, "UPDATE hosts SET hostd_health=$2::jsonb WHERE host=$1", host, encode(health)); err != nil {
		return false, err
	}
	return true, nil
}

// commandImageFlavor は指示の tag からイメージの種類を決める。
func commandImageFlavor(tag any) string {
	if strings.HasSuffix(fleet.Text(tag), "-python") {
		return "-python"
	}
	return ""
}

// checkReleasedTag は入れる・固定する版が、このPCか全PC向けの発行履歴にあるかを確かめる。
func checkReleasedTag(ctx context.Context, tx *sql.Tx, host string, tag any) error {
	var registered bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM client_release_history WHERE host IN($1,'*')AND flavor=$2 AND left(revision,12)=$3)", host, commandImageFlavor(tag), fleet.Text(tag)[:12]).Scan(&registered); err != nil {
		return err
	}
	if !registered {
		return problem(400, "tagが発行履歴に登録されていません。")
	}
	return nil
}

// previousHostdCommand は同じIDの未実行の指示か結果の記録を返す。無ければ nil を返す。
func previousHostdCommand(state map[string]any, id any) map[string]any {
	pending := fleet.Object(state["command"])
	var previous map[string]any
	if pending["id"] == id {
		previous = pending
	}
	for _, raw := range fleet.List(state["results"]) {
		result := fleet.Object(raw)
		if result["id"] == id {
			previous = result
		}
	}
	return previous
}

// clearFailedImage は入れ直す版の失敗の記録を消す。
// 失敗した版は hostd が選ばない。記録のtagは -client-python 付きで、指示のtagは -python 付きなので、版の12桁で突き合わせる。install は利用者が入れ直しを承認した操作なので、同じ版の失敗の記録を消して次の周期で再試行させる。
func clearFailedImage(ctx context.Context, tx *sql.Tx, host string, tag any) error {
	_, err := tx.ExecContext(ctx, "DELETE FROM host_image_results WHERE host=$1 AND flavor=$2 AND left(tag,12)=$3 AND status='failed'", host, commandImageFlavor(tag), fleet.Text(tag)[:12])
	return err
}

// hostdCommandsView は未実行の指示・固定の版・最後の結果を応答の形にする。
func hostdCommandsView(state map[string]any, now time.Time, write, created bool) map[string]any {
	command, pin := commandView(state, now)
	var last any
	results := fleet.List(state["results"])
	if len(results) > 0 {
		last = fleet.Object(results[len(results)-1])
		r := fleet.Object(last)
		last = map[string]any{"id": r["id"], "kind": r["kind"], "ok": r["ok"], "reason": r["reason"], "at": r["at"]}
	}
	out := map[string]any{"command": command, "pin": pin, "result": last}
	if write {
		out["created"] = created
	}
	return out
}
