// Package claudeapikey manages the shared protected distribution record.
package claudeapikey

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
)

const Filename = "claude-api-key-distribution.json"

//lint:ignore ST1005 Claude is a proper product name; preserve the user-facing sentinel message.
var ErrStore = errors.New("Claude APIキーの保護保存領域を利用できません")

//lint:ignore ST1005 Claude is a proper product name; preserve the user-facing sentinel message.
var ErrInvalidKey = errors.New("Claude APIキーの形式または長さが不正です")
var keyPattern = regexp.MustCompile(`^sk-ant-[A-Za-z0-9_-]{20,}$`)
var versionPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

type Record struct {
	State   string  `json:"state"`
	Version *string `json:"version"`
	Key     *string `json:"key"`
}
type Store struct {
	Dir    string
	rename func(int, string, int, string) error
}

func New(tokenFile string) *Store { return &Store{Dir: filepath.Dir(tokenFile)} }
func validKey(s string) bool      { return len(s) <= 4096 && keyPattern.MatchString(s) }
func check(st *syscall.Stat_t, directory bool) bool {
	kind := uint32(syscall.S_IFREG)
	if directory {
		kind = syscall.S_IFDIR
	}
	return st.Mode&syscall.S_IFMT == kind && st.Uid == uint32(os.Geteuid()) && st.Mode&07777 == map[bool]uint32{true: 0700, false: 0600}[directory] && (directory || st.Nlink == 1)
}
func (s *Store) directory() (int, error) {
	fd, err := syscall.Open(s.Dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, ErrStore
	}
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil || !check(&st, true) {
		syscall.Close(fd)
		return -1, ErrStore
	}
	return fd, nil
}
func read(fd int) (Record, error) {
	empty := Record{State: "unregistered"}
	f, err := syscall.Openat(fd, Filename, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err == syscall.ENOENT {
		return empty, nil
	}
	if err != nil {
		return Record{}, ErrStore
	}
	file := os.NewFile(uintptr(f), Filename)
	defer file.Close()
	var st syscall.Stat_t
	if syscall.Fstat(f, &st) != nil || !check(&st, false) || st.Size > 8192 {
		return Record{}, ErrStore
	}
	b, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil || len(b) > 8192 {
		return Record{}, ErrStore
	}
	return decodeRecord(b)
}

// decodeRecord accepts exactly a registered key or a cleared record with a valid version.
func decodeRecord(b []byte) (Record, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &fields) != nil || len(fields) != 3 || fields["state"] == nil || fields["version"] == nil || fields["key"] == nil {
		return Record{}, ErrStore
	}
	var r Record
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&r) != nil || decoder.Decode(new(any)) != io.EOF || r.Version == nil || !versionPattern.MatchString(*r.Version) {
		return Record{}, ErrStore
	}
	if r.State == "registered" && r.Key != nil && validKey(*r.Key) || r.State == "cleared" && r.Key == nil {
		return r, nil
	}
	return Record{}, ErrStore
}
func (s *Store) Read() (Record, error) {
	fd, err := s.directory()
	if err != nil {
		return Record{}, err
	}
	defer syscall.Close(fd)
	// Keep the opened record linked while read checks its ownership and link
	// count. Writers take the exclusive lock before replacing the record.
	if syscall.Flock(fd, syscall.LOCK_SH) != nil {
		return Record{}, ErrStore
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	return read(fd)
}
func (s *Store) Set(key string) error {
	if !validKey(key) {
		return ErrInvalidKey
	}
	return s.write(&key)
}
func (s *Store) Clear() error { return s.write(nil) }
func (s *Store) write(key *string) error {
	fd, err := s.directory()
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	if syscall.Flock(fd, syscall.LOCK_EX) != nil {
		return ErrStore
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	if _, err = read(fd); err != nil {
		return ErrStore
	}
	random := make([]byte, 32)
	if _, err = rand.Read(random); err != nil {
		return ErrStore
	}
	version := base64.RawURLEncoding.EncodeToString(random)
	state := "registered"
	if key == nil {
		state = "cleared"
	}
	data, err := json.Marshal(Record{State: state, Version: &version, Key: key})
	if err != nil {
		return ErrStore
	}
	random = make([]byte, 24)
	if _, err = rand.Read(random); err != nil {
		return ErrStore
	}
	name := ".claude-api-key-" + base64.RawURLEncoding.EncodeToString(random)
	tmp, err := syscall.Openat(fd, name, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return ErrStore
	}
	defer syscall.Unlinkat(fd, name)
	file := os.NewFile(uintptr(tmp), name)
	err = file.Chmod(0600)
	if err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return ErrStore
	}
	rename := s.rename
	if rename == nil {
		rename = syscall.Renameat
	}
	if rename(fd, name, fd, Filename) != nil {
		return ErrStore
	}
	return nil
}
