package issueapi

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"issue-desk-intake/server/internal/numbers"
	"regexp"
	"testing"
)

func TestShownTextChangesOnlyGeneratedReferences(t *testing.T) {
	lookup := func(n int64) int64 {
		if n == 3 {
			return 90
		}
		return 0
	}
	raw := "元の依頼: #3\n#3 の独立レビューで、確認\n利用者の #3 は残す\n先行IssueのPRマージ待ち: #3, #4\n<!-- issue-desk:followup -->"
	title, body := shownIssue("#3 元の件名", raw, lookup)
	if title != "#90（Gitea番号 3） 元の件名" || body != "元の依頼: #90（Gitea番号 3）\n#90（Gitea番号 3） の独立レビューで、確認\n利用者の #3 は残す\n先行IssueのPRマージ待ち: #90（Gitea番号 3）, Gitea番号 4\n<!-- issue-desk:followup -->" {
		t.Fatalf("%s\n%s", title, body)
	}
	title, body = shownIssue("#3 元の件名", "元の依頼: #3", lookup)
	if title != "#3 元の件名" || body != "元の依頼: #3" {
		t.Fatal("ordinary user text changed")
	}
	if shownComment("user text", "後続Issueを起票しました: #3", lookup) != "後続Issueを起票しました: #3" {
		t.Fatal("user comment changed")
	}
	if shownComment("<!-- gcr:repo#1:question -->", "依存待機中\n#3: PRマージ待ち", lookup) != "依存待機中\n#90（Gitea番号 3）: PRマージ待ち" {
		t.Fatal("generated dependency reason not projected")
	}
	for raw, want := range map[string]string{"plain": "you", "<!-- gcr:repo#1:question -->": "ask", "<!-- gcr:repo#1:priority-direct -->": "notice", "<!-- gcr:repo#1:start -->": "log", "<!-- gcr:repo#1:done -->": "result"} {
		if got := commentKind(raw); got != want {
			t.Fatalf("kind=%s want=%s", got, want)
		}
	}
}

func TestDetailNumberLookupIsolationAndErrors(t *testing.T) {
	db := sql.OpenDB(&lookupConnector{})
	defer db.Close()
	lookupQueries = nil
	s := &Service{o: Options{Numbers: &numbers.Store{DB: db}}}
	ctx := context.Background()
	a := s.numberLookup(ctx, "a/repo")
	for i := 0; i < 100; i++ {
		if a(3) != 90 || a(4) != 0 {
			t.Fatal("unexpected resolution")
		}
	}
	if len(lookupQueries) != 2 {
		t.Fatalf("queries: %v", lookupQueries)
	}
	shared := context.WithValue(ctx, detailLookupKey{}, detailLookup{repo: "a/repo", lookup: a})
	if s.numberLookup(shared, "a/repo")(3) != 90 || len(lookupQueries) != 2 {
		t.Fatal("detail lookup not shared")
	}
	if s.numberLookup(shared, "b/repo")(3) != 91 {
		t.Fatal("repository cache leaked")
	}
	if s.numberLookup(ctx, "a/repo")(3) != 90 || len(lookupQueries) != 4 {
		t.Fatal("request cache leaked")
	}
	failing := s.numberLookup(ctx, "error/repo")
	firstFailure := failing(3)
	secondFailure := failing(3)
	if firstFailure != 0 || secondFailure != 0 || len(lookupQueries) != 6 {
		t.Fatal("errors must remain retryable", lookupQueries)
	}
}

type lookupConnector struct{}
type lookupConn struct{}
type lookupRows struct {
	value int64
	done  bool
}

var lookupQueries []string

func (*lookupConnector) Connect(context.Context) (driver.Conn, error) { return lookupConn{}, nil }
func (*lookupConnector) Driver() driver.Driver                        { return lookupDriver{} }

type lookupDriver struct{}

func (lookupDriver) Open(string) (driver.Conn, error)  { return lookupConn{}, nil }
func (lookupConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unexpected prepare") }
func (lookupConn) Close() error                        { return nil }
func (lookupConn) Begin() (driver.Tx, error)           { return nil, errors.New("unexpected transaction") }
func (lookupConn) QueryContext(_ context.Context, _ string, args []driver.NamedValue) (driver.Rows, error) {
	repo := args[0].Value.(string)
	n := args[1].Value.(int64)
	lookupQueries = append(lookupQueries, fmt.Sprintf("%s#%d", repo, n))
	if repo == "error/repo" {
		return nil, errors.New("database failure")
	}
	if n != 3 {
		return &lookupRows{done: true}, nil
	}
	value := int64(90)
	if repo == "b/repo" {
		value = 91
	}
	return &lookupRows{value: value}, nil
}
func (*lookupRows) Columns() []string { return []string{"number"} }
func (*lookupRows) Close() error      { return nil }
func (r *lookupRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.value
	return nil
}

func BenchmarkRepeatedCommentReferences(b *testing.B) {
	db := sql.OpenDB(&lookupConnector{})
	defer db.Close()
	s := &Service{o: Options{Numbers: &numbers.Store{DB: db}}}
	raw := "<!-- gcr:repo#1:question -->"
	text := "依存待機中\n#3: PRマージ待ち\n後続Issueを起票しました: #3"
	for _, mode := range []string{"before", "after"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			count := 0
			for i := 0; i < b.N; i++ {
				lookupQueries = nil
				var lookup numberLookup
				if mode == "after" {
					lookup = s.numberLookup(context.Background(), "a/repo")
				}
				for j := 0; j < 100; j++ {
					if mode == "before" {
						lookup = func(n int64) int64 { v, _, _ := s.o.Numbers.Number(context.Background(), "a/repo", n); return v }
						replaceNumber(text, regexp.MustCompile(`(後続Issueを起票しました: )#([1-9][0-9]*)`), lookup)
						replaceNumber(text, regexp.MustCompile(`(^|\n)#([1-9][0-9]*)(:)`), lookup)
						regexp.MustCompile(`<!-- gcr:[^>]*#[0-9]+:([a-z-]+)`).FindStringSubmatch(raw)
						regexp.MustCompile(`<!-- (?:gcr|issue-desk)[\s\S]*?-->`).ReplaceAllString(raw, "")
					} else {
						shownComment(raw, shownCommentMarkers.ReplaceAllString(text, ""), lookup)
						commentKind(raw)
					}
				}
				count += len(lookupQueries)
			}
			b.ReportMetric(float64(count)/float64(b.N), "queries/op")
		})
	}
}
