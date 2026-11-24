package jobs

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"strconv"
	"time"
)

type transferQuery interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readTransferRows(ctx context.Context, q transferQuery, query string) ([]*transferJob, error) {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*transferJob{}
	for rows.Next() {
		j := &transferJob{}
		var view, result, deps []byte
		if err = rows.Scan(&j.ID, &j.Repo, &j.Issue, &j.Title, &j.Status, &j.Provider, &view, &result, &deps); err != nil {
			return nil, err
		}
		if json.Unmarshal(view, &j.View) != nil || json.Unmarshal(deps, &j.Deps) != nil {
			return nil, fmt.Errorf("invalid stored transfer graph")
		}
		j.Result = StoredResult(j.ID, result)
		out = append(out, j)
	}
	return out, rows.Err()
}

// readTransferGraph retains full queued views while projecting only fields used when
// traversing other jobs. Dependencies must include jobs of every status.
func readTransferGraph(ctx context.Context, q transferQuery) ([]*transferJob, error) {
	return readTransferRows(ctx, q, `SELECT j.id,j.repo,j.issue,j.title,j.status,coalesce(j.provider,'codex'),
 CASE WHEN j.status='queued' THEN j.view ELSE jsonb_build_object(
 'acceptanceTransferRecords',j.view->'acceptanceTransferRecords',
 'acceptanceTransferWait',jsonb_build_object('ownedTargets',j.view->'acceptanceTransferWait'->'ownedTargets'))
 || CASE WHEN j.view ? 'dependsOnIssues' THEN jsonb_build_object('dependsOnIssues',j.view->'dependsOnIssues') ELSE '{}'::jsonb END
 || CASE WHEN j.view ? 'unresolvedDependencies' THEN jsonb_build_object('unresolvedDependencies',j.view->'unresolvedDependencies') ELSE '{}'::jsonb END END,
 CASE WHEN jsonb_typeof(j.result)='object' THEN jsonb_build_object('mergeCommit',j.result->'mergeCommit') ELSE '{}'::jsonb END,
 array_to_json(ARRAY(SELECT depends_on FROM job_deps WHERE job_id=j.id)) FROM jobs j`)
}

// readScopedTransferGraph follows all possible references from active jobs. The
// full reader remains available to dependency operations with arbitrary targets.
func readScopedTransferGraph(ctx context.Context, q transferQuery) ([]*transferJob, error) {
	return readTransferRows(ctx, q, `WITH RECURSIVE reachable(id) AS (
 SELECT id FROM jobs WHERE status IN ('queued','held','running')
 UNION
 SELECT next.id FROM reachable r JOIN jobs current ON current.id=r.id
 CROSS JOIN LATERAL (
  SELECT d.depends_on AS id FROM job_deps d WHERE d.job_id=current.id
  UNION
  SELECT target.id FROM (
   SELECT ref->>'repo' AS repo, ref->'issue' AS issue
   FROM jsonb_array_elements(CASE WHEN jsonb_typeof(current.view->'dependsOnIssues')='array' THEN current.view->'dependsOnIssues' ELSE '[]'::jsonb END ||
    CASE WHEN jsonb_typeof(current.view->'unresolvedDependencies')='array' THEN current.view->'unresolvedDependencies' ELSE '[]'::jsonb END) ref
   UNION ALL
   SELECT proposal->'issue'->>'project', proposal->'issue'->'number'
   FROM jsonb_array_elements(CASE WHEN jsonb_typeof(current.view->'acceptanceTransferRecords'->'proposals')='array' THEN current.view->'acceptanceTransferRecords'->'proposals' ELSE '[]'::jsonb END) proposal
  ) refs JOIN jobs target ON lower(target.repo)=lower(refs.repo)
   AND CASE WHEN jsonb_typeof(refs.issue)='number' THEN target.issue::numeric = trunc((refs.issue #>> '{}')::numeric) ELSE false END
 ) next
 )
 SELECT j.id,j.repo,j.issue,j.title,j.status,coalesce(j.provider,'codex'),
 CASE WHEN j.status='queued' THEN j.view ELSE jsonb_build_object(
 'acceptanceTransferRecords',j.view->'acceptanceTransferRecords',
 'acceptanceTransferWait',jsonb_build_object('ownedTargets',j.view->'acceptanceTransferWait'->'ownedTargets'))
 || CASE WHEN j.view ? 'dependsOnIssues' THEN jsonb_build_object('dependsOnIssues',j.view->'dependsOnIssues') ELSE '{}'::jsonb END
 || CASE WHEN j.view ? 'unresolvedDependencies' THEN jsonb_build_object('unresolvedDependencies',j.view->'unresolvedDependencies') ELSE '{}'::jsonb END END,
 CASE WHEN jsonb_typeof(j.result)='object' THEN jsonb_build_object('mergeCommit',j.result->'mergeCommit') ELSE '{}'::jsonb END,
 array_to_json(ARRAY(SELECT depends_on FROM job_deps WHERE job_id=j.id)) FROM reachable r JOIN jobs j ON j.id=r.id`)
}

