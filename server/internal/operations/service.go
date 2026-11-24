package operations

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/hostdrift"
	"issue-desk-intake/server/internal/jobs"
	"issue-desk-intake/server/internal/statistics"
	"issue-desk-intake/server/internal/timedcache"
	"issue-desk-intake/server/internal/usage"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Service struct {
	urgentCache         *urgentIssueCache
	UrgentLookupTimeout time.Duration
	UrgentCacheTTL      time.Duration
	reworkCache         *timedcache.Cache[[]any]
	PublicLink          func(string, string, int64) string
	IncludeArchived     bool
	AIAlerts            func(context.Context) ([]any, error)
	SystemAlerts        func(context.Context) ([]any, error)
	ContractMismatches  *ContractMismatches
	Gitea               interface {
		Do(context.Context, string, string, any, any) error
	}
	Fleet            func() (fleet.Document, *fleet.Assignments, error)
	DraftStallAlerts func(context.Context) ([]any, error)
	ReadComments     func(context.Context, string, int64) ([]map[string]any, error)
	TrustedWriter    func(map[string]any) bool
	Drift            *hostdrift.Service
	Statistics       *statistics.Service
	Assignments      *fleet.Assignments
	LiveAIAlerts     func(context.Context) ([]any, error)
	StartedAt        time.Time
	DB               *sql.DB
	PostgreSQL       *sql.DB
	Repositories     func() ([]string, error)
	DraftAlerts      func(context.Context) ([]any, error)
	Desk             func(string, int64) any
	RuntimeDir       string
	Now              func() time.Time
	Audit            func(context.Context, string, map[string]any) error
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
func (s *Service) Lease(ctx context.Context, sender string, body map[string]any) (map[string]any, error) {
	ttl, primary, p := parseLeaseRequest(body)
	if p != nil {
		return nil, p
	}
	if s.DB == nil {
		return nil, contract.NewProblem(503, "予約の保存先がありません。", "")
	}
	conn, e := s.DB.Conn(ctx)
	if e != nil {
		return nil, e
	}
	defer conn.Close()
	if e = sqlDialect.BeginLockedWrite(ctx, conn, "ops-watch-lease"); e != nil {
		return nil, e
	}
	defer conn.ExecContext(context.Background(), bindSQL("ROLLBACK"))
	var raw string
	e = conn.QueryRowContext(ctx, bindSQL("SELECT json FROM settings WHERE id='ops-watch-lease'")).Scan(&raw)
	previous := map[string]any{}
	if e != nil && e != sql.ErrNoRows {
		return nil, e
	}
	if e == nil && json.Unmarshal([]byte(raw), &previous) != nil {
		return nil, contract.NewProblem(503, "保存済みの予約を読み取れません。", "")
	}
	at := s.now()
	held, p := leaseHeld(previous, e == sql.ErrNoRows, sender, primary, at)
	if p != nil {
		return nil, p
	}
	lease := previous
	if held {
		lease = map[string]any{"holder": sender, "renewedAt": iso(at), "until": iso(at.Add(time.Duration(ttl) * time.Minute))}
		raw, e := json.Marshal(lease)
		if e != nil {
			return nil, e
		}
		if _, e = conn.ExecContext(ctx, bindSQL("INSERT INTO settings(id,json)VALUES('ops-watch-lease',?)ON CONFLICT(id)DO UPDATE SET json=excluded.json"), string(raw)); e != nil {
			return nil, e
		}
	}
	if _, e = conn.ExecContext(ctx, bindSQL("COMMIT")); e != nil {
		return nil, e
	}
	return map[string]any{"held": held, "holder": lease["holder"], "until": lease["until"]}, nil
}
func iso(at time.Time) string { return contract.ISO(at) }

var revision = regexp.MustCompile(`^[a-f0-9]{40}$`)

func requestID() (string, error) {
	var raw [16]byte
	if _, e := rand.Read(raw[:]); e != nil {
		return "", e
	}
	raw[6] = raw[6]&15 | 64
	raw[8] = raw[8]&63 | 128
	value := hex.EncodeToString(raw[:])
	return value[:8] + "-" + value[8:12] + "-" + value[12:16] + "-" + value[16:20] + "-" + value[20:], nil
}
func (s *Service) ForceHandoff(ctx context.Context, token string, body map[string]any) (map[string]any, error) {
	statement, instructionAt, sha, savedRevision, p := s.parseForceHandoff(body)
	if p != nil {
		return nil, p
	}
	if s.RuntimeDir == "" {
		return nil, contract.NewProblem(503, "入れ替え要求の保存先がありません。", "")
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	id, e := requestID()
	if e != nil {
		return nil, e
	}
	issued := s.now().Unix()
	expires := issued + 1800
	directory := filepath.Join(s.RuntimeDir, "state")
	if e = os.MkdirAll(directory, 0700); e != nil {
		return nil, e
	}
	target := filepath.Join(directory, "intake-force-handoff.request")
	temporary := target + "." + id + ".tmp"
	defer os.Remove(temporary)
	if e = writeHandoffRequest(temporary, issued, expires, sha, id); e != nil {
		return nil, e
	}
	audit := map[string]any{"id": id, "issuedAt": issued, "expiresAt": expires, "revision": savedRevision, "token": token, "statement": statement, "instructionAt": instructionAt}
	if s.Audit != nil {
		e = s.Audit(ctx, id, audit)
	} else {
		e = s.fileAudit(id, audit)
	}
	if e != nil {
		return nil, e
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	if e = os.Rename(temporary, target); e != nil {
		return nil, e
	}
	dir, e := os.Open(directory)
	if e != nil {
		return nil, e
	}
	e = dir.Sync()
	dir.Close()
	if e != nil {
		return nil, e
	}
	return map[string]any{"id": id, "issuedAt": issued, "expiresAt": expires, "revision": savedRevision}, nil
}

// parseLeaseRequest は監視の予約の要求から、期限の分数と主担当として取るかを読む。
func parseLeaseRequest(body map[string]any) (float64, bool, error) {
	if p := contract.OnlyKeys(body, "ttlMinutes", "primary"); p != nil {
		return 0, false, p
	}
	ttl, ok := body["ttlMinutes"].(float64)
	primary, bok := body["primary"].(bool)
	if !ok || ttl < 1 || ttl > 1440 || ttl != float64(int64(ttl)) || !bok {
		return 0, false, contract.NewProblem(400, "ttlMinutesは1〜1440の整数、primaryは真偽値です。", "")
	}
	return ttl, primary, nil
}

// leaseHeld は予約を送り手が取れるかを決める。予約が無い・送り手のもの・主担当の要求・期限切れなら取れる。
func leaseHeld(previous map[string]any, missing bool, sender string, primary bool, at time.Time) (bool, error) {
	held := missing || previous == nil || previous["holder"] == sender || primary
	if !held {
		until, err := time.Parse(time.RFC3339Nano, fleet.Text(previous["until"]))
		if err != nil {
			return false, contract.NewProblem(503, "保存済みの予約の期限を確認できません。", "")
		}
		held = !until.After(at)
	}
	return held, nil
}

// parseForceHandoff は強制の入れ替えの要求から、利用者の発言・指示の日時・対象の版を読む。版の指定が無ければ "*" にする。
func (s *Service) parseForceHandoff(body map[string]any) (string, string, string, any, error) {
	if p := contract.OnlyKeys(body, "statement", "instructionAt", "revision"); p != nil {
		return "", "", "", nil, p
	}
	statement, ok := body["statement"].(string)
	if !ok || !jobs.MeaningfulApproval(statement) || contract.UTF16Length(statement) > 8000 {
		return "", "", "", nil, contract.NewProblem(400, "利用者の依頼の発言原文（8〜8000文字）が必要です。", "")
	}
	instructionAt, ok := contract.InstructionAt(body["instructionAt"], s.now())
	if !ok {
		return "", "", "", nil, contract.NewProblem(400, contract.InstructionAtProblem, "")
	}
	sha := "*"
	var savedRevision any
	if value, exists := body["revision"]; exists {
		v, ok := value.(string)
		if !ok || !revision.MatchString(v) {
			return "", "", "", nil, contract.NewProblem(400, "revisionは40桁の小文字のコミットSHAです。", "")
		}
		sha, savedRevision = v, v
	}
	return statement, instructionAt, sha, savedRevision, nil
}

// writeHandoffRequest は入れ替え要求を一時ファイルに書き、ディスクへ確定させる。
func writeHandoffRequest(temporary string, issued, expires int64, sha, id string) error {
	file, e := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	_, e = fmt.Fprintf(file, "v1\n%d\n%d\n%s\n%s\n", issued, expires, sha, id)
	if e == nil {
		e = file.Sync()
	}
	closeErr := file.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	return nil
}

var auditLock sync.Mutex

func (s *Service) fileAudit(id string, value map[string]any) error {
	auditLock.Lock()
	defer auditLock.Unlock()
	raw, e := json.Marshal(value)
	if e != nil {
		return e
	}
	path := filepath.Join(s.RuntimeDir, "intake.log")
	if st, err := os.Stat(path); err == nil && st.Size() > 2*1024*1024 {
		if err = os.Rename(path, path+".previous"); err != nil {
			return err
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	file, e := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
	if e != nil {
		return e
	}
	defer file.Close()
	runes := []rune(usage.Redact(string(raw)))
	parts := (len(runes) + 9999) / 10000
	var lines strings.Builder
	for part := 0; part < parts; part++ {
		end := (part + 1) * 10000
		if end > len(runes) {
			end = len(runes)
		}
		fmt.Fprintf(&lines, "%s 強制入れ替え監査 %s %d/%d %s\n", iso(s.now()), id, part+1, parts, string(runes[part*10000:end]))
	}
	if _, e = file.WriteString(lines.String()); e != nil {
		return e
	}
	return file.Sync()
}
