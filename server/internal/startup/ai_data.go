package startup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/contract"
	"regexp"
	"strings"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/consultationai"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/images"
	"issue-desk-intake/server/internal/usage"
)

func (a *App) recordUsage(ctx context.Context, request ai.Request, u ai.Usage, elapsed time.Duration, success bool) error {
	saveContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	raw, err := json.Marshal(u)
	if err != nil {
		return err
	}
	row := map[string]any{}
	if err = json.Unmarshal(raw, &row); err != nil {
		return err
	}
	row["at"], row["host"], row["source"], row["kind"] = contract.ISO(time.Now()), "nas", "intake", "intake"
	row["provider"], row["model"], row["effort"], row["elapsedMs"] = request.Provider, request.Model, request.Effort, float64(elapsed.Milliseconds())
	if request.CallID != "" {
		row["callId"] = request.CallID
	}
	if request.Tier != "" {
		row["tier"] = request.Tier
	}
	row["step"], row["attempt"], row["turn"] = request.Step, float64(request.Attempt), float64(request.Turn)
	for key, value := range map[string]string{"repo": request.Project, "draft": request.Draft} {
		if value != "" {
			row[key] = value
		}
	}
	row["outcome"] = "failed"
	if success {
		row["outcome"] = "completed"
	}
	if mode := usageSessionMode(request); mode != "" {
		row["sessionMode"] = mode
		if !usage.ValidUsage(row) {
			delete(row, "sessionMode")
		}
	}
	// Validate the complete JSON-shaped row, rolling back only optional details.
	if request.CallTrigger != "" {
		row["callTrigger"] = request.CallTrigger
		if !usage.ValidUsage(row) {
			delete(row, "callTrigger")
		}
	}
	if request.RepairKind != "" {
		row["repairKind"] = request.RepairKind
		if request.RepairField != "" {
			row["repairField"] = request.RepairField
		}
		if !usage.ValidUsage(row) {
			delete(row, "repairKind")
			delete(row, "repairField")
		}
	}
	counts, err := (&usage.Store{DB: a.DB}).AddRows(saveContext, "intake", []map[string]any{row})
	if err == nil && counts.Rejected > 0 {
		err = errors.New("AI使用量の形式を保存できません")
	}
	return err
}

var usageSessionID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

func usageSessionMode(request ai.Request) string {
	if request.Provider != "codex" && request.Provider != "claude" {
		return ""
	}
	if request.Session != "" && !usageSessionID.MatchString(request.Session) {
		return ""
	}
	if !request.PersistSession {
		if request.Resume || request.Session != "" {
			return ""
		}
		return "once"
	}
	if request.Resume {
		if request.Session == "" {
			return ""
		}
		return "resumed"
	}
	return "new"
}

// recordLimits saves only the explicit limit fields and never interrupts an AI call.
func (a *App) recordLimits(observations []ai.LimitObservation) {
	rows := make([]map[string]any, 0, len(observations))
	now := contract.ISO(time.Now())
	for _, observation := range observations {
		row := map[string]any{"at": now, "host": "nas", "source": "intake"}
		for _, key := range []string{"provider", "limit", "status", "utilization", "percent", "resetsAt", "windowMinutes"} {
			row[key] = observation[key]
		}
		rows = append(rows, row)
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(a.ctx), 5*time.Second)
	defer cancel()
	counts, err := (&usage.Store{DB: a.DB}).Add(ctx, "intake", map[string]json.RawMessage{"limits": raw})
	if err != nil || counts.Rejected > 0 {
		a.Log("AI利用枠の記録を保存できませんでした")
	}
}

func (a *App) aiImages(ctx context.Context, d consultations.Record) ([]consultationai.Image, error) {
	source, err := a.imageSource(ctx, d)
	if err != nil {
		return nil, err
	}
	order, selected, err := messageImageIDs(d)
	if err != nil {
		return nil, err
	}
	return a.loadConsultationImages(ctx, source, order, selected)
}