var errTransferWrite = errors.New("transfer write required")

type transferWriter interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
type transferProbe struct{ *sql.DB }

func (p transferProbe) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	return nil, errTransferWrite
}

type transferPrepared struct {
	Snapshot string
	Input    map[string]any
}

func transferSnapshot(job *transferJob, plan transferPlan) string {
	keys := []any{}
	for _, raw := range plan.Checks {
		keys = append(keys, fleet.Object(raw)["key"])
	}
	return transferJSON([]any{job.Repo, job.Issue, job.View["acceptanceTransferInput"], keys})
}
func (s *Store) prepareTransfers(ctx context.Context) (map[int64]transferPrepared, error) {
	all, err := readScopedTransferGraph(ctx, s.db)
	if err != nil {
		return nil, err
	}
	return s.prepareTransferGraph(ctx, all), nil
}

func (s *Store) prepareTransferGraph(ctx context.Context, all []*transferJob) map[int64]transferPrepared {
	return s.prepareTransferGraphWithIndexBuilder(ctx, all, newTransferIndex)
}

func (s *Store) prepareTransferGraphWithIndexBuilder(ctx context.Context, all []*transferJob, build func([]*transferJob) *transferIndex) map[int64]transferPrepared {
	prepared := map[int64]transferPrepared{}
	index := build(all)
	for _, job := range all {
		if job.Status != "queued" {
			continue
		}
		plan := index.plan(job)
		if len(plan.Checks) == 0 {
			continue
		}
		input := map[string]any{"unavailable": true, "body": nil, "complete": false}
		if s.options.ReadIssue != nil {
			issue, e := s.options.ReadIssue(ctx, job.Repo, job.Issue)
			number, numberOK := count(issue["number"])
			if e == nil && numberOK && number == job.Issue {
				raw, hasBody := issue["body"]
				body, valid := raw.(string)
				if hasBody && (valid || raw == nil) {
					sum := sha256.Sum256([]byte(body))
					input = map[string]any{"title": fleet.Text(issue["title"]), "body": contract.ClipUTF16(body, 12000), "complete": contract.UTF16Length(body) <= 12000, "revision": hex.EncodeToString(sum[:])}
				}
			}
		}
		prepared[job.ID] = transferPrepared{transferSnapshot(job, plan), input}
	}
	return prepared
}
func refreshTransfers(ctx context.Context, tx *sql.Tx, prepared map[int64]transferPrepared, at string) error {
	return refreshTransfersWithReader(ctx, tx, prepared, at, readScopedTransferGraph)
}

func refreshTransfersWithReader(ctx context.Context, tx *sql.Tx, prepared map[int64]transferPrepared, at string, read func(context.Context, transferQuery) ([]*transferJob, error)) error {
	if _, err := tx.ExecContext(ctx, "LOCK TABLE jobs,job_deps IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return err
	}
	all, err := read(ctx, tx)
	if err != nil {
		return err
	}
	return applyTransferChanges(ctx, tx, all, prepared, at)
}

func applyTransferChanges(ctx context.Context, tx transferWriter, all []*transferJob, prepared map[int64]transferPrepared, at string) error {
	return applyTransferChangesWithIndexBuilder(ctx, tx, all, prepared, at, newTransferIndex)
}

func applyTransferChangesWithIndexBuilder(ctx context.Context, tx transferWriter, all []*transferJob, prepared map[int64]transferPrepared, at string, build func([]*transferJob) *transferIndex) error {
	index := build(all)
	if err := applyTransferInputs(ctx, tx, all, prepared, index); err != nil {
		return err
	}
	for _, job := range all {
		if job.Status != "queued" {
			continue
		}
		if err := applyTransferWait(ctx, tx, job, index, at); err != nil {
			return err
		}
	}
	return nil
}

// applyTransferInputs は待ちの依頼ごとに、移管の確認に使う入力を準備済みの結果（計画が変わっていれば読めない印）で更新する。
func applyTransferInputs(ctx context.Context, tx transferWriter, all []*transferJob, prepared map[int64]transferPrepared, index *transferIndex) error {
	var err error
	for _, job := range all {
		if job.Status != "queued" {
			continue
		}
		plan := index.plan(job)
		if len(plan.Checks) == 0 {
			continue
		}
		p, ok := prepared[job.ID]
		input := map[string]any{"unavailable": true, "body": nil, "complete": false}
		if ok && p.Snapshot == transferSnapshot(job, plan) {
			input = p.Input
		}
		if !reflect.DeepEqual(job.View["acceptanceTransferInput"], input) {
			if _, err = tx.ExecContext(ctx, "UPDATE jobs SET view=jsonb_set(view,'{acceptanceTransferInput}',$2::jsonb)WHERE id=$1", job.ID, transferJSON(input)); err != nil {
				return err
			}
			job.View["acceptanceTransferInput"] = input
		}
	}
	return nil
}

