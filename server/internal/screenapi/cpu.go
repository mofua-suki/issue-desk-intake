package screenapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"issue-desk-intake/server/internal/fleet"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

var profileID = regexp.MustCompile(`^intake-cpu-[0-9]{8}T[0-9]{6}Z(?:-[a-f0-9]{8,})?$`)

type CPUProfiles struct {
	directory, repository string
	waiters               chan struct{}
	serial                chan struct{}
}

func NewCPUProfiles(directory, repository string) *CPUProfiles {
	return &CPUProfiles{directory: directory, repository: repository, waiters: make(chan struct{}, 8), serial: make(chan struct{}, 1)}
}
func (c *CPUProfiles) root() (*os.Root, error) {
	before, e := os.Lstat(c.directory)
	if e != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, fail(503, "プロファイルの保存先を確認できません。")
	}
	root, e := os.OpenRoot(c.directory)
	if e != nil {
		return nil, e
	}
	after, e := root.Stat(".")
	if e != nil || !os.SameFile(before, after) {
		root.Close()
		return nil, fail(503, "プロファイルの保存先が変更されました。")
	}
	return root, nil
}
func readBounded(root *os.Root, name string, max int64) ([]byte, error) {
	file, e := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		if errors.Is(e, os.ErrNotExist) {
			return nil, fail(404, "プロファイルがありません。")
		}
		return nil, fail(400, "プロファイルのファイルが不正です。")
	}
	defer file.Close()
	stat, e := file.Stat()
	if e != nil || !stat.Mode().IsRegular() {
		return nil, fail(400, "プロファイルのファイルが不正です。")
	}
	if stat.Size() > max {
		return nil, fail(413, "プロファイルが大きすぎます。")
	}
	raw, e := io.ReadAll(io.LimitReader(file, max+1))
	if e != nil {
		return nil, e
	}
	after, e := file.Stat()
	if e != nil || int64(len(raw)) > max || after.Size() > max {
		return nil, fail(413, "プロファイルが大きすぎます。")
	}
	return raw, nil
}
func (c *CPUProfiles) List(ctx context.Context) (Object, error) {
	root, e := c.root()
	if e != nil {
		return nil, e
	}
	defer root.Close()
	directory, e := root.Open(".")
	if e != nil {
		return nil, e
	}
	defer directory.Close()
	names, e := directory.Readdirnames(-1)
	if e != nil {
		return nil, e
	}
	valid := []string{}
	for _, name := range names {
		if strings.HasSuffix(name, ".cpuprofile") && profileID.MatchString(strings.TrimSuffix(name, ".cpuprofile")) {
			valid = append(valid, name)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(valid)))
	if len(valid) > 5 {
		valid = valid[:5]
	}
	out := []any{}
	for _, name := range valid {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		id := strings.TrimSuffix(name, ".cpuprofile")
		stamp := strings.TrimPrefix(id, "intake-cpu-")[:16]
		started, e := time.Parse("20060102T150405Z", stamp)
		if e != nil {
			continue
		}
		row := Object{"id": id, "startedAt": started.UTC().Format(time.RFC3339Nano), "endedAt": nil, "intervalSource": "filename-estimate-end-missing", "state": "saved", "stopIntervals": []any{}}
		stat, e := root.Lstat(name)
		if e != nil || !stat.Mode().IsRegular() {
			row["state"] = "unavailable"
		}
		if raw, e := readBounded(root, name+".meta.json", 64<<10); e == nil {
			meta := Object{}
			if json.Unmarshal(raw, &meta) == nil {
				from, fe := time.Parse(time.RFC3339Nano, fleet.Text(meta["startedAt"]))
				to, te := time.Parse(time.RFC3339Nano, fleet.Text(meta["endedAt"]))
				if fe == nil && te == nil && !to.Before(from) {
					row["startedAt"], row["endedAt"], row["intervalSource"] = from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano), "capture-record"
				}
			}
		}
		out = append(out, row)
	}
	return Object{"profiles": out}, nil
}
func (c *CPUProfiles) Detail(ctx context.Context, id string) (Object, error) {
	if !profileID.MatchString(id) {
		return nil, fail(400, "プロファイルの識別子が不正です。")
	}
	select {
	case c.waiters <- struct{}{}:
		defer func() { <-c.waiters }()
	default:
		return nil, fail(503, "プロファイルの集計待ちが上限です。")
	}
	select {
	case c.serial <- struct{}{}:
		defer func() { <-c.serial }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	root, e := c.root()
	if e != nil {
		return nil, e
	}
	defer root.Close()
	raw, e := readBounded(root, id+".cpuprofile", 20<<20)
	if e != nil {
		return nil, e
	}
	value, e := SummarizeCPU(raw, c.repository)
	if e != nil {
		return nil, e
	}
	value["id"] = id
	return value, nil
}
func profileLocation(value, root string) (string, string) {
	if regexp.MustCompile(`[A-Za-z0-9_-]{33,}`).MatchString(value) {
		return "[external]", "external"
	}
	if value == "" {
		return "[native]", "native"
	}
	if regexp.MustCompile(`^node:[a-zA-Z0-9_./-]+$`).MatchString(value) && len(value) <= 200 {
		return value, "node"
	}
	file := value
	if strings.HasPrefix(file, "file:") {
		u, e := url.Parse(file)
		if e != nil || u.Host != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return "[external]", "external"
		}
		file = u.Path
	} else if strings.ContainsAny(file, "?#") || regexp.MustCompile(`^[a-z][a-z0-9+.-]*:`).MatchString(file) {
		return "[external]", "external"
	}
	if !filepath.IsAbs(file) {
		file = filepath.Join(root, file)
	}
	relative, e := filepath.Rel(root, file)
	relative = filepath.ToSlash(relative)
	if e != nil || strings.HasPrefix(relative, "../") || relative == ".." || !regexp.MustCompile(`^(?:server|client|shared|scripts|test|test-support)/[a-zA-Z0-9_./-]+\.(?:mjs|cjs|js|ts|tsx|jsx|go)$`).MatchString(relative) || len(relative) > 200 {
		return "[external]", "external"
	}
	return relative, "repository"
}
