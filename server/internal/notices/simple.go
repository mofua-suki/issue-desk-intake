package notices

import (
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"sort"
	"strings"
	"time"
)

func (s *Service) simple(st settings, host string, body map[string]any) (any, error) {
	switch body["kind"] {
	case "boot-check", "remote-control":
		return s.bootNotice(st, host, body)
	case "format-version":
		return s.formatVersionNotice(st, host, body)
	case "stage-watch":
		return s.stageWatchNotice(st, host, body)
	}
	return nil, problem(400, "お知らせの種類が不正です。")
}

// bootNotice は起動後の点検・remote-control の配布で戻せなかった対象を検査し、お知らせにする。
func (s *Service) bootNotice(st settings, host string, body map[string]any) (any, error) {
	boot := body["kind"] == "boot-check"
	key := "instances"
	max := 20
	pattern := `^[A-Za-z0-9_.@-]{1,64}$`
	if boot {
		key = "items"
		max = 30
		pattern = `^[A-Za-z0-9_.@:-]{1,80}$`
	}
	list, ok := body[key].([]any)
	if !only(body, "kind", "signature", key) || !regexp.MustCompile(`^[a-fA-F0-9]{8,64}$`).MatchString(fleet.Text(body["signature"])) || !ok || len(list) > max || boot && len(list) == 0 {
		return nil, problem(400, "起動点検のお知らせの形が違います。")
	}
	for _, v := range list {
		if !regexp.MustCompile(pattern).MatchString(fleet.Text(v)) {
			return nil, problem(400, "起動点検の対象の形が違います。")
		}
	}
	signature := strings.ToLower(fleet.Text(body["signature"]))
	id := uuid([]any{host, signature})
	title := "remote-controlの配布に失敗: " + host
	reason := "起動しなかったインスタンス: " + strings.Join(fleet.Strings(list), ", ") + "。前の定義への復元を試みました。同じ配布定義は再適用しません。署名: " + signature
	if boot {
		id = uuid([]any{host, "boot-check", signature})
		title = "NASの起動後の点検で戻せませんでした: " + host
		reason = "戻せなかった対象: " + strings.Join(fleet.Strings(list), ", ") + "。docs/nas-worker-update.md の起動後の点検の節と journalctl -b -u issue-desk-boot-check.service で確認してください。signature: " + signature
	}
	return s.append(st, id, host, title, reason)
}

// formatVersionNotice は Python 整形の固定版と導入版の不一致を検査し、お知らせにする。
func (s *Service) formatVersionNotice(st settings, host string, body map[string]any) (any, error) {
	pinned, ok := body["pinned"].([]any)
	version := regexp.MustCompile(`^[0-9][0-9A-Za-z.+-]{0,39}$`)
	if !only(body, "kind", "repo", "tool", "code", "pinned", "installed") || !fleet.ValidRepo(fleet.Text(body["repo"])) || (body["tool"] != "black" && body["tool"] != "ruff") || (body["code"] != float64(78) && body["code"] != float64(79)) || !ok || len(pinned) > 10 {
		return nil, problem(400, "Python整形の版の形が違います。")
	}
	for _, v := range pinned {
		if !version.MatchString(fleet.Text(v)) {
			return nil, problem(400, "固定版の形が違います。")
		}
	}
	if body["installed"] != nil && !version.MatchString(fleet.Text(body["installed"])) {
		return nil, problem(400, "導入版の形が違います。")
	}
	versions := fleet.Strings(pinned)
	sort.Strings(versions)
	id := uuid([]any{body["repo"], body["tool"], versions, body["installed"]})
	return s.append(st, id, host, "Python整形の版が不一致: "+fleet.Text(body["repo"])+" ("+fleet.Text(body["tool"])+")", fmt.Sprintf("対象: %s、ツール: %s、固定版: %s、イメージの版: %v。リポジトリの固定版または配布イメージを確認してください。", body["repo"], body["tool"], strings.Join(versions, ", "), body["installed"]))
}

// stageWatchNotice は段取りの異常の報告を検査し、お知らせにする。
func (s *Service) stageWatchNotice(st settings, host string, body map[string]any) (any, error) {
	anomaly := fleet.Text(body["anomaly"])
	labels := map[string]string{"exit": "終了コードが0以外", "empty": "出力が足りない", "late": "予定時刻より遅れて終了", "overdue": "予定時刻を過ぎても実行中"}
	_, valid := labels[anomaly]
	_, atOK := timestamp(body["startedAt"])
	if !only(body, "kind", "name", "anomaly", "exitCode", "outputs", "lateMin", "startedAt") || !regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`).MatchString(fleet.Text(body["name"])) || !valid || !atOK {
		return nil, problem(400, "段取りのお知らせの形が違います。")
	}
	for _, key := range []string{"exitCode", "outputs", "lateMin"} {
		value, exists := body[key]
		min, max := 0.0, 1000000.0
		if key == "exitCode" {
			min, max = -2147483648, 4294967295
		}
		if !exists || value != nil && !integer(value, min, max) {
			return nil, problem(400, "段取りの数値の形が違います。")
		}
	}
	id := uuid([]any{host, body["name"], anomaly, body["startedAt"]})
	return s.append(st, id, host, "段取りの異常: "+fleet.Text(body["name"])+"（"+host+"）", fmt.Sprintf("種類: %s（%s）、終了コード: %v、出力の枚数: %v、遅れ: %v分、開始: %v。段取りの出力を確認してください。", anomaly, labels[anomaly], body["exitCode"], body["outputs"], body["lateMin"], body["startedAt"]))
}

func (s *Service) checkout(st settings, host string, body map[string]any) (any, error) {
	observed, ok := timestamp(body["observedAt"])
	blocked, blockedOK := timestamp(body["blockedSince"])
	if !only(body, "kind", "observedAt", "blockedSince") || !ok || observed.After(s.now().Add(time.Minute)) || body["blockedSince"] != nil && (!blockedOK || blocked.After(observed)) {
		return nil, problem(400, "checkoutの排他の報告形式が違います。")
	}
	rows, err := st.read("desk-checkout-lock-reports")
	if err != nil {
		return nil, err
	}
	next := []any{}
	for _, raw := range rows {
		row := fleet.Object(raw)
		if row["host"] == host {
			previous, _ := timestamp(row["observedAt"])
			if !observed.After(previous) {
				if observed.Equal(previous) && body["blockedSince"] != row["blockedSince"] {
					return nil, problem(409, "同じ観測時刻の状態が違います。")
				}
				return map[string]any{"kind": "desk-checkout-lock"}, nil
			}
		} else {
			next = append(next, raw)
		}
	}
	next = append(next, map[string]any{"host": host, "observedAt": body["observedAt"], "blockedSince": body["blockedSince"]})
	return map[string]any{"kind": "desk-checkout-lock"}, st.save("desk-checkout-lock-reports", next)
}
