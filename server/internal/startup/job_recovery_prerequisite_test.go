package startup

import (
	"context"
	"errors"
	"testing"

	"issue-desk-intake/server/internal/issueapi"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/operations"
)

func TestRecoverJobsRunsPrerequisiteResolver(t *testing.T) {
	resolutionErr := errors.New("resolution failed")
	candidateErr := errors.New("candidate failed")
	calls, acted := 0, 0
	err := runRecoveryCycle(t.Context(), func(context.Context) error { calls++; return resolutionErr },
		func(context.Context) ([]operations.RecoveryCandidate, error) {
			return []operations.RecoveryCandidate{{}}, nil
		},
		func(context.Context, operations.RecoveryCandidate) (operations.RecoveryResult, error) {
			acted++
			return operations.Skipped, candidateErr
		})
	if calls != 1 || acted != 1 || !errors.Is(err, resolutionErr) || !errors.Is(err, candidateErr) {
		t.Fatalf("calls=%d acted=%d err=%v", calls, acted, err)
	}
}

func TestRecoverJobsProcessesPrerequisitePostgres(t *testing.T) {
	app := reviewNotesApp(t)
	_, assignments, err := app.Fleet.Load()
	if err != nil {
		t.Fatal(err)
	}
	app.Jobs, err = jobs.New(app.DB, jobs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	app.Issues = &issueapi.Service{}
	defer func() { app.Issues = nil }()
	service := &operations.Service{PostgreSQL: app.DB, DB: app.SQLite.DB(), Assignments: assignments, Repositories: func() ([]string, error) { return app.Repositories(), nil }, RuntimeDir: app.Config.RuntimeDir}
	app.Operations = func() (*operations.Service, error) { return service, nil }
	_, err = app.DB.Exec(`INSERT INTO jobs(id,repo,issue,title,status,hold_reason,generation,view,result) VALUES
 (90001,'mofua-suki/issue-desk-intake',90001,'waiting','held','question',1,'{"prerequisiteMissingGenerations":[1]}','{"status":"failed","questions":[],"prerequisiteMissing":{"artifacts":[{"artifact":"output","repo":"mofua-suki/issue-desk-intake","issue":90002}]}}'),
 (90002,'mofua-suki/issue-desk-intake',90002,'producer','queued',NULL,0,'{}','{}')`)
	if err != nil {
		t.Fatal(err)
	}
	if err = app.recoverJobs(t.Context()); err != nil {
		t.Fatal(err)
	}
	var status string
	var generation int
	var deps int
	if err = app.DB.QueryRow(`SELECT status,generation,(SELECT count(*) FROM job_deps WHERE job_id=90001 AND depends_on=90002) FROM jobs WHERE id=90001`).Scan(&status, &generation, &deps); err != nil || status != "queued" || generation != 2 || deps != 1 {
		t.Fatalf("status=%s generation=%d deps=%d err=%v", status, generation, deps, err)
	}
}
