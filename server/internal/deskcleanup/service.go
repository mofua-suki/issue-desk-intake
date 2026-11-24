package deskcleanup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Service struct {
	DB         *sql.DB
	Auth       *auth.Authenticator
	Project    func(string) error
	Busy       func(string) bool
	Issue      func(context.Context, string, int64) (Object, error)
	History    func(context.Context, string) ([]Object, error)
	Own        func(Object) bool
	CloseIssue func(context.Context, string, int64, string, string, auth.Principal) (any, error)
	Alerts     func(context.Context, string) ([]any, error)
	Publish    func(Object)
	Now        func() time.Time
	mu         sync.Mutex
}

func fail(status int, text string) error { return contract.NewProblem(status, text, "") }
func parts(target string) (string, string, error) {
	kind, id, ok := strings.Cut(target, ":")
	if !ok || id == "" || len(target) > 1000 || kind != "draft" && kind != "issue" && kind != "attention" && kind != "stall" {
		return "", "", fail(400, "終了対象が不正です。")
	}
	return kind, id, nil
}

func canonicalTarget(target string) string {
	if strings.HasPrefix(target, "attention:") && len(target) > len("attention:") && !strings.HasPrefix(target, "attention:attention:") {
		return "attention:" + target
	}
	return target
}

var issueRef = regexp.MustCompile(`^([^/\s]+/[^#\s]+)#([1-9][0-9]*)$`)