// applyTransferWait は待ちの依頼1件の移管待ちを計画に合わせ、受け持つ依存の付け替え・取り消しと待ちの記録を更新する。
func applyTransferWait(ctx context.Context, tx transferWriter, job *transferJob, index *transferIndex, at string) error {
	var err error
	byID := index.byID
	now, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return err
	}
	plan := index.planAt(job, now)
	previous := fleet.Object(job.View["acceptanceTransferWait"])
	owned := fleet.Object(fleet.Clone(previous["ownedTargets"]))
	if owned == nil {
		owned = map[string]any{}
	}
	paths := fleet.Object(fleet.Clone(previous["ownedPaths"]))
	if paths == nil {
		paths = map[string]any{}
	}
	ownership := append([]any{}, fleet.List(previous["checks"])...)
	for _, raw := range fleet.Object(previous["ownedChecks"]) {
		ownership = append(ownership, raw)
	}
	keyMap := transferKeyMap(ownership, plan.Checks)
	currentKey := func(key string) string {
		if keyMap[key] != "" {
			return keyMap[key]
		}
		return key
	}
	revoked := func(path []string) bool {
		for _, key := range path {
			for _, raw := range plan.Checks {
				check := fleet.Object(raw)
				if check["key"] == key && check["relation"] == "unrelated" {
					return true
				}
			}
		}
		return false
	}
	if err = reconcileOwnedTransferTargets(ctx, tx, job, plan, owned, paths, byID, currentKey, revoked); err != nil {
		return err
	}
	if err = connectTransferTargets(ctx, tx, job, &plan, owned, paths, revoked); err != nil {
		return err
	}
	ownedChecks := transferOwnedChecks(append(ownership, plan.Checks...), paths)
	var since any
	if plan.Blocking {
		since = previous["since"]
		if since == nil {
			since = at
		}
	}
	next := map[string]any{"blocking": plan.Blocking, "missingSince": plan.MissingSince, "orphanedWaits": plan.OrphanedWaits, "checks": plan.Checks, "waits": plan.Waits, "targets": plan.Targets, "targetChecks": plan.TargetChecks, "targetPaths": plan.TargetPaths, "ownedTargets": owned, "ownedPaths": paths, "ownedChecks": ownedChecks, "since": since}
	if previous == nil && len(plan.Checks) == 0 {
		return nil
	}
	if !reflect.DeepEqual(previous, fleet.Clone(next)) {
		if _, err = tx.ExecContext(ctx, "UPDATE jobs SET view=jsonb_set(view,'{acceptanceTransferWait}',$2::jsonb)WHERE id=$1", job.ID, transferJSON(next)); err != nil {
			return err
		}
		job.View["acceptanceTransferWait"] = next
	}
	return nil
}

// transferKeyMap は前回受け持った確認の鍵を、確認の内容が同じ今回の確認の鍵へ対応づける。
func transferKeyMap(ownership, checks []any) map[string]string {
	keyMap := map[string]string{}
	for _, raw := range ownership {
		old := fleet.Object(raw)
		oldContext := fleet.Object(old["context"])
		for _, rawNew := range checks {
			check := fleet.Object(rawNew)
			context := fleet.Object(check["context"])
			same := true
			for _, field := range []string{"source", "criterion", "artifacts", "proposals"} {
				if !reflect.DeepEqual(oldContext[field], context[field]) {
					same = false
				}
			}
			if same {
				keyMap[fleet.Text(old["key"])] = fleet.Text(check["key"])
				break
			}
		}
	}
	return keyMap
}

