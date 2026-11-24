package adminapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"issue-desk-intake/server/internal/contract"
)

// jsonPage counts and reads only the requested rows in the same transaction.
func jsonPage(ctx context.Context, tx *sql.Tx, table, where, order string, args []any, page contract.Pagination) (contract.PageResult[map[string]any], error) {
	result := contract.PageResult[map[string]any]{Items: []map[string]any{}, Page: page.Page, Limit: page.Limit}
	if err := tx.QueryRowContext(ctx, bindSQL("SELECT COUNT(*) FROM "+table+" WHERE "+where), args...).Scan(&result.Total); err != nil {
		return result, err
	}
	// A page beyond the end needs no offset multiplication, avoiding integer overflow.
	if page.Page-1 > result.Total/page.Limit {
		return result, nil
	}
	offset := (page.Page - 1) * page.Limit
	if offset >= result.Total {
		return result, nil
	}
	queryArgs := append(append([]any{}, args...), page.Limit, offset)
	rows, err := tx.QueryContext(ctx, bindSQL("SELECT json FROM "+table+" WHERE "+where+" ORDER BY "+order+" LIMIT ? OFFSET ?"), queryArgs...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return result, err
		}
		var item map[string]any
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		if err = decoder.Decode(&item); err != nil {
			return result, err
		}
		result.Items = append(result.Items, item)
	}
	result.HasMore = offset+len(result.Items) < result.Total
	return result, rows.Err()
}
