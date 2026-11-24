package hosts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"

	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
)

type Service struct {
	StartedAt      time.Time
	DB             *sql.DB
	Now            func() time.Time
	historyMu      sync.Mutex
	historyCleaned map[string]time.Time
}

func (s *Service) clock() time.Time {
	if s.Now != nil {
		return s.Now().UTC().Truncate(time.Millisecond)
	}
	return time.Now().UTC().Truncate(time.Millisecond)
}
func invalid(message string) error { return contract.NewProblem(400, message, "") }
func finite(value any, min, max float64) bool {
	v, ok := value.(float64)
	return ok && !math.IsNaN(v) && !math.IsInf(v, 0) && v >= min && v <= max
}
func integer(value any, min, max float64) bool {
	return finite(value, min, max) && math.Trunc(fleet.Number(value)) == fleet.Number(value)
}
func text(value any, max int) bool {
	v, ok := value.(string)
	return ok && contract.UTF16Length(v) <= max
}
func boolValue(value any) bool { _, ok := value.(bool); return ok }
func encode(value any) any {
	if value == nil {
		return nil
	}
	raw, _ := json.Marshal(value)
	return raw
}
func only(body map[string]any, keys ...string) bool { return contract.OnlyKeys(body, keys...) == nil }

func Validate(body map[string]any) (map[string]any, error) {
	known := strings.Fields("revision cliVersions load busy implementing publishing capabilities ready checks imageRevision capacity intakeHold")
	if !only(body, known...) {
		return nil, invalid("本文に使えない項目があります。")
	}
	value := map[string]any{"revision": nil, "cliVersions": nil, "load": nil, "busy": false, "implementing": nil, "publishing": nil, "capabilities": nil, "ready": false, "checks": nil, "imageRevision": nil, "capacity": nil, "intakeHold": nil}
	for key, item := range body {
		value[key] = item
	}
	if value["revision"] != nil && !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(fleet.Text(value["revision"])) {
		return nil, invalid("revisionの形が違います。")
	}
	if value["cliVersions"] != nil && !ValidCLIVersions(value["cliVersions"]) {
		return nil, invalid("cliVersionsの形が違います。")
	}
	if value["load"] != nil {
		load := fleet.Object(value["load"])
		if load == nil || !only(load, strings.Fields("cpuPercent freeMemoryMB freeDiskMB gpuPercent usedVramMB freeVramMB loadAverage1m logicalCpuCount")...) {
			return nil, invalid("loadの形が違います。")
		}
		for key, item := range load {
			if !finite(item, 0, math.MaxFloat64) || key == "logicalCpuCount" && !integer(item, 1, 9007199254740991) {
				return nil, invalid("負荷の値が不正です。")
			}
		}
	}
	if !boolValue(value["busy"]) || !boolValue(value["ready"]) {
		return nil, invalid("busyとreadyは真偽値です。")
	}
	for _, key := range []string{"implementing", "publishing", "capacity"} {
		max := float64(9007199254740991)
		if key == "capacity" {
			max = 8
		}
		if value[key] != nil && !integer(value[key], 0, max) {
			return nil, invalid(key + "の形が違います。")
		}
	}
	for key, max := range map[string]int{"capabilities": 4096, "checks": 16384} {
		if value[key] != nil {
			object := fleet.Object(value[key])
			if object == nil {
				return nil, invalid(key + "はオブジェクトです。")
			}
			raw, _ := json.Marshal(object)
			if contract.UTF16Length(string(raw)) > max {
				return nil, invalid(key + "が大きすぎます。")
			}
		}
	}
	if value["imageRevision"] != nil && !regexp.MustCompile(`^[0-9A-Za-z._:@-]{1,100}$`).MatchString(fleet.Text(value["imageRevision"])) {
		return nil, invalid("imageRevisionの形が違います。")
	}
	if value["intakeHold"] != nil {
		hold := fleet.Object(value["intakeHold"])
		reasons, ok := hold["reasons"].([]any)
		if hold == nil || len(hold) != 1 || !ok || len(reasons) > 8 {
			return nil, invalid("intakeHoldの形が違います。")
		}
		for _, item := range reasons {
			row := fleet.Object(item)
			kind := fleet.Text(row["kind"])
			if !oneOf(kind, "swap_wait", "codex_unavailable", "environment_hold", "usage_limit", "color_standby", "scan_not_ready") || !validTime(row["since"]) || !text(row["detail"], 200) {
				return nil, invalid("停止理由の形が違います。")
			}
			keys := []string{"kind", "since", "detail"}
			if kind == "usage_limit" {
				keys = append(keys, "provider", "until", "source", "limitKind")
				if !text(row["provider"], 50) || fleet.Text(row["provider"]) == "" || !validTime(row["until"]) || !text(row["source"], 50) || fleet.Text(row["source"]) == "" || !oneOf(fleet.Text(row["limitKind"]), "five_hour", "seven_day", "unknown") {
					return nil, invalid("使用上限の停止理由が不正です。")
				}
			}
			if row == nil || len(row) != len(keys) || !only(row, keys...) {
				return nil, invalid("停止理由に使えない項目があります。")
			}
		}
	}
	return value, nil
}

