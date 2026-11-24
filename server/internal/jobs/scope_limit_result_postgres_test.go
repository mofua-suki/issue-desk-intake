package jobs

import (
	"errors"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/contract"
)

func TestPostgresScopeLimitResultNormalization(t *testing.T) {
	for _, tc := range []struct {
		name, initial string
		keep          bool
	}{
		{"json_null", "'null'::jsonb", false}, {"sql_null", "NULL", false}, {"object", `'{"keep":true,"error":"old"}'::jsonb`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, ctx := testPostgres(t)
			at := time.Now()
			s, _ := New(db, Options{Now: func() time.Time { return at }})
			_, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,scope_source,lease_host,lease_epoch,lease_expires_at,result) VALUES(1,'owner/repo',1,'running',ARRAY['a','b','c','d'],'declared','worker',1,$1,`+tc.initial+`)`, at.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.Widen(ctx, 1, "worker", 1, []string{"e", "f", "g", "h", "i"}, nil)
			code(t, err, 409, "lease_lost")
			if got != nil {
				t.Fatalf("widen: %+v %v", got, err)
			}
			var typ, message, status string
			var keep bool
			// Re-read on a separate connection after Widen has committed and rejected.
			reader, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			err = reader.QueryRowContext(ctx, `SELECT jsonb_typeof(result),result->>'error',status,coalesce((result->>'keep')::boolean,false) FROM jobs WHERE id=1`).Scan(&typ, &message, &status, &keep)
			if err != nil || typ != "object" || status != "stopped" || !strings.Contains(message, "理由を付けた再開") || keep != tc.keep {
				t.Fatalf("stored: %s %s %s %v %v", typ, message, status, keep, err)
			}
		})
	}
}

func TestPostgresScopeLimitSaveAndCommitFailures(t *testing.T) {
	for _, phase := range []string{"IMMEDIATE", "DEFERRED"} {
		t.Run(phase, func(t *testing.T) {
			db, ctx := testPostgres(t)
			at := time.Now()
			s, _ := New(db, Options{Now: func() time.Time { return at }})
			if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,scope,scope_source,lease_host,lease_epoch,lease_expires_at,result) VALUES(1,'owner/repo',1,'running',ARRAY['a'],'declared','worker',1,$1,'{"keep":true}');`, at.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO merge_turns(repo,holder_host,job_id,expires_at) VALUES('owner/repo','worker',1,$1)`, at.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			// The deferred trigger raises only at COMMIT, after all stop writes.
			if _, err := db.ExecContext(ctx, `CREATE FUNCTION reject_scope_stop() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.kind='scope_over_limit' THEN RAISE EXCEPTION 'injected scope stop failure'; END IF; RETURN NEW; END $$;
CREATE CONSTRAINT TRIGGER reject_scope_stop AFTER INSERT ON job_events DEFERRABLE INITIALLY `+phase+` FOR EACH ROW EXECUTE FUNCTION reject_scope_stop()`); err != nil {
				t.Fatal(err)
			}
			got, err := s.Widen(ctx, 1, "worker", 1, []string{"b", "c", "d", "e", "f", "g", "h", "i"}, nil)
			var problem *contract.Problem
			if got != nil || err == nil || errors.As(err, &problem) || !strings.Contains(err.Error(), "injected scope stop failure") {
				t.Fatalf("failure lost: %+v %v", got, err)
			}
			reader, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			var intact bool
			if err := reader.QueryRowContext(ctx, `SELECT status='running' AND scope=ARRAY['a'] AND lease_expires_at IS NOT NULL AND result='{"keep":true}'::jsonb AND EXISTS(SELECT 1 FROM merge_turns WHERE job_id=1 AND holder_host='worker' AND expires_at IS NOT NULL) AND NOT EXISTS(SELECT 1 FROM job_events WHERE job_id=1) FROM jobs WHERE id=1`).Scan(&intact); err != nil || !intact {
				t.Fatalf("rollback=%v err=%v", intact, err)
			}
		})
	}
}

func TestPostgresScopeLimitArrayDefense(t *testing.T) {
	db, ctx := testPostgres(t)
	at := time.Now()
	// Isolated test database only: production constraint remains in place.
	if _, err := db.ExecContext(ctx, `ALTER TABLE jobs DROP CONSTRAINT jobs_result_object_only`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO jobs(id,repo,issue,status,result) VALUES(1,'owner/repo',1,'running','[]'::jsonb)`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = stopScopeOverLimit(ctx, tx, &Job{ID: 1}, "worker", []string{"e"}, 5, 4, 4, nil, at); err != nil {
		t.Fatal(err)
	}
	var typ, status, message string
	if err = tx.QueryRowContext(ctx, `SELECT jsonb_typeof(result),status,result->>'error' FROM jobs WHERE id=1`).Scan(&typ, &status, &message); err != nil || typ != "object" || status != "stopped" || message == "" {
		t.Fatalf("%s %s %s %v", typ, status, message, err)
	}
}
