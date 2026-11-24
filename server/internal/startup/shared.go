package startup

import (
	"context"
	"errors"
	"strings"

	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
)

func (c *Config) Shared(ctx context.Context, source *fleet.Service) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	_, assignments, err := source.Load()
	if err != nil {
		return nil, errors.New("担当表と配布設定を読み取れません")
	}
	groups, disabled := filterRepositoryGroups(assignments.RepositoryGroups, c.Repositories)
	c.groupWarningState().report(disabled)
	value := fleet.Object(fleet.Clone(c.Values))
	if c.RepositoryEntries != nil {
		value["repositories"] = fleet.Clone(c.RepositoryEntries)
	}
	output := fleet.Object(fleet.Clone(assignments.Output))
	if output == nil {
		output = map[string]any{}
	}
	output["repositoryGroups"] = fleet.Clone(groups)
	value["assignments"] = output
	intake := fleet.Object(value["intake"])
	if intake == nil {
		intake = map[string]any{}
		value["intake"] = intake
	}
	intake["provider"] = "codex"
	if err = fleet.ApplyModels(value, assignments.Output["models"]); err != nil {
		return nil, err
	}
	return value, nil
}
func (c *Config) Project(name string) error {
	for _, configured := range c.Repositories {
		if strings.EqualFold(name, configured) {
			return nil
		}
	}
	return contract.NewProblem(400, "許可されていないプロジェクトです。", "")
}
func (c *Config) CredentialPath() (string, error) {
	if _, err := privateFile(c.TokenFile); err != nil {
		return "", errors.New("Server専用の認証ファイルを確認できません")
	}
	return c.TokenFile, nil
}
