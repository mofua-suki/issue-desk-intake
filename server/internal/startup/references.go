package startup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"issue-desk-intake/server/internal/consultationai"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/giteareads"
	"issue-desk-intake/server/internal/references"
)

func (a *App) reference(ctx context.Context, d consultations.Record, name string) (consultationai.Reference, error) {
	var text string
	var err error
	switch name {
	case "gitea-api":
		return a.giteaReference(ctx)
	case "open-requests":
		text, err = a.openRequests(ctx, d)
	case "docs/intake-skill-details.md", "intake-skill-details":
		root, e := os.OpenRoot(a.CurrentConfig().ApplicationDir)
		if e != nil {
			return consultationai.Reference{}, e
		}
		defer root.Close()
		f, e := root.Open(filepath.Join("docs", "intake-skill-details.md"))
		if e != nil {
			return consultationai.Reference{}, e
		}
		defer f.Close()
		data, e := io.ReadAll(io.LimitReader(f, (160<<10)+1))
		if e != nil || len(data) > 160<<10 {
			return consultationai.Reference{}, errors.New("相談の判断資料を上限内で読めません")
		}
		text = string(data)
	case "desk-numbers":
		v, e := a.resolveNumbers(ctx, d)
		err = e
		if err == nil {
			raw, e := json.Marshal(v)
			text = string(raw)
			err = e
		}
	case "measurement":
		if a.MeasurementReference == nil {
			return consultationai.Reference{}, errors.New("稼働の計測資料の接続を準備中です")
		}
		text, err = a.MeasurementReference(ctx, fleet.Text(d["project"]))
	case "rework-by-size":
		if a.SizeReference == nil {
			return consultationai.Reference{}, errors.New("規模別の手戻り資料の接続を準備中です")
		}
		text, err = a.SizeReference(ctx, fleet.Text(d["project"]))
	default:
		return consultationai.Reference{}, errors.New("予約された参照資料ではありません")
	}
	return consultationai.Reference{Version: digestText(text), Text: text}, err
}

func (a *App) giteaReference(ctx context.Context) (consultationai.Reference, error) {
	a.apiMu.Lock()
	defer a.apiMu.Unlock()
	if a.apiReference != nil {
		return *a.apiReference, nil
	}
	base := strings.TrimRight(fleet.Text(a.CurrentConfig().Values["giteaUrl"]), "/")
	request, err := http.NewRequestWithContext(ctx, "GET", base+"/swagger.v1.json", nil)
	if err != nil {
		return consultationai.Reference{}, errors.New("Giteaの仕様書の接続先が不正です")
	}
	client := &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		giteareads.Process.Record("GET", "/swagger.v1.json", 0, true)
		return consultationai.Reference{}, errors.New("Giteaの仕様書を取得できません")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
	giteareads.Process.Record("GET", "/swagger.v1.json", len(data), err != nil || response.StatusCode != 200)
	if err != nil || response.StatusCode != 200 || len(data) > 8<<20 {
		return consultationai.Reference{}, errors.New("Giteaの仕様書を上限内で読めません")
	}
	var document map[string]any
	if json.Unmarshal(data, &document) != nil || fleet.Object(document["paths"]) == nil || document["swagger"] == nil && document["openapi"] == nil {
		return consultationai.Reference{}, errors.New("Giteaの仕様書の形式が不正です")
	}
	lines := []string{}
	paths := fleet.Object(document["paths"])
	for _, path := range fleet.Keys(paths) {
		item := fleet.Object(paths[path])
		for _, method := range fleet.Keys(item) {
			if !strings.Contains(" get put post delete options head patch trace ", " "+method+" ") {
				continue
			}
			operation := fleet.Object(item[method])
			if operation == nil {
				continue
			}
			summary := strings.Join(strings.Fields(fleet.Text(operation["summary"])+" "+fleet.Text(operation["description"])), " ")
			if clipped := contract.ClipUTF16(summary, 500); clipped != summary {
				summary = clipped + "…（要約を省略）"
			}
			id := fleet.Text(operation["operationId"])
			if id == "" {
				id = "-"
			}
			lines = append(lines, strings.TrimSpace(strings.ToUpper(method)+" "+path+" "+strings.Join(strings.Fields(id), " ")+" "+summary))
		}
	}
	if len(lines) == 0 {
		return consultationai.Reference{}, errors.New("Giteaの操作一覧が空です")
	}
	value := consultationai.Reference{Version: digestText(string(data)), Text: strings.Join(lines, "\n") + "\n"}
	a.apiReference = &value
	return value, nil
}

func (a *App) referenceContinuation(ctx context.Context, d consultations.Record, cursor string) (consultationai.Reference, error) {
	text, err := a.openRequestsPage(ctx, d, cursor)
	return consultationai.Reference{Version: digestText(text), Text: text}, err
}

var openContinuationLine = regexp.MustCompile(`(?m)^  重なり候補の続き: [a-f0-9]{64}:[01]:[0-9]+$`)

func (a *App) openRequests(ctx context.Context, d consultations.Record) (string, error) {
	return a.openRequestsPage(ctx, d, "")
}

func (a *App) openRequestsPage(ctx context.Context, d consultations.Record, cursor string) (string, error) {
	repo := fleet.Text(d["project"])
	if err := a.Project(repo); err != nil {
		return "", err
	}
	issues, err := a.Gitea.Issues(ctx, repo, "open")
	if err != nil {
		return "", err
	}
	requests := []map[string]any{}
	for _, issue := range issues {
		if issue["pull_request"] != nil || administrativeIssue(repo, issue) {
			continue
		}
		if !a.Own(fleet.Object(issue["user"])) {
			continue
		}
		requests = append(requests, issue)
	}
	a.attachOpenRequestState(ctx, repo, requests)
	sort.SliceStable(requests, func(i, j int) bool {
		return fleet.Text(requests[i]["created_at"]) > fleet.Text(requests[j]["created_at"])
	})
	all, err := a.Records.List(ctx, "todos")
	if err != nil {
		return "", err
	}
	todos := []map[string]any{}
	for _, todo := range all {
		if todo["project"] == repo {
			todos = append(todos, map[string]any(todo))
		}
	}
	sort.SliceStable(todos, func(i, j int) bool { return fleet.Text(todos[i]["createdAt"]) > fleet.Text(todos[j]["createdAt"]) })
	if cursor != "" {
		page, err := references.OpenContinuation(repo, requests, todos, map[string]any(d), cursor)
		if err != nil {
			return "", fmt.Errorf("重なり候補の続きを読めません: %w", err)
		}
		return page.Text + "続き位置は最初の資料と同じ方法で指定します。位置がなければ候補は終わりです。\n", nil
	}
	result := references.OpenExpanded(repo, requests, todos, map[string]any(d))
	if openContinuationLine.MatchString(result) {
		result += "重なり候補の続きは readFiles に {\"file\":\"open-requests\",\"mode\":\"keyword\",\"keyword\":\"続き位置\",\"start\":0,\"end\":0,\"context\":5} を指定して読みます。\n"
	}
	return result, nil
}
