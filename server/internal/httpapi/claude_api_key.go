package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/claudeapikey"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
)

const claudeKeyReads = "claude-api-key-reads.jsonl"

func (s *WorkerRoutes) claudeAPIKeyRoute(w http.ResponseWriter, r *http.Request, host string) {
	w.Header().Set("Cache-Control", "no-store")
	principal, ok := s.identity(w, r, auth.Policy{PC: true})
	if !ok {
		return
	}
	if !fleet.ValidHost(host) || principal.Name != host {
		workerFailure(w, contract.NewProblem(403, "送り手と対象PCが一致しません。", ""))
		return
	}
	if err := workerMethod(r, http.MethodGet); err != nil {
		workerFailure(w, err)
		return
	}
	if s.options.ClaudeAPIKeyTokenFile == "" {
		workerFailure(w, contract.NewProblem(503, "配布状態を読み取れません。", ""))
		return
	}
	store := claudeapikey.New(s.options.ClaudeAPIKeyTokenFile)
	record, err := store.Read()
	if err == nil {
		err = recordClaudeKeyRead(store.Dir, principal.Name, record.Version, s.options.Now().UTC())
	}
	if err != nil {
		workerFailure(w, contract.NewProblem(503, "配布状態を読み取れません。", ""))
		return
	}
	workerSend(w, record, http.StatusOK)
}

// The directory and journal must remain private and owned by the server user.
// flock serializes whole JSON lines between both server instances.
func recordClaudeKeyRead(dir, host string, version *string, at time.Time) error {
	fail := errors.New("cannot record key distribution")
	fd, err := syscall.Open(filepath.Clean(dir), syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fail
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&07777 != 0700 {
		return fail
	}
	if syscall.Flock(fd, syscall.LOCK_EX) != nil {
		return fail
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	f, err := syscall.Openat(fd, claudeKeyReads, syscall.O_WRONLY|syscall.O_APPEND|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return fail
	}
	file := os.NewFile(uintptr(f), claudeKeyReads)
	defer file.Close()
	if syscall.Fstat(f, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Uid != uint32(os.Geteuid()) || st.Mode&07777 != 0600 || st.Nlink != 1 {
		return fail
	}
	data, err := json.Marshal(struct {
		Host    string    `json:"host"`
		Version *string   `json:"version"`
		At      time.Time `json:"at"`
	}{host, version, at})
	if err != nil {
		return fail
	}
	data = append(data, '\n')
	n, err := file.Write(data)
	if err != nil || n != len(data) || file.Sync() != nil {
		// Keep an interrupted append from corrupting the preceding records.
		_ = file.Truncate(st.Size)
		_ = file.Sync()
		return fail
	}
	if file.Close() != nil || syscall.Fsync(fd) != nil {
		return fail
	}
	return nil
}
