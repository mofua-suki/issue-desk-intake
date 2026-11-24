package adminapi

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"

	"issue-desk-intake/server/internal/auth"
)

var diagnosticStarted = time.Now().UTC()
var diagnosticInstance = randomID()

// Only fixed SELECT statements are executed within a read-only transaction.
const activitySQL = `SELECT state, GREATEST(0, EXTRACT(EPOCH FROM (clock_timestamp() - query_start)))::float8, wait_event_type, query FROM pg_catalog.pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid() AND (state = 'active' OR state IS NULL) ORDER BY query_start NULLS LAST LIMIT 100`
const connectionsSQL = `SELECT COALESCE(state, 'unknown'), count(*) FROM pg_catalog.pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid() GROUP BY state ORDER BY state`
const maxConnectionsSQL = `SELECT current_setting('max_connections')::integer`

type diagnosticQuery struct {
	State    string  `json:"state"`
	Seconds  float64 `json:"seconds"`
	WaitType *string `json:"waitType"`
	Query    string  `json:"query,omitempty"`
}
type diagnosticConnection struct {
	State string `json:"state"`
	Count int64  `json:"count"`
}

func (s *service) diagnosticsDB(r *http.Request, _ auth.Principal) (any, int, error) {
	if r.Method != http.MethodGet {
		return nil, 0, bad(http.StatusMethodNotAllowed, "GETだけを使えます。")
	}
	if s.Postgres == nil {
		return nil, 0, bad(503, "DB診断情報を読み取れません。")
	}
	tx, err := s.Postgres.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, bad(503, "DB診断情報を読み取れません。")
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(r.Context(), "SET LOCAL statement_timeout = '3s'"); err != nil {
		return nil, 0, bad(503, "DB診断情報を読み取れません。")
	}
	var stringMode string
	if err = tx.QueryRowContext(r.Context(), "SHOW standard_conforming_strings").Scan(&stringMode); err != nil {
		return nil, 0, bad(503, "DB診断情報を読み取れません。")
	}
	queries, err := readDiagnosticQueries(r.Context(), tx, stringMode)
	if err != nil {
		return nil, 0, err
	}
	counts, err := readDiagnosticConnections(r.Context(), tx)
	if err != nil {
		return nil, 0, err
	}
	var max int
	if err = tx.QueryRowContext(r.Context(), maxConnectionsSQL).Scan(&max); err != nil {
		return nil, 0, bad(503, "DB診断情報を読み取れません。")
	}
	if err = tx.Commit(); err != nil {
		return nil, 0, bad(503, "DB診断情報を読み取れません。")
	}
	result := diagnosticIdentity()
	result["pid"], result["queries"], result["connections"], result["maxConnections"] = os.Getpid(), queries, counts, max
	return result, 200, nil
}

// readDiagnosticQueries lists the activity of the other sessions, with each
// query normalized only when string literals can be delimited.
func readDiagnosticQueries(ctx context.Context, tx *sql.Tx, stringMode string) ([]diagnosticQuery, error) {
	queries := []diagnosticQuery{}
	rows, err := tx.QueryContext(ctx, activitySQL)
	if err != nil {
		return nil, bad(503, "DB診断情報を読み取れません。")
	}
	for rows.Next() {
		var state, wait, query sql.NullString
		var seconds sql.NullFloat64
		if err = rows.Scan(&state, &seconds, &wait, &query); err != nil {
			break
		}
		item := diagnosticQuery{State: state.String, Seconds: seconds.Float64}
		if !state.Valid {
			item.State = "unknown"
		}
		if wait.Valid {
			item.WaitType = &wait.String
		}
		if stringMode == "on" && query.Valid && query.String != "<insufficient privilege>" {
			item.Query = normalizeDiagnosticQuery(query.String)
		}
		queries = append(queries, item)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, bad(503, "DB診断情報を読み取れません。")
	}
	return queries, nil
}

// readDiagnosticConnections counts the connections by state.
func readDiagnosticConnections(ctx context.Context, tx *sql.Tx) ([]diagnosticConnection, error) {
	counts := []diagnosticConnection{}
	rows, err := tx.QueryContext(ctx, connectionsSQL)
	if err != nil {
		return nil, bad(503, "DB診断情報を読み取れません。")
	}
	for rows.Next() {
		var item diagnosticConnection
		if err = rows.Scan(&item.State, &item.Count); err != nil {
			break
		}
		counts = append(counts, item)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, bad(503, "DB診断情報を読み取れません。")
	}
	return counts, nil
}

// Preserve unquoted identifiers for query identification; hide literals and quoted identifiers.
// Unknown or incomplete lexical forms fail closed.
func normalizeDiagnosticQuery(q string) string {
	tokens, ok := diagnosticQueryTokens(q)
	if !ok {
		return ""
	}
	return joinDiagnosticTokens(tokens)
}