// imageSource は画像を保存した相談を決める。元の相談を指定していれば、同じ持ち主の祖先であることを確かめる。
func (a *App) imageSource(ctx context.Context, d consultations.Record) (string, error) {
	source := fleet.Text(d["id"])
	if parent := fleet.Text(d["imageSourceId"]); parent != "" && parent != source {
		current := d
		seen := map[string]bool{}
		found := false
		for i := 0; i < 64; i++ {
			id := fleet.Text(current["parentConsultation"])
			if id == "" {
				id = fleet.Text(current["parentSplit"])
			}
			if id == "" || seen[id] {
				break
			}
			seen[id] = true
			ancestor, ok, err := a.Records.Get(ctx, "drafts", id)
			if err != nil {
				return "", err
			}
			if !ok || fleet.Text(ancestor["owner"]) != fleet.Text(d["owner"]) {
				break
			}
			if id == parent {
				found = true
				break
			}
			current = ancestor
		}
		if !found {
			return "", errors.New("画像の元の相談との関係を確認できません")
		}
		source = parent
	}
	return source, nil
}

// messageImageIDs は発言に添えた画像のIDを出てきた順に集め、各画像を添えた発言を返す。同じ画像が別の発言にあれば断る。
func messageImageIDs(d consultations.Record) ([]string, map[string]string, error) {
	selected := map[string]string{}
	order := []string{}
	for _, raw := range fleet.List(d["messages"]) {
		message := fleet.Object(raw)
		for _, v := range fleet.List(message["images"]) {
			id := fleet.Text(fleet.Object(v)["id"])
			if id == "" {
				return nil, nil, errors.New("画像のIDを読み取れません")
			}
			if before, ok := selected[id]; ok && before != fleet.Text(message["id"]) {
				return nil, nil, errors.New("画像のメッセージが一致しません")
			}
			if _, exists := selected[id]; !exists {
				order = append(order, id)
			}
			selected[id] = fleet.Text(message["id"])
		}
	}
	if len(selected) > images.PerConversation {
		return nil, nil, errors.New("相談の画像の上限を超えています")
	}
	return order, selected, nil
}

// loadConsultationImages は保存した画像を読み、相談・発言・内容の指紋が記録と一致するものだけを返す。
func (a *App) loadConsultationImages(ctx context.Context, source string, order []string, selected map[string]string) ([]consultationai.Image, error) {
	result := []consultationai.Image{}
	for _, id := range order {
		message := selected[id]
		var data []byte
		var metadata, scope, savedMessage string
		if err := a.SQLite.DB().QueryRowContext(ctx, bindSQL("SELECT bytes,json,scope,message FROM images WHERE id=?"), id).Scan(&data, &metadata, &scope, &savedMessage); err != nil {
			return nil, errors.New("保存した画像を読み取れません")
		}
		meta := map[string]any{}
		if json.Unmarshal([]byte(metadata), &meta) != nil || scope != "draft:"+source || savedMessage != message {
			return nil, errors.New("画像の相談またはメッセージが一致しません")
		}
		info, err := images.Inspect(data)
		if err != nil {
			return nil, err
		}
		if info.SHA256 != fleet.Text(meta["sha256"]) {
			return nil, errors.New("画像の保存内容が一致しません")
		}
		result = append(result, consultationai.Image{ID: id, Ext: info.Ext, SHA256: info.SHA256, Bytes: data})
	}
	return result, nil
}

var commonNumber = regexp.MustCompile(`(?:共通番号\s*#?|#)([1-9][0-9]{0,8})(?:[^0-9A-Za-z_]|$)`)
var foreignNumberPrefix = regexp.MustCompile(`(?i)(?:PR|Gitea(?:番号)?)\s*$`)

func deskReferences(source string) []int64 {
	result := []int64{}
	seen := map[int64]bool{}
	for _, line := range strings.Split(source, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), ">") || regexp.MustCompile(`^\s*- 共通番号[0-9、]+: `).MatchString(line) {
			continue
		}
		for _, m := range commonNumber.FindAllStringSubmatchIndex(line, -1) {
			before := line[:m[0]]
			token := line[m[0]:m[1]]
			if strings.HasPrefix(token, "#") && (foreignNumberPrefix.MatchString(before) || before != "" && strings.ContainsRune("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz_/#&.-", rune(before[len(before)-1]))) {
				continue
			}
			var n int64
			for _, r := range line[m[2]:m[3]] {
				n = n*10 + int64(r-'0')
			}
			if !seen[n] {
				seen[n] = true
				result = append(result, n)
			}
		}
	}
	return result
}
func digestText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
