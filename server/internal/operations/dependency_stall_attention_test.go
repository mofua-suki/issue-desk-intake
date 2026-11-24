package operations

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

func stallFixture(t *testing.T, age string) *Service {
	t.Helper()
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	s.Desk = func(_ string, n int64) any { return n + 100 }
	_, err := s.PostgreSQL.Exec(`INSERT INTO jobs(id,repo,issue,title,status,hold_reason) VALUES (1,'owner/repo',1,'先行','held','question'),(2,'owner/repo',2,'後続','queued',NULL),(3,'owner/repo',3,'後続','queued',NULL); INSERT INTO job_deps(job_id,depends_on) VALUES (2,1),(3,1)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.PostgreSQL.Exec(`UPDATE job_deps SET wait_started_at=CASE WHEN $1='' THEN NULL ELSE $2::timestamptz-$1::interval END, wait_unknown=($1='')`, age, s.now())
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func stallRows(t *testing.T, s *Service) []any {
	t.Helper()
	rows, err := s.jobAttention(context.Background(), map[string]bool{"owner/repo": true})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}
func TestDependencyStallAttention(t *testing.T) {
	s := stallFixture(t, "25 hours")
	rows := stallRows(t, s)
	if len(rows) != 1 {
		t.Fatalf("rows: %v", rows)
	}
	row := rows[0].(map[string]any)
	// 番号は欄ごとに照合する。行全体の文字列では updatedAt の時刻の数字にも一致してしまう。
	if row["desk"] != int64(101) || row["state"] != "held" || row["title"] != "先行" ||
		!strings.Contains(fmt.Sprint(row["reason"]), "後続2件: 102 (owner/repo#2)、103 (owner/repo#3)。") {
		t.Fatalf("row: %v", row)
	}
}
func TestDependencyStallAttentionBoundary(t *testing.T) {
	if rows := stallRows(t, stallFixture(t, "23 hours")); len(rows) != 0 {
		t.Fatal(rows)
	}
}
func TestDependencyStallAttentionUnknownStart(t *testing.T) {
	rows := stallRows(t, stallFixture(t, ""))
	if len(rows) != 1 {
		t.Fatal(rows)
	}
	row := rows[0].(map[string]any)
	if !strings.Contains(fmt.Sprint(row["reason"]), "待ち始め不明") || row["startedAt"] != nil || regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}`).MatchString(fmt.Sprint(row["reason"])) {
		t.Fatal(row)
	}
}

func TestDependencyStallAttentionConfiguredRepos(t *testing.T) {
	s := stallFixture(t, "25 hours")
	_, err := s.PostgreSQL.Exec(`INSERT INTO jobs(id,repo,issue,title,status,hold_reason) VALUES
 (4,'other/repo',4,'未登録先行','held','question'),
 (5,'other/repo',5,'未登録後続','queued',NULL);
 INSERT INTO job_deps(job_id,depends_on) VALUES (2,4),(5,1),(5,4)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.PostgreSQL.Exec(`UPDATE job_deps SET wait_started_at=$1::timestamptz-interval '25 hours',wait_unknown=false`, s.now())
	if err != nil {
		t.Fatal(err)
	}
	rows := stallRows(t, s)
	if len(rows) != 1 {
		t.Fatal(rows)
	}
	// 未登録リポジトリの先行（104）と後続（105）が混ざらないことを、欄と後続の並びで照合する。
	// 行全体の文字列で「105」の不在を見ると updatedAt の時刻の数字に一致して落ちる。
	row := rows[0].(map[string]any)
	if row["key"] != "dependency-stall:1" || row["desk"] != int64(101) || strings.Contains(fmt.Sprint(row), "other/repo") ||
		!strings.Contains(fmt.Sprint(row["reason"]), "後続2件: 102 (owner/repo#2)、103 (owner/repo#3)。") {
		t.Fatal(rows)
	}
	rows, err = s.jobAttention(context.Background(), map[string]bool{"unrelated/repo": true})
	if err != nil || len(rows) != 0 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
}
func TestDependencyStallAttentionResolves(t *testing.T) {
	s := stallFixture(t, "25 hours")
	if len(stallRows(t, s)) != 1 {
		t.Fatal("missing")
	}
	if _, err := s.PostgreSQL.Exec(`UPDATE jobs SET status='merged',hold_reason=NULL WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if rows := stallRows(t, s); len(rows) != 0 {
		t.Fatal(rows)
	}
}