func validTime(value any) bool {
	v, ok := value.(string)
	if !ok || len(v) > 40 {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, v)
	return err == nil
}
func oneOf(value string, keys ...string) bool {
	for _, key := range keys {
		if value == key {
			return true
		}
	}
	return false
}

func (s *Service) Put(ctx context.Context, host string, body map[string]any) (map[string]any, error) {
	if !fleet.ValidHost(host) {
		return nil, contract.NewProblem(404, "PC名の形が違います。", "")
	}
	value, err := Validate(body)
	if err != nil {
		return nil, err
	}
	at := s.clock()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// The row lock serializes comparison and update with other notifications.
	args := []any{host, value["revision"], encode(value["cliVersions"]), encode(value["load"]), value["busy"], value["implementing"], value["publishing"], at, encode(value["capabilities"]), value["ready"], encode(value["checks"]), value["imageRevision"], value["capacity"], encode(value["intakeHold"])}
	_, err = tx.ExecContext(ctx, `INSERT INTO hosts(host,revision,cli_versions,load,busy,implementing,publishing,heartbeat_at,capabilities,ready,checks,image_revision,capacity,intake_hold)
VALUES($1,$2,$3::jsonb,$4::jsonb,$5,$6,$7,$8,$9::jsonb,$10,$11::jsonb,$12,$13,$14::jsonb) ON CONFLICT(host) DO NOTHING`, args...)
	if err != nil {
		return nil, err
	}
	drain, err := updateHostRow(ctx, tx, host, args, at)
	if err != nil {
		return nil, err
	}
	// Protect the in-memory schedule until commit: failed transactions must retry.
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	last, cleaned := s.historyCleaned[host]
	due := !cleaned || at.Before(last) || !at.Before(last.Add(10*time.Minute))
	historyErr, err := recordLoadSample(ctx, tx, host, value["load"], at, due)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	if historyErr == nil && due {
		if s.historyCleaned == nil {
			s.historyCleaned = make(map[string]time.Time)
		}
		s.historyCleaned[host] = at
	}
	var mark map[string]any
	if len(drain) > 0 && json.Unmarshal(drain, &mark) != nil {
		return nil, errors.New("invalid stored drain")
	}
	return map[string]any{"host": host, "heartbeatAt": at.Format(contract.TimeLayout), "drain": ActiveDrain(mark, at, DefaultDuration()), "ready": value["ready"], "configVersion": nil, "settingsVersion": nil}, nil
}