// diagnosticQueryTokens splits the query into tokens, replacing comments,
// literals and quoted identifiers with "?". It reports false for a form it
// cannot delimit.
func diagnosticQueryTokens(q string) ([]string, bool) {
	var tokens []string
	escapeString := false
	for i := 0; i < len(q); {
		c := q[i]
		if c == ' ' || c == '\n' || c == '\r' || c == '\t' {
			i++
			continue
		}
		if c == '-' && i+1 < len(q) && q[i+1] == '-' {
			i += 2
			for i < len(q) && q[i] != '\n' {
				i++
			}
			tokens = append(tokens, "?")
			continue
		}
		if c == '/' && i+1 < len(q) && q[i+1] == '*' {
			i += 2
			depth := 1
			for i < len(q) && depth > 0 {
				if i+1 < len(q) && q[i:i+2] == "/*" {
					depth++
					i += 2
				} else if i+1 < len(q) && q[i:i+2] == "*/" {
					depth--
					i += 2
				} else {
					i++
				}
			}
			if depth != 0 {
				return nil, false
			}
			tokens = append(tokens, "?")
			continue
		}
		if c == '\'' || c == '"' {
			quote := c
			i++
			closed := false
			for i < len(q) {
				// Another session's standard_conforming_strings is not observable.
				// An ordinary string containing a backslash has ambiguous boundaries.
				if q[i] == '\\' && quote == '\'' && !escapeString {
					return nil, false
				}
				if q[i] == '\\' && quote == '\'' && escapeString {
					i += 2
					continue
				}
				if q[i] == quote {
					if i+1 < len(q) && q[i+1] == quote {
						i += 2
						continue
					}
					i++
					if quote == '\'' {
						next, newline := diagnosticQuoteContinuation(q, i)
						if next < len(q) && q[next] == '\'' {
							if !newline {
								return nil, false
							}
							i = next + 1
							continue
						}
					}
					closed = true
					break
				}
				i++
			}
			if !closed {
				return nil, false
			}
			tokens = append(tokens, "?")
			escapeString = false
			continue
		}
		if c == '$' {
			j := i + 1
			for j < len(q) && (q[j] == '_' || q[j] >= 'a' && q[j] <= 'z' || q[j] >= 'A' && q[j] <= 'Z' || q[j] >= '0' && q[j] <= '9') {
				j++
			}
			if j < len(q) && q[j] == '$' {
				tag := q[i : j+1]
				end := strings.Index(q[j+1:], tag)
				if end < 0 {
					return nil, false
				}
				i = j + 1 + end + len(tag)
				tokens = append(tokens, "?")
				continue
			}
			if j > i+1 {
				i = j
				tokens = append(tokens, "?")
				continue
			}
			return nil, false
		}
		if c >= '0' && c <= '9' {
			i++
			for i < len(q) && (q[i] >= '0' && q[i] <= '9' || q[i] == '.' || q[i] == '_' || q[i] >= 'a' && q[i] <= 'z' || q[i] >= 'A' && q[i] <= 'Z') {
				i++
			}
			tokens = append(tokens, "?")
			continue
		}
		if c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			start := i
			i++
			for i < len(q) && (q[i] == '_' || q[i] == '$' || q[i] >= 'a' && q[i] <= 'z' || q[i] >= 'A' && q[i] <= 'Z' || q[i] >= '0' && q[i] <= '9') {
				i++
			}
			word := q[start:i]
			if (word == "U" || word == "u") && i+1 < len(q) && q[i] == '&' && (q[i+1] == '\'' || q[i+1] == '"') {
				return nil, false
			}
			if (word == "E" || word == "e") && i < len(q) && q[i] == '\'' {
				escapeString = true
				continue
			}
			tokens = append(tokens, word)
			continue
		}
		if strings.ContainsRune("+-*/%<>=!~^|&@#`?", rune(c)) {
			start := i
			i++
			for i < len(q) && strings.ContainsRune("+-*/%<>=!~^|&@#`?", rune(q[i])) {
				if i+1 < len(q) && (q[i:i+2] == "--" || q[i:i+2] == "/*") {
					break
				}
				i++
			}
			tokens = append(tokens, q[start:i])
			continue
		}
		if i+1 < len(q) && (q[i:i+2] == "::" || q[i:i+2] == ":=" || q[i:i+2] == "..") {
			tokens = append(tokens, q[i:i+2])
			i += 2
			continue
		}
		if strings.ContainsRune("(),.;:[]", rune(c)) {
			tokens = append(tokens, string(c))
			i++
			continue
		}
		if unicode.IsSpace(rune(c)) {
			i++
			continue
		}
		return nil, false
	}
	return tokens, true
}

// joinDiagnosticTokens joins the tokens with spaces, keeping whole tokens within 256 bytes.
func joinDiagnosticTokens(tokens []string) string {
	var result string
	for _, token := range tokens {
		separator := ""
		if result != "" {
			separator = " "
		}
		if len(result)+len(separator)+len(token) > 256 {
			break
		}
		result += separator + token
	}
	return result
}

// PostgreSQL permits a string to continue across whitespace containing a newline,
// including line comments. The original string's escape mode remains in effect.
func diagnosticQuoteContinuation(q string, i int) (int, bool) {
	newline := false
	for i < len(q) {
		switch q[i] {
		case ' ', '\t', '\f', '\v':
			i++
		case '\n', '\r':
			newline = true
			i++
		case '-':
			if i+1 >= len(q) || q[i+1] != '-' {
				return i, newline
			}
			i += 2
			for i < len(q) && q[i] != '\n' && q[i] != '\r' {
				i++
			}
		default:
			return i, newline
		}
	}
	return i, newline
}
