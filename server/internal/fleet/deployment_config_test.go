package fleet

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

type workerReleaseConfig struct {
	Hosts map[string]string `json:"hosts"`
}

func readWorkerReleaseConfig(path string) (workerReleaseConfig, error) {
	var release workerReleaseConfig
	err := ReadJSON(path, &release)
	return release, err
}

func TestWorkerReleaseIncludesAllImplementationHosts(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	s := Service{FleetPath: filepath.Join(root, "fleet.json"), AssignmentsPath: filepath.Join(root, "assignments.json")}
	_, assignments, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	release, err := readWorkerReleaseConfig(filepath.Join(root, "deploy", "nas", "worker-release.json"))
	if err != nil {
		t.Fatal(err)
	}
	missing := map[string]bool{}
	for _, hosts := range assignments.Implementers {
		for _, host := range hosts {
			if _, ok := release.Hosts[host]; !ok {
				missing[host] = true
			}
		}
	}
	if len(missing) > 0 {
		hosts := make([]string, 0, len(missing))
		for host := range missing {
			hosts = append(hosts, host)
		}
		sort.Strings(hosts)
		t.Fatalf("Worker発行設定に実装PCがありません: %v", hosts)
	}
}

// release hosts include NAS as well as implementation hosts, so compare with
// all assigned hosts and independently reject hosts marked as retired.
func invalidWorkerReleaseHosts(hosts map[string]string, assignments *Assignments) (unknown, retired []string) {
	retiredHosts := map[string]bool{}
	for _, host := range Strings(assignments.Output["retiredHosts"]) {
		retiredHosts[host] = true
	}
	for host := range hosts {
		if assignments.Hosts[host] == nil {
			unknown = append(unknown, host)
		}
		if retiredHosts[host] {
			retired = append(retired, host)
		}
	}
	sort.Strings(unknown)
	sort.Strings(retired)
	return
}

func TestWorkerReleaseHostsAreActiveAssignments(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	s := Service{FleetPath: filepath.Join(root, "fleet.json"), AssignmentsPath: filepath.Join(root, "assignments.json")}
	_, assignments, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	release, err := readWorkerReleaseConfig(filepath.Join(root, "deploy", "nas", "worker-release.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(release.Hosts) == 0 {
		t.Fatal("Worker発行設定にホストがありません")
	}
	unknown, retired := invalidWorkerReleaseHosts(release.Hosts, assignments)
	if len(unknown) != 0 || len(retired) != 0 {
		t.Fatalf("Worker発行設定に担当表にないホストまたは退役済みホストがあります: 未登録=%v 退役済み=%v", unknown, retired)
	}
}

func TestReadWorkerReleaseConfigWithBOM(t *testing.T) {
	path := filepath.Join("..", "..", "..", "deploy", "nas", "worker-release.json")
	want, err := readWorkerReleaseConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bomPath := filepath.Join(t.TempDir(), "worker-release.json")
	if err := os.WriteFile(bomPath, append([]byte{0xef, 0xbb, 0xbf}, data...), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readWorkerReleaseConfig(bomPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(want.Hosts) == 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("BOM付きのWorker発行設定が元の設定と一致しません: got=%v want=%v", got.Hosts, want.Hosts)
	}
}

func TestInvalidWorkerReleaseHosts(t *testing.T) {
	assignments := &Assignments{
		Hosts:  map[string]*Host{"active": {}, "retired": {}},
		Output: map[string]any{"retiredHosts": []any{"retired"}},
	}
	unknown, retired := invalidWorkerReleaseHosts(map[string]string{"active": "registry", "retired": "registry", "unknown": "registry"}, assignments)
	if !reflect.DeepEqual(unknown, []string{"unknown"}) || !reflect.DeepEqual(retired, []string{"retired"}) {
		t.Fatalf("不正な発行先の検出結果が違います: 未登録=%v 退役済み=%v", unknown, retired)
	}
}

func TestDistributionConfigurationKeepsImplementationOnAssignedHosts(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	s := Service{FleetPath: filepath.Join(root, "fleet.json"), AssignmentsPath: filepath.Join(root, "assignments.json")}
	document, assignments, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if assignments.MonitorHost != "gt12" || !reflect.DeepEqual(assignments.ReleaseCanaryHosts, []string{"gt12", "a9max", "x1-pro1", "x1-pro2"}) {
		t.Fatalf("監視とカナリアの設定が違います: %+v", assignments)
	}
	want := []string{"gt12", "nab-9s"}
	for _, repo := range []string{"mofua-suki/issue-desk-intake", "mofua-suki/issue-desk-worker"} {
		if got := assignments.Implementers[repo]; !reflect.DeepEqual(got, want) {
			t.Fatalf("%sの実装先が%vではありません: %v", repo, want, got)
		}
	}
	for i, host := range want[:2] {
		if assignments.Hosts[host].Max != 4 {
			t.Fatalf("%sのPC全体上限が4ではありません", host)
		}
		for _, repo := range []string{"mofua-suki/issue-desk-intake", "mofua-suki/issue-desk-worker"} {
			got := assignments.Hosts[host].Repositories[repo]
			limits := Object(Object(assignments.Output["hostRepositoryLimits"])[host])
			if _, exists := limits[repo]; got.Rank != i+2 || got.Max != 4 || exists {
				t.Fatalf("%s %s: rank=%d max指定=%v", host, repo, got.Rank, exists)
			}
		}
	}
	for _, host := range []string{"a9max", "x1-pro1", "x1-pro2"} {
		if assignments.Hosts[host].Max != 3 {
			t.Fatalf("%sのPC全体上限が変わりました", host)
		}
		for _, repo := range []string{"mofua-suki/issue-desk-intake", "mofua-suki/issue-desk-worker"} {
			if _, exists := assignments.Hosts[host].Repositories[repo]; exists {
				t.Fatalf("%sが%sの担当に残っています", host, repo)
			}
		}
		if _, ok := assignments.Hosts[host].Repositories["mofua-suki/demo-image-rating"]; !ok {
			t.Fatalf("%sの画像選別担当が失われました", host)
		}
	}
	if assignments.Hosts["nab-9s"].Max != assignments.Hosts["gt12"].Max {
		t.Fatal("nab-9sの同時実装上限がgt12と一致しません")
	}
	if !reflect.DeepEqual(document.Hosts["nab-9s"], document.Hosts["gt12"]) {
		t.Fatal("nab-9sの配布設定がgt12と一致しません")
	}
	for repo, hosts := range assignments.Implementers {
		for _, host := range hosts {
			found := false
			for _, raw := range List(document.Hosts[host]["repositories"]) {
				found = found || Text(Object(raw)["name"]) == repo
			}
			if !found {
				t.Fatalf("実装先%sに%sの配布設定がありません", host, repo)
			}
		}
	}
}
