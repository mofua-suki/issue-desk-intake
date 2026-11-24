package screenapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// snapshot reads the list snapshot with the current configuration, as the list handlers do via snapshotConfig.
func (s *Service) snapshot(ctx context.Context, origin string, timing *listTiming) (Snapshot, error) {
	config, e := s.o.Config()
	if e != nil {
		return Snapshot{}, e
	}
	return s.snapshotConfig(ctx, origin, timing, config)
}

// records reads every stored draft or todo record in full. Tests use it as the reference reader
// for topic-link comparisons and to check that stored records are retained.
func (s *Service) records(ctx context.Context, table string) (map[string]Object, error) {
	var rows *sql.Rows
	var e error
	switch table {
	case "drafts":
		rows, e = s.o.AppDB.QueryContext(ctx, bindSQL(`SELECT id,json FROM drafts`))
	case "todos":
		rows, e = s.o.AppDB.QueryContext(ctx, bindSQL(`SELECT id,json FROM todos`))
	default:
		return nil, fmt.Errorf("screenapi: unsupported records table %q", table)
	}
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := map[string]Object{}
	for rows.Next() {
		var id, raw string
		if e = rows.Scan(&id, &raw); e != nil {
			return nil, e
		}
		r := Object{}
		if e = json.Unmarshal([]byte(raw), &r); e != nil {
			return nil, e
		}
		out[id] = r
	}
	return out, rows.Err()
}
