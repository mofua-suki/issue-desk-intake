package consultations

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/images"
)

func (s *Service) Register(mux *http.ServeMux) {
	s.RegisterDrafts(mux)
	mux.HandleFunc("/api/todos", s.todoRoute)
	mux.HandleFunc("/api/todos/", s.todoRoute)
}

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(contract.Header, contract.Version)
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func (s *Service) todoView(r Record, principal auth.Principal, origin string) Record {
	value := Record(images.Present(r, origin))
	base := strings.TrimRight(s.options.PublicURL, "/")
	if base == "" {
		base = origin
	}
	value["url"] = base + "/#/todos/" + text(r["id"])
	if principal.Kind != auth.PC || principal.Delegated {
		value["actions"] = []string{"save", "start", "remove"}
	}
	return value
}

func (s *Service) todoRoute(w http.ResponseWriter, r *http.Request) {
	p, pErr := s.options.Auth.Authenticate(r)
	if pErr != nil {
		contract.WriteProblem(w, pErr)
		return
	}
	id, action, found := todoRouteTarget(r.URL.Path)
	if !found {
		contract.WriteProblem(w, refuse(404, "TODOが見つかりません。"))
		return
	}
	policy := auth.Policy{Screen: true, CLI: true, PC: true}
	if r.Method == http.MethodGet || action == "start" || action == "remove" {
		policy.RequireDelegation = true
	}
	if pErr = auth.Authorize(p, policy); pErr != nil {
		contract.WriteProblem(w, pErr)
		return
	}
	ctx, cp := s.options.Auth.Context(r)
	if cp != nil {
		contract.WriteProblem(w, cp)
		return
	}
	var value any
	status := 200
	switch {
	case r.Method == http.MethodGet && action != "":
		pErr = refuse(405, "このメソッドは利用できません。")
	case r.Method == http.MethodGet && id == "":
		value, pErr = s.listTodos(r.Context(), r.URL.Query(), p, ctx.Origin)
	case r.Method == http.MethodGet:
		value, pErr = s.todoDetail(r.Context(), id, p, ctx.Origin)
	case r.Method != http.MethodPost:
		pErr = refuse(405, "このメソッドは利用できません。")
	default:
		value, status, pErr = s.postTodoAction(r, p, ctx.Origin, id, action)
	}
	if pErr != nil {
		contract.WriteProblem(w, pErr)
		return
	}
	respond(w, status, value)
}

// todoRouteTarget splits /api/todos[/{id}[/{action}]] into the TODO ID and action.
// found is false when the path does not name a TODO.
func todoRouteTarget(path string) (id, action string, found bool) {
	parts := strings.Split(strings.TrimPrefix(path, "/api/todos"), "/")
	if len(parts) > 1 {
		id = parts[1]
	}
	if len(parts) > 2 {
		action = parts[2]
	}
	if len(parts) > 3 || (id != "" && !recordID.MatchString(id)) {
		return "", "", false
	}
	return id, action, true
}

// listTodos returns the page of TODOs the principal may read, optionally limited to a project.
func (s *Service) listTodos(ctx context.Context, query url.Values, p auth.Principal, origin string) (any, *contract.Problem) {
	pagination, pageErr := contract.ParsePagination(query)
	if pageErr != nil {
		return nil, pageErr
	}
	project := query.Get("project")
	rows, err := s.options.Records.ListTodoEntries(ctx)
	if err != nil {
		return nil, problem(err)
	}
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		if s.todoAccess(row.Record, p) == nil && (project == "" || text(row.Record["project"]) == project) {
			keys = append(keys, row.Key)
		}
	}
	page := contract.Paginate(keys, pagination)
	full, err := s.options.Records.GetTodos(ctx, page.Items)
	if err != nil {
		return nil, problem(err)
	}
	values := make([]Record, 0, len(full))
	for _, row := range full {
		if s.todoAccess(row, p) == nil && (project == "" || text(row["project"]) == project) {
			values = append(values, s.todoView(row, p, origin))
		}
	}
	return contract.PageResult[Record]{Items: values, Total: page.Total, Page: page.Page, Limit: page.Limit, HasMore: page.HasMore}, nil
}

