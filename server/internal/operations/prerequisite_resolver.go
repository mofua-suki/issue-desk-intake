package operations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
)

// PrerequisiteResolver resolves held jobs against their missing artifacts.
type PrerequisiteResolver struct {
	Service        *Service
	Store          *jobs.Store
	Control        jobs.ControlOptions
	lookupProducer func(context.Context, *sql.Tx, prerequisiteArtifact) (*prerequisiteProducer, error)
	retry          func(context.Context, *sql.Tx, int64, int64, jobs.ControlOptions, ...bool) error
}

type prerequisiteArtifact struct {
	Artifact string `json:"artifact"`
	Repo     string `json:"repo"`
	Issue    int64  `json:"issue"`
}
type prerequisiteProducer struct {
	id, issue    int64
	repo, status string
	artifact     prerequisiteArtifact
}

// Resolve processes the current prerequisite waits once. It is not a scheduler.
func (r *PrerequisiteResolver) Resolve(ctx context.Context) error {
	if r == nil || r.Service == nil || r.Service.PostgreSQL == nil || r.Store == nil {
		return errors.New("prerequisite resolver is not configured")
	}
	rows, err := r.Service.PostgreSQL.QueryContext(ctx, `SELECT id,generation FROM jobs WHERE status='held' AND (hold_reason='prerequisite_missing' OR (hold_reason='question' AND view->'prerequisiteMissingGenerations' @> jsonb_build_array(generation))) ORDER BY id`)
	if err != nil {
		return err
	}
	type candidate struct{ id, generation int64 }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.id, &c.generation); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var all error
	for _, c := range candidates {
		all = errors.Join(all, r.resolveOne(ctx, c.id, c.generation))
	}
	return all
}

func (r *PrerequisiteResolver) resolveOne(ctx context.Context, id, generation int64) error {
	s := r.Service
	tx, err := s.PostgreSQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Match the graph lock order used by ConnectPrerequisiteTx.
	if _, err = tx.ExecContext(ctx, `LOCK TABLE jobs,job_deps IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return err
	}
	view, result, ok, err := r.loadPrerequisiteHold(ctx, tx, id, generation)
	if err != nil || !ok {
		return err
	}
	artifacts, ok := prerequisiteArtifacts(result)
	if !ok {
		return nil
	}
	hash := prerequisiteKey(artifacts)
	records := fleet.Object(view["prerequisiteResolution"])
	if records == nil {
		records = map[string]any{}
	}
	record := fleet.Object(records[hash])
	if record == nil {
		record = map[string]any{}
	}
	attempts := int(fleet.Number(record["attempts"]))
	pick, err := r.selectPrerequisiteProducer(ctx, tx, id, artifacts)
	if err != nil {
		return err
	}
	selected, unknown, outside, allMerged := pick.selected, pick.unknown, pick.outside, pick.allMerged
	nowTime := s.now()
	now := iso(nowTime)
	if fleet.Text(record["startedAt"]) == "" {
		record["startedAt"] = now
	}
	record["artifact"] = artifacts[0]
	if selected != nil {
		record["artifact"] = selected.artifact
		record["issue"] = map[string]any{"project": selected.repo, "number": selected.issue}
	}
	if outside != nil {
		record["artifact"] = *outside
	}
	if outside == nil && allMerged && attempts < 3 && (unknown || fleet.Text(record["state"]) == "rejected") {
		if last, err := time.Parse(time.RFC3339Nano, fleet.Text(record["lastAttemptAt"])); err == nil && nowTime.Sub(last) < 30*time.Minute {
			return nil
		}
	}
	attempts, outcome, err := r.applyPrerequisiteAction(ctx, tx, id, generation, pick, record, attempts, now)
	if err != nil {
		return err
	}
	record["attempts"] = attempts
	records[hash] = record
	raw, err := json.Marshal(records)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE jobs SET view=jsonb_set(view,'{prerequisiteResolution}',$2::jsonb,true),updated_at=$3 WHERE id=$1`, id, string(raw), s.now()); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return outcome
}

