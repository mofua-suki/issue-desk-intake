package reviewnotes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Bump when the prompt, evidence validation or category contract changes.
const classificationVersion = 1

// Bound retained source bodies even when a repository has many distinct notes.
const maxCachedClassifications = 200
const maxCachedSourceBytes = 4000

type CachedClassification struct {
	Repo       string         `json:"repo"`
	Version    int            `json:"version"`
	ExpiresAt  time.Time      `json:"expiresAt"`
	Source     Note           `json:"source"`
	Assignment Classification `json:"assignment"`
}

type sourceKey struct{ kind, text string }

func (c CachedClassification) valid(repo string, cursor int64, now time.Time, config Config) bool {
	return c.Repo == repo && c.Version == classificationVersion && c.ExpiresAt.After(now) &&
		len(c.Source.Text) <= maxCachedSourceBytes &&
		!c.ExpiresAt.After(c.Source.At.Add(time.Duration(config.StaleDays)*24*time.Hour)) &&
		c.Source.ID > 0 && c.Source.ID <= cursor && c.Source.Issue > 0 &&
		(c.Source.SourceKind == "finding" || c.Source.SourceKind == "followup") &&
		c.Assignment.NoteID == c.Source.ID && validAssignment(c.Assignment, c.Source.Text)
}

// Verify the original row, including identity, repository, reception time and
// full untruncated text. A deleted/edited row is unknown, never a cache hit.
func (s *Service) sourceMatches(ctx context.Context, repo string, source Note) (bool, error) {
	var kind string
	var at time.Time
	var raw []byte
	err := s.DB.QueryRowContext(ctx, `SELECT kind,at,data FROM repo_notes WHERE repo=$1 AND id=$2`, repo, source.ID).Scan(&kind, &at, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var data struct {
		Issue int64  `json:"issue"`
		Text  string `json:"text"`
	}
	return json.Unmarshal(raw, &data) == nil && kind == source.SourceKind && at.Equal(source.At) && data.Issue == source.Issue && data.Text == source.Text, nil
}

func (s *Service) classify(ctx context.Context, repo string, previous State, notes []Note, now time.Time, config Config) ([]Classification, []CachedClassification, error) {
	classes, unknown, cache, err := s.reuseClassifications(ctx, repo, previous, notes, now, config)
	if err != nil {
		return nil, nil, err
	}
	if len(unknown) == 0 {
		return classes, boundedCache(cache), nil
	}
	if s.Classify == nil {
		return nil, nil, errors.New("注意点の種類分けが接続されていません")
	}
	kinds := append(append([]Item{}, previous.Items...), previous.Candidates...)
	fresh, err := s.Classify(ctx, append([]Note(nil), unknown...), kinds)
	if err != nil {
		return nil, nil, err
	}
	classes, cache = adoptClassifications(repo, unknown, fresh, classes, cache, config)
	return classes, boundedCache(cache), nil
}

// reuseClassifications assigns the notes whose single saved classification
// still matches its source, and returns the notes left to classify.
func (s *Service) reuseClassifications(ctx context.Context, repo string, previous State, notes []Note, now time.Time, config Config) (classes []Classification, unknown []Note, cache []CachedClassification, _ error) {
	cache = []CachedClassification{}
	byKey := map[sourceKey][]CachedClassification{}
	for _, c := range previous.Classifications {
		if c.valid(repo, previous.Cursor, now, config) {
			cache = append(cache, c)
			key := sourceKey{c.Source.SourceKind, c.Source.Text}
			byKey[key] = append(byKey[key], c)
		}
	}
	classes, unknown = []Classification{}, []Note{}
	verified := map[sourceKey]bool{}
	for _, note := range notes {
		key := sourceKey{note.SourceKind, note.Text}
		entries := byKey[key]
		// Ambiguous or duplicated saved entries cannot establish one assignment.
		if len(entries) == 1 {
			if _, checked := verified[key]; !checked {
				matches, err := s.sourceMatches(ctx, repo, entries[0].Source)
				if err != nil {
					return nil, nil, nil, err
				}
				verified[key] = matches
			}
			if verified[key] {
				assignment := entries[0].Assignment
				assignment.NoteID = note.ID
				classes = append(classes, assignment)
				continue
			}
		}
		unknown = append(unknown, note)
	}
	return classes, unknown, cache, nil
}

// adoptClassifications adds the new classifications of the requested notes and
// caches the unambiguous ones.
func adoptClassifications(repo string, unknown []Note, fresh, classes []Classification, cache []CachedClassification, config Config) ([]Classification, []CachedClassification) {
	// Only the requested source IDs may enter adoption or the cache. Preserve
	// duplicates for Apply's ambiguity check; do not accept forged known IDs.
	counts := map[int64]int{}
	for _, a := range fresh {
		counts[a.NoteID]++
	}
	assigned := map[sourceKey]Classification{}
	conflicts := map[sourceKey]bool{}
	for _, note := range unknown {
		for _, a := range fresh {
			if a.NoteID != note.ID {
				continue
			}
			classes = append(classes, a)
			if counts[a.NoteID] != 1 || !validAssignment(a, note.Text) {
				continue
			}
			key := sourceKey{note.SourceKind, note.Text}
			if prior, exists := assigned[key]; exists && (prior.Kind != a.Kind || prior.Text != a.Text) {
				conflicts[key] = true
			}
			assigned[key] = a
			// Replace the same exact input rather than accumulating duplicates.
			kept := cache[:0]
			for _, c := range cache {
				if (sourceKey{c.Source.SourceKind, c.Source.Text}) != key {
					kept = append(kept, c)
				}
			}
			cache = kept
			if !conflicts[key] && len(note.Text) <= maxCachedSourceBytes {
				cache = append(cache, CachedClassification{repo, classificationVersion, note.At.Add(time.Duration(config.StaleDays) * 24 * time.Hour), note, a})
			}
		}
	}
	return classes, cache
}

func boundedCache(cache []CachedClassification) []CachedClassification {
	if len(cache) > maxCachedClassifications {
		return cache[len(cache)-maxCachedClassifications:]
	}
	return cache
}

func validAssignment(a Classification, source string) bool {
	return a.NoteID > 0 && categoryPattern.MatchString(a.Kind) && strings.TrimSpace(a.Text) != "" &&
		strings.TrimSpace(a.Quote) != "" && strings.Contains(clipBody(source), a.Quote) && len([]rune(a.Text)) <= 400
}