// todoDetail returns one TODO, or tells that its consultation has already started.
func (s *Service) todoDetail(ctx context.Context, id string, p auth.Principal, origin string) (any, *contract.Problem) {
	row, found, err := s.options.Records.Get(ctx, "todos", id)
	if err != nil {
		return nil, problem(err)
	}
	if !found {
		if _, started, _ := s.options.Records.Get(ctx, "drafts", id); started {
			return nil, refuse(409, "このTODOは相談を開始済みです。相談画面から続けてください。")
		}
		return nil, refuse(404, "TODOが見つかりません。")
	}
	if err := s.todoAccess(row, p); err != nil {
		return nil, problem(err)
	}
	return s.todoView(row, p, origin), nil
}

// postTodoAction decodes a POST request and runs the TODO operation it names.
func (s *Service) postTodoAction(r *http.Request, p auth.Principal, origin, id, action string) (any, int, *contract.Problem) {
	data, pErr := contract.DecodeObject(r.Body, r.Header.Get("Content-Type"), 128<<10)
	if pErr != nil {
		return nil, 200, pErr
	}
	if p.Kind == auth.PC {
		if images, present := data["images"]; present {
			array, ok := images.([]any)
			if !ok || len(array) > 0 {
				return nil, 200, refuse(400, "トークン接続では画像を添付できません。")
			}
		}
	}
	var value Record
	var err error
	status := 200
	switch {
	case id == "" && action == "":
		value, err = s.CreateTodo(r.Context(), data, p)
		status = 201
	case id != "" && action == "save":
		value, err = s.SaveTodo(r.Context(), id, data, p)
	case id != "" && action == "remove":
		value, err = s.RemoveTodo(r.Context(), id, data, p)
	case id != "" && action == "start":
		value, err = s.StartTodo(r.Context(), id, data, p)
		status = 202
	default:
		err = refuse(404, "操作が見つかりません。")
	}
	if err != nil {
		return nil, status, problem(err)
	}
	if action == "start" || action == "remove" {
		return value, status, nil
	}
	return s.todoView(value, p, origin), status, nil
}

func instructionAt(value any, created any, now time.Time) error {
	input, ok := value.(string)
	if !ok || input == "" {
		return refuse(400, "instructionAtはUTCオフセット付きの日時で指定してください。")
	}
	t, err := time.Parse(time.RFC3339Nano, input)
	if err != nil || t.After(now) {
		return refuse(400, "instructionAtの日時が不正です。")
	}
	c, err := time.Parse(time.RFC3339Nano, text(created))
	if err != nil {
		return refuse(409, "対象の作成時刻を確認できません。")
	}
	if c.After(t) {
		return refuse(409, "指示の後に作成された対象は操作できません。")
	}
	return nil
}

func (s *Service) RemoveTodo(ctx context.Context, id string, data Record, p auth.Principal) (Record, error) {
	_, err := s.options.Records.Change(ctx, "todos", id, func(tx *sql.Tx, current Record) (Record, error) {
		if current == nil {
			var count int
			if err := tx.QueryRowContext(ctx, bindSQL("SELECT count(*) FROM drafts WHERE id=?"), id).Scan(&count); err != nil {
				return nil, err
			}
			if count > 0 {
				return nil, refuse(409, "このTODOは相談を開始済みです。相談画面から操作してください。")
			}
			return nil, nil
		}
		if err := s.todoAccess(current, p); err != nil {
			return nil, err
		}
		if p.Kind == auth.PC {
			if err := instructionAt(data["instructionAt"], current["createdAt"], s.options.Now()); err != nil {
				return nil, err
			}
		}
		if revision, present := data["revision"]; present {
			requested, valid := integer(revision)
			actual, _ := integer(current["revision"])
			if !valid || requested != actual {
				return nil, refuse(409, "TODOは別の画面から更新されています。")
			}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM images WHERE scope=?", "todo:"+id); err != nil {
			return nil, err
		}
		return nil, nil
	})
	return Record{"id": id, "removed": true}, err
}
