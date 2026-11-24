// PCへ配る実行設定（ClientConfig）を組み立てる。

package fleet

import (
	"context"
	"errors"
	"regexp"

	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/reviewnotes"
)

func (s *Service) ClientConfig(ctx context.Context, host, since string) (map[string]any, error) {
	document, assignments, err := s.Load()
	if err != nil {
		return nil, err
	}
	_, exists := document.Hosts[host]
	if !exists || assignments.Hosts[host] == nil {
		return nil, contract.NewProblem(404, "このPCの設定は登録されていません。", "")
	}
	config, names, err := hostConfig(document, assignments, host)
	if err != nil {
		return nil, err
	}
	if s.ReleasePolicy != nil {
		if policy := s.ReleasePolicy(host, config); policy != nil {
			config["releasePolicy"] = Clone(policy)
		}
	}
	verifierState := s.addWebhookVerifier(ctx, host, config)
	settings, version, err := s.Settings(ctx)
	if err != nil {
		return nil, err
	}
	reviewNotes, err := s.publishedReviewNotes(ctx, names)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"config": config, "configurationVersion": Digest(config), "settings": settings, "settingsVersion": version, "reviewNotes": reviewNotes}
	if verifierState != nil {
		body["webhookVerifierState"] = verifierState
	}
	combined := Digest(body)
	if since == combined {
		return map[string]any{"version": combined, "unchanged": true}, nil
	}
	body["version"] = combined
	return body, nil
}

// hostConfig merges the PC's settings over the defaults with the models, and
// checks that every assigned repository is configured.
func hostConfig(document Document, assignments *Assignments, host string) (map[string]any, map[string]bool, error) {
	entry := document.Hosts[host]
	config := Object(Clone(document.Defaults))
	for key, value := range entry {
		config[key] = Clone(value)
	}
	if err := applyModels(config, assignments.Output["models"]); err != nil {
		return nil, nil, err
	}
	delete(config, "intake")
	// Environment declarations are consumed by intake, not delivered to Workers.
	delete(config, "environment")
	names := map[string]bool{}
	for _, value := range List(config["repositories"]) {
		repo := Text(Object(value)["name"])
		if !ValidRepo(repo) || names[repo] {
			return nil, nil, errors.New("fleetのリポジトリが不正か重複しています")
		}
		names[repo] = true
	}
	for repo, list := range assignments.Implementers {
		if (contains(list, host) || assignments.DeployHosts[repo] == host) && !names[repo] {
			return nil, nil, errors.New("担当リポジトリのfleet設定がありません")
		}
	}
	nativeVersions := map[string]string{}
	for name, entry := range assignments.Hosts {
		if len(entry.TestRepositories) > 0 {
			nativeVersions[name] = Digest(EffectiveConfig(document, name))
		}
	}
	config["nativeTestVersions"] = nativeVersions
	config["hostRole"] = assignments.Hosts[host].Role
	config["testRepositories"] = assignments.Hosts[host].TestRepositories
	config["assignments"] = assignments.Output
	return config, names, nil
}

// addWebhookVerifier adds the verifier of a managed webhook to the config and
// returns whether it is known and available; nil for an unmanaged webhook.
func (s *Service) addWebhookVerifier(ctx context.Context, host string, config map[string]any) any {
	var verifierState any
	if Object(config["webhook"])["managed"] == true {
		known, available := false, false
		if s.WebhookVerifier != nil {
			verifier, verifierErr := s.WebhookVerifier(ctx, host, config)
			available = verifierErr == nil
			if regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(verifier) {
				config["webhookVerifier"] = verifier
				known = true
			}
		}
		verifierState = map[string]any{"known": known, "available": available}
	}
	return verifierState
}

// publishedReviewNotes reads the review notes published for the repositories.
// A malformed snapshot is delivered as no notes.
func (s *Service) publishedReviewNotes(ctx context.Context, names map[string]bool) (map[string]any, error) {
	reviewNotes := map[string]any{}
	rows, err := s.DB.QueryContext(ctx, "SELECT repo,notes FROM repo_note_processing WHERE repo=ANY($1::text[])", KeysBool(names))
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var repo string
			var raw []byte
			if err = rows.Scan(&repo, &raw); err != nil {
				return nil, err
			}
			items, decodeErr := reviewnotes.Published(raw)
			if decodeErr != nil {
				// A malformed repository snapshot must not stop fleet delivery.
				items = []map[string]string{}
			}
			reviewNotes[repo] = items
		}
		if err = rows.Err(); err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}
	return reviewNotes, nil
}
