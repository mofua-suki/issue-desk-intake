package jobs

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestPostgresJobPriorityMatchesCreationAndStorageContract(t *testing.T) {
	db, ctx := testPostgres(t)
	s, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, priority := range []int{0, 1} {
		created, err := s.Create(ctx, NewJob{Repo: "owner/repo", Issue: int64(priority + 1), Priority: priority, Scope: []string{"a.go"}})
		if err != nil || created.Job.Priority != priority {
			t.Fatal("有効な優先度を保存できません", priority, err)
		}
	}
	for _, priority := range []int{-1, 2} {
		if _, err := s.Create(ctx, NewJob{Repo: "owner/repo", Issue: 3, Priority: priority, Scope: []string{"a.go"}}); err == nil {
			t.Fatal("作成入口が範囲外の優先度を許可しました", priority)
		}
		_, err := db.ExecContext(ctx, `INSERT INTO jobs(repo,issue,priority) VALUES('owner/repo',3,$1)`, priority)
		var problem *pgconn.PgError
		if !errors.As(err, &problem) || problem.Code != "23514" || problem.ConstraintName != "jobs_priority_check" {
			t.Fatal("出荷するDB制約が範囲外の優先度を許可しました", priority, err)
		}
	}
}
