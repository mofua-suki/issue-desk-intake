package images

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
)

type Options struct {
	AppDB         *sql.DB
	Auth          *auth.Authenticator
	Project       func(string) error
	Issue         func(context.Context, string, int64) error
	Draft         func(context.Context, string) (map[string]any, error)
	Todo          func(context.Context, string) (map[string]any, error)
	DraftWritable func(map[string]any) bool
}
type Handler struct{ options Options }

var imageID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

func Register(mux *http.ServeMux, options Options) error {
	if mux == nil || options.AppDB == nil || options.Auth == nil || options.Project == nil || options.Issue == nil || options.Draft == nil || options.Todo == nil || options.DraftWritable == nil {
		return errors.New("image persistence, scope validation and authentication are required")
	}
	h := &Handler{options}
	mux.Handle("/api/images", h)
	mux.Handle("/api/images/", h)
	return nil
}
func fail(status int, message string) *contract.Problem {
	return contract.NewProblem(status, message, "")
}
func (h *Handler) scope(ctx context.Context, data map[string]any, write bool, p auth.Principal) (string, error) {
	draft, todo, project := fleet.Text(data["draft"]), fleet.Text(data["todo"]), fleet.Text(data["project"])
	if draft != "" && todo == "" {
		d, e := h.options.Draft(ctx, draft)
		if e != nil {
			return "", e
		}
		if d == nil {
			return "", fail(404, "相談が見つかりません。")
		}
		if p.Kind == auth.PC && !p.Delegated && fleet.Text(d["owner"]) != "token:"+p.TokenID {
			return "", fail(404, "相談が見つかりません。")
		}
		if d["unassigned"] == true && (d["project"] == nil || d["project"] == "") {
			if project != "" && project != "null" {
				return "", fail(403, "画像の依頼先が一致しません。")
			}
			if write && !h.options.DraftWritable(d) {
				return "", fail(409, "現在画像を変更できません。")
			}
			return "draft:" + fleet.Text(d["id"]), nil
		}
	}
	if e := h.options.Project(project); e != nil {
		return "", e
	}
	if todo != "" {
		t, e := h.options.Todo(ctx, todo)
		if e != nil {
			return "", e
		}
		if t == nil {
			return "", fail(404, "TODOが見つかりません。")
		}
		if p.Kind == auth.PC && !p.Delegated && fleet.Text(t["owner"]) != "token:"+p.TokenID {
			return "", fail(404, "TODOが見つかりません。")
		}
		if t["project"] != project {
			return "", fail(403, "画像の依頼先が一致しません。")
		}
		return "todo:" + fleet.Text(t["id"]), nil
	}
	if draft != "" {
		d, e := h.options.Draft(ctx, draft)
		if e != nil {
			return "", e
		}
		if d == nil {
			return "", fail(404, "相談が見つかりません。")
		}
		if p.Kind == auth.PC && !p.Delegated && fleet.Text(d["owner"]) != "token:"+p.TokenID {
			return "", fail(404, "相談が見つかりません。")
		}
		if d["project"] != project {
			return "", fail(403, "画像の依頼先が一致しません。")
		}
		if write && !h.options.DraftWritable(d) {
			return "", fail(409, "現在画像を変更できません。")
		}
		return "draft:" + fleet.Text(d["id"]), nil
	}
	number, e := strconv.ParseInt(fmt.Sprint(data["number"]), 10, 64)
	if e != nil || number < 1 {
		return "", fail(400, "画像の依頼番号が不正です。")
	}
	if e = h.options.Issue(ctx, project, number); e != nil {
		return "", e
	}
	return fmt.Sprintf("issue:%s#%d", project, number), nil
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, problem := h.options.Auth.Authenticate(r)
	if problem != nil {
		contract.WriteProblem(w, problem)
		return
	}
	if problem = auth.Authorize(p, auth.Policy{Screen: true, CLI: true}); problem != nil {
		contract.WriteProblem(w, problem)
		return
	}
	var data map[string]any
	var err error
	if r.Method == http.MethodGet {
		data = map[string]any{}
		for key, values := range r.URL.Query() {
			data[key] = values[len(values)-1]
		}
	} else {
		var problem *contract.Problem
		data, problem = contract.DecodeObject(r.Body, r.Header.Get("Content-Type"), 2900000)
		if problem != nil {
			contract.WriteProblem(w, problem)
			return
		}
	}
	scope, err := h.scope(r.Context(), data, r.Method != http.MethodGet, p)
	if err != nil {
		writeError(w, err)
		return
	}
	id, action := "", ""
	if r.URL.Path != "/api/images" {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/images/"), "/")
		if len(parts) > 2 || !imageID.MatchString(parts[0]) {
			contract.WriteProblem(w, fail(404, "画像が見つかりません。"))
			return
		}
		id = parts[0]
		if len(parts) == 2 {
			action = parts[1]
		}
	}
	if r.Method == http.MethodGet && id != "" && action == "" {
		meta, raw, e := h.read(r.Context(), id, scope, data)
		if e != nil {
			writeError(w, e)
			return
		}
		w.Header().Set("Content-Type", fleet.Text(meta["mime"]))
		w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
		w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="%s.%s"`, id, fleet.Text(meta["ext"])))
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(200)
		_, _ = w.Write(raw)
		return
	}
	var value any
	status := 200
	if r.Method == http.MethodPost && id == "" {
		value, err = h.upload(r.Context(), scope, data, p)
		status = 201
	} else if r.Method == http.MethodPost && action == "remove" {
		value, err = h.remove(r.Context(), id, scope, data, p)
	} else {
		err = fail(405, "このメソッドは使えません。")
	}
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(contract.Header, contract.Version)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, err error) {
	var problem *contract.Problem
	if !errors.As(err, &problem) || problem == nil {
		contract.LogFailure("画像のAPI", err)
		problem = fail(503, "画像を読み書きできません。")
	}
	contract.WriteProblem(w, problem)
}

// Present attaches same-origin, scope-specific URLs without altering stored records.
func Present(record map[string]any, origin string) map[string]any {
	out := clone(record)
	id := fleet.Text(record["id"])
	scope := url.Values{}
	scope.Set("project", fleet.Text(record["project"]))
	if record["status"] != nil {
		scope.Set("draft", id)
		if record["project"] == nil {
			scope.Set("project", "null")
		}
	} else {
		scope.Set("todo", id)
	}
	decorate := func(value any) any {
		list := fleet.Items(value)
		result := []any{}
		for _, entry := range list {
			meta := mapValue(entry)
			if meta == nil {
				continue
			}
			copy := clone(meta)
			image := fleet.Text(copy["id"])
			if !imageID.MatchString(image) {
				continue
			}
			copy["url"] = strings.TrimRight(origin, "/") + "/api/images/" + image + "?" + scope.Encode()
			result = append(result, copy)
		}
		return result
	}
	if _, ok := out["images"]; ok {
		out["images"] = decorate(out["images"])
	}
	if _, ok := out["messages"]; ok {
		messages := fleet.Items(out["messages"])
		next := []any{}
		for _, item := range messages {
			message := mapValue(item)
			if message == nil {
				next = append(next, item)
				continue
			}
			copy := clone(message)
			if _, ok := copy["images"]; ok {
				copy["images"] = decorate(copy["images"])
			}
			next = append(next, copy)
		}
		out["messages"] = next
	}
	return out
}
func clone(v map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range v {
		out[key] = value
	}
	return out
}

func mapValue(value any) map[string]any {
	if record, ok := value.(map[string]any); ok {
		return record
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var record map[string]any
	if json.Unmarshal(raw, &record) != nil {
		return nil
	}
	return record
}
