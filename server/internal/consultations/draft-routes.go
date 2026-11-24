package consultations

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
)

func (s *Service) RegisterDrafts(mux *http.ServeMux) {
	mux.HandleFunc("/api/attention/consultation-budget/", s.continueBudgetRoute)
	mux.HandleFunc("/api/drafts", s.draftRoute)
	mux.HandleFunc("/api/drafts/", s.draftRoute)
	mux.HandleFunc("POST /api/issues", s.acceptRoute)
}
func (s *Service) acceptRoute(w http.ResponseWriter, r *http.Request) {
	p, err := s.options.Auth.Authenticate(r)
	if err != nil {
		contract.WriteProblem(w, err)
		return
	}
	if err = auth.Authorize(p, auth.Policy{PC: true, CLI: true, Screen: true}); err != nil {
		contract.WriteProblem(w, err)
		return
	}
	data, err := contract.DecodeObject(r.Body, r.Header.Get("Content-Type"), 128<<10)
	if err != nil {
		contract.WriteProblem(w, err)
		return
	}
	value, failure := s.AcceptIssue(r.Context(), data, p)
	if failure != nil {
		contract.WriteProblem(w, problem(failure))
		return
	}
	respond(w, 202, value)
}
func (s *Service) draftRoute(w http.ResponseWriter, r *http.Request) {
	p, authErr := s.options.Auth.Authenticate(r)
	if authErr != nil {
		contract.WriteProblem(w, authErr)
		return
	}
	id, action, found := draftRouteTarget(r.URL.Path)
	if !found {
		contract.WriteProblem(w, refuse(404, "下書きが見つかりません。"))
		return
	}
	if authErr = auth.Authorize(p, draftRoutePolicy(r.Method, id, action)); authErr != nil {
		contract.WriteProblem(w, authErr)
		return
	}
	authContext, authErr := s.options.Auth.Context(r)
	if authErr != nil {
		contract.WriteProblem(w, authErr)
		return
	}
	value, status, err := s.serveDraftRequest(w, r, p, authContext.Origin, id, action)
	if err != nil {
		contract.WriteProblem(w, problem(err))
		return
	}
	respond(w, status, value)
}

// draftRouteTarget splits /api/drafts[/{id}[/{action}]] into the draft ID and action.
// found is false when the path does not name a draft.
func draftRouteTarget(urlPath string) (id, action string, found bool) {
	path := strings.TrimPrefix(urlPath, "/api/drafts")
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if path != "" {
		id = parts[0]
		if len(parts) > 2 || !recordID.MatchString(id) {
			return "", "", false
		}
		if len(parts) == 2 {
			action = parts[1]
		}
	}
	return id, action, true
}

// draftRoutePolicy decides which callers may use the draft route for the method and action.
func draftRoutePolicy(method, id, action string) auth.Policy {
	policy := auth.Policy{Screen: true, CLI: true, PC: true}
	if id == "" && method == http.MethodPost {
		policy.PC = false
	}
	if id == "" && method == http.MethodGet || method == http.MethodDelete || is(action, "todo", "finish", "stop") {
		policy.RequireDelegation = true
	}
	if is(action, "priority", "edit") {
		policy.PC = false
	}
	if action == "proxy-undo" {
		policy = auth.Policy{Screen: true, PC: true, RequireDelegation: true}
	}
	return policy
}

// serveDraftRequest dispatches an authorized draft request to its operation.
func (s *Service) serveDraftRequest(w http.ResponseWriter, r *http.Request, p auth.Principal, origin, id, action string) (any, int, error) {
	switch {
	case r.Method == http.MethodGet && id == "":
		value, err := s.listDrafts(w, r, p, origin)
		return value, 200, err
	case r.Method == http.MethodGet && id != "" && action == "":
		view := r.URL.Query().Get("view")
		if view != "" && view != "summary" {
			return nil, 200, refuse(400, "不明なviewです。")
		}
		if view == "summary" {
			value, err := s.draftSummary(r.Context(), id, p)
			return value, 200, err
		}
		value, err := s.draftDetail(r.Context(), id, p, origin)
		return value, 200, err
	case r.Method == http.MethodDelete && id != "" && action == "":
		data := Record{}
		for key, values := range r.URL.Query() {
			data[key] = values[len(values)-1]
		}
		value, err := s.Discard(r.Context(), id, data, p)
		return value, 200, err
	case r.Method == http.MethodPost:
		data, err := contract.DecodeObject(r.Body, r.Header.Get("Content-Type"), 128<<10)
		if err != nil {
			return nil, 200, err
		}
		return s.postDraftAction(r.Context(), data, p, origin, id, action)
	default:
		return nil, 200, refuse(405, "このメソッドは利用できません。")
	}
}

