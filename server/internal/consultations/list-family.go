package consultations

import (
	"context"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"log"
	"reflect"
)

// familyListViews reads the parents and their children in one projection query.
// Selected parents use their already-read snapshot, as in the individual view.
func (s *Service) familyListViews(ctx context.Context, drafts []Record) func(Record, string) string {
	parents := []string{}
	children := []string{}
	for _, d := range drafts {
		if id := text(d["parentSplit"]); id != "" {
			parents = append(parents, id)
		} else {
			for _, id := range fleet.Items(d["splitChildren"]) {
				children = append(children, text(id))
			}
		}
	}
	if len(parents) == 0 && len(children) == 0 {
		return func(d Record, action string) string { return s.familyRefusal(d, d, nil, action) }
	}
	p, _ := json.Marshal(parents)
	c, _ := json.Marshal(children)
	const query = `WITH parent_ids AS (SELECT json_array_elements_text(?::json) AS id),
 parent_rows AS (SELECT d.id, d.json FROM drafts d JOIN parent_ids p ON d.id=p.id),
 child_ids AS (
 SELECT json_array_elements_text(?::json) AS id
 UNION SELECT CASE WHEN json_typeof(v) = 'string' THEN v #>> '{}' ELSE '' END
 FROM parent_rows p CROSS JOIN LATERAL json_array_elements(CASE WHEN json_typeof(p.json::json->'splitChildren') = 'array' THEN p.json::json->'splitChildren' ELSE '[]'::json END) v
 ), ids AS (SELECT id FROM parent_ids UNION SELECT id FROM child_ids)
 SELECT d.id, CASE WHEN json_typeof(d.json::json) = 'object' THEN (SELECT COALESCE(json_object_agg(key,value), '{}'::json) FROM json_each(d.json::json) WHERE key IN ('id','parentSplit','splitChildren','splitIssues','status','issue','issues','investigations','attempted','finishedWithoutIssue'))::text ELSE d.json END FROM drafts d JOIN ids ON d.id=ids.id`
	rows, err := s.options.Records.DB.QueryContext(ctx, bindSQL(query), string(p), string(c))
	byID := map[string]Record{}
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id, raw string
			if err = rows.Scan(&id, &raw); err != nil {
				break
			}
			var d Record
			d, err = decode(raw)
			if err != nil {
				break
			}
			byID[id] = d
		}
		if err == nil {
			err = rows.Err()
		}
	}
	var state interface{ SQLState() string }
	if errors.As(err, &state) && (state.SQLState() == "22P02" || state.SQLState() == "22P05") {
		log.Print("consultations: family projection JSON conversion failed; using full read")
		return s.familyViewRefusal
	}
	if err != nil {
		return func(Record, string) string { return "分割した相談全体の状態を読み取れません。" }
	}
	return s.familyRefusalIn(byID)
}

// familyRefusalIn returns familyRefusal with each consultation's parent and
// direct children looked up in byID.
func (s *Service) familyRefusalIn(byID map[string]Record) func(Record, string) string {
	return func(d Record, action string) string {
		parent := d
		if id := text(d["parentSplit"]); id != "" {
			parent = byID[id]
		}
		children := make([]Record, 0, len(fleet.Items(parent["splitChildren"])))
		for _, id := range fleet.Items(parent["splitChildren"]) {
			children = append(children, byID[text(id)])
		}
		return s.familyRefusal(d, parent, children, action)
	}
}

// detailFamilyRecords retains the original ordering and aggregation inputs while
// loading full bodies only for the requested parent and its direct children.
// Unusual IDs require the original full-list semantics (including duplicates).
func (s *Service) detailFamilyRecords(ctx context.Context, parent Record) ([]Record, error) {
	entries, err := s.options.Records.ListDraftEntries(ctx, DraftReadPage)
	if err != nil {
		return s.options.Records.List(ctx, "drafts")
	}
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		id, ok := entry.Record["id"].(string)
		if !ok || id == "" || id != entry.Key || seen[id] {
			return s.options.Records.List(ctx, "drafts")
		}
		seen[id] = true
	}
	id, ok := parent["id"].(string)
	if !ok || id == "" {
		return s.options.Records.List(ctx, "drafts")
	}
	keys := []string{id}
	wanted := map[string]bool{id: true}
	for _, child := range fleet.Items(parent["splitChildren"]) {
		childID, ok := child.(string)
		if !ok || childID == "" {
			return s.options.Records.List(ctx, "drafts")
		}
		keys = append(keys, childID)
		wanted[childID] = true
	}
	full, err := s.options.Records.GetDraftEntries(ctx, keys)
	if err != nil {
		return s.options.Records.List(ctx, "drafts")
	}
	byKey := make(map[string]Record, len(full))
	for _, entry := range full {
		fullID, valid := entry.Record["id"].(string)
		if !valid || fullID != entry.Key || entry.Key == id && !reflect.DeepEqual(entry.Record["splitChildren"], parent["splitChildren"]) {
			return s.options.Records.List(ctx, "drafts")
		}
		byKey[entry.Key] = entry.Record
	}
	records := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if selected, found := byKey[entry.Key]; found {
			records = append(records, selected)
		} else if !wanted[entry.Key] {
			records = append(records, entry.Record)
		}
	}
	return records, nil
}
