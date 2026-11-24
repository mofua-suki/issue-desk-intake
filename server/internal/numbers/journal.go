package numbers

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
)

func (s *Store) read() ([]Entry, int, error) {
	file, err := os.Open(s.Journal)
	if errors.Is(err, os.ErrNotExist) {
		return []Entry{}, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	entries := []Entry{}
	invalid := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var e Entry
		if json.Unmarshal(line, &e) != nil {
			invalid++
			continue
		}
		switch {
		case valid(e.Number) && e.Project != "" && valid(e.Gitea):
		case len(e.Rename) == 2 && e.Rename[0] != "" && e.Rename[1] != "":
		case valid(e.Next):
		default:
			invalid++
			continue
		}
		entries = append(entries, e)
	}
	return entries, invalid, scanner.Err()
}
func (s *Store) append(entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}
	file, err := os.OpenFile(s.Journal, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	prefix := []byte{}
	if info.Size() > 0 {
		last := make([]byte, 1)
		if _, err = file.ReadAt(last, info.Size()-1); err != nil {
			return err
		}
		if last[0] != '\n' {
			prefix = append(prefix, '\n')
		}
	}
	for _, entry := range entries {
		raw, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		prefix = append(prefix, raw...)
		prefix = append(prefix, '\n')
	}
	for len(prefix) > 0 {
		n, err := file.Write(prefix)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		prefix = prefix[n:]
	}
	return file.Sync()
}
