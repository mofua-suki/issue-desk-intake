package reviewnotes

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"
)

var testNow = time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC)

func TestDistinctIssuesAndThreshold(t *testing.T) {
	c, _ := ParseConfig(nil)
	notes := []Note{{ID: 1, SourceKind: "finding", Issue: 10, Text: "指摘", At: testNow}, {ID: 2, SourceKind: "followup", Issue: 10, Text: "別の指摘", At: testNow}}
	classes := []Classification{{NoteID: 1, Kind: "validation", Text: "入力を検証する"}, {NoteID: 2, Kind: "validation", Text: "入力を検証する"}}
	state, err := Apply(State{}, notes, classes, testNow, c)
	if err != nil || len(state.Items) != 0 || len(state.Candidates) != 1 || !reflect.DeepEqual(state.Candidates[0].Issues, []int64{10}) {
		t.Fatalf("同じIssueを重複して採用: %+v %v", state, err)
	}
	// Save/reload a candidate before a distinct Issue arrives on another day.
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	state, err = Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	notes[0].Issue = 11
	state, err = Apply(state, notes[:1], classes[:1], testNow.Add(24*time.Hour), c)
	if err != nil || len(state.Items) != 1 || !reflect.DeepEqual(state.Items[0].Issues, []int64{10, 11}) || len(state.Candidates) != 0 {
		t.Fatalf("異なる2件で採用されない: %+v %v", state, err)
	}
	c.MinOccurrences = 3
	state, err = Apply(state, nil, nil, testNow.Add(24*time.Hour), c)
	if err != nil || len(state.Items) != 0 || len(state.Candidates) != 1 {
		t.Fatalf("設定された最小件数が無視された: %+v %v", state, err)
	}
	c.MinOccurrences = 1
	state, err = Apply(State{}, notes[:1], classes[:1], testNow, c)
	if err != nil || len(state.Items) != 0 || len(state.Candidates) != 1 {
		t.Fatalf("設定1でも単一Issueは採用しない: %+v %v", state, err)
	}
}

func TestExpiryAndUTF8Budget(t *testing.T) {
	c := Config{MinOccurrences: 2, StaleDays: 60, MaxBytes: 6}
	cutoff := testNow.Add(-60 * 24 * time.Hour)
	previous := State{
		Items: []Item{
			{Kind: "popular", Text: "あい", Issues: []int64{1, 2, 3}, LastSeen: testNow.Add(-time.Hour)},
			{Kind: "new", Text: "う", Issues: []int64{1, 2}, LastSeen: testNow},
			{Kind: "old", Text: "え", Issues: []int64{1, 2}, LastSeen: cutoff},
			{Kind: "expired", Text: "失効", Issues: []int64{1, 2}, LastSeen: cutoff.Add(-time.Nanosecond)},
		},
		Candidates: []Item{{Kind: "expired-candidate", Text: "候補", Issues: []int64{1}, LastSeen: cutoff.Add(-time.Second)}},
	}
	state, err := Apply(previous, nil, nil, testNow, c)
	if err != nil || len(state.Items) != 1 || state.Items[0].Kind != "popular" || len(state.Candidates) != 2 || len(state.Removed) != 2 {
		t.Fatalf("期限・UTF-8上限: %+v %v", state, err)
	}
	if state.Candidates[0].Kind != "new" || state.Candidates[1].Kind != "old" {
		t.Fatal("上限超過分を候補へ戻さず消している", state)
	}
	// Same counts: only the older one is demoted; exact cutoff is retained.
	c.MaxBytes = 9
	previous.Items[0].Issues = []int64{1, 2}
	state, err = Apply(previous, nil, nil, testNow, c)
	if err != nil || len(state.Items) != 2 || len(state.Candidates) != 1 || state.Candidates[0].Kind != "old" {
		t.Fatalf("同数は最後の出現が古い順: %+v %v", state, err)
	}
	// Demotion retains evidence: a larger budget can adopt the same kind again.
	c.MaxBytes = 12
	state, err = Apply(state, nil, nil, testNow, c)
	if err != nil || len(state.Items) != 3 || len(state.Candidates) != 0 {
		t.Fatalf("候補を再採用できない: %+v %v", state, err)
	}
}

func TestStateRoundTripLegacy(t *testing.T) {
	for _, raw := range []string{"", "null", "{}", `{"items":[{"kind":"old","text":"従来の注意点"}]}`} {
		state, err := Decode([]byte(raw))
		if err != nil || state.Items == nil || state.Candidates == nil || state.Removed == nil || state.Cursor != 0 {
			t.Fatalf("旧状態の省略項目: %+v %v", state, err)
		}
		body, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = Decode(body); err != nil {
			t.Fatal("再読込できない", err)
		}
		public, err := Published([]byte(raw))
		if err != nil || public == nil || (len(state.Items) == 1 && len(public) != 1) {
			t.Fatalf("旧状態の配信: %+v %v", public, err)
		}
	}
}

func TestClassificationSourceAndAmbiguity(t *testing.T) {
	c, _ := ParseConfig(nil)
	notes := []Note{
		{ID: 1, SourceKind: "finding", Issue: 1, Text: "有効", At: testNow},
		{ID: 2, SourceKind: "followup", Issue: 2, Text: "有効", At: testNow},
		{ID: 3, SourceKind: "improvement", Issue: 3, Text: "対象外", At: testNow},
	}
	classes := []Classification{{1, "a", "注意点", ""}, {1, "b", "曖昧", ""}, {2, "a", "注意点", ""}, {3, "a", "注意点", ""}, {999, "a", "存在しない行", ""}}
	state, err := Apply(State{}, notes, classes, testNow, c)
	if err != nil || len(state.Items) != 0 || len(state.Candidates) != 1 || !reflect.DeepEqual(state.Candidates[0].Issues, []int64{2}) {
		t.Fatalf("出典のない・曖昧な分類を集計: %+v %v", state, err)
	}
}

func TestShippedConfigurationAndInvalidValues(t *testing.T) {
	for _, path := range []string{"../../../assignments.json", "../fleet/defaults.json"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var config map[string]json.RawMessage
		if err = json.Unmarshal(raw, &config); err != nil {
			t.Fatal(err)
		}
		got, err := ParseConfig(config["reviewNotes"])
		if err != nil || got != (Config{2, 60, 4000}) {
			t.Fatalf("配備する%sの設定を読めない: %+v %v", path, got, err)
		}
	}
	for _, raw := range []string{`null`, `[]`, `{"maxBytes":null}`, `{"staleDays":0}`, `{"minOccurrences":1.5}`, `{"unknown":1}`, `{"staleDays":999999999}`} {
		if _, err := ParseConfig([]byte(raw)); err == nil {
			t.Fatal("不正な設定を受理", raw)
		}
	}
	if c, err := ParseConfig([]byte(`{"maxBytes":9}`)); err != nil || c != (Config{2, 60, 9}) {
		t.Fatalf("省略時の既定値: %+v %v", c, err)
	}
}
