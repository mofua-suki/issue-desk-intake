package issueapi

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"

	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/numbers"
)

type listSnapshotKey struct{}
type listRecord struct {
	raw   string
	found bool
}

type listSnapshot struct {
	closeReasons map[string]listRecord
	proposals    map[string]listRecord
	drafts       map[string]Object
	config       Object
	jobs         map[numbers.Issue]*jobs.Job
	numbers      map[numbers.Issue]int64
	dependencies map[int64][]string
	edges        map[int64]map[string]bool
}

func (s *Service) listSnapshot(ctx context.Context, config Object) (*listSnapshot, error) {
	snapshot := &listSnapshot{config: config, jobs: map[numbers.Issue]*jobs.Job{}, dependencies: map[int64][]string{}, edges: map[int64]map[string]bool{}}
	all, err := s.o.Jobs.AllForIssues(ctx)
	if err != nil {
		return nil, err
	}
	for _, job := range all {
		snapshot.jobs[numbers.Issue{Project: job.Repo, Number: job.Issue}] = job
	}
	snapshot.numbers, err = s.o.Numbers.All(ctx)
	if err != nil {
		return nil, err
	}
	if s.o.Postgres != nil {
		rows, err := s.o.Postgres.QueryContext(ctx, `SELECT d.job_id,p.repo,p.issue, NOT (`+jobs.DependencyPairReadySQL+`) FROM job_deps d JOIN jobs p ON p.id=d.depends_on JOIN jobs j ON j.id=d.job_id ORDER BY p.id`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id, n int64
			var repo string
			var waiting bool
			if err = rows.Scan(&id, &repo, &n, &waiting); err != nil {
				return nil, err
			}
			if snapshot.edges[id] == nil {
				snapshot.edges[id] = map[string]bool{}
			}
			snapshot.edges[id][fmt.Sprintf("%s#%d", strings.ToLower(repo), n)] = true
			if waiting {
				snapshot.dependencies[id] = append(snapshot.dependencies[id], fmt.Sprintf("%s#%d: PRマージ待ち", repo, n))
			}
		}
		if err = rows.Err(); err != nil {
			return nil, err
		}
	}
	for _, job := range all {
		present, _ := jobs.DependencyReferences(job.View, func(repo string, issue int64) (bool, error) {
			return snapshot.edges[job.ID][fmt.Sprintf("%s#%d", strings.ToLower(repo), issue)], nil
		})
		if !present {
			snapshot.dependencies[job.ID] = append(snapshot.dependencies[job.ID], "依存の辺が未作成です")
		}
	}
	return snapshot, nil
}
func (v *listSnapshot) dependencyLines(id int64, repo string) []string {
	lines := make([]string, 0, len(v.dependencies[id]))
	for _, line := range v.dependencies[id] {
		i := strings.IndexByte(line, '#')
		if i >= 0 && strings.EqualFold(line[:i], repo) {
			line = line[i:]
		}
		lines = append(lines, line)
	}
	return lines
}