func reference(id string) (string, int64, error) {
	m := issueRef.FindStringSubmatch(id)
	if m == nil || !fleet.ValidRepo(m[1]) {
		return "", 0, fail(400, "Issueの対象が不正です。")
	}
	n, e := strconv.ParseInt(m[2], 10, 64)
	if e != nil || n > 9007199254740991 {
		return "", 0, fail(400, "Issueの番号が不正です。")
	}
	return m[1], n, nil
}
func draft(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (Object, error) {
	var raw string
	e := q.QueryRowContext(ctx, bindSQL(`SELECT json FROM drafts WHERE id=?`), id).Scan(&raw)
	if e == sql.ErrNoRows {
		return nil, fail(404, "相談が見つかりません。")
	}
	if e != nil {
		return nil, e
	}
	var d Object
	e = json.Unmarshal([]byte(raw), &d)
	return d, e
}
func draftValue(d Object) Object {
	return Object{"id": d["id"], "project": d["project"], "status": d["status"], "revision": d["revision"], "createdAt": d["createdAt"], "issue": d["issue"], "issues": append([]any{}, fleet.List(d["issues"])...), "splitChildren": append([]any{}, fleet.List(d["splitChildren"])...), "consultationChildren": append([]any{}, fleet.List(d["consultationChildren"])...), "attempted": d["attempted"] == true, "projectHandoff": d["projectHandoff"]}
}
func (s *Service) Inspect(ctx context.Context, target string) (Object, error) {
	target = canonicalTarget(target)
	kind, id, e := parts(target)
	if e != nil {
		return nil, e
	}
	var value Object
	switch kind {
	case "draft":
		d, err := draft(ctx, s.DB, id)
		if err != nil {
			return nil, err
		}
		value = draftValue(d)
	case "issue":
		repo, n, err := reference(id)
		if err != nil {
			return nil, err
		}
		if e = s.Project(repo); e != nil {
			return nil, e
		}
		d, err := s.Issue(ctx, repo, n)
		if err != nil {
			return nil, err
		}
		value = issueSnapshot(repo, n, d)
	default:
		rows, err := s.Alerts(ctx, kind)
		if err != nil {
			return nil, err
		}
		for _, raw := range rows {
			r := fleet.Object(raw)
			if AlertID(kind, r) == id {
				value = r
				break
			}
		}
		if value == nil {
			records, err := Archived(ctx, s.DB)
			if err != nil {
				return nil, err
			}
			for i := len(records) - 1; i >= 0; i-- {
				if records[i]["target"] == target {
					return Object{"target": target, "archived": true, "record": records[i]}, nil
				}
			}
			return nil, fail(404, "現在の通知が見つかりません。")
		}
		if repo := fleet.Text(value["project"]); repo != "" && repo != fleet.SystemProject && repo != fleet.LegacySystemProject {
			if e = s.Project(repo); e != nil {
				return nil, e
			}
		}
	}
	fp := Fingerprint(value)
	if kind == "attention" || kind == "stall" {
		fp = AlertFingerprint(value)
	}
	return Object{"target": target, "value": value, "fingerprint": fp}, nil
}
func (s *Service) Apply(ctx context.Context, data Object, p auth.Principal) (Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	target, fp := canonicalTarget(fleet.Text(data["target"])), fleet.Text(data["fingerprint"])
	kind, id, e := parts(target)
	if e != nil {
		return nil, e
	}
	reason, statement := fleet.Text(data["reason"]), fleet.Text(data["userStatement"])
	if strings.TrimSpace(reason) == "" || contract.UTF16Length(reason) > 2000 || strings.TrimSpace(statement) == "" || contract.UTF16Length(statement) > 8000 {
		return nil, fail(400, "終了の理由と利用者の指示原文が必要です。")
	}
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(fp) {
		return nil, fail(400, "照合値が不正です。")
	}
	records, e := Archived(ctx, s.DB)
	if e != nil {
		return nil, e
	}
	if closed, found, e := s.archivedClose(ctx, records, kind, id, target, fp); e != nil || found {
		return closed, e
	}
	candidate, e := s.Inspect(ctx, target)
	if e != nil {
		return nil, e
	}
	if candidate["fingerprint"] != fp {
		return nil, fail(409, "確認後に対象が変わりました。読み直してください。")
	}
	via := "screen"
	if p.Kind == auth.PC {
		via = "token:" + p.Name
	}
	result, refs, e := s.closeCleanupTarget(ctx, kind, id, reason, candidate, p)
	if e != nil {
		return nil, e
	}
	at := time.Now()
	if s.Now != nil {
		at = s.Now()
	}
	record := Object{"target": target, "fingerprint": fp, "snapshot": candidate["value"], "reason": reason, "userStatement": statement, "via": via, "at": at.UTC().Format(time.RFC3339Nano), "result": result}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	if kind == "draft" {
		if e = s.closeDraftRecord(ctx, tx, id, fp, Object{"reason": reason, "userStatement": statement, "via": via, "at": record["at"], "refs": refs}); e != nil {
			return nil, e
		}
	}
	if e = saveArchive(ctx, tx, record); e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	if s.Publish != nil {
		s.Publish(Object{"type": "list"})
	}
	return Object{"target": target, "closed": true, "record": record}, nil
}

// archivedClose は同じ照合値で終了済みの記録があれば、その結果を返す。記録の後に対象が再開されていれば断る。
func (s *Service) archivedClose(ctx context.Context, records []Object, kind, id, target, fp string) (Object, bool, error) {
	for i := len(records) - 1; i >= 0; i-- {
		r := records[i]
		if r["target"] != target || r["fingerprint"] != fp {
			continue
		}
		terminal := true
		if kind == "draft" {
			d, err := draft(ctx, s.DB, id)
			if err != nil {
				return nil, false, err
			}
			terminal = d["status"] == "closed"
		} else if kind == "issue" {
			repo, n, _ := reference(id)
			d, err := s.Issue(ctx, repo, n)
			if err != nil {
				return nil, false, err
			}
			terminal = d["state"] == "closed"
		}
		if terminal {
			return Object{"target": target, "closed": true, "record": r}, true, nil
		}
		return nil, false, fail(409, "終了後に対象が再開されています。読み直してください。")
	}
	return nil, false, nil
}

// closeCleanupTarget は対象を閉じる。相談は関連Issueがすべて閉じているかを確かめ、Issueは受付の終了操作で閉じる。
func (s *Service) closeCleanupTarget(ctx context.Context, kind, id, reason string, candidate Object, p auth.Principal) (any, []Object, error) {
	var e error
	result := any(Object{"acknowledged": true})
	var refs []Object
	if kind == "draft" {
		d, err := draft(ctx, s.DB, id)
		if err != nil {
			return nil, nil, err
		}
		if s.Busy(id) || d["queued"] == true || d["retryAt"] != nil {
			return nil, nil, fail(409, "相談の処理中です。終了を待ってください。")
		}
		refs, err = s.draftIssues(ctx, d)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range refs {
			repo, n := fleet.Text(r["project"]), int64(fleet.Number(r["number"]))
			if e = s.Project(repo); e != nil {
				return nil, nil, e
			}
			detail, err := s.Issue(ctx, repo, n)
			if err != nil {
				return nil, nil, err
			}
			if detail["state"] != "closed" {
				return nil, nil, fail(409, "関連Issueが開いています。先にIssueを終了してください。")
			}
		}
		result = Object{"status": "closed", "refs": refs}
	} else if kind == "issue" {
		repo, n, _ := reference(id)
		result, e = s.CloseIssue(ctx, repo, n, reason, fleet.Text(fleet.Object(candidate["value"])["updatedAt"]), p)
		if e != nil {
			return nil, nil, e
		}
	}
	return result, refs, nil
}

// closeDraftRecord は取引の中で相談と子の相談が確認時から変わっていないかを照合し、相談を終了済みとして保存する。
func (s *Service) closeDraftRecord(ctx context.Context, tx *sql.Tx, id, fp string, cleanup Object) error {
	d, err := draft(ctx, tx, id)
	if err != nil {
		return err
	}
	if Fingerprint(draftValue(d)) != fp || s.Busy(id) || d["queued"] == true || d["retryAt"] != nil {
		return fail(409, "照合中に相談が変わりました。読み直してください。")
	}
	for _, v := range append(fleet.List(d["splitChildren"]), fleet.List(d["consultationChildren"])...) {
		childID := fleet.Text(v)
		child, err := draft(ctx, tx, childID)
		if err != nil {
			return err
		}
		if child["status"] != "closed" && child["status"] != "submitted" || s.Busy(childID) || child["queued"] == true || child["retryAt"] != nil {
			return fail(409, "未終了または処理中の子の相談があります。子から終了してください。")
		}
	}
	d["status"] = "closed"
	d["revision"] = fleet.Number(d["revision"]) + 1
	d["cleanup"] = cleanup
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if _, e := tx.ExecContext(ctx, bindSQL(`UPDATE drafts SET json=? WHERE id=?`), string(raw), id); e != nil {
		return e
	}
	return nil
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, problem := s.Auth.Authenticate(r)
	if problem != nil {
		contract.WriteProblem(w, problem)
		return
	}
	if problem = auth.Authorize(p, auth.Policy{Screen: true, PC: true, RequireDelegation: true}); problem != nil {
		contract.WriteProblem(w, problem)
		return
	}
	var value any
	var e error
	if r.Method == "GET" && r.URL.Path == "/api/desk-cleanup/archive" {
		value, e = Archived(r.Context(), s.DB)
	} else if r.Method == "POST" && (r.URL.Path == "/api/desk-cleanup/inspect" || r.URL.Path == "/api/desk-cleanup/close") {
		data, err := contract.DecodeObject(r.Body, r.Header.Get("Content-Type"), 1<<20)
		if err != nil {
			contract.WriteProblem(w, err)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/inspect") {
			value, e = s.Inspect(r.Context(), fleet.Text(data["target"]))
		} else {
			value, e = s.Apply(r.Context(), data, p)
		}
	} else {
		e = fail(405, "対象のHTTPメソッドを確認してください。")
	}
	if e != nil {
		if problem, ok := e.(*contract.Problem); ok {
			contract.WriteProblem(w, problem)
		} else {
			contract.LogFailure("後片付けのAPI "+r.URL.Path, e)
			contract.WriteProblem(w, contract.NewProblem(503, "保存済みの状態を読み取れません。", ""))
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}

var marker = regexp.MustCompile(`<!-- issue-desk:([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}) -->`)

func (s *Service) draftIssues(ctx context.Context, d Object) ([]Object, error) {
	refs := []Object{}
	invalid := false
	seen := map[string]bool{}
	add := func(r Object) {
		repo := fleet.Text(r["project"])
		if repo == "" {
			repo = fleet.Text(d["project"])
		}
		n := fleet.Number(r["number"])
		if !fleet.ValidRepo(repo) || math.IsNaN(n) || math.IsInf(n, 0) || n < 1 || n > 9007199254740991 || n != math.Trunc(n) {
			invalid = true
			return
		}
		key := fmt.Sprintf("%s#%.0f", repo, n)
		if !seen[key] {
			seen[key] = true
			refs = append(refs, Object{"project": repo, "number": n})
		}
	}
	if r := fleet.Object(d["issue"]); r != nil {
		add(r)
	}
	for _, v := range append(fleet.List(d["issues"]), fleet.List(d["investigations"])...) {
		add(fleet.Object(v))
	}
	if d["attempted"] == true && fleet.Text(d["project"]) != "" {
		rows, e := s.History(ctx, fleet.Text(d["project"]))
		if e != nil {
			return nil, e
		}
		for _, r := range rows {
			matches := marker.FindAllStringSubmatch(fleet.Text(r["body"]), -1)
			if s.Own(fleet.Object(r["user"])) && len(matches) > 0 && matches[len(matches)-1][1] == d["id"] {
				add(Object{"number": r["number"]})
			}
		}
	}
	if invalid {
		return nil, fail(409, "関連Issueの参照を確認できません。読み直してください。")
	}
	return refs, nil
}
