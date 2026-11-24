package reviewnotes_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/reviewnotes"
)

type cacheScenario struct {
	db      *sql.DB
	ctx     context.Context
	repo    string
	clock   time.Time
	config  reviewnotes.Config
	service reviewnotes.Service
}

func newCacheScenario(t *testing.T) *cacheScenario {
	t.Helper()
	db, ctx := postgres(t)
	s := &cacheScenario{db: db, ctx: ctx, repo: "mofua-suki/issue-desk-intake", clock: now}
	f := &fleet.Service{DB: db, FleetPath: "../../../fleet.json", AssignmentsPath: "../../../assignments.json"}
	_, assignments, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(assignments.Output["reviewNotes"])
	if err != nil {
		t.Fatal(err)
	}
	s.config, err = reviewnotes.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	s.service = reviewnotes.Service{DB: db, Now: func() time.Time { return s.clock }}
	return s
}

// Produce the same persisted data as deployed workers through jobs.Note.
func (s *cacheScenario) add(t *testing.T, issue int64, kind, text string) int64 {
	t.Helper()
	var jobID int64
	if err := s.db.QueryRowContext(s.ctx, `INSERT INTO jobs(repo,issue,status,lease_host,lease_epoch,lease_expires_at)
VALUES($1,$2,'running','nas',1,$3) RETURNING id`, s.repo, issue, s.clock.Add(time.Hour)).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	store, err := jobs.New(s.db, jobs.Options{Now: func() time.Time { return s.clock }, DrainDuration: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.Note(s.ctx, jobID, "nas", 1, kind, map[string]any{"issue": issue, "text": text})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (s *cacheScenario) run(t *testing.T) reviewnotes.State {
	t.Helper()
	if attempted, err := s.service.Run(s.ctx, s.repo, s.config); !attempted || err != nil {
		t.Fatal(attempted, err)
	}
	return load(t, s.db, s.ctx, s.repo)
}

func (s *cacheScenario) answer(notes []reviewnotes.Note, kinds []reviewnotes.Item) map[string]any {
	assignments := []map[string]any{}
	for _, note := range notes {
		assignments = append(assignments, map[string]any{"noteID": note.ID, "kind": "validation", "text": "入力を検証する", "quote": note.Text})
	}
	return map[string]any{"assignments": assignments}
}

func TestPostgresKnownOnlySkipsAIAndPublishes(t *testing.T) {
	s := newCacheScenario(t)
	calls := 0
	s.service.Classify = reviewnotes.AIClassifier(func(_ context.Context, input string) (map[string]any, error) {
		calls++
		// The production builder is exercised; decode only the data after its
		// final newline, rather than supplying a different note input shape.
		var data struct {
			Notes []struct {
				ID   int64  `json:"noteID"`
				Text string `json:"text"`
			} `json:"notes"`
		}
		start := len(input)
		for start > 0 && input[start-1] != '\n' {
			start--
		}
		if err := json.Unmarshal([]byte(input[start:]), &data); err != nil {
			t.Fatal(err)
		}
		notes := []reviewnotes.Note{}
		for _, n := range data.Notes {
			notes = append(notes, reviewnotes.Note{ID: n.ID, Text: n.Text})
		}
		return s.answer(notes, nil), nil
	})
	original := s.add(t, 101, "finding", "入力検証が不足")
	state := s.run(t)
	if calls != 1 || len(state.Classifications) != 1 || state.Classifications[0].Source.ID != original {
		t.Fatal(calls, state)
	}
	s.clock = s.clock.Add(24 * time.Hour)
	last := s.add(t, 102, "finding", "入力検証が不足")
	// A newly constructed service must reuse persisted state, not process memory.
	s.service = reviewnotes.Service{DB: s.db, Now: s.service.Now, Classify: s.service.Classify}
	state = s.run(t)
	if calls != 1 || state.Cursor != last || len(state.Items) != 1 || !reflect.DeepEqual(state.Items[0].Issues, []int64{101, 102}) {
		t.Fatal(calls, state)
	}
	f := &fleet.Service{DB: s.db, FleetPath: "../../../fleet.json", AssignmentsPath: "../../../assignments.json"}
	body, err := f.ClientConfig(s.ctx, "nas", "")
	if err != nil || !reflect.DeepEqual(fleet.Object(body["reviewNotes"])[s.repo], []map[string]string{{"kind": "validation", "text": "入力を検証する"}}) {
		t.Fatal(body, err)
	}
	wire, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	items := fleet.List(fleet.Object(decoded["reviewNotes"])[s.repo])
	if len(items) != 1 || len(fleet.Object(items[0])) != 2 {
		t.Fatal("分類の内部情報が配信された", items)
	}
}

func TestPostgresLongSourceIsNotCachedAndIsReclassified(t *testing.T) {
	s := newCacheScenario(t)
	calls := 0
	s.service.Classify = func(_ context.Context, notes []reviewnotes.Note, _ []reviewnotes.Item) ([]reviewnotes.Classification, error) {
		calls++
		return []reviewnotes.Classification{{NoteID: notes[0].ID, Kind: "validation", Text: "入力を検証する", Quote: "根拠"}}, nil
	}
	body := "根拠" + strings.Repeat("x", 4000)
	s.add(t, 101, "finding", body)
	state := s.run(t)
	if len(state.Classifications) != 0 || len(state.Candidates) != 1 {
		t.Fatal(state)
	}
	s.clock = s.clock.Add(24 * time.Hour)
	s.add(t, 102, "finding", body)
	state = s.run(t)
	if calls != 2 || len(state.Classifications) != 0 || len(state.Items) != 1 {
		t.Fatal(calls, state)
	}
}

func TestPostgresOnlyUnknownNotesReachAI(t *testing.T) {
	s := newCacheScenario(t)
	s.service.Classify = func(_ context.Context, notes []reviewnotes.Note, _ []reviewnotes.Item) ([]reviewnotes.Classification, error) {
		out := []reviewnotes.Classification{}
		for _, n := range notes {
			out = append(out, reviewnotes.Classification{NoteID: n.ID, Kind: "validation", Text: "入力を検証する", Quote: n.Text})
		}
		return out, nil
	}
	s.add(t, 101, "finding", "同じ本文")
	s.run(t)
	s.clock = s.clock.Add(24 * time.Hour)
	known := s.add(t, 102, "finding", "同じ本文")
	unknownKind := s.add(t, 103, "followup", "同じ本文")
	unknownText := s.add(t, 104, "finding", "同じ本文 ")
	calls := 0
	s.service.Classify = func(_ context.Context, notes []reviewnotes.Note, kinds []reviewnotes.Item) ([]reviewnotes.Classification, error) {
		calls++
		if len(notes) != 2 || notes[0].ID != unknownKind || notes[1].ID != unknownText || len(kinds) != 1 || kinds[0].Kind != "validation" {
			t.Fatal(notes, kinds, known)
		}
		return []reviewnotes.Classification{{NoteID: unknownKind, Kind: "validation", Text: "入力を検証する", Quote: "同じ本文"}, {NoteID: unknownText, Kind: "validation", Text: "入力を検証する", Quote: "同じ本文"}}, nil
	}
	state := s.run(t)
	if calls != 1 || len(state.Items) != 1 || len(state.Items[0].Issues) != 4 {
		t.Fatal(calls, state)
	}
}

func TestPostgresCacheRequiresVersionExpiryRepositoryAndOriginalRow(t *testing.T) {
	for _, mutation := range []string{"version", "expired", "repo", "kind", "text", "issue", "at", "deleted", "source-id", "source-repo"} {
		t.Run(mutation, func(t *testing.T) {
			s := newCacheScenario(t)
			s.service.Classify = func(_ context.Context, notes []reviewnotes.Note, _ []reviewnotes.Item) ([]reviewnotes.Classification, error) {
				return []reviewnotes.Classification{{NoteID: notes[0].ID, Kind: "validation", Text: "入力を検証する", Quote: "指摘"}}, nil
			}
			original := s.add(t, 101, "finding", "指摘")
			state := s.run(t)
			s.clock = s.clock.Add(24 * time.Hour)
			switch mutation {
			case "version":
				state.Classifications[0].Version++
			case "expired":
				state.Classifications[0].ExpiresAt = s.clock
			case "repo":
				state.Classifications[0].Repo = "other/repo"
			case "kind":
				exec(t, s.db, s.ctx, `UPDATE repo_notes SET kind='followup' WHERE id=$1`, original)
			case "text":
				exec(t, s.db, s.ctx, `UPDATE repo_notes SET data=jsonb_set(data,'{text}','"変更"') WHERE id=$1`, original)
			case "issue":
				exec(t, s.db, s.ctx, `UPDATE repo_notes SET data=jsonb_set(data,'{issue}','999') WHERE id=$1`, original)
			case "at":
				exec(t, s.db, s.ctx, `UPDATE repo_notes SET at=at-interval '1 second' WHERE id=$1`, original)
			case "deleted":
				exec(t, s.db, s.ctx, `DELETE FROM repo_notes WHERE id=$1`, original)
			case "source-repo":
				exec(t, s.db, s.ctx, `UPDATE repo_notes SET repo='other/repo' WHERE id=$1`, original)
			case "source-id":
				state.Classifications[0].Source.ID = 999
			}
			raw, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			exec(t, s.db, s.ctx, `UPDATE repo_note_processing SET notes=$2::jsonb WHERE repo=$1`, s.repo, string(raw))
			last := s.add(t, 102, "finding", "指摘")
			calls := 0
			s.service.Classify = func(_ context.Context, notes []reviewnotes.Note, _ []reviewnotes.Item) ([]reviewnotes.Classification, error) {
				calls++
				if len(notes) != 1 || notes[0].ID != last {
					t.Fatal(notes)
				}
				return nil, nil
			}
			s.run(t)
			if calls != 1 {
				t.Fatal("不正なキャッシュを再利用", mutation)
			}
		})
	}
}

func TestPostgresAIFailureKeepsCursorResultsAndCacheUntilNextDay(t *testing.T) {
	for _, failure := range []string{"execution", "timeout", "malformed"} {
		t.Run(failure, func(t *testing.T) {
			s := newCacheScenario(t)
			s.service.Classify = func(_ context.Context, notes []reviewnotes.Note, _ []reviewnotes.Item) ([]reviewnotes.Classification, error) {
				return []reviewnotes.Classification{{NoteID: notes[0].ID, Kind: "validation", Text: "入力を検証する", Quote: "既知"}}, nil
			}
			s.add(t, 101, "finding", "既知")
			s.run(t)
			s.clock = s.clock.Add(24 * time.Hour)
			s.add(t, 102, "finding", "既知")
			unknown := s.add(t, 103, "finding", "未知")
			var before, after []byte
			if err := s.db.QueryRowContext(s.ctx, `SELECT notes FROM repo_note_processing WHERE repo=$1`, s.repo).Scan(&before); err != nil {
				t.Fatal(err)
			}
			calls := 0
			s.service.Classify = reviewnotes.AIClassifier(func(_ context.Context, _ string) (map[string]any, error) {
				calls++
				switch failure {
				case "execution":
					return nil, errors.New("AI失敗")
				case "timeout":
					return nil, context.DeadlineExceeded
				default:
					return map[string]any{"assignments": "不正"}, nil
				}
			})
			if attempted, err := s.service.Run(s.ctx, s.repo, s.config); !attempted || err == nil {
				t.Fatal(attempted, err)
			}
			var date string
			if err := s.db.QueryRowContext(s.ctx, `SELECT notes,attempt_date FROM repo_note_processing WHERE repo=$1`, s.repo).Scan(&after, &date); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) || date != s.clock.In(time.FixedZone("JST", 9*3600)).Format("2006-01-02") {
				t.Fatal("失敗時に結果が変わった", date)
			}
			if attempted, err := s.service.Run(s.ctx, s.repo, s.config); attempted || err != nil || calls != 1 {
				t.Fatal(attempted, err, calls)
			}
			s.clock = s.clock.Add(24 * time.Hour)
			s.service.Classify = func(_ context.Context, notes []reviewnotes.Note, _ []reviewnotes.Item) ([]reviewnotes.Classification, error) {
				if len(notes) != 1 || notes[0].ID != unknown {
					t.Fatal(notes)
				}
				return []reviewnotes.Classification{{NoteID: unknown, Kind: "validation", Text: "入力を検証する", Quote: "未知"}}, nil
			}
			state := s.run(t)
			if state.Cursor != unknown || len(state.Items) != 1 || len(state.Classifications) != 2 {
				t.Fatal(state)
			}
		})
	}
}

func TestPostgresCacheDoesNotMatchClippedPrefix(t *testing.T) {
	s := newCacheScenario(t)
	prefix := strings.Repeat("あ", 400)
	s.service.Classify = func(_ context.Context, notes []reviewnotes.Note, _ []reviewnotes.Item) ([]reviewnotes.Classification, error) {
		return []reviewnotes.Classification{{NoteID: notes[0].ID, Kind: "validation", Text: "入力を検証する", Quote: prefix}}, nil
	}
	s.add(t, 101, "finding", prefix+"A")
	s.run(t)
	s.clock = s.clock.Add(24 * time.Hour)
	id := s.add(t, 102, "finding", prefix+"B")
	calls := 0
	s.service.Classify = func(_ context.Context, notes []reviewnotes.Note, _ []reviewnotes.Item) ([]reviewnotes.Classification, error) {
		calls++
		if len(notes) != 1 || notes[0].ID != id {
			t.Fatal(notes)
		}
		return nil, nil
	}
	s.run(t)
	if calls != 1 {
		t.Fatal("切り詰め後だけで完全一致と判断した")
	}
}

func TestPostgresCacheAndAdoptionSaveAtomically(t *testing.T) {
	s := newCacheScenario(t)
	s.service.Classify = func(_ context.Context, notes []reviewnotes.Note, _ []reviewnotes.Item) ([]reviewnotes.Classification, error) {
		out := []reviewnotes.Classification{}
		for _, n := range notes {
			out = append(out, reviewnotes.Classification{NoteID: n.ID, Kind: "validation", Text: "入力を検証する", Quote: n.Text})
		}
		return out, nil
	}
	s.add(t, 101, "finding", "既知")
	s.run(t)
	s.clock = s.clock.Add(24 * time.Hour)
	s.add(t, 102, "finding", "未知")
	var before, after []byte
	if err := s.db.QueryRowContext(s.ctx, `SELECT notes FROM repo_note_processing WHERE repo=$1`, s.repo).Scan(&before); err != nil {
		t.Fatal(err)
	}
	exec(t, s.db, s.ctx, `CREATE FUNCTION fail_classification_save() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test write failure'; END $$`)
	exec(t, s.db, s.ctx, `CREATE TRIGGER fail_classification_save BEFORE UPDATE OF notes ON repo_note_processing FOR EACH ROW EXECUTE FUNCTION fail_classification_save()`)
	if attempted, err := s.service.Run(s.ctx, s.repo, s.config); !attempted || err == nil {
		t.Fatal(attempted, err)
	}
	if err := s.db.QueryRowContext(s.ctx, `SELECT notes FROM repo_note_processing WHERE repo=$1`, s.repo).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("保存失敗で分類または採用結果だけが変わった")
	}
}
