package fleet

import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
)

type Host struct {
	Role             string
	TestRepositories []string
	Max              int
	Repositories     map[string]RepositoryRank
}
type RepositoryRank struct{ Rank, Max int }
type Assignments struct {
	RepositoryGroups   [][]string
	MonitorHost        string
	ReleaseCanaryHosts []string
	Hosts              map[string]*Host
	Implementers       map[string][]string
	DeployHosts        map[string]string
	Output             map[string]any
	// Captured before Output is filled with the shared defaults.
	scopeLimitFiles int
}

func integer(value any, min, max int) bool {
	v, ok := value.(float64)
	return ok && !math.IsNaN(v) && !math.IsInf(v, 0) && v == math.Trunc(v) && v >= float64(min) && v <= float64(max)
}
func known(value map[string]any, keys ...string) bool {
	for key := range value {
		found := false
		for _, candidate := range keys {
			if key == candidate {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func invalid() error { return errors.New("担当表の形式・順位・上限が不正です") }

func NormalizeAssignments(raw map[string]any) (*Assignments, error) {
	if err := checkAssignmentsShape(raw); err != nil {
		return nil, err
	}
	a := &Assignments{Hosts: map[string]*Host{}, Implementers: map[string][]string{}, DeployHosts: map[string]string{}}
	var err error
	a.RepositoryGroups, err = normalizeRepositoryGroups(raw)
	if err != nil {
		return nil, err
	}
	if value := Object(raw["intakeSplit"])["files"]; integer(value, 1, 9007199254740991) {
		a.scopeLimitFiles = int(Number(value))
	}
	if err := a.mergeOutputSettings(raw); err != nil {
		return nil, err
	}
	entries := Object(raw["hosts"])
	if entries == nil {
		return nil, invalid()
	}
	hosts, err := a.normalizeHosts(entries)
	if err != nil {
		return nil, err
	}
	if err := a.normalizeMonitorHosts(raw); err != nil {
		return nil, err
	}
	if err := a.rankImplementers(hosts.groups); err != nil {
		return nil, err
	}
	canary, readers, err := a.normalizeRepositorySettings(raw)
	if err != nil {
		return nil, err
	}
	representatives := a.representatives()
	accounts, err := a.normalizeWorkerAccounts(raw)
	if err != nil {
		return nil, err
	}
	if err := a.checkRetiredHosts(); err != nil {
		return nil, err
	}
	if err := a.normalizeDispatch(); err != nil {
		return nil, err
	}
	if err := a.checkRunLimits(); err != nil {
		return nil, err
	}
	if err := a.writeHostOutput(raw, entries, hosts, representatives, readers, canary, accounts); err != nil {
		return nil, err
	}
	a.Output["testBudget"] = testBudgetOutput(raw)
	if err := a.normalizeAntigravityRouting(raw); err != nil {
		return nil, err
	}
	return a, nil
}

// checkAssignmentsShape rejects retired and malformed top-level settings.
func checkAssignmentsShape(raw map[string]any) error {
	if raw == nil {
		return invalid()
	}
	if _, exists := raw["testPlacement"]; exists {
		return errors.New("現行WorkerはtestPlacementを実装していません")
	}
	for _, key := range strings.Fields("default claim stall followupIdleMinutes selfDeployWaitMinutes scopeWaitAlertMinutes interruptDelayMinutes improvementBatch improvementShare projectDependencies") {
		if _, exists := raw[key]; exists {
			return invalid()
		}
	}
	if !integer(raw["pollSeconds"], 30, 300) {
		return invalid()
	}
	if value, exists := raw["repositories"]; exists && Object(value) == nil {
		return invalid()
	}
	if value, exists := raw["models"]; exists && Object(value) == nil {
		return invalid()
	}
	for _, key := range []string{"retiredHosts"} {
		if value, exists := raw[key]; exists {
			if _, ok := value.([]any); !ok {
				return invalid()
			}
		}
	}
	return nil
}

// mergeOutputSettings starts the output from the shared defaults and overlays
// the settings the file gives.
func (a *Assignments) mergeOutputSettings(raw map[string]any) error {
	if err := json.Unmarshal(defaultsJSON, &a.Output); err != nil {
		return err
	}
	for key, value := range raw {
		if _, exists := a.Output[key]; exists {
			if previous := Object(a.Output[key]); previous != nil {
				if Object(value) == nil {
					return invalid()
				}
				for k, v := range Object(value) {
					previous[k] = Clone(v)
				}
			} else {
				a.Output[key] = Clone(value)
			}
		}
	}
	return nil
}

type hostRank struct {
	host string
	rank int
}

// normalizedHosts holds what the host entries contribute beyond Assignments.Hosts.
type normalizedHosts struct {
	groups                 map[string][]hostRank
	hostLimits, repoLimits map[string]any
}

// normalizeHosts validates each host entry and collects the implementation
// rank of every repository.
func (a *Assignments) normalizeHosts(entries map[string]any) (normalizedHosts, error) {
	hosts := normalizedHosts{groups: map[string][]hostRank{}, hostLimits: map[string]any{}, repoLimits: map[string]any{}}
	machines := map[string]bool{}
	repoCase := map[string]string{}
	for _, name := range Keys(entries) {
		entry := Object(entries[name])
		host, err := normalizeHost(name, entry, entries, machines)
		if err != nil {
			return hosts, err
		}
		hosts.hostLimits[name] = host.Max
		if entry["repositories"] != nil && Object(entry["repositories"]) == nil {
			return hosts, invalid()
		}
		if err := hosts.addRepositories(name, host, Object(entry["repositories"]), repoCase); err != nil {
			return hosts, err
		}
		a.Hosts[name] = host
	}
	return hosts, nil
}

// normalizeHost validates one host's own settings: limit, machine name, role and test repositories.
func normalizeHost(name string, entry, entries map[string]any, machines map[string]bool) (*Host, error) {
	if !ValidHost(name) || entry == nil || !known(entry, "maxImplementations", "repositories", "machineName", "role", "testRepositories") || !integer(entry["maxImplementations"], 1, 8) {
		return nil, invalid()
	}
	if value, exists := entry["machineName"]; exists {
		machine := Text(value)
		if !ValidHost(machine) || machine == name || entries[machine] != nil || machines[machine] {
			return nil, invalid()
		}
		machines[machine] = true
	}
	host := &Host{Max: int(Number(entry["maxImplementations"])), Repositories: map[string]RepositoryRank{}}
	host.Role = Text(entry["role"])
	if host.Role == "" {
		host.Role = "implementation"
	}
	if host.Role != "implementation" && host.Role != "test-only" {
		return nil, invalid()
	}
	if value, exists := entry["testRepositories"]; exists {
		list, ok := value.([]any)
		if !ok {
			return nil, invalid()
		}
		seen := map[string]bool{}
		for _, value := range list {
			repo := Text(value)
			if !ValidRepo(repo) || seen[repo] {
				return nil, invalid()
			}
			seen[repo] = true
			host.TestRepositories = append(host.TestRepositories, repo)
		}
	}
	if host.Role == "test-only" && (len(Object(entry["repositories"])) != 0 || len(host.TestRepositories) == 0) {
		return nil, invalid()
	}
	return host, nil
}

// addRepositories validates the host's repository ranks and limits, and records them.
func (hosts normalizedHosts) addRepositories(name string, host *Host, repositories map[string]any, repoCase map[string]string) error {
	for repo, value := range repositories {
		rank := Object(value)
		if !ValidRepo(repo) || rank == nil || !known(rank, "rank", "max") || !integer(rank["rank"], 1, math.MaxInt32) {
			return invalid()
		}
		max := host.Max
		if value, exists := rank["max"]; exists {
			if !integer(value, 1, host.Max) {
				return invalid()
			}
			max = int(Number(value))
			if hosts.repoLimits[name] == nil {
				hosts.repoLimits[name] = map[string]any{}
			}
			Object(hosts.repoLimits[name])[repo] = max
		}
		lower := strings.ToLower(repo)
		if previous, exists := repoCase[lower]; exists && previous != repo {
			return invalid()
		}
		repoCase[lower] = repo
		host.Repositories[repo] = RepositoryRank{Rank: int(Number(rank["rank"])), Max: max}
		hosts.groups[repo] = append(hosts.groups[repo], hostRank{name, int(Number(rank["rank"]))})
	}
	return nil
}

// normalizeMonitorHosts validates the monitor and release canary hosts.
func (a *Assignments) normalizeMonitorHosts(raw map[string]any) error {
	if value, exists := raw["monitorHost"]; exists {
		host, ok := value.(string)
		if !ok || !ValidHost(host) || a.Hosts[host] == nil {
			return invalid()
		}
		a.MonitorHost = host
	}
	if value, exists := raw["releaseCanaryHosts"]; exists {
		list, ok := value.([]any)
		if !ok || len(list) == 0 {
			return invalid()
		}
		seen := map[string]bool{}
		for _, item := range list {
			host, ok := item.(string)
			if !ok || !ValidHost(host) || a.Hosts[host] == nil || seen[host] {
				return invalid()
			}
			seen[host] = true
			a.ReleaseCanaryHosts = append(a.ReleaseCanaryHosts, host)
		}
	}
	return nil
}

// rankImplementers orders each repository's implementers by rank and rejects equal ranks.
func (a *Assignments) rankImplementers(groups map[string][]hostRank) error {
	for repo, group := range groups {
		sort.Slice(group, func(i, j int) bool { return group[i].rank < group[j].rank })
		for i, item := range group {
			if i > 0 && group[i-1].rank == item.rank {
				return invalid()
			}
			a.Implementers[repo] = append(a.Implementers[repo], item.host)
		}
	}
	return nil
}

// normalizeRepositorySettings validates the per-repository deploy, canary and reader hosts.
func (a *Assignments) normalizeRepositorySettings(raw map[string]any) (canary, readers map[string]any, _ error) {
	canary, readers = map[string]any{}, map[string]any{}
	for repo, value := range Object(raw["repositories"]) {
		entry := Object(value)
		if !ValidRepo(repo) || entry == nil || !known(entry, "deployHost", "canaryHost", "readerHosts") || len(a.Implementers[repo]) == 0 {
			return nil, nil, invalid()
		}
		if value, exists := entry["deployHost"]; exists {
			host := Text(value)
			if a.Hosts[host] == nil {
				return nil, nil, invalid()
			}
			a.DeployHosts[repo] = host
		}
		if value, exists := entry["canaryHost"]; exists {
			host := Text(value)
			if !contains(a.Implementers[repo], host) {
				return nil, nil, invalid()
			}
			canary[repo] = host
		}
		if value, exists := entry["readerHosts"]; exists {
			list := Strings(value)
			seen := map[string]bool{}
			if len(list) == 0 || len(list) != len(List(value)) || a.DeployHosts[repo] == "" {
				return nil, nil, invalid()
			}
			for _, host := range list {
				if a.Hosts[host] == nil || host == a.DeployHosts[repo] || seen[host] {
					return nil, nil, invalid()
				}
				seen[host] = true
			}
			readers[repo] = list
		}
	}
	return canary, readers, nil
}

// representatives names the host that represents each repository: its deploy
// host, or else its first implementer.
func (a *Assignments) representatives() map[string]any {
	representatives := map[string]any{}
	for repo, hosts := range a.Implementers {
		representative := a.DeployHosts[repo]
		if representative == "" {
			representative = hosts[0]
		}
		representatives[repo] = representative
	}
	return representatives
}

// normalizeWorkerAccounts validates the worker account of each host.
func (a *Assignments) normalizeWorkerAccounts(raw map[string]any) (map[string]any, error) {
	accounts := Object(raw["workerAccounts"])
	if accounts == nil {
		accounts = map[string]any{}
	}
	for host, value := range accounts {
		if a.Hosts[host] == nil || Text(value) == "" {
			return nil, invalid()
		}
	}
	return accounts, nil
}

// checkRetiredHosts rejects a retired host that still has work assigned.
func (a *Assignments) checkRetiredHosts() error {
	for _, host := range Strings(a.Output["retiredHosts"]) {
		if a.Hosts[host] == nil || len(a.Hosts[host].Repositories) > 0 || a.Output["usageReviewHost"] == host {
			return invalid()
		}
	}
	return nil
}

// normalizeDispatch validates the dispatch settings and fills the load limit defaults.
func (a *Assignments) normalizeDispatch() error {
	dispatch := Object(a.Output["dispatch"])
	if !known(dispatch, "leaseMinutes", "preferenceSeconds", "conflictRestartLines", "loadLimit", "scopeExemptPaths", "scopeExemptRepositories") || !integer(dispatch["leaseMinutes"], 1, 120) || !integer(dispatch["preferenceSeconds"], 1, 300) || !integer(dispatch["conflictRestartLines"], 0, math.MaxInt32) {
		return invalid()
	}
	if err := validateExemptions(dispatch); err != nil {
		return err
	}
	load := Object(dispatch["loadLimit"])
	defaults := map[string]any{"ratio": float64(1), "cpuPercent": float64(90), "minutes": float64(10)}
	for key, value := range load {
		defaults[key] = value
	}
	dispatch["loadLimit"] = defaults
	if !known(defaults, "ratio", "cpuPercent", "minutes") || Number(defaults["ratio"]) <= 0 || Number(defaults["cpuPercent"]) < 0 || Number(defaults["cpuPercent"]) > 100 || !integer(defaults["minutes"], 1, 180) {
		return invalid()
	}
	if _, ok := defaults["ratio"].(float64); !ok {
		return invalid()
	}
	if _, ok := defaults["cpuPercent"].(float64); !ok {
		return invalid()
	}
	return nil
}

// checkRunLimits validates the review, implementation and repair round limits.
func (a *Assignments) checkRunLimits() error {
	for _, key := range []string{"reviewRoundLimit", "implementationRunLimit", "sizeFixRounds", "prSizeFixRounds"} {
		if !integer(a.Output[key], 1, 100) {
			return invalid()
		}
	}
	if !integer(a.Output["fixedTestRepairRounds"], 0, 100) || !integer(a.Output["implementationUsageLimit"], 1, 9007199254740991) {
		return invalid()
	}
	return nil
}

// writeHostOutput publishes the normalized hosts, repositories and models in the output.
func (a *Assignments) writeHostOutput(raw, entries map[string]any, hosts normalizedHosts, representatives, readers, canary, accounts map[string]any) error {
	roles, tests := map[string]any{}, map[string]any{}
	for name, host := range a.Hosts {
		roles[name] = host.Role
		tests[name] = host.TestRepositories
	}
	a.Output["hostRoles"], a.Output["testRepositories"] = roles, tests
	a.Output["repositoryGroups"] = a.RepositoryGroups
	a.Output["hosts"], a.Output["hostLimits"], a.Output["hostRepositoryLimits"] = Keys(entries), hosts.hostLimits, hosts.repoLimits
	a.Output["repositories"], a.Output["implementers"], a.Output["deployHosts"] = representatives, a.Implementers, a.DeployHosts
	a.Output["readerHosts"], a.Output["canaryHosts"], a.Output["workerAccounts"], a.Output["pollSeconds"] = readers, canary, accounts, raw["pollSeconds"]
	a.Output["models"], a.Output["modelsDigest"] = raw["models"], nil
	if raw["models"] != nil {
		if Object(raw["models"]) == nil {
			return invalid()
		}
		normalized, err := normalizeModels(raw["models"])
		if err != nil {
			return err
		}
		a.Output["models"] = normalized
		a.Output["modelsDigest"] = Digest(normalized)[:12]
	}
	return nil
}

// testBudgetOutput overlays the file's test budget on its defaults.
func testBudgetOutput(raw map[string]any) map[string]any {
	testBudget := map[string]any{"spawns": float64(400), "seconds": float64(60), "increasePercent": float64(30), "repositories": map[string]any{}}
	for key, value := range Object(raw["testBudget"]) {
		testBudget[key] = value
	}
	return testBudget
}

// normalizeAntigravityRouting validates and publishes the Antigravity routing setting.
func (a *Assignments) normalizeAntigravityRouting(raw map[string]any) error {
	if value, exists := raw["antigravityRouting"]; exists {
		if value != false {
			object := Object(value)
			if object == nil || !known(object, "maxFiles", "repositories") || !integer(object["maxFiles"], 1, 9007199254740991) {
				return invalid()
			}
			if _, ok := object["repositories"].([]any); !ok {
				return invalid()
			}
			for _, repo := range List(object["repositories"]) {
				if Text(repo) == "" {
					return invalid()
				}
			}
		}
		a.Output["antigravityRouting"] = Clone(value)
	}
	return nil
}

func (a *Assignments) Repositories(host string) []string {
	entry := a.Hosts[host]
	if entry == nil {
		return nil
	}
	repos := make([]string, 0, len(entry.Repositories))
	for repo := range entry.Repositories {
		repos = append(repos, repo)
	}
	sort.Slice(repos, func(i, j int) bool {
		x, y := entry.Repositories[repos[i]], entry.Repositories[repos[j]]
		if x.Rank != y.Rank {
			return x.Rank < y.Rank
		}
		return repos[i] < repos[j]
	})
	return repos
}
func (a *Assignments) Exempt(repo string) []string {
	dispatch := Object(a.Output["dispatch"])
	paths := Strings(dispatch["scopeExemptPaths"])
	extra := Object(dispatch["scopeExemptRepositories"])
	for name, value := range extra {
		if strings.EqualFold(name, repo) {
			paths = append(paths, Strings(value)...)
		}
	}
	return paths
}
func (a *Assignments) RepoLimit(host, repo string) int {
	entry := a.Hosts[host]
	if entry == nil {
		return 1
	}
	for name, rank := range entry.Repositories {
		if strings.EqualFold(name, repo) {
			return rank.Max
		}
	}
	return entry.Max
}
