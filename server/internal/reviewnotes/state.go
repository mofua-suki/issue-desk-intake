// Package reviewnotes provides the daily review-note persistence and adoption
// rules. Classification and scheduling are supplied by the caller, not here.
package reviewnotes

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

type Config struct {
	MinOccurrences int `json:"minOccurrences"`
	StaleDays      int `json:"staleDays"`
	MaxBytes       int `json:"maxBytes"`
}

// ParseConfig accepts the reviewNotes object from assignments, with the same
// defaults as assignments.json and fleet/defaults.json. Missing keys use defaults.
func ParseConfig(raw []byte) (Config, error) {
	c := Config{MinOccurrences: 2, StaleDays: 60, MaxBytes: 4000}
	if len(raw) != 0 {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			return c, errors.New("reviewNotesはオブジェクトで指定してください")
		}
		for key, value := range fields {
			var target *int
			switch key {
			case "minOccurrences":
				target = &c.MinOccurrences
			case "staleDays":
				target = &c.StaleDays
			case "maxBytes":
				target = &c.MaxBytes
			default:
				return c, errors.New("reviewNotesに不明な項目があります")
			}
			if string(value) == "null" || json.Unmarshal(value, target) != nil {
				return c, errors.New("reviewNotesの値は正の整数です")
			}
		}
	}
	return c, c.validate()
}

func (c Config) validate() error {
	// Cutoffs use time.Duration, so reject values which would overflow it.
	if c.MinOccurrences < 1 || c.StaleDays < 1 || int64(c.StaleDays) > int64((1<<63-1)/(24*time.Hour)) || c.MaxBytes < 1 {
		return errors.New("reviewNotesの回数・日数・バイト数が不正です")
	}
	return nil
}

type Item struct {
	Kind     string    `json:"kind"`
	Text     string    `json:"text"`
	Issues   []int64   `json:"issues"`
	LastSeen time.Time `json:"lastSeen"`
}

type State struct {
	Items           []Item                 `json:"items"`
	Candidates      []Item                 `json:"candidates"`
	Removed         []Item                 `json:"removed"`
	Cursor          int64                  `json:"cursor"`
	UpdatedAt       time.Time              `json:"updatedAt"`
	Classifications []CachedClassification `json:"classifications,omitempty"`
}

// Decode treats SQL NULL, JSON null and missing legacy fields as empty. Unknown
// fields remain readable, including state written by a later classification step.
func Decode(raw []byte) (State, error) {
	var s State
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &s); err != nil {
			return s, err
		}
	}
	if s.Items == nil {
		s.Items = []Item{}
	}
	if s.Candidates == nil {
		s.Candidates = []Item{}
	}
	if s.Removed == nil {
		s.Removed = []Item{}
	}
	return s, nil
}

// Published deliberately decodes only the public fields. Legacy entries need
// not contain occurrence metadata to be delivered to workers.
func Published(raw []byte) ([]map[string]string, error) {
	var s struct {
		Items []struct {
			Kind string `json:"kind"`
			Text string `json:"text"`
		} `json:"items"`
	}
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
	}
	items := []map[string]string{}
	for _, item := range s.Items {
		if strings.TrimSpace(item.Kind) != "" && strings.TrimSpace(item.Text) != "" {
			items = append(items, map[string]string{"kind": item.Kind, "text": item.Text})
		}
	}
	return items, nil
}

type Note struct {
	ID         int64
	SourceKind string
	Issue      int64
	Text       string
	At         time.Time
}

// Classification refers to a source row, never to a caller-supplied Issue.
// Kind is a semantic category, distinct from finding/followup (SourceKind).
type Classification struct {
	NoteID int64
	Kind   string
	Text   string
	Quote  string
}

