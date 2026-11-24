package reviewnotes_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/reviewnotes"
	"issue-desk-intake/server/internal/store"
)

var now = time.Date(2026, 11, 4, 14, 59, 0, 0, time.UTC)

func postgres(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("専用のISSUE_DESK_TEST_POSTGRES_DSNが未設定")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil || !strings.HasPrefix(config.Database, "issue_desk_test") {
		t.Fatal("専用のissue_desk_testデータベースが必要です")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := store.OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	var id [8]byte
	if _, err = rand.Read(id[:]); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	schema := "reviewnotes_test_" + hex.EncodeToString(id[:])
	if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(5)
	t.Cleanup(func() {
		db.Close()
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if _, err := admin.ExecContext(cleanup, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("試験スキーマの片付け: %v", err)
		}
		admin.Close()
	})
	// Use the shipped schema, including migration 10; never create substitute tables.
	if _, err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db, ctx
}

func exec(t *testing.T, db *sql.DB, ctx context.Context, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

func load(t *testing.T, db *sql.DB, ctx context.Context, repo string) reviewnotes.State {
	t.Helper()
	var raw []byte
	if err := db.QueryRowContext(ctx, "SELECT notes FROM repo_note_processing WHERE repo=$1", repo).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	state, err := reviewnotes.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func classifications(_ context.Context, notes []reviewnotes.Note, _ []reviewnotes.Item) ([]reviewnotes.Classification, error) {
	result := []reviewnotes.Classification{}
	for _, note := range notes {
		result = append(result, reviewnotes.Classification{NoteID: note.ID, Kind: "validation", Text: "入力の形を検証する"})
	}
	return result, nil
}

func TestPostgresReceivedNotesDailyPersistenceAndFleet(t *testing.T) {
	db, ctx := postgres(t)
	repo := "mofua-suki/issue-desk-intake"
	clock := now
	// Load the configuration actually shipped to NAS, rather than a test-only fleet.
	f := &fleet.Service{DB: db, FleetPath: filepath.Join("..", "..", "..", "fleet.json"), AssignmentsPath: filepath.Join("..", "..", "..", "assignments.json")}
	_, assignments, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(assignments.Output["reviewNotes"])
	if err != nil {
		t.Fatal(err)
	}
	config, err := reviewnotes.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	jobStore, err := jobs.New(db, jobs.Options{Now: func() time.Time { return clock }, DrainDuration: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	add := func(issue int64, kind string) int64 {
		t.Helper()
		var jobID int64
		if err := db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,status,lease_host,lease_epoch,lease_expires_at) VALUES($1,$2,'running','nas',1,$3) ON CONFLICT(repo,issue) DO UPDATE SET lease_expires_at=EXCLUDED.lease_expires_at RETURNING id`, repo, issue, clock.Add(time.Hour)).Scan(&jobID); err != nil {
			t.Fatal(err)
		}
		noteID, err := jobStore.Note(ctx, jobID, "nas", 1, kind, map[string]any{"issue": issue, "text": "本文の形の検証が不足"})
		if err != nil {
			t.Fatal(err)
		}
		return noteID
	}
	add(1, "finding")
	add(1, "followup")
	s := reviewnotes.Service{DB: db, Now: func() time.Time { return clock }}
	calls := 0
	s.Classify = func(ctx context.Context, notes []reviewnotes.Note, _ []reviewnotes.Item) ([]reviewnotes.Classification, error) {
		calls++
		var date string
		if err := db.QueryRowContext(ctx, "SELECT attempt_date FROM repo_note_processing WHERE repo=$1", repo).Scan(&date); err != nil || date != clock.In(time.FixedZone("JST", 9*3600)).Format("2006-01-02") {
			t.Fatalf("分類前に試行日が永続化されていない: %q %v", date, err)
		}
		return classifications(ctx, notes, nil)
	}
	if attempted, err := s.Run(ctx, repo, config); err != nil || !attempted {
		t.Fatalf("初回: %v %v", attempted, err)
	}
	state := load(t, db, ctx, repo)
	if len(state.Items) != 0 || len(state.Candidates) != 1 || !reflect.DeepEqual(state.Candidates[0].Issues, []int64{1}) {
		t.Fatal("単一Issueが採用された", state)
	}
	last := add(2, "finding")
	if attempted, err := s.Run(ctx, repo, config); err != nil || attempted || calls != 1 {
		t.Fatalf("同日に繰り返した: %v %v %d", attempted, err, calls)
	}
	clock = clock.Add(2 * time.Minute) // UTC is still Nov 4, JST has crossed midnight.
	if attempted, err := s.Run(ctx, repo, config); err != nil || !attempted || calls != 2 {
		t.Fatalf("日本時間の翌日: %v %v %d", attempted, err, calls)
	}
	state = load(t, db, ctx, repo)
	if len(state.Items) != 1 || state.Cursor != last || state.UpdatedAt.IsZero() || len(state.Candidates) != 0 || !reflect.DeepEqual(state.Items[0].Issues, []int64{1, 2}) {
		t.Fatal("採用結果・cursorが保存されていない", state)
	}
	// SQL NULL and legacy missing fields must also be publishable.
	exec(t, db, ctx, `INSERT INTO repo_note_processing(repo,notes) VALUES('mofua-suki/sample-scraper',NULL),('mofua-suki/demo-image-culling','{"items":[{"kind":"legacy","text":"旧状態"}]}'::jsonb)`)
	body, err := f.ClientConfig(ctx, "nas", "")
	if err != nil {
		t.Fatal(err)
	}
	public := fleet.Object(body["reviewNotes"])
	if !reflect.DeepEqual(public[repo], []map[string]string{{"kind": "validation", "text": "入力の形を検証する"}}) {
		t.Fatal("配信がkind/textに限定されていない", public[repo])
	}
	if !reflect.DeepEqual(public["mofua-suki/sample-scraper"], []map[string]string{}) || !reflect.DeepEqual(public["mofua-suki/demo-image-culling"], []map[string]string{{"kind": "legacy", "text": "旧状態"}}) {
		t.Fatal("NULL・旧状態の配信が不正", public)
	}
	// Check actual serialized output too, not only the Go value shape.
	wire, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	items := fleet.List(fleet.Object(decoded["reviewNotes"])[repo])
	if len(items) != 1 || len(fleet.Object(items[0])) != 2 {
		t.Fatal("配信JSONに内部の集計情報が混入", items)
	}
	clock = clock.Add(time.Duration(config.StaleDays+1) * 24 * time.Hour)
	if _, err := s.Run(ctx, repo, config); err != nil {
		t.Fatal(err)
	}
	state = load(t, db, ctx, repo)
	if len(state.Items) != 0 || len(state.Removed) != 1 || calls != 2 {
		t.Fatal("新しい行がない日の期限切れ", state, calls)
	}
	cleared, err := f.ClientConfig(ctx, "nas", fleet.Text(body["version"]))
	if err != nil || cleared["unchanged"] == true {
		t.Fatalf("期限切れが配信の版に反映されない: %+v %v", cleared, err)
	}
}

func TestPostgresBatchFilteringAndCursor(t *testing.T) {
	db, ctx := postgres(t)
	config, _ := reviewnotes.ParseConfig(nil)
	exec(t, db, ctx, `INSERT INTO repo_notes(repo,kind,at,data) VALUES('owner/repo','improvement',$1,'{"issue":1,"text":"対象外"}')`, now)
	exec(t, db, ctx, `INSERT INTO repo_notes(repo,kind,at,data)
SELECT 'owner/repo',CASE WHEN n%2=0 THEN 'finding' ELSE 'followup' END,
CASE WHEN n=1 THEN $1::timestamptz-interval '61 days' ELSE $1::timestamptz END,
CASE n WHEN 1 THEN '{"issue":1,"text":"古い","at":"2099-01-01"}'::jsonb
WHEN 2 THEN '{"text":"Issueが無い"}'::jsonb WHEN 3 THEN '{"issue":"3","text":"文字列"}'::jsonb
WHEN 4 THEN '{"issue":4,"text":[]}'::jsonb WHEN 5 THEN '{"issue":5,"text":"  "}'::jsonb
WHEN 6 THEN '{"issue":0,"text":"ゼロ"}'::jsonb WHEN 7 THEN '{"issue":1.5,"text":"小数"}'::jsonb
WHEN 8 THEN 'null'::jsonb ELSE jsonb_build_object('issue',n,'text','有効') END
FROM generate_series(1,205) AS n`, now)
	exec(t, db, ctx, `INSERT INTO repo_notes(repo,kind,at,data) VALUES('other/repo','finding',$1,'{"issue":999,"text":"別のrepo"}')`, now)
	clock := now
	var batches [][]reviewnotes.Note
	s := reviewnotes.Service{DB: db, Now: func() time.Time { return clock }, Classify: func(ctx context.Context, notes []reviewnotes.Note, _ []reviewnotes.Item) ([]reviewnotes.Classification, error) {
		batches = append(batches, notes)
		return classifications(ctx, notes, nil)
	}}
	if _, err := s.Run(ctx, "owner/repo", config); err != nil {
		t.Fatal(err)
	}
	state := load(t, db, ctx, "owner/repo")
	if len(batches) != 1 || len(batches[0]) != 192 || state.Cursor != 201 || len(state.Items) != 1 || len(state.Items[0].Issues) != 192 {
		t.Fatalf("200件・不正行・期限の選択: %+v batches=%v", state, batches)
	}
	for i, note := range batches[0] {
		if note.ID != int64(i+10) || note.Issue != int64(i+9) {
			t.Fatal("id昇順ではない・不正な行が混入", note)
		}
	}
	clock = clock.Add(24 * time.Hour)
	if _, err := s.Run(ctx, "owner/repo", config); err != nil {
		t.Fatal(err)
	}
	state = load(t, db, ctx, "owner/repo")
	if len(batches) != 2 || len(batches[1]) != 5 || state.Cursor != 206 || len(state.Items[0].Issues) != 197 {
		t.Fatal("次の日にcursorから再開できない", state)
	}
}

func TestPostgresFailuresPreserveAttemptAndResults(t *testing.T) {
	for _, stage := range []string{"attempt", "read", "classify", "missing", "save"} {
		t.Run(stage, func(t *testing.T) {
			db, ctx := postgres(t)
			config, _ := reviewnotes.ParseConfig(nil)
			exec(t, db, ctx, `INSERT INTO repo_note_processing(repo,notes,improvement_cursor) VALUES('owner/repo','{"cursor":0,"items":[{"kind":"legacy","text":"元の注意点"}]}',17)`)
			exec(t, db, ctx, `INSERT INTO repo_notes(repo,kind,at,data) VALUES('owner/repo','finding',$1,'{"issue":1,"text":"指摘"}')`, now)
			if stage == "attempt" || stage == "save" {
				exec(t, db, ctx, `CREATE FUNCTION fail_note_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test write failure'; END $$`)
				column := "attempt_date"
				if stage == "save" {
					column = "notes"
				}
				exec(t, db, ctx, "CREATE TRIGGER fail_write BEFORE UPDATE OF "+column+" ON repo_note_processing FOR EACH ROW EXECUTE FUNCTION fail_note_write()")
			}
			if stage == "read" {
				exec(t, db, ctx, "ALTER TABLE repo_notes RENAME TO unreadable_repo_notes")
			}
			calls := 0
			s := reviewnotes.Service{DB: db, Now: func() time.Time { return now }, Classify: func(ctx context.Context, notes []reviewnotes.Note, _ []reviewnotes.Item) ([]reviewnotes.Classification, error) {
				calls++
				if stage == "classify" {
					return nil, errors.New("分類失敗")
				}
				return classifications(ctx, notes, nil)
			}}
			if stage == "missing" {
				s.Classify = nil
			}
			attempted, err := s.Run(ctx, "owner/repo", config)
			if err == nil || attempted != (stage != "attempt") || ((stage == "attempt" || stage == "read") && calls != 0) {
				t.Fatalf("失敗後に処理が継続: attempted=%v calls=%d err=%v", attempted, calls, err)
			}
			state := load(t, db, ctx, "owner/repo")
			var date sql.NullString
			var improvementCursor int64
			if err := db.QueryRowContext(ctx, "SELECT attempt_date,improvement_cursor FROM repo_note_processing WHERE repo='owner/repo'").Scan(&date, &improvementCursor); err != nil {
				t.Fatal(err)
			}
			if state.Cursor != 0 || len(state.Items) != 1 || state.Items[0].Text != "元の注意点" || improvementCursor != 17 || date.Valid != (stage != "attempt") {
				t.Fatal("失敗時に保存結果が変わった", state, date, improvementCursor)
			}
			if stage == "attempt" || stage == "save" {
				exec(t, db, ctx, "DROP TRIGGER fail_write ON repo_note_processing")
			}
			if stage == "read" {
				exec(t, db, ctx, "ALTER TABLE unreadable_repo_notes RENAME TO repo_notes")
			}
			before := calls
			s.Classify = func(ctx context.Context, notes []reviewnotes.Note, _ []reviewnotes.Item) ([]reviewnotes.Classification, error) {
				calls++
				return classifications(ctx, notes, nil)
			}
			if stage != "attempt" {
				if attempted, err := s.Run(ctx, "owner/repo", config); err != nil || attempted || before != calls {
					t.Fatalf("失敗後に同日再試行した: %v %v", attempted, err)
				}
			}
			s.Now = func() time.Time { return now.Add(24 * time.Hour) }
			if attempted, err := s.Run(ctx, "owner/repo", config); err != nil || !attempted || calls != before+1 {
				t.Fatalf("翌日に同じ行を再処理できない: %v %v", attempted, err)
			}
		})
	}
}

func TestPostgresBudgetUsesShippedConfig(t *testing.T) {
	db, ctx := postgres(t)
	f := &fleet.Service{DB: db, FleetPath: "../../../fleet.json", AssignmentsPath: "../../../assignments.json"}
	_, assignments, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(assignments.Output["reviewNotes"])
	if err != nil {
		t.Fatal(err)
	}
	config, err := reviewnotes.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("あ", config.MaxBytes/6+1)
	exec(t, db, ctx, `INSERT INTO repo_notes(repo,kind,at,data)
SELECT 'mofua-suki/issue-desk-intake','finding',$1::timestamptz,
jsonb_build_object('issue',n,'text',$2::text) FROM generate_series(1,5) AS n`, now, text)
	s := reviewnotes.Service{DB: db, Now: func() time.Time { return now }, Classify: func(_ context.Context, notes []reviewnotes.Note, _ []reviewnotes.Item) ([]reviewnotes.Classification, error) {
		classes := []reviewnotes.Classification{}
		for _, note := range notes {
			kind := "popular"
			if note.Issue <= 2 {
				kind = "few"
			}
			classes = append(classes, reviewnotes.Classification{NoteID: note.ID, Kind: kind, Text: note.Text})
		}
		return classes, nil
	}}
	if _, err := s.Run(ctx, "mofua-suki/issue-desk-intake", config); err != nil {
		t.Fatal(err)
	}
	state := load(t, db, ctx, "mofua-suki/issue-desk-intake")
	if len(state.Items) != 1 || state.Items[0].Kind != "popular" || len(state.Candidates) != 1 || state.Candidates[0].Kind != "few" || state.Cursor != 5 || len(state.Items[0].Text) > config.MaxBytes {
		t.Fatal("配備設定のUTF-8上限で候補へ戻らない", state)
	}
	body, err := f.ClientConfig(ctx, "nas", "")
	if err != nil || !reflect.DeepEqual(fleet.Object(body["reviewNotes"])["mofua-suki/issue-desk-intake"], []map[string]string{{"kind": "popular", "text": text}}) {
		t.Fatalf("上限を適用した配信が不正: %v", err)
	}
}

func TestPostgresConcurrentDailyClaim(t *testing.T) {
	db, ctx := postgres(t)
	config, _ := reviewnotes.ParseConfig(nil)
	exec(t, db, ctx, `INSERT INTO repo_notes(repo,kind,at,data) VALUES('owner/repo','finding',$1,'{"issue":1,"text":"指摘"}')`, now)
	started, release := make(chan struct{}), make(chan struct{}, 1)
	defer close(release)
	var calls atomic.Int32
	s := reviewnotes.Service{DB: db, Now: func() time.Time { return now }, Classify: func(ctx context.Context, notes []reviewnotes.Note, _ []reviewnotes.Item) ([]reviewnotes.Classification, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return classifications(ctx, notes, nil)
	}}
	finished := make(chan error, 1)
	go func() { _, err := s.Run(ctx, "owner/repo", config); finished <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// A separate service represents another process sharing the same PostgreSQL.
	other := s
	if attempted, err := other.Run(ctx, "owner/repo", config); err != nil || attempted || calls.Load() != 1 {
		t.Fatalf("並行して同日に種類分けを実行: %v %v", attempted, err)
	}
	if attempted, err := other.Run(ctx, "other/repo", config); err != nil || !attempted {
		t.Fatalf("別のrepoも抑止された: %v %v", attempted, err)
	}
	// Release without closing twice in the deferred cleanup.
	release <- struct{}{}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

func TestPostgresRestoreNotesKeepsDailyFence(t *testing.T) {
	db, ctx := postgres(t)
	config, _ := reviewnotes.ParseConfig(nil)
	clock := now
	backup := `{"items":[{"kind":"legacy","text":"復元する注意点"}],"cursor":0}`
	exec(t, db, ctx, `INSERT INTO repo_note_processing(repo,notes,improvement_cursor) VALUES('mofua-suki/issue-desk-intake',$1::jsonb,17)`, backup)
	exec(t, db, ctx, `INSERT INTO repo_notes(repo,kind,at,data) VALUES('mofua-suki/issue-desk-intake','finding',$1,'{"issue":1,"text":"指摘"}')`, now)
	s := reviewnotes.Service{DB: db, Now: func() time.Time { return clock }, Classify: classifications}
	if _, err := s.Run(ctx, "mofua-suki/issue-desk-intake", config); err != nil {
		t.Fatal(err)
	}
	// Documented recovery: restore only notes, with the caller stopped.
	exec(t, db, ctx, `UPDATE repo_note_processing SET notes=$1::jsonb WHERE repo='mofua-suki/issue-desk-intake'`, backup)
	var date string
	var improvementCursor int64
	if err := db.QueryRowContext(ctx, `SELECT attempt_date,improvement_cursor FROM repo_note_processing WHERE repo='mofua-suki/issue-desk-intake'`).Scan(&date, &improvementCursor); err != nil || date != "2026-11-04" || improvementCursor != 17 {
		t.Fatalf("復元で独立した列が変わった: %q %d %v", date, improvementCursor, err)
	}
	f := fleet.Service{DB: db, FleetPath: "../../../fleet.json", AssignmentsPath: "../../../assignments.json"}
	body, err := f.ClientConfig(ctx, "nas", "")
	if err != nil || !reflect.DeepEqual(fleet.Object(body["reviewNotes"])["mofua-suki/issue-desk-intake"], []map[string]string{{"kind": "legacy", "text": "復元する注意点"}}) {
		t.Fatalf("復元内容が配信されない: %v", err)
	}
	if attempted, err := s.Run(ctx, "mofua-suki/issue-desk-intake", config); err != nil || attempted {
		t.Fatalf("復元後に同日再試行した: %v %v", attempted, err)
	}
	clock = clock.Add(24 * time.Hour)
	if _, err := s.Run(ctx, "mofua-suki/issue-desk-intake", config); err != nil {
		t.Fatal(err)
	}
	state := load(t, db, ctx, "mofua-suki/issue-desk-intake")
	if state.Cursor != 1 || len(state.Candidates) != 1 || len(state.Removed) != 1 {
		t.Fatal("復元したcursorから翌日に再処理できない", state)
	}
}

func TestPostgresPoolWaitUsesCurrentJSTDate(t *testing.T) {
	db, ctx := postgres(t)
	db.SetMaxOpenConns(1)
	occupied, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	var clock atomic.Int64
	clock.Store(now.UnixNano())
	var nowCalls atomic.Int32
	s := reviewnotes.Service{DB: db, Now: func() time.Time {
		nowCalls.Add(1)
		return time.Unix(0, clock.Load())
	}}
	config, _ := reviewnotes.ParseConfig(nil)
	before := db.Stats().WaitCount
	finished := make(chan error, 1)
	go func() { _, err := s.Run(ctx, "owner/repo", config); finished <- err }()
	for db.Stats().WaitCount == before {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	if nowCalls.Load() != 0 {
		t.Fatal("DB接続を待つ前の日付を使っている")
	}
	clock.Store(now.Add(2 * time.Minute).UnixNano())
	if err := occupied.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	var date string
	if err := db.QueryRowContext(ctx, "SELECT attempt_date FROM repo_note_processing WHERE repo='owner/repo'").Scan(&date); err != nil || date != "2026-11-05" {
		t.Fatalf("接続の待ち中の日付変更: %q %v", date, err)
	}
	s.Now = func() time.Time { return now } // An old caller cannot move the date back.
	if attempted, err := s.Run(ctx, "owner/repo", config); err != nil || attempted {
		t.Fatalf("試行日が過去へ戻った: %v %v", attempted, err)
	}
}
