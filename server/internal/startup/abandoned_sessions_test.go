package startup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// plantAbandonedSession writes the marker, image directory and Codex record a
// stopped owner leaves behind for one consultation conversation.
func plantAbandonedSession(t *testing.T, runtimeDir, codexHome, id string) (marker, images, record string) {
	t.Helper()
	work := filepath.Join(runtimeDir, "intake-workspace")
	if err := os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	marker = filepath.Join(work, "intake-session-"+id+".json")
	raw, _ := json.Marshal(map[string]any{"id": id, "draft": "44444444-4444-4444-8444-444444444444", "provider": "codex", "workspace": work})
	if err := os.WriteFile(marker, raw, 0600); err != nil {
		t.Fatal(err)
	}
	images = filepath.Join(work, "intake-images-"+id)
	if err := os.MkdirAll(images, 0700); err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(codexHome, "sessions")
	if err := os.MkdirAll(sessions, 0700); err != nil {
		t.Fatal(err)
	}
	record = filepath.Join(sessions, "rollout-"+id+".jsonl")
	meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": id, "cwd": work}})
	if err := os.WriteFile(record, meta, 0600); err != nil {
		t.Fatal(err)
	}
	return marker, images, record
}

func TestStartExclusiveCleansAbandonedSessionsOnlyAsOwnerPostgres(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	active := openLifecycleApp(t, lifecycleGitea, nil)
	active.StopBackground()
	id := "55555555-5555-4555-8555-555555555555"
	marker, images, record := plantAbandonedSession(t, active.Config.RuntimeDir, codexHome, id)
	other := filepath.Join(active.Config.RuntimeDir, "intake-workspace", "notes.txt")
	if err := os.WriteFile(other, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	app, err := OpenBusinessApp(context.Background(), active.Config, active.DB, active.Revision, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	// Business initialization can run beside the current owner, so it must keep the owner's markers.
	for _, path := range []string{marker, images, record} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("非担当が会話の記録を消した", path, err)
		}
	}
	if err = app.StartExclusive(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer app.StopBackground()
	for _, path := range []string{marker, images, record} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("中断した会話の記録が残った", path, err)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("無関係なファイルを消した", err)
	}
}

func TestStartExclusiveContinuesWhenAbandonedCleanupFailsPostgres(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	active := openLifecycleApp(t, lifecycleGitea, nil)
	active.StopBackground()
	work := filepath.Join(active.Config.RuntimeDir, "intake-workspace")
	if err := os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(work, "intake-session-66666666-6666-4666-8666-666666666666.json")
	if err := os.WriteFile(broken, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var logs []string
	app, err := OpenBusinessApp(context.Background(), active.Config, active.DB, active.Revision, func(line string) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, line)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if err = app.StartExclusive(context.Background()); err != nil {
		t.Fatal("後片付けの失敗で起動が止まった", err)
	}
	defer app.StopBackground()
	if app.currentGate() == nil || app.backgroundCtx == nil {
		t.Fatal("単独実行が開始されていない")
	}
	if _, err := os.Stat(broken); err != nil {
		t.Fatal("確認できない目印を消した", err)
	}
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, line := range logs {
		if strings.Contains(line, "中断した相談の会話記録を後片付けできませんでした") && strings.Contains(line, "中断した会話の記録が不正です") && strings.Contains(line, filepath.Base(broken)) {
			found = true
		}
	}
	if !found {
		t.Fatal("後片付けの失敗の原因が記録されていない", logs)
	}
}
