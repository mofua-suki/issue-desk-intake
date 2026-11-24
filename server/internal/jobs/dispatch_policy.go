package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"issue-desk-intake/server/internal/fleet"
)

func dispatchOverloaded(ctx context.Context, tx *sql.Tx, host string, at time.Time, limit map[string]any, lease time.Duration) (bool, error) {
	if _, err := tx.ExecContext(ctx, "SAVEPOINT dispatch_load_history"); err != nil {
		return false, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT received_at,load FROM host_load_samples WHERE host=$1 AND received_at>=$2 AND received_at<=$3", host, at.Add(-time.Duration(fleet.Number(limit["minutes"]))*time.Minute), at)
	if err != nil {
		if _, rollback := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT dispatch_load_history"); rollback != nil {
			return false, rollback
		}
		_, release := tx.ExecContext(ctx, "RELEASE SAVEPOINT dispatch_load_history")
		return false, release
	}
	count := 0
	overloaded := true
	latest := time.Time{}
	for rows.Next() {
		var stamp time.Time
		var raw []byte
		if err = rows.Scan(&stamp, &raw); err != nil {
			rows.Close()
			return false, err
		}
		var load map[string]any
		if len(raw) > 0 {
			if err = json.Unmarshal(raw, &load); err != nil {
				rows.Close()
				return false, err
			}
		}
		count++
		if stamp.After(latest) {
			latest = stamp
		}
		if !sampleOverloaded(load, limit) {
			overloaded = false
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, "RELEASE SAVEPOINT dispatch_load_history"); err != nil {
		return false, err
	}
	return count >= 2 && at.Sub(latest) <= lease && overloaded, nil
}

// sampleOverloaded は負荷の1件が、1分の平均負荷（コアあたり）か CPU 使用率で上限を超えているかを判定する。
func sampleOverloaded(load, limit map[string]any) bool {
	if average, present := load["loadAverage1m"]; present && average != nil {
		cores, ok := countNumber(load["logicalCpuCount"])
		value, number := average.(float64)
		if !ok || cores < 1 || !number || value/float64(cores) <= fleet.Number(limit["ratio"]) {
			return false
		}
	} else {
		value, ok := load["cpuPercent"].(float64)
		if !ok || value > 100 || value < fleet.Number(limit["cpuPercent"]) {
			return false
		}
	}
	return true
}

func countNumber(value any) (int64, bool) { return count(value) }
