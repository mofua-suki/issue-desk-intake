// Codex の rollout ファイルを会話IDから探し、行の上限を守って1行ずつ読む。

package ai

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func rolloutPath(id string) string {
	if !sessionID.MatchString(id) {
		return ""
	}
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = filepath.Join(h, ".codex")
	}
	var paths []string
	_ = filepath.WalkDir(filepath.Join(home, "sessions"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), "-"+id+".jsonl") {
			paths = append(paths, path)
		}
		return nil
	})
	if len(paths) != 1 {
		return ""
	}
	return paths[0]
}

// Twelve 2 MiB attachments encoded in one JSON line fit within this limit,
// with room for the prompt and metadata. Reject larger records rather than
// allowing a damaged or untrusted rollout to exhaust the server's memory.
const maxRolloutLineBytes = 64 << 20

var errRolloutLineTooLarge = errors.New("rollout line too large")

func readRolloutLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(part) > maxRolloutLineBytes-len(line) {
			return nil, errRolloutLineTooLarge
		}
		line = append(line, part...)
		if err != bufio.ErrBufferFull {
			return line, err
		}
	}
}

// scanRollout rejects malformed JSON, including incomplete final records.
// Its reader is bounded to a statted size, so a concurrent writer cannot extend
// the observation indefinitely.
func scanRollout(reader io.Reader, visit func(map[string]any) bool) bool {
	lines := bufio.NewReader(reader)
	for {
		line, err := readRolloutLine(lines)
		if err == io.EOF && len(line) == 0 {
			return true
		}
		if err != nil && err != io.EOF {
			return false
		}
		d := json.NewDecoder(bytes.NewReader(line))
		d.UseNumber()
		var row map[string]any
		var extra any
		if d.Decode(&row) != nil || row == nil || d.Decode(&extra) != io.EOF || !visit(row) {
			return false
		}
		if err == io.EOF {
			return true
		}
	}
}

func openRollout(path string) (*os.File, os.FileInfo) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() == 0 {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	info, err := f.Stat()
	if err != nil || !os.SameFile(before, info) {
		f.Close()
		return nil, nil
	}
	return f, info
}
