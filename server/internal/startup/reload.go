package startup

import (
	"errors"
	"reflect"
	"strings"

	"issue-desk-intake/server/internal/fleet"
)

func (a *App) validatedConfig(value map[string]any) (*Config, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	input := fleet.Object(fleet.Clone(value))
	if declaration, ok := a.Config.Values["projectCreation"]; ok && input["projectCreation"] == nil {
		input["projectCreation"] = fleet.Clone(declaration)
	}
	next, err := normalize(a.Config.File, a.Config.ApplicationDir, input)
	if err != nil {
		return nil, err
	}
	old := fleet.Object(fleet.Clone(a.Config.Values))
	candidate := fleet.Object(fleet.Clone(next.Values))
	delete(old, "repositories")
	delete(candidate, "repositories")
	if !reflect.DeepEqual(old, candidate) {
		return nil, errors.New("プロジェクト登録ではリポジトリ以外の設定を変更できません")
	}
	byName := map[string]any{}
	for _, v := range fleet.List(next.Values["repositories"]) {
		entry := fleet.Object(v)
		byName[strings.ToLower(fleet.Text(entry["name"]))] = entry
	}
	for _, v := range fleet.List(a.Config.Values["repositories"]) {
		entry := fleet.Object(v)
		if !reflect.DeepEqual(entry, byName[strings.ToLower(fleet.Text(entry["name"]))]) {
			return nil, errors.New("登録済みプロジェクトの設定を変更できません")
		}
	}
	if err := next.mergeFleetRepositories(); err != nil {
		return nil, err
	}
	return next, nil
}
func (a *App) ValidateConfig(value map[string]any) error {
	_, err := a.validatedConfig(value)
	return err
}
func (a *App) Reload(value map[string]any) error {
	next, err := a.validatedConfig(value)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// Keep the warning state so a reload does not repeat an unchanged mismatch.
	next.groupWarnings = a.Config.groupWarningState()
	a.Config = next
	return nil
}
