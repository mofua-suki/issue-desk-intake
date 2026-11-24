package operations

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"strings"
	"time"
)

var noticeMarker = regexp.MustCompile(`<!-- gcr:(\S+?) -->\s*$`)
var completeMarker = regexp.MustCompile(`^complete:[0-9]+$`)

func (s *Service) humanMergeWait(ctx, commentsCtx context.Context, repo string, issue int64) (bool, error) {
	if s.PostgreSQL == nil || s.TrustedWriter == nil || s.ReadComments == nil {
		return false, nil
	}
	leaseAt, held := s.reviewHeldLease(ctx, repo, issue)
	if !held {
		return false, nil
	}
	return s.mergeNoticeCompleted(commentsCtx, repo, issue, leaseAt), nil
}

// reviewHeldLease は依頼が今の lease のままレビュー保留で止まっているかを出来事から確かめ、その lease の開始時刻を返す。
func (s *Service) reviewHeldLease(ctx context.Context, repo string, issue int64) (time.Time, bool) {
	var id, generation, epoch int64
	var status string
	var hold, host sql.NullString
	var pr sql.NullInt64
	e := s.PostgreSQL.QueryRowContext(ctx, "SELECT id,status,hold_reason,pr,generation,lease_host,lease_epoch FROM jobs WHERE lower(repo)=lower($1)AND issue=$2", repo, issue).Scan(&id, &status, &hold, &pr, &generation, &host, &epoch)
	if e != nil {
		return time.Time{}, false
	}
	if status != "held" || hold.String != "review_held" || !pr.Valid || pr.Int64 < 1 || generation < 0 || host.String == "" || epoch < 1 {
		return time.Time{}, false
	}
	rows, e := s.PostgreSQL.QueryContext(ctx, "SELECT seq,kind,host,at,data FROM job_events WHERE job_id=$1 ORDER BY seq DESC LIMIT 1000", id)
	if e != nil {
		return time.Time{}, false
	}
	var leaseAt time.Time
	var leaseSeq, stateSeq int64
	leaseValid, stateValid := false, false
	for rows.Next() {
		var seq int64
		var kind string
		var eventHost sql.NullString
		var at time.Time
		var raw []byte
		if e = rows.Scan(&seq, &kind, &eventHost, &at, &raw); e != nil {
			rows.Close()
			return time.Time{}, false
		}
		var data map[string]any
		if json.Unmarshal(raw, &data) != nil {
			rows.Close()
			return time.Time{}, false
		}
		if kind == "leased" && leaseSeq == 0 {
			leaseSeq = seq
			leaseAt = at
			leaseValid = eventHost.String == host.String && fleet.Number(data["generation"]) == float64(generation) && fleet.Number(data["epoch"]) == float64(epoch)
		}
		if kind == "state" && stateSeq == 0 {
			stateSeq = seq
			stateValid = eventHost.String == host.String && data["to"] == "held" && data["holdReason"] == "review_held" && fleet.Number(data["epoch"]) == float64(epoch)
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil || !leaseValid || !stateValid || stateSeq < leaseSeq {
		return time.Time{}, false
	}
	return leaseAt, true
}

// mergeNoticeCompleted は信頼する書き手の作業通知のうち、lease の後に出た完了の通知が最後の通知（その分割の続きを除く）かを判定する。
func (s *Service) mergeNoticeCompleted(commentsCtx context.Context, repo string, issue int64, leaseAt time.Time) bool {
	comments, e := s.ReadComments(commentsCtx, repo, issue)
	if e != nil {
		return false
	}
	type notice struct {
		id     float64
		at     time.Time
		suffix string
	}
	list := []notice{}
	anchor := strings.ToLower(fmt.Sprintf(":%s#%d:", repo, issue))
	for _, c := range comments {
		body, ok := c["body"].(string)
		if !ok || !s.TrustedWriter(fleet.Object(c["user"])) {
			continue
		}
		match := noticeMarker.FindStringSubmatch(body)
		if len(match) == 0 {
			continue
		}
		index := strings.Index(strings.ToLower(match[1]), anchor)
		if index < 0 {
			continue
		}
		id := fleet.Number(c["id"])
		if id <= 0 {
			continue
		}
		at, _ := time.Parse(time.RFC3339Nano, fleet.Text(c["created_at"]))
		list = append(list, notice{id, at, match[1][index+len(anchor):]})
	}
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].id < list[j-1].id; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
	index := -1
	for i, n := range list {
		if completeMarker.MatchString(n.suffix) {
			index = i
		}
	}
	if index < 0 || list[index].at.Before(leaseAt) {
		return false
	}
	for _, n := range list[index+1:] {
		if !strings.HasPrefix(n.suffix, list[index].suffix+":part") {
			return false
		}
	}
	return true
}
