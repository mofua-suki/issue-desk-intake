package startup

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
)

func accountID(user map[string]any) (int64, bool) {
	return positiveNumber(user["id"])
}
func (a *App) resolveWriters(ctx context.Context, identity map[string]any) error {
	id, ok := accountID(identity)
	if !ok {
		return errors.New("Server専用のGiteaアカウントIDを確認できません")
	}
	a.intakeID = id
	a.ownIDs = map[int64]bool{id: true}
	a.writerIDs = map[int64]bool{}
	values, err := a.Values()
	if err != nil {
		return err
	}
	legacy := fleet.Text(values["gitUser"])
	if legacy == "" {
		return errors.New("既存の依頼を書いたGiteaアカウントを設定してください")
	}
	user := map[string]any{}
	if err = a.Gitea.Do(ctx, "GET", "/users/"+url.PathEscape(legacy), nil, &user); err != nil {
		return errors.New("既存の依頼を書いたGiteaアカウントを確認できません")
	}
	legacyID, ok := accountID(user)
	if !ok || !strings.EqualFold(fleet.Text(user["login"]), legacy) {
		return errors.New("既存の依頼を書いたGiteaアカウントIDを確認できません")
	}
	a.ownIDs[legacyID] = true
	a.writerIDs[legacyID] = true
	seen := map[string]bool{strings.ToLower(legacy): true}
	accounts := fleet.Object(fleet.Object(values["assignments"])["workerAccounts"])
	for _, key := range fleet.Keys(accounts) {
		login := fleet.Text(accounts[key])
		if login == "" || seen[strings.ToLower(login)] {
			continue
		}
		seen[strings.ToLower(login)] = true
		actor := map[string]any{}
		if err = a.Gitea.Do(ctx, "GET", "/users/"+url.PathEscape(login), nil, &actor); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			a.Log("監視アカウントのIDを確認できないため、そのアカウントの状態投稿は信用しません。原因: " + contract.SafeCause(err))
			continue
		}
		actorID, ok := accountID(actor)
		if ok && strings.EqualFold(fleet.Text(actor["login"]), login) {
			a.writerIDs[actorID] = true
		} else {
			a.Log("監視アカウントのIDが不正なため、そのアカウントの状態投稿は信用しません。")
		}
	}
	return nil
}
