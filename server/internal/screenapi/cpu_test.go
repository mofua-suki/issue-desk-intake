package screenapi

import (
	"context"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func cpuFixture() []byte {
	raw, _ := json.Marshal(Object{"nodes": []any{Object{"id": 1, "callFrame": Object{"functionName": "(root)", "url": "", "lineNumber": -1}, "children": []int{2}}, Object{"id": 2, "callFrame": Object{"functionName": "work", "url": "file:///repo/server/src/task.ts", "lineNumber": 2, "columnNumber": 0, "scriptId": "1"}, "children": []int{3}}, Object{"id": 3, "callFrame": Object{"functionName": "work", "url": "file:///repo/server/src/task.ts", "lineNumber": 2, "columnNumber": 0, "scriptId": "1"}}}, "samples": []int{2, 3, 999}, "timeDeltas": []int{1000, 2000, 4000}})
	return raw
}
func TestCPUSummaryRecursionMissingSamplesAndLocationRedaction(t *testing.T) {
	result, e := SummarizeCPU(cpuFixture(), "/repo")
	if e != nil {
		t.Fatal(e)
	}
	if result["samples"] != 2 || result["missingSamples"] != 1 || result["observedMs"] != float64(3) || result["incomplete"] != true {
		t.Fatal(result)
	}
	var self []cpuRow
	encoded, _ := json.Marshal(result["self"])
	if e = json.Unmarshal(encoded, &self); e != nil {
		t.Fatal(e)
	}
	if self[0].FunctionName != "work" || self[0].SelfMS == nil || *self[0].SelfMS != 3 || self[0].TotalMS == nil || *self[0].TotalMS != 3 || self[0].File != "server/src/task.ts" {
		t.Fatal("recursive total double counted", self)
	}
	if file, category := profileLocation("file:///home/secret/token.txt", "/repo"); file != "[external]" || category != "external" {
		t.Fatal("external file exposed", file)
	}
	if displayName("token=private-value") != "[redacted]" {
		t.Fatal("secret function name exposed")
	}
	bad := []byte(`{"nodes":[{"id":1,"callFrame":{},"children":[2]},{"id":2,"callFrame":{},"children":[1]}]}`)
	if _, e = SummarizeCPU(bad, "/repo"); e == nil {
		t.Fatal("profile cycle accepted")
	}
}
func TestCPUFilesRejectTraversalSymlinksAndOversize(t *testing.T) {
	directory := t.TempDir()
	id := "intake-cpu-20261101T000000Z"
	name := id + ".cpuprofile"
	if e := os.WriteFile(filepath.Join(directory, name), cpuFixture(), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(directory, name+".meta.json"), []byte(`{"startedAt":"2026-11-01T00:00:00Z","endedAt":"2026-11-01T00:01:00Z"}`), 0600); e != nil {
		t.Fatal(e)
	}
	profiles := NewCPUProfiles(directory, "/repo")
	value, e := profiles.List(context.Background())
	if e != nil || len(fleet.Items(value["profiles"])) != 1 || fleet.ObjectOrEmpty(fleet.Items(value["profiles"])[0])["intervalSource"] != "capture-record" {
		t.Fatal(value, e)
	}
	if _, e = profiles.Detail(context.Background(), id); e != nil {
		t.Fatal(e)
	}
	if _, e = profiles.Detail(context.Background(), "../"+id); e == nil {
		t.Fatal("traversal accepted")
	}
	outside := filepath.Join(t.TempDir(), "private.cpuprofile")
	if e = os.WriteFile(outside, cpuFixture(), 0600); e != nil {
		t.Fatal(e)
	}
	linkedID := "intake-cpu-20261101T000001Z"
	if e = os.Symlink(outside, filepath.Join(directory, linkedID+".cpuprofile")); e != nil {
		t.Fatal(e)
	}
	if _, e = profiles.Detail(context.Background(), linkedID); e == nil {
		t.Fatal("symlink read")
	}
	rootLink := filepath.Join(t.TempDir(), "runtime")
	if e = os.Symlink(directory, rootLink); e != nil {
		t.Fatal(e)
	}
	if _, e = NewCPUProfiles(rootLink, "/repo").List(context.Background()); e == nil {
		t.Fatal("symlink root read")
	}
	hugeID := "intake-cpu-20261101T000002Z"
	f, e := os.Create(filepath.Join(directory, hugeID+".cpuprofile"))
	if e != nil {
		t.Fatal(e)
	}
	if e = f.Truncate((20 << 20) + 1); e != nil {
		t.Fatal(e)
	}
	f.Close()
	if _, e = profiles.Detail(context.Background(), hugeID); e == nil || !strings.Contains(e.Error(), "大きすぎ") {
		t.Fatal("oversize accepted", e)
	}
}
