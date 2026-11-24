package jobs

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"strings"
)

type TransferCheck struct {
	JobID          int64
	Repo           string
	Provider       string
	Issue          int64
	Key            string
	Context        map[string]any
	Relation       string
	ConsultationID string
}

func TransferConsultationID(jobID int64, key string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", jobID, key)))
	hash := hex.EncodeToString(sum[:])
	return hash[:8] + "-" + hash[8:12] + "-" + hash[12:16] + "-" + hash[16:20] + "-" + hash[20:32]
}
func (s *Store) RefreshTransfers(ctx context.Context) error {
	return s.refreshTransfersWithReader(ctx, readScopedTransferGraph)
}

func (s *Store) refreshTransfersWithReader(ctx context.Context, read func(context.Context, transferQuery) ([]*transferJob, error)) error {
	all, err := read(ctx, s.db)
	if err != nil {
		return err
	}
	prepared := s.prepareTransferGraph(ctx, all)
	at := iso(s.options.Now())
	err = applyTransferChanges(ctx, transferProbe{s.db}, all, prepared, at)
	if err == nil {
		// Issue reads can take time. Recheck the graph before returning without
		// the lock so changes made during those reads are not missed.
		all, err = read(ctx, s.db)
		if err != nil {
			return err
		}
		err = applyTransferChanges(ctx, transferProbe{s.db}, all, prepared, at)
		if err == nil {
			return nil
		}
	}
	if !errors.Is(err, errTransferWrite) {
		return err
	}
	return s.transaction(ctx, func(tx *sql.Tx) error { return refreshTransfersWithReader(ctx, tx, prepared, at, read) })
}
func (s *Store) TransferChecks(ctx context.Context) ([]TransferCheck, error) {
	all, err := readTransferRows(ctx, s.db, `SELECT j.id,j.repo,j.issue,j.title,j.status,coalesce(j.provider,'codex'),
 jsonb_build_object('acceptanceTransferInput',j.view->'acceptanceTransferInput',
 'acceptanceTransferWait',jsonb_build_object('checks',j.view->'acceptanceTransferWait'->'checks')),
 '{}'::jsonb, '[]'::json FROM jobs j WHERE j.status='queued'`)
	if err != nil {
		return nil, err
	}
	out := []TransferCheck{}
	for _, job := range all {
		if job.Status != "queued" || fleet.Object(job.View["acceptanceTransferInput"])["unavailable"] == true {
			continue
		}
		for _, raw := range fleet.List(fleet.Object(job.View["acceptanceTransferWait"])["checks"]) {
			check := fleet.Object(raw)
			relation := fleet.Text(check["relation"])
			consultation := fleet.Text(check["consultationId"])
			if relation != "unknown" && consultation == "" {
				continue
			}
			out = append(out, TransferCheck{JobID: job.ID, Repo: job.Repo, Provider: job.Provider, Issue: job.Issue, Key: fleet.Text(check["key"]), Context: fleet.Object(check["context"]), Relation: relation, ConsultationID: consultation})
		}
	}
	return out, nil
}
func (s *Store) ResolveTransfer(ctx context.Context, id int64, key, consultation, relation, evidence string) error {
	if relation != "" && relation != "required" && relation != "unrelated" || relation != "" && (strings.TrimSpace(evidence) == "" || contract.UTF16Length(evidence) > 4000) {
		return contract.NewProblem(400, "移管関係の照合結果の形が違います。", "")
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "LOCK TABLE jobs,job_deps IN SHARE ROW EXCLUSIVE MODE"); err != nil {
			return err
		}
		job, err := readJob(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if job.Status != "queued" {
			return nil
		}
		found := false
		for _, raw := range fleet.List(fleet.Object(job.View["acceptanceTransferWait"])["checks"]) {
			if fleet.Object(raw)["key"] == key {
				found = true
			}
		}
		if !found {
			return nil
		}
		checks := fleet.Object(job.View["acceptanceTransferChecks"])
		if checks == nil {
			checks = map[string]any{}
		}
		answer := map[string]any{"consultationId": consultation}
		if relation != "" {
			answer["relation"], answer["evidence"] = relation, evidence
		}
		checks[key] = answer
		_, err = tx.ExecContext(ctx, "UPDATE jobs SET view=jsonb_set(view,'{acceptanceTransferChecks}',$2::jsonb)WHERE id=$1", id, transferJSON(checks))
		return err
	})
}
