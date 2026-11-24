package adminapi

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

//go:embed decision-seeds.json
var decisionSeeds []byte

func (s *service) decisionMirror() string {
	if file := str(s.Config["decisionMirrorFile"]); file != "" {
		return file
	}
	if s.RuntimeDir == "" {
		return ""
	}
	return filepath.Join(s.RuntimeDir, "state", "intake-decisions.json")
}
func (s *service) writeDecisionMirror(ctx context.Context) error {
	file := s.decisionMirror()
	if file == "" {
		return nil
	}
	entries, e := s.allDecisions(ctx)
	if e != nil {
		return e
	}
	raw, e := json.MarshalIndent(s.exportDecisions(entries), "", "  ")
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(file), 0700); e != nil {
		return e
	}
	temporary := file + "." + randomID() + ".tmp"
	f, e := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	defer os.Remove(temporary)
	_, e = f.Write(append(raw, '\n'))
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	if e = os.Rename(temporary, file); e != nil {
		return e
	}
	directory, e := os.Open(filepath.Dir(file))
	if e != nil {
		return e
	}
	defer directory.Close()
	return directory.Sync()
}
func validImportedDecision(d map[string]any) bool {
	revision, ok := integer(d["revision"])
	_, created := moment(d["createdAt"])
	_, updated := moment(d["updatedAt"])
	_, enabled := d["enabled"].(bool)
	_, note := d["note"].(string)
	projectValid := d["project"] == nil || str(d["project"]) != ""
	removed := true
	if d["removedAt"] != nil {
		_, removed = moment(d["removedAt"])
	}
	return uuidPattern.MatchString(str(d["id"])) && strings.TrimSpace(str(d["text"])) != "" && (d["note"] == nil || note) && projectValid && enabled && (d["source"] == "manual" || d["source"] == "seed") && ok && revision >= 0 && created && updated && removed
}
func (s *service) restoreDecisions(ctx context.Context) error {
	file := s.decisionMirror()
	entries, e := s.allDecisions(ctx)
	if e != nil {
		return e
	}
	if file != "" {
		if e = s.restoreFromMirror(ctx, file, entries); e != nil {
			return e
		}
	}
	if e = s.addDecisionSeeds(ctx); e != nil {
		return e
	}
	return s.writeDecisionMirror(ctx)
}

// restoreFromMirror imports the mirror file into an empty store. A mirror that
// is invalid or differs from the store is set aside instead of overwritten.
func (s *service) restoreFromMirror(ctx context.Context, file string, entries []map[string]any) error {
	var e error
	raw, readErr := os.ReadFile(file)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if readErr == nil {
		var snapshot map[string]any
		decodeErr := decodeJSON(raw, &snapshot)
		list, listOK := snapshot["decisions"].([]any)
		version, _ := integer(snapshot["version"])
		valid := decodeErr == nil && snapshot["format"] == "issue-desk-decisions" && version == 1 && listOK
		keep := !valid
		if valid && len(entries) == 0 {
			tx, e := s.AppDB.BeginTx(ctx, nil)
			if e != nil {
				return e
			}
			seen := map[string]bool{}
			for _, value := range list {
				d := obj(value)
				id := str(d["id"])
				if !validImportedDecision(d) || seen[id] {
					keep = true
					continue
				}
				seen[id] = true
				if e = jsonPut(ctx, tx, "decisions", id, exportedDecision(d)); e != nil {
					tx.Rollback()
					return e
				}
			}
			if e = tx.Commit(); e != nil {
				return e
			}
		} else if valid {
			exported := []any{}
			for _, d := range entries {
				exported = append(exported, exportedDecision(d))
			}
			keep = !reflect.DeepEqual(list, exported)
		}
		if keep {
			if e = os.Rename(file, file+".kept-"+randomID()); e != nil {
				return e
			}
		}
	}
	return nil
}

// addDecisionSeeds registers the seeded decisions that are not stored yet.
func (s *service) addDecisionSeeds(ctx context.Context) error {
	tx, e := s.AppDB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var seeds []map[string]any
	if e = json.Unmarshal(decisionSeeds, &seeds); e != nil {
		return e
	}
	at := iso(s.Now())
	for _, seed := range seeds {
		id := str(seed["id"])
		existing, e := jsonGet(ctx, tx, "decisions", id)
		if e != nil {
			return e
		}
		if existing != nil {
			continue
		}
		seed["note"] = "受付の起動時に登録した利用者の方針。画面で編集・無効化・削除できます。"
		seed["project"] = nil
		seed["enabled"] = true
		seed["source"] = "seed"
		seed["revision"] = 0
		seed["createdAt"] = at
		seed["updatedAt"] = at
		if e = jsonPut(ctx, tx, "decisions", id, seed); e != nil {
			return e
		}
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	return nil
}
