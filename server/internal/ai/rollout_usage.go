// rollout の使用量の計測（開始時のファイルの指紋と累計を記録し、終了時に追記分だけから増えたトークン数と文脈の大きさを数える）。

package ai

import (
	"crypto/sha256"
	"io"
	"os"
	"time"
)

// A snapshot retains only counters and a fingerprint, never conversation text.
// The byte boundary and inode distinguish appends from replaced/rewritten logs.
type rolloutSnapshot struct {
	path       string
	file       os.FileInfo
	digest     [sha256.Size]byte
	total      [3]int64
	at         time.Time
	terminated bool
}

type rolloutUsage struct {
	total        [3]int64
	contextMax   *int64
	contextTotal *int64
}

func rolloutCounters(v any) ([3]int64, bool) {
	var result [3]int64
	m := object(v)
	for i, key := range []string{"input_tokens", "cached_input_tokens", "output_tokens"} {
		n := count(m[key])
		if n == nil {
			return result, false
		}
		result[i] = *n
	}
	return result, true
}

func snapshotRollout(id string) *rolloutSnapshot {
	path := rolloutPath(id)
	f, info := openRollout(path)
	if f == nil {
		return nil
	}
	defer f.Close()
	captured := time.Now()
	snapshot := &rolloutSnapshot{path: path, file: info}
	var last [1]byte
	if _, err := f.ReadAt(last[:], info.Size()-1); err != nil {
		return nil
	}
	snapshot.terminated = last[0] == '\n'
	hash := sha256.New()
	identity, measured := false, false
	valid := scanRollout(io.TeeReader(io.LimitReader(f, info.Size()), hash), func(row map[string]any) bool {
		p := object(row["payload"])
		if row["type"] == "session_meta" {
			if identity || p["id"] != id {
				return false
			}
			identity = true
		}
		if row["type"] == "event_msg" && p["type"] == "token_count" && p["info"] != nil {
			var ok bool
			snapshot.total, ok = rolloutCounters(object(p["info"])["total_token_usage"])
			at, err := time.Parse(time.RFC3339Nano, stringValue(row["timestamp"]))
			if !ok || err != nil || at.After(captured) || at.Before(snapshot.at) {
				return false
			}
			snapshot.at, measured = at, true
		}
		return true
	})
	after, err := f.Stat()
	if !valid || !identity || !measured || err != nil || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return nil
	}
	copy(snapshot.digest[:], hash.Sum(nil))
	return snapshot
}

// appended checks that the file still begins with the snapshot and has grown
// after it, and returns the size of the appended part positioned for reading.
func (base *rolloutSnapshot) appended(f *os.File, info os.FileInfo) (int64, bool) {
	if !os.SameFile(base.file, info) || info.Size() <= base.file.Size() {
		return 0, false
	}
	hash := sha256.New()
	if _, err := io.CopyN(hash, f, base.file.Size()); err != nil || string(hash.Sum(nil)) != string(base.digest[:]) {
		return 0, false
	}
	remaining := info.Size() - base.file.Size()
	if !base.terminated {
		var separator [1]byte
		if _, err := io.ReadFull(f, separator[:]); err != nil || separator[0] != '\n' {
			return 0, false
		}
		remaining--
	}
	return remaining, true
}

func (base *rolloutSnapshot) finish(id string, started, ended time.Time) *rolloutUsage {
	if base == nil || rolloutPath(id) != base.path {
		return nil
	}
	f, info := openRollout(base.path)
	if f == nil {
		return nil
	}
	defer f.Close()
	remaining, ok := base.appended(f, info)
	if !ok {
		return nil
	}
	result := &rolloutUsage{total: base.total}
	var max, total int64
	contextKnown, measured := true, false
	lastAt := base.at
	turnID := ""
	// CLI timestamps may have millisecond precision; compare at that precision.
	valid := scanRollout(io.LimitReader(f, remaining), func(row map[string]any) bool {
		at, err := time.Parse(time.RFC3339Nano, stringValue(row["timestamp"]))
		if err != nil || at.Before(started.Truncate(time.Millisecond)) || at.After(ended) || at.Before(lastAt) || row["type"] == "session_meta" {
			return false
		}
		lastAt = at
		p := object(row["payload"])
		if row["type"] == "turn_context" {
			id := stringValue(p["turn_id"])
			if turnID != "" && id != turnID {
				return false
			}
			turnID = id
		}
		if row["type"] != "event_msg" || p["type"] != "token_count" || p["info"] == nil {
			return true
		}
		info := object(p["info"])
		next, ok := rolloutCounters(info["total_token_usage"])
		if !ok {
			return false
		}
		measured = true
		// Rate-limit updates can repeat a token measurement. Do not count its
		// last_token_usage again, including an echo of the baseline.
		if next != result.total {
			n := count(object(info["last_token_usage"])["input_tokens"])
			if n == nil || total > 9007199254740991-*n {
				contextKnown = false
			} else {
				total += *n
				if *n > max {
					max = *n
				}
			}
		}
		result.total = next
		return true
	})
	after, err := f.Stat()
	current, pathErr := os.Lstat(base.path)
	if !valid || !measured || err != nil || pathErr != nil || !os.SameFile(info, current) || !current.Mode().IsRegular() || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		return nil
	}
	if contextKnown && result.total != base.total {
		result.contextMax, result.contextTotal = &max, &total
	}
	return result
}
