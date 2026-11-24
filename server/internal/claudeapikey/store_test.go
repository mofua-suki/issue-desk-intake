package claudeapikey

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func TestClaudeAPIKeyStoreAtomicity(t *testing.T) {
	t.Run("rename failure removes temporary secret", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0700); err != nil {
			t.Fatal(err)
		}
		s := New(filepath.Join(dir, "token"))
		key := "sk-ant-" + strings.Repeat("a", 24)
		renameCalled := false
		s.rename = func(oldfd int, old string, newfd int, new string) error {
			renameCalled = true
			data, err := os.ReadFile(filepath.Join(dir, old))
			if err != nil || !strings.Contains(string(data), key) {
				t.Fatalf("temporary record does not contain key: %v", err)
			}
			if err := os.Mkdir(filepath.Join(dir, Filename), 0700); err != nil {
				t.Fatal(err)
			}
			err = syscall.Renameat(oldfd, old, newfd, new)
			if err == nil {
				t.Fatal("rename unexpectedly succeeded")
			}
			return err
		}
		if err := s.Set(key); !errors.Is(err, ErrStore) || !renameCalled {
			t.Fatalf("expected rename failure, called=%v err=%v", renameCalled, err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != Filename || !entries[0].IsDir() {
			t.Fatalf("unexpected files after failed rename: %v", entries)
		}
	})
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s := New(filepath.Join(dir, "token"))
	reader := New(filepath.Join(dir, "token"))
	key := "sk-ant-" + strings.Repeat("a", 24)
	if err := s.Set(key); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	failures := make(chan error, 256)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				var err error
				if (i+j)%2 == 0 {
					err = s.Set(key)
				} else {
					err = s.Clear()
				}
				if err != nil {
					failures <- err
				}
				r, e := reader.Read()
				if e != nil {
					failures <- e
				} else if r.Version == nil || r.State == "registered" && (r.Key == nil || *r.Key != key) || r.State == "cleared" && r.Key != nil {
					failures <- ErrStore
				}
			}
		}(i)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	path := filepath.Join(dir, Filename)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(); err == nil {
		t.Fatal("accepted symlink")
	}
	if err := s.Set(key); err == nil {
		t.Fatal("replaced symlink")
	}
	os.Remove(path)
	if err := os.Link(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(); err == nil {
		t.Fatal("accepted hard link")
	}
	if err := s.Set(key); err == nil {
		t.Fatal("replaced hard link")
	}
	os.Remove(path)
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(); err == nil {
		t.Fatal("accepted unprotected directory")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		data string
		mode os.FileMode
	}{{"broken", 0600}, {"{}", 0644}} {
		if err := os.WriteFile(path, []byte(tc.data), tc.mode); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Read(); err == nil {
			t.Fatal("accepted invalid file")
		}
		if err := s.Clear(); err == nil {
			t.Fatal("replaced invalid file")
		}
		os.Remove(path)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".claude-api-key-") {
			t.Fatal("temporary file remains")
		}
	}
}
