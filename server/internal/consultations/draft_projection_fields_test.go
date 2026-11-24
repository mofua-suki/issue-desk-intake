package consultations

import (
	"context"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/pgtest"
)

// 保存された下書きの形（欠けた欄・型違い・null・重複キー・数値の表記・エスケープ・NUL）ごとに、
// 一覧用の射影が返す欄と値を固定する。
var draftProjectionRows = []struct{ id, raw string }{
	{"full", `{"id":"full","owner":"pc","project":"owner/app","unassigned":false,"status":"draft","title":"題名","shownTitle":"表示","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-02T00:00:00Z","priority":"high","retryAt":null,"questions":[{"id":"q","text":"?"}],"issue":{"number":7,"project":"owner/app"},"issues":[{"number":8}],"parentSplit":"p","parentConsultation":"pc","splitChildren":["a","b"],"splitIssues":[{"project":"owner/app","body":"long"},{"project":5},"x",null],"messages":[{"role":"proxy","text":"p"},{"role":"user","text":"` + strings.Repeat("😀", 70) + `","images":[{"id":"00000000-0000-0000-0000-00000000abcd"}]},{"role":"user","text":"second"}],"body":"x <!-- issue-desk:followup --> y","usageMeasurementHold":{"at":"t"},"consultationBudget":{"stopped":{"at":"t","reason":"r"},"pendingCall":{"id":1},"unknownCalls":3,"other":"dropped"},"consultationParts":["a","b","c"],"proposal":{"kind":"improvements","source":"s","id":"i"},"savedMaterials":"big","revision":4,"filingCheckCalls":[{"a":1}],"stallCause":"c","error":"e","consultationStops":[1],"splitScopeHeld":true,"ready":true,"kind":"k","consultationChildren":["c"],"basisConflicts":[{"fact":"f"}],"attempted":true,"_stopped":false,"finishedWithoutIssue":true,"origin":"o","followup":{"x":1},"run":{"id":"r"},"queued":true}`},
	{"numbers", `{"id":42,"project":1e3,"status":"draft","revision":9007199254740993,"priority":1.50,"proposal":{"kind":1e3},"consultationBudget":{"stopped":true,"pendingCall":null,"unknownCalls":1.0e2},"usageMeasurementHold":null,"consultationParts":"none","splitIssues":null,"messages":false,"body":42}`},
	{"dupkeys", `{"id":"first","id":"last","status":"a","status":"b","proposal":{"kind":"x"},"proposal":{"kind":"y","kind":"z"},"splitIssues":[],"splitIssues":[{"project":"p"}],"title":"t1","title":"t2"}`},
	{"escaped", `{"\u0069d":"esc","stat\u0075s":"draft","prop\u006fsal":null,"split\u0049ssues":false,"title":"\u00e9\ud83d\ude00\"\\","messages":[{"role":"user","text":42,"images":[{"id":"bad"}]},{"role":"user","images":"x"}]}`},
	{"spaced", "{ \"id\" : \"spaced\" ,\n \"status\" : \"draft\" , \"project\" : \"owner/app\" , \"issue\" : { \"number\" : 1 } , \"questions\" : null , \"messages\" : [ ] , \"proposal\" : 5 , \"consultationBudget\" : 7 , \"splitIssues\" : { \"a\" : 1 } }"},
	{"empty", `{}`},
	{"nulls", `{"id":null,"status":null,"project":null,"title":null,"messages":null,"body":null,"proposal":null,"consultationBudget":null,"usageMeasurementHold":false,"consultationParts":null,"splitIssues":null,"issue":null}`},
	{"imgonly", `{"id":"imgonly","status":"thinking","messages":[{"role":"assistant","text":"a","images":[{"id":"00000000-0000-0000-0000-000000000000"},{"id":7}]},{"role":"proxy"}],"body":"plain","consultationBudget":{"stopped":"s","pendingCall":false},"usageMeasurementHold":0,"consultationParts":[]}`},
	// NULを含む行は射影せず全体を返す。
	{"nul", `{"id":"nul","status":"draft","title":"a\u0000b","body":"full\u0000body","messages":[{"role":"user","text":"x"}]}`},
}

const (
	draftProjectionNoSummary = `"body":"","messages":[],"consultationBudget":{"stopped":null,"pendingCall":null,"unknownCalls":null},"usageMeasurementHold":null`
	draftProjectionImage     = `{"role":"image-marker","images":[{"id":"00000000-0000-0000-0000-000000000000"}]}`
)

