package startup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
)

// Destinations come only from administrator-created Gitea hooks.
func (a *App) projectWebhook(ctx context.Context, admin *gitea.Client, repo string) (map[string]any, error) {
	missing := func(reason string) (map[string]any, error) {
		return map[string]any{"result": "missing", "reason": reason}, nil
	}
	values, e := a.Values()
	if e != nil {
		return nil, e
	}
	config := a.CurrentConfig()
	secret, reason := webhookSecret(values, config.ApplicationDir, admin)
	if reason != "" {
		return missing(reason)
	}
	excluded, reason := registeredWebhookURLs(config.RuntimeDir)
	if reason != "" {
		return missing(reason)
	}
	target, reason := a.webhookTarget(ctx, admin, repo, secret, excluded)
	if reason != "" {
		return missing(reason)
	}
	hooks, e := admin.Pages(ctx, "/repos/"+repo+"/hooks")
	if e != nil {
		return missing("新しいプロジェクトのWebhookを確認できません。")
	}
	method, route := "POST", "/repos/"+repo+"/hooks"
	for _, hook := range hooks {
		if fleet.Object(hook["config"])["url"] == target {
			method = "PATCH"
			route += fmt.Sprintf("/%v", hook["id"])
			break
		}
	}
	payload := map[string]any{"type": "gitea", "active": true, "branch_filter": "*", "events": []string{"issues", "issue_comment", "pull_request", "push"}, "authorization_header": "Bearer " + secret, "config": map[string]any{"url": target, "content_type": "json"}}
	if e = admin.Do(ctx, method, route, payload, nil); e != nil {
		return map[string]any{"result": "failed", "reason": "Webhookを登録できません。定期確認を継続します。"}, nil
	}
	return map[string]any{"result": "registered", "url": target}, nil
}

// webhookSecret はWebhookの認証ファイルを所有者だけが読める形で読み、認証の値を返す。使えなければその理由を返す。
func webhookSecret(values map[string]any, applicationDir string, admin *gitea.Client) (string, string) {
	file := fleet.Text(fleet.Object(values["webhook"])["tokenFile"])
	if file == "" || admin == nil {
		return "", "Webhookの認証ファイルが設定されていません。"
	}
	credentialFile := absolute(applicationDir, file)
	info, e := privateFile(credentialFile)
	if e != nil {
		return "", "Webhookの認証ファイルを読み取れません。"
	}
	if info.Size() > 65536 {
		return "", "Webhookの認証ファイルが大きすぎます。"
	}
	raw, e := os.ReadFile(credentialFile)
	if e != nil {
		return "", "Webhookの認証ファイルを読み取れません。"
	}
	secret := strings.TrimSpace(string(raw))
	if len(secret) < 32 || strings.ContainsAny(secret, " \r\n\t") {
		return "", "Webhookの認証ファイルを確認できません。"
	}
	return secret, ""
}

// registeredWebhookURLs は保存済みのWebhookの登録に含まれる送信先を集める。これらは送信先の候補から外す。
func registeredWebhookURLs(runtimeDir string) (map[string]bool, string) {
	excluded := map[string]bool{}
	registrations, e := os.ReadFile(filepath.Join(runtimeDir, "state", "webhook-registrations.json"))
	if e == nil {
		var stored any
		if json.Unmarshal(registrations, &stored) != nil {
			return nil, "保存済みWebhookの登録を読み取れません。"
		}
		var visit func(any)
		visit = func(v any) {
			switch x := v.(type) {
			case map[string]any:
				for key, value := range x {
					if key == "url" {
						excluded[fleet.Text(value)] = true
					} else {
						visit(value)
					}
				}
			case []any:
				for _, v := range x {
					visit(v)
				}
			}
		}
		visit(stored)
	} else if !os.IsNotExist(e) {
		return nil, "保存済みWebhookの登録を読み取れません。"
	}
	return excluded, ""
}

// webhookTarget は管理者が登録したほかのリポジトリのWebhookから、受付への送信先を1つに確定する。
func (a *App) webhookTarget(ctx context.Context, admin *gitea.Client, repo, secret string, excluded map[string]bool) (string, string) {
	repos := a.Repositories()
	home := []string{}
	others := []string{}
	for _, r := range repos {
		if strings.EqualFold(r, repo) {
			continue
		}
		others = append(others, r)
		if a.Home(r) == "nas" {
			home = append(home, r)
		}
	}
	if len(home) > 0 {
		repos = home
	} else {
		repos = others
	}
	urls := map[string]bool{}
	for _, r := range repos {
		hooks, e := admin.Pages(ctx, "/repos/"+r+"/hooks")
		if e != nil {
			return "", "登録済みWebhookを確認できません。"
		}
		for _, hook := range hooks {
			target := fleet.Text(fleet.Object(hook["config"])["url"])
			header := fleet.Text(hook["authorization_header"])
			if !strings.HasSuffix(target, "/webhooks/issue-desk") || excluded[target] || header != "" && !strings.Contains(header, "*") && header != "Bearer "+secret {
				continue
			}
			urls[target] = true
		}
	}
	if len(urls) != 1 {
		return "", "Webhookの送信先を1つに確定できません。"
	}
	target := ""
	for value := range urls {
		target = value
	}
	return target, ""
}