// listDrafts reads, filters and pages the draft list and records the stage timings.
func (s *Service) listDrafts(w http.ResponseWriter, r *http.Request, p auth.Principal, origin string) (any, error) {
	view := r.URL.Query().Get("view")
	if view != "" && view != "summary" {
		return nil, refuse(400, "不明なviewです。")
	}
	pagination, pageErr := contract.ParsePagination(r.URL.Query())
	if pageErr != nil {
		return nil, pageErr
	}
	now := s.timingNow
	if now == nil {
		now = time.Now
	}
	started := now()
	entries, listErr := s.options.Records.ListDraftEntries(r.Context(), DraftReadPage)
	readDuration := now().Sub(started)
	if listErr != nil {
		return nil, listErr
	}
	started = now()
	fields := make([]Record, len(entries))
	for i, entry := range entries {
		fields[i] = entry.Record
	}
	if view != "summary" {
		fields = s.FamilyProgress(r.Context(), fields)
	}
	matches := s.draftListMatcher(r.URL.Query(), p)
	matched := []DraftFields{}
	for i, d := range fields {
		if matches(d) {
			matched = append(matched, DraftFields{Key: entries[i].Key, Record: d})
		}
	}
	filterDuration := now().Sub(started)
	started = now()
	selection := contract.Paginate(matched, pagination)
	pageDuration := now().Sub(started)
	if view == "summary" {
		w.Header().Add("Server-Timing", contract.ServerTiming(
			contract.TimingMetric{Name: "draft_read", Duration: readDuration},
			contract.TimingMetric{Name: "draft_filter", Duration: filterDuration},
			contract.TimingMetric{Name: "draft_page", Duration: pageDuration},
		))
		page := contract.Paginate([]Record{}, pagination)
		page.Items, page.Total, page.HasMore = draftSummaryRows(selection.Items), selection.Total, selection.HasMore
		return page, nil
	}
	started = now()
	views, readErr := s.selectedFullDrafts(r.Context(), selection.Items, matches)
	if readErr != nil {
		return nil, readErr
	}
	// draft_full measures the selected full-record lookups, decoding, rechecks,
	// and progress copying; it excludes projection filtering and DraftView.
	fullDuration := now().Sub(started)
	started = now()
	family := s.familyListViews(r.Context(), views)
	for i, d := range views {
		views[i] = s.draftView(d, p, origin, func(d Record, action string) string {
			return s.refusalWithFamily(d, action, family)
		})
	}
	viewDuration := now().Sub(started)
	w.Header().Add("Server-Timing", contract.ServerTiming(
		contract.TimingMetric{Name: "draft_read", Duration: readDuration},
		contract.TimingMetric{Name: "draft_filter", Duration: filterDuration},
		contract.TimingMetric{Name: "draft_page", Duration: pageDuration},
		contract.TimingMetric{Name: "draft_full", Duration: fullDuration},
		contract.TimingMetric{Name: "draft_view", Duration: viewDuration},
	))
	page := contract.Paginate([]Record{}, pagination)
	page.Items, page.Total, page.HasMore = views, selection.Total, selection.HasMore
	return page, nil
}

// draftListMatcher builds the list filter from the access check and the project and status queries.
func (s *Service) draftListMatcher(query url.Values, p auth.Principal) func(Record) bool {
	project := query.Get("project")
	statuses := strings.Split(query.Get("status"), ",")
	excluded := strings.Split(query.Get("excludeStatus"), ",")
	return func(d Record) bool {
		return s.draftAccess(d, p) == nil && (project == "" || d["project"] == project) && (query.Get("status") == "" || containsStatus(statuses, d["status"])) && (query.Get("excludeStatus") == "" || !containsStatus(excluded, d["status"]))
	}
}

// draftSummaryRows keeps only the summary fields of the selected drafts.
func draftSummaryRows(items []DraftFields) []Record {
	views := make([]Record, 0, len(items))
	for _, selected := range items {
		row := Record{}
		for _, key := range []string{"id", "project", "unassigned", "status", "title", "shownTitle", "createdAt", "updatedAt"} {
			if v, ok := selected.Record[key]; ok {
				row[key] = v
			}
		}
		views = append(views, row)
	}
	return views
}

