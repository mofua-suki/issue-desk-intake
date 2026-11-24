package reviewnotes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCacheDoesNotChooseBetweenConflictingExactInputs(t *testing.T) {
	notes := []Note{{ID: 1, SourceKind: "finding", Issue: 1, Text: "同じ指摘", At: testNow}, {ID: 2, SourceKind: "finding", Issue: 2, Text: "同じ指摘", At: testNow}}
	s := Service{Classify: func(context.Context, []Note, []Item) ([]Classification, error) {
		return []Classification{{NoteID: 1, Kind: "a", Text: "注意点A", Quote: "同じ指摘"}, {NoteID: 2, Kind: "b", Text: "注意点B", Quote: "同じ指摘"}}, nil
	}}
	classes, cache, err := s.classify(t.Context(), "owner/repo", State{}, notes, testNow, Config{2, 60, 4000})
	if err != nil || len(classes) != 2 || len(cache) != 0 {
		t.Fatal(classes, cache, err)
	}
}

func TestCacheStorageLimits(t *testing.T) {
	notes := make([]Note, 202)
	for i := range notes {
		notes[i] = Note{ID: int64(i + 1), Issue: int64(i + 1), SourceKind: "finding", Text: fmt.Sprintf("根拠%d", i), At: testNow}
	}
	notes[201].Text = strings.Repeat("x", maxCachedSourceBytes+1)
	s := Service{Classify: func(_ context.Context, input []Note, _ []Item) ([]Classification, error) {
		var result []Classification
		for _, n := range input {
			result = append(result, Classification{NoteID: n.ID, Kind: "a", Text: "注意", Quote: clipBody(n.Text)})
		}
		return result, nil
	}}
	_, cache, err := s.classify(t.Context(), "owner/repo", State{}, notes, testNow, Config{2, 60, 4000})
	if err != nil || len(cache) != maxCachedClassifications {
		t.Fatalf("cache=%d err=%v", len(cache), err)
	}
	raw, err := json.Marshal(cache)
	if err != nil || strings.Contains(string(raw), notes[201].Text) {
		t.Fatal("長い本文を保存した", err)
	}
	for _, c := range cache {
		if len(c.Source.Text) > maxCachedSourceBytes {
			t.Fatal("本文上限超過")
		}
	}
}

func TestCacheRejectsMissingProvenanceAndNeverExtendsExpiry(t *testing.T) {
	config := Config{2, 60, 4000}
	original := Note{ID: 1, SourceKind: "finding", Issue: 1, Text: "根拠", At: testNow}
	entry := CachedClassification{Repo: "owner/repo", Version: classificationVersion, Source: original, Assignment: Classification{NoteID: 1, Kind: "a", Text: "注意点", Quote: "根拠"}, ExpiresAt: testNow.Add(60 * 24 * time.Hour)}
	if !entry.valid("owner/repo", 1, testNow.Add(time.Hour), config) {
		t.Fatal("有効な出典を拒否")
	}
	for _, mutate := range []func(*CachedClassification){
		func(c *CachedClassification) { c.ExpiresAt = c.ExpiresAt.Add(time.Nanosecond) },
		func(c *CachedClassification) { c.Assignment.NoteID = 2 },
		func(c *CachedClassification) { c.Assignment.Quote = "存在しない" },
		func(c *CachedClassification) { c.Source.Issue = 0 },
	} {
		bad := entry
		mutate(&bad)
		if bad.valid("owner/repo", 1, testNow.Add(time.Hour), config) {
			t.Fatal("不正な保存値を再利用", bad)
		}
	}
	if entry.valid("owner/repo", 1, entry.ExpiresAt, config) {
		t.Fatal("期限ちょうどでも再利用")
	}
}