// Apply implements adoption without advancing the cursor. Run advances it only
// after all reads/classification succeed. Removed describes this update's expiry.
func Apply(previous State, notes []Note, classified []Classification, now time.Time, c Config) (State, error) {
	if err := c.validate(); err != nil {
		return State{}, err
	}
	next, _ := Decode(nil)
	next.Cursor, next.UpdatedAt = previous.Cursor, now.UTC()
	cutoff := now.Add(-time.Duration(c.StaleDays) * 24 * time.Hour)
	byKind := keepFresh(&next, previous, cutoff)
	addNotes(byKind, notes, uniqueAssignments(classified), cutoff)
	promote(&next, byKind, c)
	for _, list := range [][]Item{next.Items, next.Candidates, next.Removed} {
		sort.Slice(list, func(i, j int) bool { return list[i].Kind < list[j].Kind })
	}
	return next, nil
}

// keepFresh carries over the items and candidates seen since the cutoff, and moves the older ones to Removed.
func keepFresh(next *State, previous State, cutoff time.Time) map[string]Item {
	byKind := map[string]Item{}
	for _, list := range [][]Item{previous.Items, previous.Candidates} {
		for _, item := range list {
			if strings.TrimSpace(item.Kind) == "" || strings.TrimSpace(item.Text) == "" {
				continue
			}
			if item.LastSeen.Before(cutoff) {
				next.Removed = append(next.Removed, item)
				continue
			}
			item.Issues = uniqueIssues(item.Issues)
			byKind[item.Kind] = item
		}
	}
	return byKind
}

// uniqueAssignments keeps the classifications of notes classified exactly once with a kind and text.
func uniqueAssignments(classified []Classification) map[int64]Classification {
	counts := map[int64]int{}
	for _, assignment := range classified {
		counts[assignment.NoteID]++
	}
	assignments := map[int64]Classification{}
	for _, assignment := range classified {
		if counts[assignment.NoteID] == 1 && strings.TrimSpace(assignment.Kind) != "" && strings.TrimSpace(assignment.Text) != "" {
			assignments[assignment.NoteID] = assignment
		}
	}
	return assignments
}

// addNotes adds each classified recent finding or follow-up to its kind.
func addNotes(byKind map[string]Item, notes []Note, assignments map[int64]Classification, cutoff time.Time) {
	for _, note := range notes {
		a, ok := assignments[note.ID]
		if !ok || note.Issue < 1 || strings.TrimSpace(note.Text) == "" || note.At.Before(cutoff) || (note.SourceKind != "finding" && note.SourceKind != "followup") {
			continue
		}
		item := byKind[a.Kind]
		item.Kind = a.Kind
		item.Issues = uniqueIssues(append(item.Issues, note.Issue))
		if !note.At.Before(item.LastSeen) {
			item.LastSeen, item.Text = note.At.UTC(), a.Text
		}
		byKind[a.Kind] = item
	}
}

// promote publishes the kinds seen in enough issues within the byte budget;
// the rest stay candidates.
func promote(next *State, byKind map[string]Item, c Config) {
	threshold := max(2, c.MinOccurrences)
	for _, item := range byKind {
		if len(item.Issues) >= threshold {
			next.Items = append(next.Items, item)
		} else {
			next.Candidates = append(next.Candidates, item)
		}
	}
	// Lowest occurrence count is demoted first; an older last observation wins
	// ties. Kind provides deterministic ordering if both values are identical.
	sort.Slice(next.Items, func(i, j int) bool {
		a, b := next.Items[i], next.Items[j]
		if len(a.Issues) != len(b.Issues) {
			return len(a.Issues) < len(b.Issues)
		}
		if !a.LastSeen.Equal(b.LastSeen) {
			return a.LastSeen.Before(b.LastSeen)
		}
		return a.Kind < b.Kind
	})
	var size int64
	for _, item := range next.Items {
		size += int64(len(item.Text)) // Go string length is UTF-8 bytes.
	}
	for size > int64(c.MaxBytes) {
		item := next.Items[0]
		next.Items = next.Items[1:]
		next.Candidates = append(next.Candidates, item)
		size -= int64(len(item.Text))
	}
}

func uniqueIssues(issues []int64) []int64 {
	seen := map[int64]bool{}
	out := []int64{}
	for _, issue := range issues {
		if issue > 0 && !seen[issue] {
			out = append(out, issue)
			seen[issue] = true
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
