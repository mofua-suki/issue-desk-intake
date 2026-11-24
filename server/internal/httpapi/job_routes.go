package httpapi

import (
	"encoding/json"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *WorkerRoutes) jobsRoute(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.identity(w, r, auth.Policy{PC: true})
	if !ok {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/jobs")
	var value any
	var err error
	if path == "" {
		value, err = s.listJobs(r)
	} else if path == "/next" {
		var outcome nextJobOutcome
		value, outcome, err = s.nextJob(r, principal)
		switch outcome {
		case nextJobAbandoned:
			return
		case nextJobNone:
			workerSend(w, nil, 204)
			return
		}
	} else {
		value, err = s.jobAction(r, principal.Name, path)
	}
	if err != nil {
		workerFailure(w, err)
		return
	}
	workerSend(w, value, 200)
}

// listJobs は状態とリポジトリで絞ったジョブの一覧をページに分けて返す。
func (s *WorkerRoutes) listJobs(r *http.Request) (any, error) {
	var value any
	err := workerMethod(r, "GET")
	if err == nil {
		statuses := []string{}
		if raw := r.URL.Query().Get("status"); raw != "" {
			statuses = strings.Split(raw, ",")
		}
		query := r.URL.Query()
		if _, exists := query["limit"]; !exists {
			query.Set("limit", "200")
		}
		pagination, problem := contract.ParsePagination(query)
		if problem != nil {
			err = problem
		} else if int64(pagination.Page-1) > int64(9007199254740991)/int64(pagination.Limit) {
			err = contract.NewProblem(400, "ページ指定が不正です。", "")
		} else {
			offset := (pagination.Page - 1) * pagination.Limit
			var list []*jobs.Job
			var total int
			list, total, err = s.jobs.List(r.Context(), query.Get("repo"), statuses, pagination.Limit, offset)
			value = map[string]any{"jobs": list, "total": total, "page": pagination.Page, "limit": pagination.Limit, "hasMore": offset < total && len(list) < total-offset}
		}
	}
	return value, err
}

// nextJobOutcome は次のジョブの引き取りの応答の仕方。
type nextJobOutcome int

const (
	// nextJobSend は値またはエラーをそのまま返す。
	nextJobSend nextJobOutcome = iota
	// nextJobNone は渡すジョブが無いことを204で返す。
	nextJobNone
	// nextJobAbandoned は要求が切れたため応答を書かない。
	nextJobAbandoned
)

// nextJob はPCの引き取りの要求を検査し、渡せるジョブを待って、lease と実装の回数・使用量を添えて返す。
func (s *WorkerRoutes) nextJob(r *http.Request, principal auth.Principal) (any, nextJobOutcome, error) {
	err := workerMethod(r, "POST")
	var body map[string]any
	if err == nil {
		body, err = workerBody(r, "host", "capacity", "waitSeconds", "repos")
		if err == nil {
			err = senderHost(body, principal.Name)
		}
	}
	if err != nil {
		return nil, nextJobSend, err
	}
	requested, provided, repositoryErr := requestedRepositories(body)
	if repositoryErr != nil {
		return nil, nextJobSend, repositoryErr
	}
	var capacity int64
	waitSeconds := int64(0)
	capacity, err = workerInteger(body["capacity"], 1, 9007199254740991)
	if err == nil {
		if raw, exists := body["waitSeconds"]; exists {
			waitSeconds, err = workerInteger(raw, 0, 60)
		}
	}
	if err != nil {
		return nil, nextJobSend, err
	}
	document, assignment, err := s.fleet.Load()
	if err == nil && assignment.Hosts[principal.Name] != nil && assignment.Hosts[principal.Name].Role == "test-only" {
		return nil, nextJobSend, contract.NewProblem(403, "テスト専用PCは実装を引き取れません。", "")
	}
	if err != nil {
		return nil, nextJobSend, err
	}
	job, lease, details, abandoned, err := s.waitForJob(r, principal.Name, capacity, waitSeconds, assignment, fleet.Object(document.Defaults["usageWeights"]), requested, provided)
	if abandoned {
		return nil, nextJobAbandoned, nil
	}
	if err == nil && job == nil {
		return nil, nextJobNone, nil
	}
	if err != nil {
		return nil, nextJobSend, err
	}
	return leasedJobDescriptor(job, lease, details), nextJobSend, nil
}

// waitForJob は最長 waitSeconds の間、版ずれで止めるPCでなければ渡せるジョブを1秒ごとに探す。要求が切れたら abandoned を返す。
func (s *WorkerRoutes) waitForJob(r *http.Request, host string, capacity, waitSeconds int64, assignment *fleet.Assignments, weights map[string]any, requested []string, provided bool) (*jobs.Job, *jobs.Lease, jobs.Delivery, bool, error) {
	var err error
	var job *jobs.Job
	var lease *jobs.Lease
	var details jobs.Delivery
	deadline := time.Now().Add(time.Duration(waitSeconds) * time.Second)
waitForJob:
	for {
		drift, driftErr := s.drift.Evaluate(r.Context(), assignment)
		if driftErr != nil {
			err = driftErr
		} else if _, held := drift.Holds[host]; held {
			job, lease = nil, nil
		} else if repos, restricted := dispatchRestriction(requested, provided, drift.Allowed[host]); restricted {
			job, lease, details, err = s.jobs.NextForDelivery(r.Context(), host, int(capacity), assignment, weights, repos)
		} else {
			job, lease, details, err = s.jobs.NextForDelivery(r.Context(), host, int(capacity), assignment, weights)
		}
		if err != nil || job != nil || !time.Now().Before(deadline) {
			break
		}
		remaining := time.Until(deadline)
		if remaining > time.Second {
			remaining = time.Second
		}
		timer := time.NewTimer(remaining)
		select {
		case <-r.Context().Done():
			timer.Stop()
			return nil, nil, details, true, nil
		case <-HandoffDone(r.Context()):
			timer.Stop()
			if r.Context().Err() != nil {
				return nil, nil, details, true, nil
			}
			// Only stop idle waiting. A job already acquired above
			// must retain its lease and follow the normal 200 path.
			break waitForJob
		case <-timer.C:
		}
		document, nextAssignment, loadErr := s.fleet.Load()
		assignment, err = nextAssignment, loadErr
		if err == nil {
			weights = fleet.Object(document.Defaults["usageWeights"])
		}
		if err == nil && assignment.Hosts[host] != nil && assignment.Hosts[host].Role == "test-only" {
			return nil, nil, details, false, contract.NewProblem(403, "テスト専用PCは実装を引き取れません。", "")
		}
		if err != nil {
			break
		}
	}
	return job, lease, details, false, err
}

// leasedJobDescriptor は貸し出したジョブに実装の回数・使用量と依存のマージを添えて、PCへ渡す形にする。
func leasedJobDescriptor(job *jobs.Job, lease *jobs.Lease, details jobs.Delivery) any {
	if job.View == nil {
		job.View = map[string]any{}
	}
	job.View["implementationRuns"] = jobs.ImplementationRuns(job)
	job.View["implementationWeightedUsage"] = details.WeightedUsage
	return map[string]any{"job": struct {
		*jobs.Job
		DependencyMerges []any `json:"dependencyMerges,omitempty"`
	}{job, details.DependencyMerges}, "lease": lease}
}

func senderHost(body map[string]any, host string) error {
	if value, exists := body["host"]; exists && value != host {
		return contract.NewProblem(403, "送り手のPCと同じhostだけを送れます。", "")
	}
	return nil
}

func (s *WorkerRoutes) jobAction(r *http.Request, host, path string) (any, error) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) > 2 {
		return nil, contract.NewProblem(404, "経路が見つかりません。", "")
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id < 1 || id > 9007199254740991 {
		return nil, contract.NewProblem(404, "ジョブが見つかりません。", "")
	}
	if len(parts) == 1 {
		if err = workerMethod(r, "GET"); err != nil {
			return nil, err
		}
		return s.jobs.Detail(r.Context(), id)
	}
	if err = workerMethod(r, "POST"); err != nil {
		return nil, err
	}
	keys := map[string][]string{"lease": {"host", "epoch", "progress"}, "state": {"host", "epoch", "status", "holdReason", "branch", "pr", "headSha", "diffFingerprint", "view", "result", "controllerTest", "controllerReport"}, "scope": {"host", "epoch", "add", "actual"}, "events": {"host", "epoch", "kind", "data"}, "notes": {"host", "epoch", "kind", "data"}}
	allowed, exists := keys[parts[1]]
	if !exists {
		return nil, contract.NewProblem(404, "経路が見つかりません。", "")
	}
	body, err := workerBody(r, allowed...)
	if err != nil {
		return nil, err
	}
	if err = senderHost(body, host); err != nil {
		return nil, err
	}
	epoch, err := workerInteger(body["epoch"], 1, 9007199254740991)
	if err != nil {
		return nil, err
	}
	switch parts[1] {
	case "lease":
		if value, exists := body["progress"]; exists && value != nil && fleet.Object(value) == nil {
			return nil, contract.NewProblem(400, "progressはオブジェクトです。", "")
		}
		job, lease, err := s.jobs.Renew(r.Context(), id, host, epoch, fleet.Object(body["progress"]))
		return map[string]any{"job": job, "lease": lease}, err
	case "state":
		raw, _ := json.Marshal(body)
		var report jobs.Report
		if err = json.Unmarshal(raw, &report); err != nil {
			return nil, contract.NewProblem(400, "状態報告の形が違います。", "")
		}
		job, err := s.jobs.Report(r.Context(), id, host, report)
		return map[string]any{"job": job}, err
	case "scope":
		_, hasAdd := body["add"]
		_, hasActual := body["actual"]
		if hasAdd == hasActual {
			return nil, contract.NewProblem(400, "addとactualのいずれか一方が必要です。", "")
		}
		key := "add"
		if hasActual {
			key = "actual"
		}
		paths := fleet.Strings(body[key])
		if len(paths) != len(fleet.List(body[key])) || len(paths) == 0 {
			return nil, contract.NewProblem(400, key+"はパスの配列です。", "")
		}
		if hasActual {
			return s.jobs.Narrow(r.Context(), id, host, epoch, paths)
		}
		job, err := s.jobs.Get(r.Context(), id)
		if err != nil {
			return nil, err
		}
		_, assignment, err := s.fleet.Load()
		if err != nil {
			return nil, err
		}
		return s.jobs.Widen(r.Context(), id, host, epoch, paths, assignment.Exempt(job.Repo), assignment.ScopeLimitFiles())
	case "events":
		if body["kind"] == "implementation_usage_check" {
			document, a, err := s.fleet.Load()
			if err != nil {
				return nil, err
			}
			limit := fleet.Number(a.Output["implementationUsageLimit"])
			if limit <= 0 {
				limit = 8000000
			}
			return s.jobs.CheckAutomaticUsage(r.Context(), id, host, epoch, fleet.Object(body["data"]), limit, fleet.Object(document.Defaults["usageWeights"]))
		}
		if body["data"] != nil && fleet.Object(body["data"]) == nil {
			return nil, contract.NewProblem(400, "dataはオブジェクトです。", "")
		}
		err := s.jobs.Event(r.Context(), id, host, epoch, fleet.Text(body["kind"]), fleet.Object(body["data"]))
		return map[string]any{"ok": true}, err
	case "notes":
		id, err := s.jobs.Note(r.Context(), id, host, epoch, fleet.Text(body["kind"]), fleet.Object(body["data"]))
		return map[string]any{"id": id}, err
	}
	return nil, contract.NewProblem(404, "経路が見つかりません。", "")
}