// loadListDrafts projects referenced drafts and batches their row metadata.
// NUL-containing JSON cannot be projected by PostgreSQL and is decoded by Go.
func (s *Service) loadListDrafts(ctx context.Context, snapshot *listSnapshot, ids, closedIDs map[string]bool) error {
	snapshot.drafts = make(map[string]Object, len(ids))
	keys := make([]string, 0, len(ids))
	for id := range ids {
		keys = append(keys, id)
	}
	const chunkSize = 500
	for start := 0; start < len(keys); start += chunkSize {
		end := start + chunkSize
		if end > len(keys) {
			end = len(keys)
		}
		args := make([]any, end-start)
		marks := make([]string, end-start)
		for i, id := range keys[start:end] {
			args[i] = id
			marks[i] = "?"
		}
		query := `WITH selected AS MATERIALIZED (SELECT id, json AS raw, CASE WHEN position('\u0000' in json) > 0 THEN NULL ELSE json::jsonb END AS value FROM drafts WHERE id IN (` + strings.Join(marks, ",") + `)) SELECT id, CASE WHEN position('\u0000' in raw) > 0 THEN raw WHEN jsonb_typeof(value) = 'object' THEN jsonb_build_object('project', value->'project', 'parentSplit', value->'parentSplit', 'projectDefinition', value->'projectDefinition', 'proposal', jsonb_build_object('source', value->'proposal'->'source', 'kind', value->'proposal'->'kind', 'id', value->'proposal'->'id'), 'attention', jsonb_build_object('project', value->'attention'->'project', 'number', value->'attention'->'number'), 'stallFix', jsonb_build_object('project', value->'stallFix'->'project', 'issue', value->'stallFix'->'issue'))::text ELSE value::text END FROM selected`
		rows, err := s.o.AppDB.QueryContext(ctx, bindSQL(query), args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, raw string
			if err = rows.Scan(&id, &raw); err != nil {
				break
			}
			var draft Object
			if err = json.Unmarshal([]byte(raw), &draft); err != nil {
				break
			}
			snapshot.drafts[id] = draft
		}
		if err == nil {
			err = rows.Err()
		}
		closeErr := rows.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if snapshot.closeReasons == nil {
		snapshot.closeReasons = map[string]listRecord{}
	}
	if err := s.loadListRecords(ctx, "close_reasons", closedIDs, snapshot.closeReasons); err != nil {
		return err
	}
	proposalIDs := map[string]bool{}
	for _, d := range snapshot.drafts {
		proposal := fleet.ObjectOrEmpty(d["proposal"])
		if proposal["kind"] == "improvements" && fleet.Text(proposal["source"]) != "" {
			proposalIDs[fleet.Text(proposal["source"])+":"+fleet.Text(proposal["id"])] = true
		}
	}
	if snapshot.proposals == nil {
		snapshot.proposals = map[string]listRecord{}
	}
	return s.loadListRecords(ctx, "proposals", proposalIDs, snapshot.proposals)
}

// Cache only referenced records, including absent IDs. Keep raw JSON so invalid
// records fail at their original row position, after that row's earlier checks.
func (s *Service) loadListRecords(ctx context.Context, table string, ids map[string]bool, records map[string]listRecord) error {
	keys := make([]string, 0, len(ids))
	for id := range ids {
		if _, loaded := records[id]; !loaded {
			keys = append(keys, id)
		}
	}
	const chunkSize = 500
	for start := 0; start < len(keys); start += chunkSize {
		end := min(start+chunkSize, len(keys))
		marks, args := make([]string, end-start), make([]any, end-start)
		for i, id := range keys[start:end] {
			marks[i], args[i] = "?", id
			records[id] = listRecord{}
		}
		var query string
		switch table {
		case "close_reasons":
			query = "SELECT id,json FROM close_reasons WHERE id IN (" + strings.Join(marks, ",") + ")"
		case "proposals":
			query = "SELECT id,json FROM proposals WHERE id IN (" + strings.Join(marks, ",") + ")"
		default:
			return fmt.Errorf("unknown list metadata table %q", table)
		}
		rows, err := s.o.AppDB.QueryContext(ctx, bindSQL(query), args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, raw string
			if err = rows.Scan(&id, &raw); err != nil {
				break
			}
			records[id] = listRecord{raw: raw, found: true}
		}
		if err == nil {
			err = rows.Err()
		}
		closeErr := rows.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func (s *Service) readListRecord(ctx context.Context, table, id string) (Object, error) {
	if snapshot, ok := ctx.Value(listSnapshotKey{}).(*listSnapshot); ok {
		var records map[string]listRecord
		switch table {
		case "close_reasons":
			records = snapshot.closeReasons
		case "proposals":
			records = snapshot.proposals
		}
		if records != nil {
			record := records[id]
			if !record.found {
				return nil, nil
			}
			var value Object
			err := json.Unmarshal([]byte(record.raw), &value)
			return value, err
		}
	}
	return s.read(ctx, table, id)
}