// updateHostRow は報告のうち変わった列と heartbeat の時刻だけを更新し、保存済みの停止の記録を返す。
func updateHostRow(ctx context.Context, tx *sql.Tx, host string, args []any, at time.Time) ([]byte, error) {
	var drain []byte
	var changed [12]bool
	err := tx.QueryRowContext(ctx, `SELECT drain, revision IS DISTINCT FROM $2, cli_versions IS DISTINCT FROM $3::jsonb, load IS DISTINCT FROM $4::jsonb, busy IS DISTINCT FROM $5, implementing IS DISTINCT FROM $6, publishing IS DISTINCT FROM $7, capabilities IS DISTINCT FROM $9::jsonb, ready IS DISTINCT FROM $10, checks IS DISTINCT FROM $11::jsonb, image_revision IS DISTINCT FROM $12, capacity IS DISTINCT FROM $13, intake_hold IS DISTINCT FROM $14::jsonb FROM hosts WHERE host=$1 AND $8::timestamptz IS NOT NULL FOR UPDATE`, args...).Scan(&drain, &changed[0], &changed[1], &changed[2], &changed[3], &changed[4], &changed[5], &changed[6], &changed[7], &changed[8], &changed[9], &changed[10], &changed[11])
	if err != nil {
		return nil, err
	}
	columns := []string{"revision", "cli_versions", "load", "busy", "implementing", "publishing", "capabilities", "ready", "checks", "image_revision", "capacity", "intake_hold"}
	indices := []int{2, 3, 4, 5, 6, 7, 9, 10, 11, 12, 13, 14}
	updateArgs := []any{host, at}
	sets := []string{"heartbeat_at=$2"}
	for i, column := range columns {
		if changed[i] {
			updateArgs = append(updateArgs, args[indices[i]-1])
			sets = append(sets, fmt.Sprintf("%s=$%d", column, len(updateArgs)))
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE hosts SET "+strings.Join(sets, ",")+" WHERE host=$1", updateArgs...); err != nil {
		return nil, err
	}
	return drain, nil
}

// recordLoadSample は負荷の履歴を1件足し、掃除の時期なら3時間より古い履歴を消す。履歴の失敗は SAVEPOINT で巻き戻し、その失敗を historyErr で返す。
func recordLoadSample(ctx context.Context, tx *sql.Tx, host string, load any, at time.Time, due bool) (error, error) {
	if _, err := tx.ExecContext(ctx, "SAVEPOINT heartbeat_history"); err != nil {
		return nil, err
	}
	_, historyErr := tx.ExecContext(ctx, "INSERT INTO host_load_samples(host,received_at,load) VALUES($1,$2,$3::jsonb)", host, at, encode(load))
	if historyErr == nil && due {
		_, historyErr = tx.ExecContext(ctx, "DELETE FROM host_load_samples WHERE host=$1 AND received_at < $2", host, at.Add(-3*time.Hour))
	}
	if historyErr != nil {
		if _, err := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT heartbeat_history"); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT heartbeat_history"); err != nil {
		return nil, err
	}
	return historyErr, nil
}

func ActiveDrain(mark map[string]any, at time.Time, duration time.Duration) map[string]any {
	active := func(value map[string]any) map[string]any {
		if value == nil {
			return nil
		}
		if value["indefinite"] == true {
			copy := fleet.Object(fleet.Clone(value))
			delete(copy, "hostd")
			return copy
		}
		until, err := ParseDrainTime(value["until"])
		if err != nil {
			stamp, e := ParseDrainTime(value["at"])
			if e != nil {
				return nil
			}
			until = stamp.Add(duration)
		}
		if !until.After(at) {
			return nil
		}
		copy := fleet.Object(fleet.Clone(value))
		delete(copy, "hostd")
		copy["until"] = contract.ISO(until)
		return copy
	}
	top, nested := active(mark), active(fleet.Object(mark["hostd"]))
	if top != nil {
		if nested != nil {
			top["hostd"] = nested
		}
		return top
	}
	return nested
}

func (s *Service) Drain(ctx context.Context, host, by string, body map[string]any, stop bool) (map[string]any, error) {
	var mark any
	if stop {
		if !only(body, "until", "reason", "indefinite") || !text(body["reason"], 500) || strings.TrimSpace(fleet.Text(body["reason"])) == "" {
			return nil, invalid("本文はuntilと空でないreasonです。")
		}
		indefinite, ok := body["indefinite"].(bool)
		if _, exists := body["indefinite"]; exists && !ok {
			return nil, invalid("indefiniteは真偽値です。")
		}
		if indefinite && body["until"] != nil {
			return nil, invalid("indefiniteとuntilは同時に指定できません。")
		}
		at := s.clock()
		until := at.Add(DefaultDuration())
		if body["until"] != nil {
			var err error
			until, err = ParseDrainTime(body["until"])
			if err != nil || !until.After(at) {
				return nil, invalid("untilは未来のISO日時です。")
			}
		}
		entry := map[string]any{"reason": strings.TrimSpace(fleet.Text(body["reason"])), "by": by, "at": at.Format(contract.TimeLayout)}
		if indefinite {
			entry["indefinite"] = true
		} else {
			entry["until"] = contract.ISO(until)
		}
		mark = entry
	}
	result, err := s.DB.ExecContext(ctx, "UPDATE hosts SET drain=$2::jsonb WHERE host=$1", host, encode(mark))
	if err != nil {
		return nil, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, contract.NewProblem(404, "そのPCの生存の行がありません。", "")
	}
	return map[string]any{"host": host, "drain": mark}, nil
}

func (s *Service) Notice(ctx context.Context, host string, body map[string]any) (map[string]any, error) {
	if !only(body, "kind", "seq", "issues", "truncated", "initial") || !oneOf(fleet.Text(body["kind"]), "webhook", "scan", "state", "full-test") || !integer(body["seq"], 0, 9007199254740991) {
		return nil, invalid("合図の形が違います。")
	}
	issues := []any{}
	if value, exists := body["issues"]; exists {
		var ok bool
		issues, ok = value.([]any)
		if !ok || len(issues) > 50 {
			return nil, invalid("issuesの形が違います。")
		}
	}
	unique := []any{}
	seen := map[string]bool{}
	for _, item := range issues {
		row := fleet.Object(item)
		if row == nil || !fleet.ValidRepo(fleet.Text(row["repo"])) || !integer(row["number"], 1, 9007199254740991) {
			return nil, invalid("Issue参照の形が違います。")
		}
		key := fleet.Text(row["repo"]) + ":" + string(json.Number(formatNumber(row["number"])))
		if !seen[key] {
			seen[key] = true
			unique = append(unique, map[string]any{"repo": row["repo"], "number": row["number"]})
		}
	}
	data := map[string]any{"seq": body["seq"], "issues": unique}
	for _, key := range []string{"truncated", "initial"} {
		if value, exists := body[key]; exists {
			if !boolValue(value) {
				return nil, invalid("合図のフラグは真偽値です。")
			}
			if value == true {
				data[key] = true
			}
		}
	}
	var id int64
	err := s.DB.QueryRowContext(ctx, "INSERT INTO notices(host,kind,data,at) VALUES($1,$2,$3::jsonb,$4) RETURNING id", host, body["kind"], encode(data), s.clock()).Scan(&id)
	return map[string]any{"id": id}, err
}

func formatNumber(value any) string { raw, _ := json.Marshal(value); return string(raw) }
