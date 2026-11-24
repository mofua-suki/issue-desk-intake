package operations

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/usage"
)

// UserWaitNoticeItem describes one currently outstanding user decision.
type UserWaitNoticeItem struct {
	Project      string
	IssueNumber  int64
	CommonNumber int64
	Title        string
	Kind         jobs.UserWaitKind
}

type UserWaitNotice struct {
	Service *Service
	Sources func(context.Context) ([]UserWaitNoticeItem, error)
	Notify  func(context.Context, map[string]any) (map[string]any, error)
}

type userWaitKey struct {
	Project     string            `json:"project"`
	IssueNumber int64             `json:"issueNumber"`
	Kind        jobs.UserWaitKind `json:"kind"`
}

func validUserWaitKey(k userWaitKey) bool {
	switch k.Kind {
	case jobs.UserWaitPermission, jobs.UserWaitDeletion, jobs.UserWaitFinalApproval, jobs.UserWaitRunLimit, jobs.UserWaitUsageLimit:
		return k.Project != "" && k.IssueNumber > 0
	}
	return false
}

// Reconcile reserves newly observed waits before invoking the notification function.
// A committed reservation is retained even if delivery fails or the process stops.
func (m *UserWaitNotice) Reconcile(ctx context.Context) error {
	items, err := m.Sources(ctx)
	if err != nil {
		return err
	}
	current, unique, err := uniqueUserWaits(items)
	if err != nil {
		return err
	}
	if m.Service == nil || m.Service.PostgreSQL == nil {
		return contract.NewProblem(503, "利用者判断待ちの保存先がありません。", "")
	}
	added, err := m.reserveUserWaits(ctx, current, unique)
	if err != nil {
		return err
	}
	return m.notifyUserWaits(ctx, added)
}

// uniqueUserWaits は判断待ちを依頼と種類ごとに1件にまとめ、形の崩れた待ちを断る。
func uniqueUserWaits(items []UserWaitNoticeItem) ([]userWaitKey, map[userWaitKey]UserWaitNoticeItem, error) {
	current := make([]userWaitKey, 0, len(items))
	unique := make(map[userWaitKey]UserWaitNoticeItem, len(items))
	for _, item := range items {
		key := userWaitKey{item.Project, item.IssueNumber, item.Kind}
		if !validUserWaitKey(key) {
			return nil, nil, fmt.Errorf("invalid user wait key")
		}
		if _, exists := unique[key]; !exists {
			current = append(current, key)
			unique[key] = item
		}
	}
	return current, unique, nil
}

// reserveUserWaits は今の判断待ちを台帳に保存し、前回の台帳に無かった待ちを返す。通知の前に保存を確定させる。
func (m *UserWaitNotice) reserveUserWaits(ctx context.Context, current []userWaitKey, unique map[userWaitKey]UserWaitNoticeItem) ([]UserWaitNoticeItem, error) {
	tx, err := m.Service.PostgreSQL.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext('issue-desk-user-wait-notice'))"); err != nil {
		return nil, err
	}
	saved, err := loadUserWaitLedger(ctx, tx)
	if err != nil {
		return nil, err
	}
	old := make(map[userWaitKey]bool, len(saved))
	for _, key := range saved {
		old[key] = true
	}
	added := []UserWaitNoticeItem{}
	for _, key := range current {
		if !old[key] {
			added = append(added, unique[key])
		}
	}
	value, err := json.Marshal(current)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value,expires_at,updated_at,updated_by) VALUES('user-wait-notice',$1::jsonb,NULL,$2,'user-wait-notice') ON CONFLICT(key) DO UPDATE SET value=excluded.value,expires_at=NULL,updated_at=excluded.updated_at,updated_by=excluded.updated_by", string(value), m.Service.now())
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return added, nil
}

// loadUserWaitLedger は通知済みの判断待ちの台帳を行ロック付きで読む。台帳が無ければ空を返す。
func loadUserWaitLedger(ctx context.Context, tx *sql.Tx) ([]userWaitKey, error) {
	var raw string
	err := tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='user-wait-notice' FOR UPDATE").Scan(&raw)
	saved := []userWaitKey{}
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if err == nil {
		if err = json.Unmarshal([]byte(raw), &saved); err != nil {
			return nil, err
		}
		if saved == nil {
			return nil, fmt.Errorf("invalid user wait notice ledger")
		}
		for _, key := range saved {
			if !validUserWaitKey(key) {
				return nil, fmt.Errorf("invalid user wait notice key")
			}
		}
	}
	return saved, nil
}

// notifyUserWaits は新しい判断待ちがあれば、件数と最初の1件を購読先へ知らせる。
func (m *UserWaitNotice) notifyUserWaits(ctx context.Context, added []UserWaitNoticeItem) error {
	if len(added) > 0 && m.Notify != nil {
		first := added[0]
		_, err := m.Notify(ctx, map[string]any{"title": fmt.Sprintf("利用者の判断待ち: %d件", len(added)), "body": fmt.Sprintf("#%d %s (%s)", first.CommonNumber, usage.Redact(first.Title), first.Kind), "url": "/#/list/attention"})
		return err
	}
	return nil
}