// loadPrerequisiteHold は依頼を行ロックで読み、同じ世代の前提待ちで許可対象のリポジトリにあり、停止の指示が無いときだけ view と result を返す。
func (r *PrerequisiteResolver) loadPrerequisiteHold(ctx context.Context, tx *sql.Tx, id, generation int64) (map[string]any, map[string]any, bool, error) {
	var err error
	var status string
	var held sql.NullString
	var gen int64
	var viewRaw, resultRaw []byte
	var leaseHost sql.NullString
	var repo string
	var issue int64
	if err = tx.QueryRowContext(ctx, `SELECT status,hold_reason,generation,view,result,lease_host,repo,issue FROM jobs WHERE id=$1 FOR UPDATE`, id).Scan(&status, &held, &gen, &viewRaw, &resultRaw, &leaseHost, &repo, &issue); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, false, nil
		}
		return nil, nil, false, err
	}
	if status != "held" || gen != generation || !held.Valid {
		return nil, nil, false, nil
	}
	if !r.allowed(repo) {
		return nil, nil, false, nil
	}
	var view, result map[string]any
	if err = json.Unmarshal(viewRaw, &view); err != nil {
		return nil, nil, false, err
	}
	if err = json.Unmarshal(resultRaw, &result); err != nil {
		return nil, nil, false, err
	}
	if view == nil {
		view = map[string]any{}
	}
	if !prerequisiteHoldEligible(held.String, gen, view, result) {
		return nil, nil, false, nil
	}
	if leaseHost.Valid {
		var stopped bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM job_commands WHERE target_host=$1 AND repo=$2 AND issue=$3 AND kind='stop' AND status IN('queued','acked'))`, leaseHost.String, repo, issue).Scan(&stopped); err != nil {
			return nil, nil, false, err
		}
		if stopped {
			return nil, nil, false, nil
		}
	}
	return view, result, true, nil
}

// applyPrerequisiteAction は選んだ作り手に応じて再試行・依存の接続・見送りを行い、結果を record に書いて試行回数と呼び出し側へ返す失敗を返す。
func (r *PrerequisiteResolver) applyPrerequisiteAction(ctx context.Context, tx *sql.Tx, id, generation int64, pick prerequisiteSelection, record map[string]any, attempts int, now string) (int, error, error) {
	var outcome error
	selected, self, outside, allMerged := pick.selected, pick.self, pick.outside, pick.allMerged
	if outside != nil {
		record["state"] = "out_of_scope"
		record["reason"] = "許可対象外のリポジトリ: " + outside.Repo
	} else if attempts >= 3 {
		record["state"] = "limit_reached"
		record["reason"] = "同じ不足に対する自動処理は3回までです。"
	} else if allMerged {
		retry := r.retry
		if retry == nil {
			retry = r.Store.RetryPrerequisiteTx
		}
		err := retry(ctx, tx, id, generation, r.Control, self && attempts > 0)
		if err != nil {
			var p *contract.Problem
			if !errors.As(err, &p) || p.Status != 409 {
				return attempts, nil, err
			}
			record["state"] = "rejected"
			record["reason"] = p.Message
			record["lastAttemptAt"] = now
		} else {
			attempts++
			record["state"] = "retried"
			record["reason"] = ""
			record["lastAttemptAt"] = now
		}
	} else {
		err := r.Store.ConnectPrerequisiteTx(ctx, tx, id, generation, selected.id)
		if err != nil {
			var p *contract.Problem
			if !errors.As(err, &p) || p.Code != "acceptance_transfer_cycle" {
				return attempts, nil, err
			}
			record["state"] = "cycle"
			record["reason"] = p.Message
			outcome = err
		} else {
			attempts++
			record["state"] = "connected"
			record["reason"] = ""
			record["lastAttemptAt"] = now
		}
	}
	return attempts, outcome, nil
}

// prerequisiteHoldEligible は止まった原因が前提の不足で、停止・実地確認・権限・削除の承認待ちでないかを判定する。
func prerequisiteHoldEligible(hold string, gen int64, view, result map[string]any) bool {
	if hold != "prerequisite_missing" {
		valid := false
		for _, v := range fleet.List(view["prerequisiteMissingGenerations"]) {
			if int64(fleet.Number(v)) == gen {
				valid = true
			}
		}
		if hold != "question" || !valid || result["status"] != "failed" {
			return false
		}
	}
	if view["stopRequested"] == true || fleet.Object(view["liveCheck"])["status"] == "pending" || view["permissionHold"] != nil || view["deletionApproval"] != nil {
		return false
	}
	return true
}

// prerequisiteArtifacts は失敗の結果から不足した成果物を読み、並べて返す。前提の不足だけで止まった結果でなければ ok=false を返す。
func prerequisiteArtifacts(result map[string]any) ([]prerequisiteArtifact, bool) {
	missing := fleet.Object(result["prerequisiteMissing"])
	rawArtifacts := fleet.List(missing["artifacts"])
	if len(rawArtifacts) == 0 || result["status"] != "failed" || result["incomplete"] != nil || len(fleet.List(result["questions"])) != 0 {
		return nil, false
	}
	artifacts := make([]prerequisiteArtifact, 0, len(rawArtifacts))
	for _, v := range rawArtifacts {
		a := fleet.Object(v)
		n := fleet.Number(a["issue"])
		item := prerequisiteArtifact{fleet.Text(a["artifact"]), fleet.Text(a["repo"]), int64(n)}
		if item.Artifact == "" || !fleet.ValidRepo(item.Repo) || n < 1 || float64(item.Issue) != n {
			return nil, false
		}
		artifacts = append(artifacts, item)
	}
	sort.Slice(artifacts, func(i, j int) bool {
		a, b := artifacts[i], artifacts[j]
		if a.Artifact != b.Artifact {
			return a.Artifact < b.Artifact
		}
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		return a.Issue < b.Issue
	})
	return artifacts, true
}

// prerequisiteKey は不足した依頼の組から、自動処理の回数を数える鍵を作る。
func prerequisiteKey(artifacts []prerequisiteArtifact) string {
	// The artifact text is free-form AI output that changes between runs, so the
	// retry limit must be keyed only by which issues are missing.
	targets := map[string]bool{}
	for _, a := range artifacts {
		targets[fmt.Sprintf("%s#%d", a.Repo, a.Issue)] = true
	}
	keys := make([]string, 0, len(targets))
	for key := range targets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	canonical, _ := json.Marshal(keys)
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// prerequisiteSelection は不足した成果物を作る依頼を探した結果。
type prerequisiteSelection struct {
	selected      *prerequisiteProducer
	outside       *prerequisiteArtifact
	unknown, self bool
	allMerged     bool
}

// selectPrerequisiteProducer は不足した成果物ごとに作る依頼を探し、待つ先（未マージの最初の依頼、無ければマージ済みの最初の依頼）と、許可対象外・不明・自分自身の有無を返す。
func (r *PrerequisiteResolver) selectPrerequisiteProducer(ctx context.Context, tx *sql.Tx, id int64, artifacts []prerequisiteArtifact) (prerequisiteSelection, error) {
	var selected *prerequisiteProducer
	unknown, self := false, false
	var outside *prerequisiteArtifact
	for i := range artifacts {
		if !r.allowed(artifacts[i].Repo) {
			outside = &artifacts[i]
			break
		}
	}
	allMerged := true
	if outside == nil {
		for _, a := range artifacts {
			lookup := r.lookupProducer
			if lookup == nil {
				lookup = r.producer
			}
			p, err := lookup(ctx, tx, a)
			if err != nil {
				return prerequisiteSelection{}, err
			}
			if p == nil || (p.status != "queued" && p.status != "running" && p.status != "held" && p.status != "failed" && p.status != "merged") {
				unknown = true
				continue
			}
			if p.id == id {
				self = true
				continue
			}
			if p.status != "merged" {
				if allMerged {
					selected = p
				}
				allMerged = false
			} else if selected == nil {
				selected = p
			}
		}
	}
	return prerequisiteSelection{selected: selected, outside: outside, unknown: unknown, self: self, allMerged: allMerged}, nil
}

func (r *PrerequisiteResolver) allowed(repo string) bool {
	if r.Service.Assignments == nil {
		return false
	}
	for name, hosts := range r.Service.Assignments.Implementers {
		if strings.EqualFold(name, repo) && len(hosts) > 0 {
			return true
		}
	}
	return false
}

func (r *PrerequisiteResolver) producer(ctx context.Context, tx *sql.Tx, a prerequisiteArtifact) (*prerequisiteProducer, error) {
	// The desk ledger belongs to the application store; the Gitea number and
	// ledger mapping must identify the same job when both exist.
	numbers := []int64{a.Issue}
	if r.Service.DB == nil {
		return nil, errors.New("desk number store is unavailable")
	}
	var repo string
	var issue int64
	err := r.Service.DB.QueryRowContext(ctx, bindSQL(`SELECT project,gitea FROM desk_numbers WHERE number=?`), a.Issue).Scan(&repo, &issue)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && strings.EqualFold(repo, a.Repo) && issue != a.Issue {
		numbers = append(numbers, issue)
	}
	var found *prerequisiteProducer
	for _, number := range numbers {
		rows, err := tx.QueryContext(ctx, `SELECT id,repo,issue,status FROM jobs WHERE lower(repo)=lower($1) AND issue=$2`, a.Repo, number)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var p prerequisiteProducer
			if err = rows.Scan(&p.id, &p.repo, &p.issue, &p.status); err != nil {
				rows.Close()
				return nil, err
			}
			if found != nil && found.id != p.id {
				rows.Close()
				return nil, nil
			}
			p.artifact = a
			found = &p
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return found, nil
}
