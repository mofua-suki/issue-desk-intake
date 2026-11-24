// Package deskcleanup applies only explicitly requested, snapshot-bound endings.
package deskcleanup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"maps"
	"regexp"
)

type Object = map[string]any

var detailTimestamp = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:\d{2})?`)
var elapsedDetailRules = map[string][]struct {
	pattern     *regexp.Regexp
	replacement string
}{
	"urgent_fix_hold": {
		{regexp.MustCompile(`(?m)^(状態: [^\n]* / )[+-]?[0-9]+分停止$`), "${1}<minutes>分停止"},
	},
	"job_dispatch_stall": {
		{regexp.MustCompile(`(?m)^(過去の予約による見送り / 予約元: [^\n]* / 見送り記録時の待ち時間: )[0-9]+分$`), "${1}<minutes>分"},
		{regexp.MustCompile(`(?m)^実行待ち: [+-]?[0-9]+分`), "実行待ち: <minutes>分"},
		{regexp.MustCompile(`(?m)^(直近の見送り: [^\n]*)(（[+-]?[0-9]+分前）)$`), "${1}（<minutes>分前）"},
	},
}

func Fingerprint(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func AlertFingerprint(v Object) string {
	value := Object{}
	for k, x := range v {
		switch k {
		// codeRevisionChanged は配置中のコードの版との比較で、デプロイだけで変わる。非表示にした通知を版の更新で出し直さない。
		case "updatedAt", "detectedAt", "checkedAt", "elapsedMinutes", "codeRevisionChanged":
		default:
			value[k] = x
		}
	}
	if detail, ok := value["detail"].(string); ok {
		detail = detailTimestamp.ReplaceAllString(detail, "<timestamp>")
		kind, _ := value["kind"].(string)
		for _, rule := range elapsedDetailRules[kind] {
			detail = rule.pattern.ReplaceAllString(detail, rule.replacement)
		}
		value["detail"] = detail
	}
	return Fingerprint(value)
}
func AlertID(kind string, v Object) string {
	if kind == "attention" {
		return "attention:" + fleet.Text(v["key"])
	}
	return fleet.Text(v["id"])
}
func archive(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) ([]Object, error) {
	var raw string
	err := q.QueryRowContext(ctx, bindSQL(`SELECT json FROM settings WHERE id='desk-cleanup-archive'`)).Scan(&raw)
	if err == sql.ErrNoRows {
		return []Object{}, nil
	}
	if err != nil {
		return nil, err
	}
	var rows []Object
	err = json.Unmarshal([]byte(raw), &rows)
	if rows == nil {
		rows = []Object{}
	}
	return rows, err
}
func Archived(ctx context.Context, db *sql.DB) ([]Object, error) {
	if db == nil {
		return []Object{}, nil
	}
	return archive(ctx, db)
}
func issueSnapshot(repo string, n int64, issue Object) Object {
	return Object{
		"project": repo, "number": n, "desk": issue["desk"], "title": issue["title"],
		"state": issue["state"], "status": issue["status"], "updatedAt": issue["updatedAt"],
	}
}

// ApplyIssueArchives moves explicitly ended, already closed Issues to the
// closed list when the inspected Issue is unchanged. The stored checks remain intact.
func ApplyIssueArchives(ctx context.Context, db *sql.DB, rows []Object) error {
	var candidates []int
	for i, row := range rows {
		progress := fleet.ObjectOrEmpty(row["executionProgress"])
		if row["state"] == "closed" && (row["status"] == "failed" || row["status"] == "completed" && fleet.Number(progress["pending"]) > 0) {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	records, err := Archived(ctx, db)
	if err != nil {
		return err
	}
	type entry struct{ target, fingerprint string }
	archived := map[entry]bool{}
	for _, record := range records {
		result := fleet.ObjectOrEmpty(record["result"])
		if result["alreadyClosed"] == true {
			archived[entry{fleet.Text(record["target"]), fleet.Text(record["fingerprint"])}] = true
		}
	}
	for _, i := range candidates {
		row := rows[i]
		repo, n := fleet.Text(row["project"]), int64(fleet.Number(row["number"]))
		key := entry{fmt.Sprintf("issue:%s#%d", repo, n), Fingerprint(issueSnapshot(repo, n, row))}
		if archived[key] {
			rows[i] = maps.Clone(row)
			rows[i]["status"] = "closed"
		}
	}
	return nil
}

func VisibleAlerts(ctx context.Context, db *sql.DB, kind string, alerts []any) ([]any, error) {
	records, err := Archived(ctx, db)
	if err != nil {
		return nil, err
	}
	// Index snapshots once: previously every alert rehashed every archived snapshot.
	index := map[string]map[string]bool{}
	for _, r := range records {
		target, ok := r["target"].(string)
		if !ok {
			continue
		}
		saved := r["fingerprint"]
		if r["snapshot"] != nil {
			saved = AlertFingerprint(fleet.Object(r["snapshot"]))
		}
		fp, ok := saved.(string)
		if !ok {
			continue
		}
		if index[target] == nil {
			index[target] = map[string]bool{}
		}
		index[target][fp] = true
	}
	out := []any{}
	for _, raw := range alerts {
		a := fleet.Object(raw)
		hidden := false
		target := kind + ":" + AlertID(kind, a)
		fp := AlertFingerprint(a)
		legacyFP := ""
		if a["project"] == fleet.SystemProject {
			legacy := Object{}
			for k, v := range a {
				legacy[k] = v
			}
			legacy["project"] = fleet.LegacySystemProject
			legacyFP = AlertFingerprint(legacy)
		}
		hidden = index[target][fp] || legacyFP != "" && index[target][legacyFP]
		if !hidden {
			out = append(out, raw)
		}
	}
	return out, nil
}
func saveArchive(ctx context.Context, tx *sql.Tx, r Object) error {
	records, err := archive(ctx, tx)
	if err != nil {
		return err
	}
	records = append(records, r)
	raw, err := json.Marshal(records)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, bindSQL(`INSERT INTO settings(id,json)VALUES('desk-cleanup-archive',?)ON CONFLICT(id)DO UPDATE SET json=excluded.json`), string(raw))
	return err
}
