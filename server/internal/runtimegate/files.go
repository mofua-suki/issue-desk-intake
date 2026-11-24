package runtimegate

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

func (g *Gate) directory() string { return filepath.Join(g.options.RuntimeDir, "state") }
func (g *Gate) prepare() error {
	if err := os.MkdirAll(g.directory(), 0700); err != nil {
		return err
	}
	return g.removeArtifacts(true)
}
func (g *Gate) removeArtifacts(request bool) error {
	names := []string{"intake-drain.held", "intake-drain.status"}
	if request {
		names = append(names, "intake-drain.request")
	}
	for _, name := range names {
		if err := os.Remove(filepath.Join(g.directory(), name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("入れ替えの古い印を解除できません")
		}
	}
	return nil
}
func (g *Gate) write(name string, data []byte) error {
	root, err := os.OpenRoot(g.directory())
	if err != nil {
		return err
	}
	defer root.Close()
	var random [12]byte
	if _, err = rand.Read(random[:]); err != nil {
		return err
	}
	temporary := ".intake-" + hex.EncodeToString(random[:]) + ".tmp"
	f, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temporary)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return root.Rename(temporary, name)
}
func (g *Gate) writeHeartbeat(at int64, busy bool) error {
	var revision any
	if regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(g.options.Revision) {
		revision = g.options.Revision
	}
	value := struct {
		Time     int64 `json:"time"`
		PID      int   `json:"pid"`
		Revision any   `json:"revision"`
		Busy     bool  `json:"busy"`
	}{at, os.Getpid(), revision, busy}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return g.write("intake-heartbeat.json", data)
}
func (g *Gate) request(now int64) int64 {
	f, err := os.Open(filepath.Join(g.directory(), "intake-drain.request"))
	if err != nil {
		return 0
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return 0
	}
	data, err := io.ReadAll(io.LimitReader(f, 65))
	if err != nil || len(data) > 64 {
		return 0
	}
	text := strings.TrimSpace(string(data))
	if !regexp.MustCompile(`^[1-9][0-9]{0,14}$`).MatchString(text) {
		return 0
	}
	until, err := strconv.ParseInt(text, 10, 64)
	if err != nil || until <= now || until-now > 15*60*1000 {
		return 0
	}
	return until
}

func (g *Gate) Tick() error {
	g.ioMu.Lock()
	defer g.ioMu.Unlock()
	now := g.options.Now().UnixMilli()
	until := g.request(now)
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}
	if g.until != until {
		g.until = until
		g.notify()
	}
	ai, filing, project := g.active["ai"], g.active["filing"], g.active["project"]
	busy := ai+filing+project > 0
	phase := "pending"
	if !busy {
		phase = "held"
	}
	g.lastTime = now
	g.mu.Unlock()
	if err := g.writeHeartbeat(now, busy); err != nil {
		return err
	}
	if until == 0 {
		return g.removeArtifacts(false)
	}
	status, _ := json.Marshal(map[string]any{"until": until, "phase": phase, "ai": ai, "filing": filing, "project": project})
	if err := g.write("intake-drain.status", status); err != nil {
		return err
	}
	if busy {
		if err := os.Remove(filepath.Join(g.directory(), "intake-drain.held")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return g.write("intake-drain.held", []byte(strconv.FormatInt(until, 10)+"\n"))
}
