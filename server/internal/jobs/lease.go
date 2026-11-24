package jobs

import (
	"context"
	"database/sql"
)

func (s *Store) Renew(ctx context.Context, id int64, host string, epoch int64, progress map[string]any) (*Job, *Lease, error) {
	if err := checkObject(progress, "progress"); err != nil {
		return nil, nil, err
	}
	at := s.options.Now()
	var job *Job
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		current, err := fenced(ctx, tx, id, host, epoch, at, false)
		if err != nil {
			return err
		}
		body, err := jsonValue(progress)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE jobs SET lease_expires_at=$2,
progress_at=CASE WHEN $3::jsonb IS NULL THEN progress_at ELSE $4::timestamptz END,
view=CASE WHEN $3::jsonb IS NULL THEN view ELSE jsonb_set(view,'{progress}',$3::jsonb) END,
updated_at=$4 WHERE id=$1`, id, at.Add(s.options.LeaseDuration), body, at)
		if err != nil {
			return err
		}
		diagnostic := map[string]any{"generation": current.Generation, "epoch": epoch, "at": iso(at), "expiresAt": iso(at.Add(s.options.LeaseDuration))}
		body, err = jsonValue(diagnostic)
		if err != nil {
			return err
		}
		updated, err := tx.ExecContext(ctx, `UPDATE job_events SET at=$2,host=$3,data=$4::jsonb WHERE job_id=$1 AND kind='lease_renewed'`, id, at, host, body)
		if err != nil {
			return err
		}
		count, err := updated.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			if err = addEvent(ctx, tx, id, host, "lease_renewed", diagnostic, at); err != nil {
				return err
			}
		}
		job, err = readJob(ctx, tx, id, false)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return job, &Lease{Epoch: job.LeaseEpoch, ExpiresAt: *job.LeaseExpiresAt}, nil
}
