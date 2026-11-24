package adminapi

import (
	"context"
	"database/sql"
	"net/http"
	"strings"

	"issue-desk-intake/server/internal/auth"
)

const tokenSelect = `SELECT t.id,t.name,t.issued_at,t.last_used_at,g.granted_at FROM api_tokens t LEFT JOIN api_token_grants g ON t.id=g.token_id`

func tokenView(id, name, issued string, last, granted sql.NullString) map[string]any {
	out := map[string]any{"id": id, "name": name, "issuedAt": issued, "lastUsedAt": nil, "delegate": granted.Valid, "grantedAt": nil}
	if last.Valid {
		out["lastUsedAt"] = last.String
	}
	if granted.Valid {
		out["grantedAt"] = granted.String
	}
	return out
}
func getToken(ctx context.Context, tx *sql.Tx, id string) (map[string]any, error) {
	var name, issued string
	var last, granted sql.NullString
	e := tx.QueryRowContext(ctx, bindSQL(tokenSelect+" WHERE t.id=?"), id).Scan(&id, &name, &issued, &last, &granted)
	if e == sql.ErrNoRows {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	return tokenView(id, name, issued, last, granted), nil
}
func (s *service) tokens(r *http.Request, p auth.Principal) (any, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, e := s.AppDB.BeginTx(r.Context(), nil)
	if e != nil {
		return nil, 0, e
	}
	defer tx.Rollback()
	if r.URL.Path == "/api/tokens" && r.Method == http.MethodGet {
		return listTokens(r.Context(), tx)
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/tokens/"), "/")
	if r.Method != http.MethodPost || len(parts) != 2 || !uuidPattern.MatchString(parts[0]) || parts[1] != "delegate" {
		return nil, 0, bad(404, "経路が見つかりません。")
	}
	data, e := body(r)
	if e != nil {
		return nil, 0, e
	}
	delegate, ok := data["delegate"].(bool)
	if !ok {
		return nil, 0, bad(400, "delegateにtrueかfalseを指定してください。")
	}
	before, e := getToken(r.Context(), tx, parts[0])
	if e != nil {
		return nil, 0, e
	}
	if before == nil {
		return nil, 0, bad(404, "このトークンは失効したか、発行し直されています。")
	}
	if delegate {
		_, e = tx.ExecContext(r.Context(), insertIgnoreSQL("api_token_grants", []string{"token_id", "granted_at"}, "SELECT id,? FROM api_tokens WHERE id=?"), iso(s.Now()), parts[0])
	} else {
		_, e = tx.ExecContext(r.Context(), "DELETE FROM api_token_grants WHERE token_id=?", parts[0])
	}
	if e != nil {
		return nil, 0, e
	}
	token, e := getToken(r.Context(), tx, parts[0])
	if e != nil {
		return nil, 0, e
	}
	if token == nil {
		return nil, 0, bad(404, "このトークンは失効しています。")
	}
	if before["delegate"] != delegate {
		if e = s.audit(r.Context(), tx, p, "token:delegate", parts[0]); e != nil {
			return nil, 0, e
		}
	}
	if e = tx.Commit(); e != nil {
		return nil, 0, e
	}
	return token, 200, nil
}

// listTokens lists the issued tokens by name with their delegation.
func listTokens(ctx context.Context, tx *sql.Tx) (any, int, error) {
	rows, e := tx.QueryContext(ctx, bindSQL(tokenSelect+" ORDER BY t.name"))
	if e != nil {
		return nil, 0, e
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name, issued string
		var last, granted sql.NullString
		if e = rows.Scan(&id, &name, &issued, &last, &granted); e != nil {
			return nil, 0, e
		}
		out = append(out, tokenView(id, name, issued, last, granted))
	}
	return out, 200, rows.Err()
}
