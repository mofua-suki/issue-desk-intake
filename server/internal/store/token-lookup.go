package store

import (
	"context"

	"issue-desk-intake/server/internal/auth"
)

// TokenByID revalidates the durable sender before recovered work is started.
// Token hashes are kept inside authentication and are never returned here.
func (s *SQLite) TokenByID(ctx context.Context, id string) (auth.Token, bool, error) {
	return s.tokenWhere(ctx, `SELECT t.id,t.name,CASE WHEN g.token_id IS NULL THEN 0 ELSE 1 END FROM api_tokens t LEFT JOIN api_token_grants g ON t.id=g.token_id WHERE t.id=?`, id)
}
func (s *SQLite) TokenNames(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM api_tokens ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}
