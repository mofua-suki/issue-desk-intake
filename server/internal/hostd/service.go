package hostd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/hosts"
	"regexp"
	"time"
)

type Service struct {
	Targets     func() (map[string]string, map[string]map[string]any, error)
	DB          *sql.DB
	Now         func() time.Time
	Assignments func() (*fleet.Assignments, error)
	IssueState  func(context.Context, string, int64, string) error
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
func problem(status int, message string) error { return contract.NewProblem(status, message, "") }
func encode(v any) string                      { b, _ := json.Marshal(v); return string(b) }
func one(v any, values ...string) bool {
	for _, x := range values {
		if v == x {
			return true
		}
	}
	return false
}
func only(v map[string]any, fields ...string) bool {
	for k := range v {
		if !one(k, fields...) {
			return false
		}
	}
	return true
}
func iso(v time.Time) string { return contract.ISO(v) }

var tagPattern = regexp.MustCompile(`^[0-9a-f]{12}(?:-[a-z0-9-]+)?$`)
var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func (s *Service) Drain(ctx context.Context, host string, enable bool, body map[string]any) (any, error) {
	if enable && !only(body, "until") {
		return nil, problem(400, "停止期限の形が違います。")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var raw []byte
	if err = tx.QueryRowContext(ctx, "SELECT drain FROM hosts WHERE host=$1 FOR UPDATE", host).Scan(&raw); errors.Is(err, sql.ErrNoRows) {
		return nil, problem(409, "PCが未確認です。")
	}
	if err != nil {
		return nil, err
	}
	now := s.now()
	var old map[string]any
	_ = json.Unmarshal(raw, &old)
	delete(old, "hostd")
	manual := hosts.ActiveDrain(old, now, hosts.DefaultDuration())
	if fleet.Text(manual["by"]) == "hostd:"+host {
		manual = nil
	}
	var drain map[string]any
	if enable {
		drain, err = hostdDrain(ctx, tx, host, body, now)
		if err != nil {
			return nil, err
		}
	}
	saved := drain
	if manual != nil {
		saved = manual
		if enable {
			saved["hostd"] = drain
		}
	}
	var value any
	if saved != nil {
		value = encode(saved)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE hosts SET drain=$2::jsonb WHERE host=$1", host, value); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	out := map[string]any{"host": host, "drain": drain}
	if enable {
		out["manualDrain"] = manual
	}
	return out, nil
}

// hostdDrain は hostd がイメージを入れ替える間の停止の記録を作る。期限の指定が無ければ既定の長さにし、直近2回の更新が失敗していれば15分に縮める。
func hostdDrain(ctx context.Context, tx *sql.Tx, host string, body map[string]any, now time.Time) (map[string]any, error) {
	until := now.Add(hosts.DefaultDuration())
	if v, exists := body["until"]; exists && v != nil {
		parsed, e := hosts.ParseDrainTime(v)
		if e != nil || !parsed.After(now) {
			return nil, problem(400, "停止期限は将来の日時です。")
		}
		until = parsed
	}
	var failures int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM(SELECT status FROM host_image_results WHERE host=$1 ORDER BY at DESC LIMIT 2)r WHERE status='failed'", host).Scan(&failures); err != nil {
		return nil, err
	}
	if failures == 2 && until.After(now.Add(15*time.Minute)) {
		until = now.Add(15 * time.Minute)
	}
	return map[string]any{"by": "hostd:" + host, "reason": "client image update", "at": iso(now), "until": iso(until)}, nil
}

type Release struct {
	Host      string    `json:"host"`
	Flavor    string    `json:"flavor"`
	Registry  string    `json:"registry"`
	Tag       string    `json:"tag"`
	Digest    string    `json:"digest"`
	Revision  string    `json:"revision"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Service) Desired(ctx context.Context, host, flavor, current string) (any, error) {
	if !one(flavor, "", "-python") || current != "" && !tagPattern.MatchString(current) && !shaPattern.MatchString(current) {
		return nil, problem(400, "flavorまたはcurrentRevisionの形が違います。")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "LOCK TABLE releases IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return nil, err
	}
	release, err := selectedRelease(ctx, tx, host, flavor)
	if err != nil {
		return nil, err
	}
	if release != nil {
		release, err = s.rolloutDesired(ctx, tx, release, host)
		if err != nil {
			return nil, err
		}
	}
	revision, heartbeatValue, healthValue, err := s.hostHeartbeat(ctx, tx, host)
	if err != nil {
		return nil, err
	}
	if current == "" {
		current = revision.String
	}
	current, err = resolveCurrentRevision(ctx, tx, flavor, current)
	if err != nil {
		return nil, err
	}
	if release != nil {
		release.Kind, err = releaseKind(ctx, tx, release, current)
		if err != nil {
			return nil, err
		}
	}
	failed, err := failedImageTags(ctx, tx, host, flavor)
	if err != nil {
		return nil, err
	}
	commands := fleet.Object(healthValue["commands"])
	command, pin := commandView(commands, s.now())
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{"desired": release, "heartbeat": heartbeatValue, "failedTags": failed, "command": command, "pin": pin}, nil
}

// hostHeartbeat はPCの最後の報告を読み、応答に出す形と hostd の状態を返す。PCが未登録なら空を返す。
func (s *Service) hostHeartbeat(ctx context.Context, tx *sql.Tx, host string) (sql.NullString, any, map[string]any, error) {
	var revision sql.NullString
	var implementing, publishing sql.NullInt64
	var heartbeat time.Time
	var ready bool
	var drain, health []byte
	err := tx.QueryRowContext(ctx, "SELECT revision,implementing,publishing,heartbeat_at,ready,drain,hostd_health FROM hosts WHERE host=$1", host).Scan(&revision, &implementing, &publishing, &heartbeat, &ready, &drain, &health)
	var heartbeatValue any
	var healthValue map[string]any
	if err == nil {
		var mark map[string]any
		_ = json.Unmarshal(drain, &mark)
		heartbeatValue = map[string]any{"revision": nullText(revision), "implementing": nullInt(implementing), "publishing": nullInt(publishing), "heartbeat_at": iso(heartbeat), "ready": ready, "drain": hosts.ActiveDrain(mark, s.now(), hosts.DefaultDuration())}
		_ = json.Unmarshal(health, &healthValue)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return revision, nil, nil, err
	}
	return revision, heartbeatValue, healthValue, nil
}

// resolveCurrentRevision はタグの12桁で示された今の版を、発行履歴で1つに決まれば完全な revision に、決まらなければ unknown にする。
func resolveCurrentRevision(ctx context.Context, tx *sql.Tx, flavor, current string) (string, error) {
	if current != "" && !shaPattern.MatchString(current) {
		var matches int
		var full string
		if err := tx.QueryRowContext(ctx, "SELECT count(DISTINCT revision),coalesce(min(revision),'') FROM client_release_history WHERE flavor=$1 AND left(revision,12)=$2", flavor, current[:12]).Scan(&matches, &full); err != nil {
			return "", err
		}
		if matches == 1 {
			current = full
		} else {
			current = "unknown"
		}
	}
	return current, nil
}

// failedImageTags はPCで失敗したイメージのタグを読む。
func failedImageTags(ctx context.Context, tx *sql.Tx, host, flavor string) ([]string, error) {
	failed := []string{}
	rows, err := tx.QueryContext(ctx, "SELECT tag FROM host_image_results WHERE host=$1 AND flavor=$2 AND status='failed'", host, flavor)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var tag string
		if err = rows.Scan(&tag); err != nil {
			rows.Close()
			return nil, err
		}
		failed = append(failed, tag)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return failed, nil
}

func releaseKind(ctx context.Context, tx *sql.Tx, release *Release, current string) (string, error) {
	if current == release.Revision {
		return "lazy", nil
	}
	rows, err := tx.QueryContext(ctx, "SELECT revision,base_revision,kind FROM client_release_history WHERE host=$1 AND flavor=$2", release.Host, release.Flavor)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	history := map[string][2]string{}
	for rows.Next() {
		var rev, kind string
		var base sql.NullString
		if err = rows.Scan(&rev, &base, &kind); err != nil {
			return "", err
		}
		history[rev] = [2]string{base.String, kind}
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	kind := "lazy"
	seen := map[string]bool{}
	rev := release.Revision
	for rev != "" && rev != current && !seen[rev] {
		seen[rev] = true
		entry, ok := history[rev]
		if !ok {
			return "align", nil
		}
		if entry[1] == "align" {
			kind = "align"
		} else if entry[1] == "soon" && kind != "align" {
			kind = "soon"
		}
		rev = entry[0]
	}
	if rev != current {
		return "align", nil
	}
	return kind, nil
}

func nullInt(v sql.NullInt64) any {
	if v.Valid {
		return v.Int64
	}
	return nil
}
func nullText(v sql.NullString) any {
	if v.Valid {
		return v.String
	}
	return nil
}