var draftProjectionWant = map[DraftReadPurpose]map[string]string{
	DraftReadScreen: {
		"full":    `{"id":"full","owner":"pc","project":"owner/app","unassigned":false,"status":"draft","revision":4,"title":"題名","shownTitle":"表示","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-02T00:00:00Z","priority":"high","retryAt":null,"questions":[{"id":"q","text":"?"}],"issue":{"number":7,"project":"owner/app"},"issues":[{"number":8}],"parentSplit":"p","parentConsultation":"pc","splitChildren":["a","b"],"ready":true,"kind":"k","consultationChildren":["c"],"basisConflicts":[{"fact":"f"}],"attempted":true,"_stopped":false,"finishedWithoutIssue":true,"origin":"o","followup":{"x":1},"run":{"id":"r"},"queued":true,"proposal":{"kind":"improvements"},"body":"<!-- issue-desk:followup -->","messages":[{"role":"user","text":"` + strings.Repeat("😀", 60) + `"},{"role":"proxy"},` + draftProjectionImage + `],"splitIssues":[{"project":"owner/app"},{},{},{}],"consultationBudget":{"stopped":{},"pendingCall":true,"unknownCalls":3},"usageMeasurementHold":true,"_consultationPartCount":3}`,
		"numbers": `{"id":42,"project":1e3,"status":"draft","revision":9007199254740993,"priority":1.50,"proposal":{"kind":1e3},"body":"","messages":[],"splitIssues":null,"consultationBudget":{"stopped":true,"pendingCall":null,"unknownCalls":100},"usageMeasurementHold":null}`,
		"dupkeys": `{"id":"last","status":"b","title":"t2","proposal":{"kind":"z"},"splitIssues":[{"project":"p"}],` + draftProjectionNoSummary + `}`,
		"escaped": `{"id":"esc","status":"draft","title":"é😀\"\\","proposal":null,"body":"","messages":[{"role":"user","text":""}],"splitIssues":true,"consultationBudget":{"stopped":null,"pendingCall":null,"unknownCalls":null},"usageMeasurementHold":null}`,
		"spaced":  `{"id":"spaced","status":"draft","project":"owner/app","issue":{"number":1},"questions":null,"proposal":{"kind":null},"splitIssues":true,` + draftProjectionNoSummary + `}`,
		"empty":   `{` + draftProjectionNoSummary + `}`,
		"nulls":   `{"id":null,"status":null,"project":null,"title":null,"issue":null,"proposal":null,"body":"","messages":[],"splitIssues":null,"consultationBudget":{"stopped":null,"pendingCall":null,"unknownCalls":null},"usageMeasurementHold":true}`,
		"imgonly": `{"id":"imgonly","status":"thinking","body":"","messages":[{"role":"proxy"},` + draftProjectionImage + `],"consultationBudget":{"stopped":"s","pendingCall":true,"unknownCalls":null},"usageMeasurementHold":true,"_consultationPartCount":0}`,
	},
	DraftReadPage: {
		"full":    `{"id":"full","owner":"pc","project":"owner/app","unassigned":false,"status":"draft","title":"題名","shownTitle":"表示","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-02T00:00:00Z","issue":{"number":7,"project":"owner/app"},"parentSplit":"p","splitChildren":["a","b"],"finishedWithoutIssue":true,"splitIssues":[{"project":"owner/app"},{},{},{}]}`,
		"numbers": `{"id":42,"project":1e3,"status":"draft","splitIssues":null}`,
		"dupkeys": `{"id":"last","status":"b","title":"t2","splitIssues":[{"project":"p"}]}`,
		"escaped": `{"id":"esc","status":"draft","title":"é😀\"\\","splitIssues":true}`,
		"spaced":  `{"id":"spaced","status":"draft","project":"owner/app","issue":{"number":1},"splitIssues":true}`,
		"empty":   `{}`,
		"nulls":   `{"id":null,"status":null,"project":null,"title":null,"issue":null,"splitIssues":null}`,
		"imgonly": `{"id":"imgonly","status":"thinking"}`,
	},
	DraftReadAlerts: {
		"full":    `{"id":"full","project":"owner/app","status":"draft","title":"題名","updatedAt":"2026-01-02T00:00:00Z","parentSplit":"p","consultationBudget":{"stopped":{"at":"t","reason":"r"},"pendingCall":{"id":1},"unknownCalls":3,"other":"dropped"},"stallCause":"c","error":"e","consultationStops":[1],"splitScopeHeld":true}`,
		"numbers": `{"id":42,"project":1e3,"status":"draft","consultationBudget":{"stopped":true,"pendingCall":null,"unknownCalls":1.0e2}}`,
		"dupkeys": `{"id":"last","status":"b","title":"t2"}`,
		"escaped": `{"id":"esc","status":"draft","title":"é😀\"\\"}`,
		"spaced":  `{"id":"spaced","status":"draft","project":"owner/app","consultationBudget":7}`,
		"empty":   `{}`,
		"nulls":   `{"id":null,"status":null,"project":null,"title":null,"consultationBudget":null}`,
		"imgonly": `{"id":"imgonly","status":"thinking","consultationBudget":{"stopped":"s","pendingCall":false}}`,
	},
	DraftReadFilingAlerts: {
		"full":    `{"id":"full","project":"owner/app","status":"draft","title":"題名","issue":{"number":7,"project":"owner/app"},"revision":4,"filingCheckCalls":[{"a":1}]}`,
		"numbers": `{"id":42,"project":1e3,"status":"draft","revision":9007199254740993}`,
		"dupkeys": `{"id":"last","status":"b","title":"t2"}`,
		"escaped": `{"id":"esc","status":"draft","title":"é😀\"\\"}`,
		"spaced":  `{"id":"spaced","status":"draft","project":"owner/app","issue":{"number":1}}`,
		"empty":   `{}`,
		"nulls":   `{"id":null,"status":null,"project":null,"title":null,"issue":null}`,
		"imgonly": `{"id":"imgonly","status":"thinking"}`,
	},
}

