package usage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"math"
	"regexp"
	"strings"
	"time"

	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
)

type Store struct{ DB *sql.DB }
type Counts struct {
	Added     int `json:"added"`
	Duplicate int `json:"duplicate"`
	Rejected  int `json:"rejected"`
}

var fixedFields = strings.Fields("runId startedAt host repo scope jobKey generation commit implementingAtStart loadavg1AtStart elapsedMs timeoutSeconds percent status")

func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (s *Store) Add(ctx context.Context, sender string, records map[string]json.RawMessage) (Counts, error) {
	result := Counts{}
	lists, err := readUsageLists(records)
	if err != nil {
		return result, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Counts{}, err
	}
	defer tx.Rollback()
	for _, kind := range []string{"usage", "limits", "fixedTests", "logLines"} {
		batch := newInsertBatch(tx, kind)
		for _, raw := range lists[kind] {
			values, callID, stored, ok, e := storedRowValues(kind, raw, sender)
			if e != nil {
				return Counts{}, e
			}
			if !ok {
				result.Rejected++
				continue
			}
			if callID != "" {
				if err = batch.flush(ctx, &result); err != nil {
					return Counts{}, err
				}
				values[0] = hash("intake-call:" + callID)
				if e := addIntakeCall(ctx, tx, values, stored, &result); e != nil {
					return Counts{}, e
				}
				continue
			}
			if err = batch.add(ctx, values, &result); err != nil {
				return Counts{}, err
			}
		}
		if err = batch.flush(ctx, &result); err != nil {
			return Counts{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return Counts{}, err
	}
	return result, nil
}

// readUsageLists checks the body's items and splits each into its rows.
func readUsageLists(records map[string]json.RawMessage) (map[string][]json.RawMessage, error) {
	for key := range records {
		if !oneOf(key, "usage", "limits", "fixedTests", "logLines") {
			return nil, contract.NewProblem(400, "使用量の本文に使えない項目があります。", "")
		}
	}
	lists := map[string][]json.RawMessage{}
	total := 0
	for _, key := range []string{"usage", "limits", "fixedTests", "logLines"} {
		raw, exists := records[key]
		if !exists {
			lists[key] = []json.RawMessage{}
			continue
		}
		if len(raw) == 0 || raw[0] != '[' {
			return nil, contract.NewProblem(400, "使用量の各項目は配列です。", "")
		}
		var rows []json.RawMessage
		if err := json.Unmarshal(raw, &rows); err != nil {
			return nil, contract.NewProblem(400, "使用量の行を読めません。", "")
		}
		lists[key] = rows
		total += len(rows)
	}
	if total > 1000 {
		return nil, contract.NewProblem(400, "使用量は合わせて1000行以内です。", "")
	}
	return lists, nil
}

// storedRowValues validates one row of the kind and returns the column values
// to insert. ok is false for a row to reject. An intake usage row with a call
// id is returned with that id, to be stored individually.
func storedRowValues(kind string, raw json.RawMessage, sender string) (values []any, callID, stored string, ok bool, err error) {
	var row map[string]any
	var ordered map[string]json.RawMessage
	if json.Unmarshal(raw, &row) != nil || row == nil || json.Unmarshal(raw, &ordered) != nil {
		return nil, "", "", false, nil
	}
	switch kind {
	case "usage":
		if !ValidUsage(row) {
			return nil, "", "", false, nil
		}
		stored, e := OrderedRow(ordered, schema["storageFields"])
		if e != nil {
			return nil, "", "", false, e
		}
		var issue any
		if count(row["issue"], 1) && fleet.Number(row["issue"]) <= 2147483647 {
			issue = int64(fleet.Number(row["issue"]))
		}
		values = []any{hash(stored), row["at"], row["host"], row["source"], row["repo"], issue, row["step"], row["provider"], row["model"], stored, sender}
		if id, ok := row["callId"].(string); ok {
			return values, id, stored, true, nil
		}
	case "limits":
		if !ValidLimit(row) {
			return nil, "", "", false, nil
		}
		stored, e := OrderedRow(ordered, schema["limitFields"])
		if e != nil {
			return nil, "", "", false, e
		}
		values = []any{hash(stored), row["at"], row["host"], row["source"], row["provider"], row["limit"], stored, sender}
	case "fixedTests":
		if !validFixed(row) {
			return nil, "", "", false, nil
		}
		stored, e := OrderedRow(ordered, fixedFields)
		if e != nil {
			return nil, "", "", false, e
		}
		identity := quote(fleet.Text(row["host"])) + "," + quote(fleet.Text(row["runId"]))
		values = []any{hash("[" + identity + "]"), row["startedAt"], row["host"], row["repo"], row["scope"], stored, sender}
	case "logLines":
		if !validLog(row) {
			return nil, "", "", false, nil
		}
		at, _ := time.Parse(time.RFC3339Nano, fleet.Text(row["at"]))
		timestamp := contract.ISO(at)
		identity := "[" + quote(sender) + "," + quote(timestamp) + "," + quote(fleet.Text(row["line"])) + "]"
		values = []any{hash(identity), timestamp, sender, row["line"], sender}
	}
	return values, "", "", true, nil
}

// addIntakeCall stores an intake usage row keyed by its call id. A repeated
// call with the same content is a duplicate; different content is rejected.
func addIntakeCall(ctx context.Context, tx *sql.Tx, values []any, stored string, result *Counts) error {
	inserted, e := tx.ExecContext(ctx, "INSERT INTO usage_rows(key,at,host,source,repo,issue,step,provider,model,json,sender) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT(key) DO NOTHING", values...)
	if e != nil {
		return e
	}
	n, e := inserted.RowsAffected()
	if e != nil {
		return e
	}
	if n == 1 {
		result.Added++
	} else {
		var existing string
		if e = tx.QueryRowContext(ctx, "SELECT json FROM usage_rows WHERE key=$1", values[0]).Scan(&existing); e != nil {
			return e
		}
		if existing == stored {
			result.Duplicate++
		} else {
			result.Rejected++
		}
	}
	return nil
}

func validFixed(row map[string]any) bool {
	if !fields(row, fixedFields) || !regexp.MustCompile(`^[\w-]{1,80}$`).MatchString(fleet.Text(row["runId"])) || !atTime(row["startedAt"]) || !text(row["host"], 200) || !text(row["repo"], 200) || !oneOf(row["scope"], "full", "selected", "early", "docs") || !oneOf(row["status"], "passed", "failed", "cut") || !count(row["elapsedMs"], 0) || !amount(row["timeoutSeconds"]) || fleet.Number(row["timeoutSeconds"]) <= 0 || !amount(row["percent"]) {
		return false
	}
	for _, key := range []string{"implementingAtStart", "loadavg1AtStart"} {
		value, exists := row[key]
		if !exists || value != nil && !amount(value) {
			return false
		}
		if key == "implementingAtStart" && value != nil && !count(value, 0) {
			return false
		}
	}
	if value, exists := row["generation"]; exists {
		if _, ok := value.(float64); !ok {
			return false
		}
	}
	for _, key := range []string{"jobKey", "commit"} {
		if value, exists := row[key]; exists {
			v, ok := value.(string)
			if !ok || contract.UTF16Length(v) > 200 {
				return false
			}
		}
	}
	expected := math.Floor(fleet.Number(row["elapsedMs"])/fleet.Number(row["timeoutSeconds"])+0.5) / 10
	return expected == fleet.Number(row["percent"])
}

func validLog(row map[string]any) bool {
	if len(row) != 2 || !fields(row, []string{"at", "line"}) || !regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,3})?Z$`).MatchString(fleet.Text(row["at"])) {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, fleet.Text(row["at"]))
	if err != nil || at.Year() < 1 {
		return false
	}
	line, ok := row["line"].(string)
	return ok && contract.UTF16Length(line) > 0 && contract.UTF16Length(line) <= 16384 && !strings.ContainsAny(line, "\x00\r\n")
}

func (s *Store) Read(ctx context.Context, project string, from, to time.Time) ([]map[string]any, int, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT json FROM usage_rows WHERE at>=$1 AND at<$2 AND($3='' OR repo=$3) ORDER BY at,key", from, to, project)
	if err != nil {
		return nil, 0, err
	}
	return appsql.JSONObjects[string](rows, appsql.JSONRowsOptions{SkipInvalid: true})
}
