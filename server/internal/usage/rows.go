package usage

import (
	"context"
	"encoding/json"
)

func (s *Store) AddRows(ctx context.Context, sender string, rows []map[string]any) (Counts, error) {
	raw, err := json.Marshal(rows)
	if err != nil {
		return Counts{}, err
	}
	return s.Add(ctx, sender, map[string]json.RawMessage{"usage": raw})
}
