package consultationai

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"issue-desk-intake/server/internal/fleet"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// errUnowned marks a conversation record kept because its owner could not be
// proved; it is reported separately from read failures.
var errUnowned = errors.New("会話記録の所有者を確認できないため残しました")

// shortCause drops the absolute path a *fs.PathError carries so that a failure
// stays short and names only the file it is reported with.
func shortCause(err error) error {
	var path *fs.PathError
	if errors.As(err, &path) {
		return path.Err
	}
	return err
}

var uuid = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

func sessionHome(provider string) string {
	key, tail := "CODEX_HOME", ".codex"
	if provider == "claude" {
		key, tail = "CLAUDE_CONFIG_DIR", ".claude"
	}
	if value := os.Getenv(key); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, tail)
}

// Cleanup is limited to records proved by a controller-owned marker and matching session metadata.
func (s *Service) cleanupSession(state sessionState) error {
	return s.cleanupMarkedSession(state, s.sessionMarker(state.ID))
}
func (s *Service) cleanupMarkedSession(state sessionState, marker string) error {
	if !uuid.MatchString(state.ID) || (state.Workspace != filepath.Join(s.options.RuntimeDir, "intake-workspace") && state.Workspace != filepath.Join(s.options.RuntimeDir, "filing-scope-workspace")) || filepath.Dir(marker) != state.Workspace {
		return errors.New("会話の削除対象が不正です")
	}
	found, err := markerMatchesSession(marker, state)
	if err != nil || !found {
		return err
	}
	home := sessionHome(state.Provider)
	if home == "" {
		return errors.New("会話記録の保存先が不明です")
	}
	dir := "sessions"
	if state.Provider == "claude" {
		dir = "projects"
	}
	root := filepath.Join(home, dir)
	err = filepath.WalkDir(root, func(file string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if name != state.ID+".jsonl" && !strings.HasSuffix(name, "-"+state.ID+".jsonl") {
			return nil
		}
		owned, err := sessionFileOwned(file, state)
		if err != nil {
			return err
		}
		if !owned {
			return errUnowned
		}
		return os.Remove(file)
	})
	if errors.Is(err, errUnowned) {
		return errUnowned
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("相談の会話記録を読み取れないため残しました: %w", shortCause(err))
	}
	return os.Remove(marker)
}

// markerMatchesSession reads the controller-owned marker, including the legacy format,
// and confirms it names the same session, draft and workspace. found is false when the
// marker no longer exists.
func markerMatchesSession(marker string, state sessionState) (bool, error) {
	raw, err := os.ReadFile(marker)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var saved sessionState
	if json.Unmarshal(raw, &saved) != nil {
		return false, errors.New("会話の目印が不正です")
	}
	if saved.ID == "" {
		var old map[string]any
		if json.Unmarshal(raw, &old) != nil {
			return false, errors.New("会話の目印が不正です")
		}
		saved.ID = fleet.Text(old["sessionId"])
		if state.Provider == "codex" {
			saved.ID = fleet.Text(old["threadId"])
		}
		saved.Workspace = fleet.Text(old["cwd"])
	}
	if saved.ID != state.ID || saved.Draft != state.Draft || saved.Workspace != state.Workspace {
		return false, errors.New("会話の削除対象を確認できません")
	}
	return true, nil
}

// sessionFileOwned reports whether the first records of the provider session file name
// this session and workspace.
func sessionFileOwned(file string, state sessionState) (bool, error) {
	f, err := os.Open(file)
	if err != nil {
		return false, err
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	owned := false
	for n := 0; n < 10 && scanner.Scan(); n++ {
		var value map[string]any
		if json.Unmarshal(scanner.Bytes(), &value) != nil {
			continue
		}
		if state.Provider == "codex" {
			payload := fleet.Object(value["payload"])
			owned = fleet.Text(payload["id"]) == state.ID && fleet.Text(payload["cwd"]) == state.Workspace
		} else {
			owned = fleet.Text(value["sessionId"]) == state.ID && fleet.Text(value["cwd"]) == state.Workspace
		}
		if owned {
			break
		}
	}
	readErr := scanner.Err()
	f.Close()
	if readErr != nil {
		return false, readErr
	}
	return owned, nil
}

// CleanupAbandoned removes every marker and image directory it can prove and
// keeps going past one that fails, so a single broken marker does not keep the
// rest in place. Each failure names the file it concerns relative to RuntimeDir
// and is joined with errors.Join so callers can report them one by one.
func (s *Service) CleanupAbandoned() error {
	var failures []error
	fail := func(name string, err error) {
		failures = append(failures, fmt.Errorf("%s: %w", name, shortCause(err)))
	}
	for _, name := range []string{"intake-workspace", "filing-scope-workspace"} {
		workspace := filepath.Join(s.options.RuntimeDir, name)
		entries, err := os.ReadDir(workspace)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			fail(name, err)
			continue
		}
		for _, entry := range entries {
			if entry.Type()&os.ModeSymlink != 0 {
				continue
			}
			label := filepath.Join(name, entry.Name())
			if entry.IsDir() {
				if strings.HasPrefix(entry.Name(), ".intake-images-") || strings.HasPrefix(entry.Name(), "intake-images-") && uuid.MatchString(strings.TrimPrefix(entry.Name(), "intake-images-")) {
					if err := os.RemoveAll(filepath.Join(workspace, entry.Name())); err != nil {
						fail(label, err)
					}
				}
				continue
			}
			prefix := "intake-session-"
			provider := "claude"
			if strings.HasPrefix(entry.Name(), "intake-codex-session-") {
				prefix = "intake-codex-session-"
				provider = "codex"
			}
			if !strings.HasPrefix(entry.Name(), prefix) || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			id := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), prefix), ".json")
			if !uuid.MatchString(id) {
				continue
			}
			marker := filepath.Join(workspace, entry.Name())
			raw, err := os.ReadFile(marker)
			if err != nil {
				fail(label, err)
				continue
			}
			var state sessionState
			if json.Unmarshal(raw, &state) != nil {
				fail(label, errors.New("中断した会話の記録が不正です"))
				continue
			}
			if state.ID == "" {
				state.ID, state.Provider, state.Workspace = id, provider, workspace
			}
			if state.ID != id {
				fail(label, errors.New("会話の目印とIDが一致しません"))
				continue
			}
			if err := s.cleanupMarkedSession(state, marker); err != nil {
				fail(label, err)
			}
		}
	}
	return errors.Join(failures...)
}