// reconcileOwnedTransferTargets は前回受け持った依存先ごとに確認の経路を今回の鍵へ付け替え、明示の依存になった先は手放し、すべての経路が無関係になった先は依存を消す。
func reconcileOwnedTransferTargets(ctx context.Context, tx transferWriter, job *transferJob, plan transferPlan, owned, paths map[string]any, byID map[int64]*transferJob, currentKey func(string) string, revoked func([]string) bool) error {
	var err error
	for target, oldRaw := range owned {
		oldKeys := fleet.Strings(oldRaw)
		oldPaths := []any{}
		consistent := true
		rawPaths := fleet.List(paths[target])
		if len(rawPaths) == 0 {
			consistent = false
		}
		for _, raw := range rawPaths {
			path := fleet.Strings(fleet.Clone(raw))
			if len(path) == 0 {
				consistent = false
				break
			}
			oldPaths = append(oldPaths, path)
		}
		if !consistent {
			oldPaths = []any{}
			for _, key := range oldKeys {
				oldPaths = append(oldPaths, []string{key})
			}
		}
		mappedPaths := []any{}
		keys := []string{}
		for _, raw := range oldPaths {
			var path []string
			switch v := raw.(type) {
			case []string:
				path = v
			default:
				path = fleet.Strings(v)
			}
			mapped := []string{}
			for _, key := range path {
				mapped = append(mapped, currentKey(key))
			}
			if len(mapped) > 0 {
				mappedPaths = append(mappedPaths, mapped)
				keys = appendUniqueString(keys, mapped[len(mapped)-1])
			}
		}
		owned[target] = keys
		paths[target] = mappedPaths
		id, _ := strconv.ParseInt(target, 10, 64)
		destination := byID[id]
		explicit := false
		for _, field := range []string{"dependsOnIssues", "unresolvedDependencies"} {
			for _, raw := range fleet.List(job.View[field]) {
				d := fleet.Object(raw)
				if destination != nil && transferKey(destination.Repo, destination.Issue) == transferKey(fleet.Text(d["repo"]), int64(fleet.Number(d["issue"]))) {
					explicit = true
				}
			}
		}
		if explicit {
			delete(owned, target)
			delete(paths, target)
			continue
		}
		required := false
		for _, id := range plan.Targets {
			if fmt.Sprint(id) == target {
				required = true
			}
		}
		allRevoked := len(mappedPaths) > 0
		for _, raw := range mappedPaths {
			path := raw.([]string)
			if !revoked(path) {
				allRevoked = false
			}
		}
		if !required && allRevoked {
			if _, err = tx.ExecContext(ctx, "DELETE FROM job_deps WHERE job_id=$1 AND depends_on=$2", job.ID, id); err != nil {
				return err
			}
			next := []int64{}
			for _, dep := range job.Deps {
				if dep != id {
					next = append(next, dep)
				}
			}
			job.Deps = next
			delete(owned, target)
			delete(paths, target)
		}
	}
	return nil
}

// connectTransferTargets は計画が求める依存先を足し（循環するなら止める待ちにする）、受け持つ確認の経路を今回の計画で更新する。
func connectTransferTargets(ctx context.Context, tx transferWriter, job *transferJob, plan *transferPlan, owned, paths map[string]any, revoked func([]string) bool) error {
	var err error
	for _, target := range plan.Targets {
		exists := false
		for _, id := range job.Deps {
			if id == target {
				exists = true
			}
		}
		if !exists {
			var cycle bool
			if err = tx.QueryRowContext(ctx, `WITH RECURSIVE reachable(id)AS(SELECT $1::bigint UNION SELECT d.depends_on FROM job_deps d JOIN reachable r ON d.job_id=r.id)SELECT EXISTS(SELECT 1 FROM reachable WHERE id=$2)`, target, job.ID).Scan(&cycle); err != nil {
				return err
			}
			if cycle {
				plan.Blocking = true
				plan.Waits = append(plan.Waits, map[string]any{"reason": "移管成果物の依存が循環するため開始できません。", "artifacts": []any{}, "source": "", "criterion": "", "destination": nil})
				continue
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO job_deps(job_id,depends_on)VALUES($1,$2)ON CONFLICT DO NOTHING", job.ID, target); err != nil {
				return err
			}
			job.Deps = append(job.Deps, target)
			owned[fmt.Sprint(target)] = plan.TargetChecks[target]
		}
		key := fmt.Sprint(target)
		if owned[key] != nil {
			next := []any{}
			for _, raw := range fleet.List(paths[key]) {
				path := fleet.Strings(fleet.Clone(raw))
				if len(path) > 0 && !revoked(path) {
					next = append(next, path)
				}
			}
			for _, path := range plan.TargetPaths[target] {
				duplicate := false
				for _, raw := range next {
					if reflect.DeepEqual(raw, path) {
						duplicate = true
					}
				}
				if !duplicate {
					next = append(next, path)
				}
			}
			paths[key] = next
			keys := []string{}
			for _, raw := range next {
				path := raw.([]string)
				keys = appendUniqueString(keys, path[len(path)-1])
			}
			owned[key] = keys
		}
	}
	return nil
}

// transferOwnedChecks は受け持つ経路に含まれる確認を、鍵ごとに集める。
func transferOwnedChecks(checks []any, paths map[string]any) map[string]any {
	ownedChecks := map[string]any{}
	for _, raw := range checks {
		check := fleet.Object(raw)
		key := fleet.Text(check["key"])
		for _, rawPaths := range paths {
			for _, rawPath := range fleet.List(fleet.Clone(rawPaths)) {
				path := fleet.Strings(rawPath)
				for _, part := range path {
					if part == key {
						ownedChecks[key] = check
					}
				}
			}
		}
	}
	return ownedChecks
}