func (s *WorkerRoutes) turnRoute(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.identity(w, r, auth.Policy{PC: true})
	if !ok {
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/merge-turns/"), "/")
	if len(parts) != 3 || !fleet.ValidRepo(parts[0]+"/"+parts[1]) {
		workerFailure(w, contract.NewProblem(404, "経路が見つかりません。", ""))
		return
	}
	if err := workerMethod(r, "POST"); err != nil {
		workerFailure(w, err)
		return
	}
	body, err := workerBody(r, "host", "jobId", "requestId", "epoch", "turnEpoch", "nativeTestGate")
	if err == nil {
		err = senderHost(body, principal.Name)
	}
	if err != nil {
		workerFailure(w, err)
		return
	}
	raw, _ := json.Marshal(body)
	var who jobs.TurnRequest
	if json.Unmarshal(raw, &who) != nil {
		workerFailure(w, contract.NewProblem(400, "マージの番の要求が不正です。", ""))
		return
	}
	who.Host = principal.Name
	repo := parts[0] + "/" + parts[1]
	if parts[2] == "acquire" || parts[2] == "renew" {
		if repo, err = s.nativeTestGateRepo(repo, body); err != nil {
			workerFailure(w, err)
			return
		}
	}
	var value any
	switch parts[2] {
	case "acquire":
		value, err = s.jobs.AcquireTurn(r.Context(), repo, who)
	case "renew":
		var turn *jobs.Lease
		turn, err = s.jobs.RenewTurn(r.Context(), repo, who)
		value = map[string]any{"turn": turn}
	case "release":
		err = s.jobs.ReleaseTurn(r.Context(), repo, who)
		value = map[string]any{"released": true}
	default:
		err = contract.NewProblem(404, "経路が見つかりません。", "")
	}
	if err != nil {
		workerFailure(w, err)
		return
	}
	workerSend(w, value, 200)
}

// nativeTestGateRepo はリポジトリ名を担当表の綴りにそろえ、Mac検証の担当があるリポジトリでは Worker の検証設定がそろっているかを確かめる。
func (s *WorkerRoutes) nativeTestGateRepo(repo string, body map[string]any) (string, error) {
	document, assignments, loadErr := s.fleet.Load()
	if loadErr != nil {
		return "", loadErr
	}
	for configured := range assignments.Implementers {
		if strings.EqualFold(configured, repo) {
			repo = configured
			break
		}
	}
	implementers := assignments.Implementers[repo]
	if len(implementers) > 0 {
		target := fleet.NativeTestTarget(document, assignments, implementers[0], repo)
		if target != "" {
			gate := fleet.Object(body["nativeTestGate"])
			if len(gate) != 3 || gate["version"] != float64(1) || gate["host"] != target || gate["configurationVersion"] != fleet.Digest(fleet.EffectiveConfig(document, target)) {
				return "", contract.NewProblem(409, "Mac検証に対応したWorkerと現在の検証設定が必要です。", "NATIVE_TEST_GATE_REQUIRED")
			}
		}
	}
	return repo, nil
}