func insertDraftProjectionRows(t *testing.T, records *Records) {
	t.Helper()
	for _, row := range draftProjectionRows {
		if _, err := records.DB.ExecContext(context.Background(), `INSERT INTO drafts(id,json) VALUES($1,$2)`, row.id, row.raw); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDraftProjectionFields(t *testing.T) {
	ctx := context.Background()
	records, err := OpenRecords(ctx, pgtest.NewIsolated(t).DB)
	if err != nil {
		t.Fatal(err)
	}
	insertDraftProjectionRows(t, records)
	for purpose, want := range draftProjectionWant {
		t.Run(string(purpose), func(t *testing.T) {
			entries, err := records.ListDraftEntries(ctx, purpose)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != len(draftProjectionRows) {
				t.Fatalf("行数 %d、期待 %d", len(entries), len(draftProjectionRows))
			}
			for i, entry := range entries {
				// 新しく保存した行から順に返す。
				row := draftProjectionRows[len(draftProjectionRows)-1-i]
				if entry.Key != row.id {
					t.Fatalf("%d 行目のキー %q、期待 %q", i, entry.Key, row.id)
				}
				expected := want[row.id]
				if row.id == "nul" {
					expected = row.raw
				}
				wantRecord, err := decode(expected)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(entry.Record, wantRecord) {
					t.Errorf("%s:\n got %#v\nwant %#v", row.id, entry.Record, wantRecord)
				}
			}
		})
	}
}

// 下書きは大きいので、1行の保存文字列の解析は json に1回、jsonb に高々1回だけにする。
// 欄ごとに変換をやり直す形へ戻ると、一覧の初回の読取が数十秒かかる。
func TestDraftProjectionParsesEachDraftOnce(t *testing.T) {
	ctx := context.Background()
	records, err := OpenRecords(ctx, pgtest.NewIsolated(t).DB)
	if err != nil {
		t.Fatal(err)
	}
	insertDraftProjectionRows(t, records)
	stored := regexp.MustCompile(`drafts\."?json"?\)::`)
	// 定数は '...'::jsonb と表示されるので、括弧の後ろの型変換だけが値の変換にあたる。
	converted := regexp.MustCompile(`\)::jsonb\b`)
	for purpose, jsonb := range map[DraftReadPurpose]int{DraftReadScreen: 1, DraftReadPage: 1, DraftReadAlerts: 0, DraftReadFilingAlerts: 0} {
		query, err := draftProjectionSQL(purpose)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := records.DB.QueryContext(ctx, "EXPLAIN (VERBOSE, COSTS OFF) "+query)
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err = rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(line + "\n")
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		if got := len(stored.FindAllString(plan.String(), -1)); got != 1 {
			t.Errorf("%s: 保存文字列の解析が %d 回（期待 1 回）\n%s", purpose, got, plan.String())
		}
		if got := len(converted.FindAllString(plan.String(), -1)); got != jsonb {
			t.Errorf("%s: jsonb への変換が %d 回（期待 %d 回）\n%s", purpose, got, jsonb, plan.String())
		}
	}
}