// selectedFullDrafts reads the full records of the selected page, drops records that no
// longer match and carries the family progress computed from the list fields.
func (s *Service) selectedFullDrafts(ctx context.Context, items []DraftFields, matches func(Record) bool) ([]Record, error) {
	views := make([]Record, 0, len(items))
	keys := make([]string, len(items))
	for i, selected := range items {
		keys[i] = selected.Key
	}
	full, err := s.options.Records.GetDraftEntries(ctx, keys)
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]Record, len(full))
	for _, entry := range full {
		byKey[entry.Key] = entry.Record
	}
	for _, selected := range items {
		d := byKey[selected.Key]
		if d == nil || !matches(d) {
			continue
		}
		delete(d, "familyProgress")
		if progress, ok := selected.Record["familyProgress"]; ok {
			d["familyProgress"] = progress
		}
		views = append(views, d)
	}
	return views, nil
}

// draftDetail reads one draft and, for a split parent, adds the family progress.
func (s *Service) draftDetail(ctx context.Context, id string, p auth.Principal, origin string) (any, error) {
	d, err := s.readDraft(ctx, id, p)
	if err != nil {
		return nil, err
	}
	if d["parentSplit"] == nil && d["splitChildren"] != nil {
		if records, e := s.detailFamilyRecords(ctx, d); e == nil {
			enriched := s.FamilyProgress(ctx, records)
			for _, candidate := range enriched {
				if candidate["id"] == d["id"] {
					d = candidate
					break
				}
			}
		}
	}
	return s.DraftView(d, p, origin), nil
}

// draftSummaryFields are the fields of one draft that view=summary returns. They cover what a
// caller waiting for the reply reads: the state, the revision, the open questions and the
// filing result. The receipt fields are present only while the receipt stands in for the draft.
var draftSummaryFields = []string{
	"id", "project", "unassigned", "status", "revision", "title", "shownTitle", "createdAt", "updatedAt",
	"ready", "hold", "questions", "error", "errorCode", "issue", "issues", "parentSplit", "splitChildren", "finishedWithoutIssue",
	"accepted", "receiptId", "requestId", "draftId", "receiptState",
}

// draftSummary reads one draft with the same receipt branch and access check as the full
// read and keeps only the summary fields. It skips the family progress and the actions.
func (s *Service) draftSummary(ctx context.Context, id string, p auth.Principal) (any, error) {
	d, err := s.readDraft(ctx, id, p)
	if err != nil {
		return nil, err
	}
	out := Record{"view": "summary"}
	for _, key := range draftSummaryFields {
		if v, ok := d[key]; ok {
			out[key] = v
		}
	}
	if stopped, ok := record(d["consultationBudget"])["stopped"]; ok {
		out["consultationBudget"] = Record{"stopped": stopped}
	}
	return out, nil
}

// postDraftAction runs a POST operation on the drafts and returns its view and status.
func (s *Service) postDraftAction(ctx context.Context, data Record, p auth.Principal, origin, id, action string) (any, int, error) {
	var d Record
	var err error
	status := 200
	switch {
	case id == "":
		d, err = s.createDraft(ctx, data, p, false, true)
		status = 201
	case action == "proxy-undo":
		d, err = s.UndoProxy(ctx, id, data, p)
	case action == "chat":
		d, err = s.Chat(ctx, id, data, p)
		status = 202
	case action == "edit":
		d, err = s.EditDraft(ctx, id, data, p)
	case action == "priority":
		d, err = s.SetDraftPriority(ctx, id, data, p)
	case action == "finish":
		d, err = s.Finish(ctx, id, data, p)
	case action == "stop":
		d, err = s.Stop(ctx, id, data, p)
	case action == "todo":
		d, err = s.ReturnToTodo(ctx, id, data, p)
	case action == "submit":
		d, err = s.AcceptSubmission(ctx, id, data, p)
		status = 202
	default:
		err = refuse(404, "相談の操作が見つかりません。")
	}
	if err != nil {
		return nil, status, err
	}
	if action == "todo" {
		return s.todoView(d, p, origin), status, nil
	}
	return s.DraftView(d, p, origin), status, nil
}

func containsStatus(statuses []string, value any) bool {
	for _, status := range statuses {
		if strings.TrimSpace(status) == value {
			return true
		}
	}
	return false
}
