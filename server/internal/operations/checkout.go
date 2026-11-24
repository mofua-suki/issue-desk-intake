package operations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"math"
	"time"
)

func (s *Service) checkoutAlerts(ctx context.Context) ([]any, error) {
	saved, e := s.setting(ctx, "desk-checkout-lock-reports")
	if e != nil {
		return nil, e
	}
	out := []any{}
	for _, raw := range fleet.List(saved) {
		r := fleet.Object(raw)
		blocked, e := time.Parse(time.RFC3339Nano, fleet.Text(r["blockedSince"]))
		if e != nil {
			continue
		}
		observed, e := time.Parse(time.RFC3339Nano, fleet.Text(r["observedAt"]))
		if e != nil {
			continue
		}
		host := fleet.Text(r["host"])
		hash := sha256.Sum256([]byte(host))
		seconds := math.Max(0, math.Floor(observed.Sub(blocked).Seconds()))
		out = append(out, map[string]any{"key": "desk-checkout-lock:" + hex.EncodeToString(hash[:])[:32], "kind": "ops_watch", "kindName": "運用の見張り", "stateLabel": "停止の観測", "title": "checkout の保護排他が使用中または終了不明（" + host + "）", "reason": fmt.Sprintf("停止件数: 1件、最終観測時の保持時間: %.0f秒。系列CLIとcheckout更新が待つ可能性があります。開始: %v、最終観測: %v。ホスト上で全ガード・子CLI・更新処理の終了を確認し、記録を退避してから ~/.issue-desk/checkout/.paced-lock だけを解除してください。PID・経過時間で自動解除しません。保護記録 .paced-use は別に照合してください。次のNAS更新で不在を確認すると表示が消えます。トークンでホストの排他は解除できません。", seconds, r["blockedSince"], r["observedAt"]), "host": host, "updatedAt": r["observedAt"], "project": nil, "number": nil})
	}
	return out, nil
}
