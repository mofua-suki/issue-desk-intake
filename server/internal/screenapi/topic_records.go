package screenapi

import (
	"context"
	"database/sql"
	"fmt"
	"issue-desk-intake/server/internal/appsql"
)

// topicRecords reads the saved projection, falling back to the original JSON.
func (s *Service) topicRecords(ctx context.Context, table string) (map[string]Object, error) {
	return s.readTopicRecords(ctx, table, nil)
}

func (s *Service) readTopicRecords(ctx context.Context, table string, observe func(int)) (map[string]Object, error) {
	var rows *sql.Rows
	var err error
	switch table {
	case "drafts":
		rows, err = s.o.AppDB.QueryContext(ctx, bindSQL(`SELECT id, COALESCE(topic_summary, json) FROM drafts`))
	case "todos":
		rows, err = s.o.AppDB.QueryContext(ctx, bindSQL(`SELECT id, COALESCE(topic_summary, json) FROM todos`))
	default:
		return nil, fmt.Errorf("screenapi: unsupported records table %q", table)
	}
	if err != nil {
		return nil, err
	}
	out, _, err := appsql.JSONObjectsByID[string](rows, appsql.JSONRowsOptions{Observe: observe})
	return out, err
}
