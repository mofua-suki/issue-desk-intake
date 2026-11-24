package adminapi

import (
	"context"
	"database/sql"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
)

var aiID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
var aiPC = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
var aiProject = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var aiKind = regexp.MustCompile(`^[a-z][a-z0-9-]{0,99}$`)
var aiUTC = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,3})?Z$`)

func textValid(v any, max int) bool {
	value, ok := v.(string)
	return ok && strings.TrimSpace(value) != "" && !strings.ContainsRune(value, 0) && contract.UTF16Length(value) <= max
}
func aiWho(who string) bool {
	return who == "chat" || who == "intake" || who == "implementation" || who == "review"
}
func aiScore(value any) bool { return value == "hit" || value == "miss" || value == "partial" }
func normalizeAI(input map[string]any) (map[string]any, error) {
	fail := func() (map[string]any, error) {
		return nil, bad(400, "AI判断記録の入力を確認してください。")
	}
	id, at, who, pc, project, kind := str(input["id"]), str(input["at"]), str(input["who"]), str(input["pc"]), str(input["project"]), str(input["kind"])
	if !textValid(id, 128) || !aiID.MatchString(id) || !aiUTC.MatchString(at) || !aiWho(who) || !textValid(pc, 100) || !aiPC.MatchString(pc) || !textValid(project, 200) || !aiProject.MatchString(project) || !aiKind.MatchString(kind) {
		return fail()
	}
	if _, e := time.Parse(time.RFC3339Nano, at); e != nil {
		return fail()
	}
	options, ok := input["options"].([]any)
	if !ok || len(options) < 1 || len(options) > 100 {
		return fail()
	}
	for _, option := range options {
		if !textValid(option, 2000) {
			return fail()
		}
	}
	record := map[string]any{"id": id, "at": at, "who": who, "pc": pc, "project": project, "kind": kind, "options": options}
	for _, key := range []string{"context", "chosen", "reason", "expected", "check"} {
		if !textValid(input[key], 16000) {
			return fail()
		}
		record[key] = input[key]
	}
	for _, key := range []string{"outcome", "lesson"} {
		if input[key] != nil && !textValid(input[key], 16000) {
			return fail()
		}
		record[key] = input[key]
	}
	if input["score"] != nil && !aiScore(input["score"]) {
		return fail()
	}
	record["score"] = input["score"]
	return record, nil
}
func (s *service) aiDecisions(r *http.Request, p auth.Principal) (any, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := r.Context()
	tx, e := s.AppDB.BeginTx(ctx, nil)
	if e != nil {
		return nil, 0, e
	}
	defer tx.Rollback()
	path := strings.TrimPrefix(r.URL.Path, "/api/ai-decisions")
	if r.Method == http.MethodGet && (path == "" || path == "/stats") {
		return listAIDecisions(ctx, tx, r, path)
	}
	if r.Method != http.MethodPost {
		return nil, 0, bad(404, "AI判断記録の経路が見つかりません。")
	}
	input, e := body(r)
	if e != nil {
		return nil, 0, e
	}
	var record map[string]any
	created := false
	if path == "" {
		var existing map[string]any
		record, existing, e = newAIDecision(ctx, tx, input)
		if e != nil {
			return nil, 0, e
		}
		if existing != nil {
			return map[string]any{"record": existing, "created": false}, 200, nil
		}
		created = true
	} else {
		record, e = scoredAIDecision(ctx, tx, path, input)
		if e != nil {
			return nil, 0, e
		}
	}
	if e = jsonPut(ctx, tx, "ai_decisions", str(record["id"]), record); e != nil {
		return nil, 0, e
	}
	if e = s.audit(ctx, tx, p, "ai-decision:"+path, str(record["id"])); e != nil {
		return nil, 0, e
	}
	if e = tx.Commit(); e != nil {
		return nil, 0, e
	}
	if created {
		return map[string]any{"record": record, "created": true}, 200, nil
	}
	return record, 200, nil
}

// listAIDecisions answers the filtered page of AI decision records or their statistics.
func listAIDecisions(ctx context.Context, tx *sql.Tx, r *http.Request, path string) (any, int, error) {
	if path == "" {
		kind, who := r.URL.Query().Get("kind"), r.URL.Query().Get("who")
		if kind != "" && !aiKind.MatchString(kind) || who != "" && !aiWho(who) {
			return nil, 0, bad(400, "AI判断記録の絞り込みが不正です。")
		}
		page, problem := contract.ParsePagination(r.URL.Query())
		if problem != nil {
			return nil, 0, problem
		}
		result, err := jsonPage(ctx, tx, "ai_decisions", aiDecisionsWhere(), aiDecisionsOrder(), []any{kind, kind, who, who}, page)
		return result, 200, err
	}
	groups, err := aiDecisionStats(ctx, tx)
	return groups, 200, err
}

// newAIDecision validates a new record. When the id is already saved, the
// saved record is returned as existing.
func newAIDecision(ctx context.Context, tx *sql.Tx, input map[string]any) (record, existing map[string]any, _ error) {
	record, e := normalizeAI(input)
	if e != nil {
		return nil, nil, e
	}
	existing, e = jsonGet(ctx, tx, "ai_decisions", str(record["id"]))
	if e != nil {
		return nil, nil, e
	}
	return record, existing, nil
}

// scoredAIDecision validates a score for the record named in the path and applies it.
func scoredAIDecision(ctx context.Context, tx *sql.Tx, path string, input map[string]any) (map[string]any, error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 2 || parts[1] != "score" || !textValid(parts[0], 128) || !aiID.MatchString(parts[0]) {
		return nil, bad(404, "AI判断記録の経路が見つかりません。")
	}
	if !aiScore(input["score"]) || !textValid(input["outcome"], 16000) || !textValid(input["lesson"], 16000) {
		return nil, bad(400, "採点と結果と教訓を指定してください。")
	}
	record, e := jsonGet(ctx, tx, "ai_decisions", parts[0])
	if e != nil {
		return nil, e
	}
	if record == nil {
		return nil, bad(404, "AI判断記録が見つかりません。")
	}
	for _, key := range []string{"score", "outcome", "lesson"} {
		record[key] = input[key]
	}
	return record, nil
}

// aiDecisionStats uses compact, string-only fields on the normal path. JSONB
// still parses the complete TEXT value in PostgreSQL, once per record. Unlike
// JSONText, this projection does not stringify numbers, arrays or objects.
// Unsupported JSONB values (including escaped NUL, invalid surrogates, numeric
// overflow and trailing input), nested containers, duplicate keys, or non-object
// roots use the original decoder regardless of PostgreSQL stack-depth settings.
// A savepoint is required: a failed SELECT otherwise aborts a PostgreSQL tx.
func aiDecisionStats(ctx context.Context, tx *sql.Tx) ([]map[string]any, error) {
	if _, err := tx.ExecContext(ctx, bindSQL("SAVEPOINT ai_stats_projection")); err != nil {
		return nil, err
	}
	entries, err := aiDecisionStatsFields(ctx, tx)
	if err != nil {
		if _, rollbackErr := tx.ExecContext(ctx, bindSQL("ROLLBACK TO SAVEPOINT ai_stats_projection")); rollbackErr != nil {
			return nil, rollbackErr
		}
	}
	if _, releaseErr := tx.ExecContext(ctx, bindSQL("RELEASE SAVEPOINT ai_stats_projection")); releaseErr != nil {
		return nil, releaseErr
	}
	if err != nil {
		// Re-read every row, in the original order, with UseNumber and Decode's
		// original success/failure boundary. Preserve all stored values without repairs.
		legacy, readErr := jsonList(ctx, tx, "ai_decisions")
		if readErr != nil {
			return nil, readErr
		}
		entries = make([]aiStatsFields, 0, len(legacy))
		for _, record := range legacy {
			entries = append(entries, aiStatsFields{str(record["kind"]), str(record["score"]), str(record["at"]), str(record["id"])})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].at == entries[j].at {
			return entries[i].id < entries[j].id
		}
		return entries[i].at < entries[j].at
	})
	groups := []map[string]any{}
	indices := map[string]int{}
	for _, record := range entries {
		index, exists := indices[record.kind]
		if !exists {
			index = len(groups)
			indices[record.kind] = index
			groups = append(groups, map[string]any{"kind": record.kind, "hit": 0, "miss": 0, "partial": 0, "scored": 0, "rate": nil})
		}
		g := groups[index]
		if aiScore(record.score) {
			g[record.score] = g[record.score].(int) + 1
			g["scored"] = g["scored"].(int) + 1
		}
	}
	for _, g := range groups {
		if g["scored"].(int) > 0 {
			g["rate"] = float64(g["hit"].(int)) / float64(g["scored"].(int))
		}
	}
	return groups, nil
}

type aiStatsFields struct{ kind, score, at, id string }

func aiDecisionStatsFields(ctx context.Context, tx *sql.Tx) ([]aiStatsFields, error) {
	rows, err := tx.QueryContext(ctx, bindSQL(aiDecisionStatsSQL()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []aiStatsFields{}
	compatibleRows := true
	for rows.Next() {
		var record aiStatsFields
		var compatible bool
		if err := rows.Scan(&record.kind, &record.score, &record.at, &record.id, &compatible); err != nil {
			return nil, err
		}
		// Consume the whole projection even for an unsupported root, so the
		// observed column-value bytes include every returned projected row.
		compatibleRows = compatibleRows && compatible
		entries = append(entries, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !compatibleRows {
		return nil, bad(503, "AI判断記録を従来形式で読み直します。")
	}
	return entries, nil
}
