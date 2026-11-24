package issueapi

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"regexp"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
)

func closeRecoveryPending(r Object) bool {
	return closeNotePending(r) || fleet.ObjectOrEmpty(r["prClose"])["result"] == "failed" || r["recoveryPending"] == true
}

func closeRecoveryDue(r Object, now time.Time) bool {
	at, err := time.Parse(time.RFC3339Nano, fleet.Text(r["retryAt"]))
	return err != nil || !at.After(now)
}

func (s *Service) recoverCloseCycle(ctx context.Context) error {
	records, err := s.closeRecords(ctx, closeRecoveryFilter())
	if err != nil {
		return err
	}
	for _, r := range records {
		if oneOf(fleet.Text(r["status"]), "reviewing", "closing") || r["status"] == "closed" && closeRecoveryPending(r) && closeRecoveryDue(r, s.o.Now()) {
			s.launchClose(fleet.Text(r["id"]))
		}
	}
	return nil
}

func (s *Service) recordCloseRecovery(ctx context.Context, r Object, err error) {
	if err == nil {
		delete(r, "recoveryPending")
	}
	if err != nil || closeRecoveryPending(r) {
		if err != nil {
			r["recoveryPending"] = true
		}
		attempts, _ := number(r["recoveryAttempts"])
		if attempts < 7 {
			attempts++
		} else {
			attempts = 7
		}
		delay := time.Minute * time.Duration(1<<(attempts-1))
		if delay > time.Hour {
			delay = time.Hour
		}
		r["recoveryAttempts"] = attempts
		r["retryAt"] = s.o.Now().UTC().Add(delay).Format(time.RFC3339Nano)
	} else {
		delete(r, "recoveryPending")
		delete(r, "recoveryAttempts")
		r["retryAt"] = nil
	}
	_ = s.saveClose(ctx, r)
}

func (s *Service) holdClose(ctx context.Context, r Object, message string) error {
	r["status"] = "confirm"
	r["message"] = message
	return s.saveClose(ctx, r)
}
func (s *Service) validCloseToken(ctx context.Context, r Object) bool {
	if fleet.Text(r["tokenId"]) == "" {
		return false
	}
	var name string
	if e := s.o.AppDB.QueryRowContext(ctx, bindSQL(`SELECT name FROM api_tokens WHERE id=?`), fleet.Text(r["tokenId"])).Scan(&name); e != nil || name != fleet.Text(r["pc"]) {
		return false
	}
	return true
}
func (s *Service) reviewClose(ctx context.Context, r Object) error {
	repo := fleet.Text(r["project"])
	n, _ := number(r["number"])
	detail, e := s.Detail(ctx, repo, n, false, false)
	if e != nil {
		if ctx.Err() != nil {
			return e
		}
		return s.holdClose(ctx, r, "対象の依頼を確認できません。")
	}
	if detail["state"] == "closed" {
		r["status"] = "already_closed"
		r["message"] = "確認の間に閉じられました。終了理由は変更していません。"
		return s.saveClose(ctx, r)
	}
	// 送信中かどうかは、閉じる直前に executeClose が依頼のロックの中で確かめる。
	if block := s.closeBlockerFor(ctx, detail, false); block != "" {
		return s.holdClose(ctx, r, block)
	}
	if !s.validCloseToken(ctx, r) {
		return s.holdClose(ctx, r, "依頼したトークンが失効または再発行されています。")
	}
	material, e := s.gatherClose(ctx, r, detail)
	if e != nil {
		return s.holdClose(ctx, r, "確認資料を集められません。")
	}
	r["target"] = closeTarget(detail)
	r["material"] = material
	r["fingerprint"] = closeFingerprint(material)
	if e = s.saveClose(ctx, r); e != nil {
		return e
	}
	if len(fleet.Items(material["unavailable"])) > 0 || len(fleet.Items(material["ambiguous"])) > 0 || len(fleet.Items(fleet.ObjectOrEmpty(material["successors"])["broken"])) > 0 {
		return s.holdClose(ctx, r, "資料または依存関係を確認できないため、利用者の判断が必要です。")
	}
	decide := s.closeByAI
	if r["reason"] == "voluntary" {
		decide = s.closeByMachine
	}
	if proceed, e := decide(ctx, r, detail, material); !proceed {
		return e
	}
	r["status"] = "closing"
	if e = s.saveClose(ctx, r); e != nil {
		return e
	}
	return s.executeClose(ctx, r, false)
}

// closeByMachine は任意終了の説明が対象の題名と一致し、後続の依頼が無いことを機械的に確かめ、判断を記録する。確かめられなければ保留にして false を返す。
func (s *Service) closeByMachine(ctx context.Context, r, detail, material Object) (bool, error) {
	comparable := func(value string) string {
		value = strings.TrimPrefix(strings.TrimSpace(norm.NFKC.String(value)), "[フォローアップ]")
		value = regexp.MustCompile(`#[0-9]+`).ReplaceAllString(value, " ")
		return strings.Join(strings.Fields(value), " ")
	}
	title := comparable(fleet.Text(detail["title"]))
	if title == "" || !strings.Contains(comparable(fleet.Text(r["description"])), title) || len(fleet.Items(fleet.ObjectOrEmpty(material["successors"])["found"])) > 0 {
		return false, s.holdClose(ctx, r, "説明と対象の題名、または後続の依頼を機械的に照合できません。")
	}
	r["review"] = Object{"decision": "close", "contentMatch": true, "reasonConfirmed": true, "reason": "対象の題名と任意終了の説明が一致し、後続がありません。", "evidence": []any{}, "source": "machine"}
	r["decision"] = Object{"method": "machine", "at": s.now()}
	return true, nil
}

// closeByAI は内容と終了理由の両方を受付AIに確かめさせ、判断を記録する。確かめられなければ保留にして false を返す。
func (s *Service) closeByAI(ctx context.Context, r, detail, material Object) (bool, error) {
	if s.o.Answer == nil {
		return false, s.holdClose(ctx, r, "受付AIの確認を利用できません。")
	}
	review, e := s.o.Answer(ctx, Object{"kind": "close-review", "material": material})
	if e != nil {
		if ctx.Err() != nil {
			return false, e
		}
		return false, s.holdClose(ctx, r, "受付AIの確認に失敗しました。")
	}
	if !oneOf(fleet.Text(review["decision"]), "close", "confirm") || fleet.Text(review["reason"]) == "" || review["contentMatch"] != true || review["reasonConfirmed"] != true || review["decision"] != "close" {
		r["review"] = review
		return false, s.holdClose(ctx, r, "内容と終了理由の両方をAIが確認できませんでした。")
	}
	if _, ok := review["evidence"].([]any); !ok {
		return false, s.holdClose(ctx, r, "AIの確認根拠の形式が不正です。")
	}
	r["review"] = review
	r["decision"] = Object{"method": "ai", "reason": review["reason"], "at": s.now()}
	return true, nil
}

func (s *Service) gatherClose(ctx context.Context, r, detail Object) (Object, error) {
	repo := fleet.Text(r["project"])
	n, _ := number(r["number"])
	target := closeTarget(detail)
	target["bodyLength"] = len([]rune(fleet.Text(detail["body"])))
	target["bodySha256"] = digest(detail["body"])
	found, broken, unavailable := s.closeSuccessors(ctx, repo, n)
	references := []any{}
	ambiguous := []any{}
	defaultBranch := any(nil)
	if r["reason"] != "voluntary" {
		text := fleet.Text(r["description"]) + "\n" + fleet.Text(r["comment"])
		var referenceMissing []any
		references, ambiguous, referenceMissing = s.closeReferences(ctx, text)
		unavailable = append(unavailable, referenceMissing...)
		var branchMissing []any
		defaultBranch, branchMissing = s.closeDefaultBranch(ctx, repo)
		unavailable = append(unavailable, branchMissing...)
	}
	if len(fleet.Text(detail["body"])) > 20000 && r["reason"] != "voluntary" {
		unavailable = append(unavailable, "対象本文が20000文字を超えています。全文のハッシュだけを保持します。")
		target["body"] = clipText(fleet.Text(detail["body"]), 20000)
	}
	return Object{"request": Object{"reason": r["reason"], "description": r["description"], "comment": r["comment"], "pc": r["pc"], "route": r["route"]}, "target": target, "references": references, "ambiguous": ambiguous, "defaultBranch": defaultBranch, "successors": Object{"found": found, "broken": broken}, "unavailable": unavailable}, nil
}

// closeSuccessors は閉じる依頼を待っている後続の依頼を、本文の依存の印と Gitea の依存の両方から集める。読めなかった取得元は unavailable に残す。
func (s *Service) closeSuccessors(ctx context.Context, repo string, n int64) ([]any, []any, []any) {
	unavailable := []any{}
	found, broken := []any{}, []any{}
	for _, candidateRepo := range s.repositories() {
		issues, e := s.o.Gitea.Issues(ctx, candidateRepo, "open")
		if e != nil {
			unavailable = append(unavailable, candidateRepo+" の後続一覧を取得できません。")
			continue
		}
		for _, issue := range issues {
			candidateN, _ := number(issue["number"])
			if issue["pull_request"] != nil || issue["state"] != "open" || candidateRepo == repo && candidateN == n {
				continue
			}
			for _, v := range s.dependencies(ctx, candidateRepo, fleet.Text(issue["body"])) {
				dep := fleet.ObjectOrEmpty(v)
				if dep["invalid"] != nil {
					broken = append(broken, Object{"project": candidateRepo, "number": candidateN, "invalid": dep["invalid"]})
					continue
				}
				targetRepo := fleet.Text(dep["project"])
				if targetRepo == "" {
					targetRepo = candidateRepo
				}
				depN, _ := number(dep["number"])
				if targetRepo == repo && depN == n {
					found = append(found, Object{"project": candidateRepo, "number": candidateN, "title": issue["title"], "source": "marker"})
				}
			}
		}
	}
	blocks, e := s.o.Gitea.Pages(ctx, fmt.Sprintf("/repos/%s/issues/%d/blocks", repo, n))
	if e != nil {
		unavailable = append(unavailable, "Gitea標準の後続依存を取得できません。")
	} else {
		for _, v := range blocks {
			if v["state"] == "open" {
				found = append(found, Object{"project": fleet.ObjectOrEmpty(v["repository"])["full_name"], "number": v["number"], "title": v["title"], "source": "gitea"})
			}
		}
	}
	return found, broken, unavailable
}

// closeReferences は終了の説明に書かれた根拠の参照（Issue・PR・コミット）を10件まで読み、曖昧な #番号を集める。読めなかった根拠は unavailable に残す。
func (s *Service) closeReferences(ctx context.Context, text string) ([]any, []any, []any) {
	var e error
	references := []any{}
	ambiguous := []any{}
	unavailable := []any{}
	pattern := regexp.MustCompile(`([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)(?:[#!]([1-9][0-9]{0,9})|@([0-9a-fA-F]{7,40}))`)
	seen := map[string]bool{}
	for _, match := range pattern.FindAllStringSubmatch(text, -1) {
		if seen[match[0]] {
			continue
		}
		seen[match[0]] = true
		if len(seen) > 10 {
			unavailable = append(unavailable, "根拠の参照が10件を超えています。")
			break
		}
		referenceRepo := match[1]
		if e = s.o.Project(referenceRepo); e != nil {
			unavailable = append(unavailable, "許可対象外の根拠は取得しません。")
			continue
		}
		route := "/repos/" + referenceRepo
		kind := "commit"
		if match[2] != "" {
			route += "/issues/" + match[2]
			kind = "issue"
		} else {
			route += "/git/commits/" + strings.ToLower(match[3])
		}
		value := Object{}
		if e = s.o.Gitea.Do(ctx, "GET", route, nil, &value); e != nil {
			unavailable = append(unavailable, referenceRepo+" の根拠を取得できません。")
			continue
		}
		reference := Object{"project": referenceRepo, "kind": kind, "record": value}
		if match[2] != "" && value["pull_request"] != nil {
			pull := Object{}
			if e = s.o.Gitea.Do(ctx, "GET", "/repos/"+referenceRepo+"/pulls/"+match[2], nil, &pull); e != nil {
				unavailable = append(unavailable, "根拠のPR状態を取得できません。")
			} else {
				reference["pull"] = pull
			}
		}
		references = append(references, reference)
	}
	for _, match := range regexp.MustCompile(`(?:^|[^\w/.-])#([1-9][0-9]{0,9})`).FindAllStringSubmatch(text, -1) {
		ambiguous = append(ambiguous, match[1])
	}
	return references, ambiguous, unavailable
}

// closeDefaultBranch はリポジトリの既定ブランチとその先頭を読む。読めなければ理由を unavailable に返す。
func (s *Service) closeDefaultBranch(ctx context.Context, repo string) (any, []any) {
	defaultBranch := any(nil)
	unavailable := []any{}
	repository := Object{}
	if e := s.o.Gitea.Do(ctx, "GET", "/repos/"+repo, nil, &repository); e != nil {
		unavailable = append(unavailable, "既定ブランチを取得できません。")
	} else {
		branch := fleet.Text(repository["default_branch"])
		current := Object{}
		if branch == "" {
			unavailable = append(unavailable, "既定ブランチが不明です。")
		} else if e := s.o.Gitea.Do(ctx, "GET", "/repos/"+repo+"/branches/"+branch, nil, &current); e != nil {
			unavailable = append(unavailable, "既定ブランチの先頭を取得できません。")
		} else {
			defaultBranch = Object{"name": branch, "head": fleet.ObjectOrEmpty(current["commit"])["id"]}
		}
	}
	return defaultBranch, unavailable
}

func closeFingerprint(material Object) string {
	b, _ := json.Marshal([]any{material["target"], material["references"], material["ambiguous"], material["defaultBranch"], material["successors"], material["unavailable"]})
	return digest(string(b))
}
func clipText(s string, n int) string {
	out := []rune(s)
	if len(out) > n {
		return string(out[:n]) + "…"
	}
	return s
}
func (s *Service) executeClose(ctx context.Context, r Object, direct bool) error {
	repo := fleet.Text(r["project"])
	n, _ := number(r["number"])
	// 最後の確認から閉じて記録するまで、この依頼への操作の送信と並ばないようにする。
	issueUnlock := lock(&s.locks, fmt.Sprintf("issue:%s#%d", repo, n))
	defer issueUnlock()
	detail, e := s.Detail(ctx, repo, n, false, false)
	if e != nil {
		return e
	}
	if detail["state"] == "closed" {
		patched := fleet.ObjectOrEmpty(r["patched"])
		if r["status"] == "closed" || r["attempted"] == true && patched["closedAt"] != nil && patched["closedAt"] == detail["closedAt"] {
			return s.finishClose(ctx, r, detail)
		}
		r["status"] = "already_closed"
		r["message"] = "既に閉じられています。終了理由を上書きしません。"
		return s.saveClose(ctx, r)
	}
	if r["command"] != nil || r["orderAttempt"] != nil || r["orderAction"] != nil {
		return s.holdClose(ctx, r, "以前の形式のクローズ指示は再送せず、現在の状態で判断してください。")
	}
	if !direct {
		if stopped, e := s.recheckClose(ctx, r, detail); stopped {
			return e
		}
	}
	latest, e := s.o.Gitea.Issue(ctx, repo, n)
	if e != nil {
		return e
	}
	if latest["title"] != detail["title"] || latest["body"] != detail["body"] || latest["state"] != "open" {
		return s.holdClose(ctx, r, "最終確認後にIssueが変わりました。")
	}
	job, e := s.o.Jobs.ByIssue(ctx, repo, n)
	if e != nil {
		return e
	}
	if job != nil {
		_, e = s.o.Jobs.Control(ctx, job.ID, jobs.ControlInput{ID: operationID("close-job:" + fleet.Text(r["id"])), Action: "close", By: "close-request:" + fleet.Text(r["id"]), IfIdle: r["force"] != true}, jobs.ControlOptions{})
		if e != nil {
			return e
		}
	}
	if e = s.patchCloseIssue(ctx, r, repo, n); e != nil {
		return e
	}
	detail, e = s.Detail(ctx, repo, n, false, false)
	if e != nil {
		return e
	}
	return s.finishClose(ctx, r, detail)
}

// recheckClose は閉じる直前に、依頼の後の閉じる・再オープン、閉じられない理由、トークン、確認資料の変化を確かめる。閉じずに終える（または失敗した）ときは stopped を返す。
func (s *Service) recheckClose(ctx context.Context, r, detail Object) (bool, error) {
	repo := fleet.Text(r["project"])
	n, _ := number(r["number"])
	events, e := s.timeline(ctx, repo, n)
	if e != nil {
		return true, e
	}
	baseline, _ := number(r["baseline"])
	for _, event := range events {
		id, _ := number(event["id"])
		if id > baseline && oneOf(fleet.Text(event["type"]), "close", "reopen") {
			r["status"] = "expired"
			r["message"] = "依頼の後に閉じる・再オープンがありました。"
			return true, s.saveClose(ctx, r)
		}
	}
	if reason := s.closeBlockerFor(ctx, detail, false); reason != "" {
		return true, s.holdClose(ctx, r, reason)
	}
	method := fleet.Text(fleet.ObjectOrEmpty(r["decision"])["method"])
	if oneOf(method, "ai", "machine") && !s.validCloseToken(ctx, r) {
		return true, s.holdClose(ctx, r, "依頼したトークンが失効または再発行されています。")
	}
	material, e := s.gatherClose(ctx, r, detail)
	if e != nil {
		return true, e
	}
	fingerprint := closeFingerprint(material)
	if r["fingerprint"] != fingerprint {
		r["target"] = closeTarget(detail)
		r["material"] = material
		r["fingerprint"] = fingerprint
		return true, s.holdClose(ctx, r, "確認後に対象または根拠が変わりました。最新の内容で判断してください。")
	}
	return false, nil
}

// patchCloseIssue は閉じる前に試行を記録してから Issue を閉じ、閉じた時刻を記録する。
func (s *Service) patchCloseIssue(ctx context.Context, r Object, repo string, n int64) error {
	var e error
	r["attempted"] = true
	r["attemptedAt"] = s.now()
	if e = s.saveClose(ctx, r); e != nil {
		return e
	}
	closed := Object{}
	if e = s.o.Gitea.Do(ctx, "PATCH", fmt.Sprintf("/repos/%s/issues/%d", repo, n), Object{"state": "closed"}, &closed); e != nil {
		return e
	}
	if closed["state"] != "closed" {
		return fail(503, "Issueを閉じた結果を確認できません。")
	}
	r["patched"] = Object{"closedAt": closed["closed_at"]}
	if e = s.saveClose(ctx, r); e != nil {
		return e
	}
	return nil
}

func (s *Service) finishClose(ctx context.Context, r, detail Object) error {
	repo := fleet.Text(r["project"])
	n, _ := number(r["number"])
	if r["status"] != "closed" {
		r["notePending"] = true
		// 結果を照合できない操作は成功扱いにせず、閉じた時点の事実として記録と終了コメントに残す。
		// Issueは既に閉じているので、読めなくても止めずに「確認できない」と残す。
		if unresolved, e := s.unresolvedActions(ctx, repo, n); e != nil {
			r["unresolvedActionsUnknown"] = true
		} else if len(unresolved) > 0 {
			r["unresolvedActions"] = unresolved
		}
	}
	r["status"] = "closed"
	r["closedAt"] = detail["closedAt"]
	r["message"] = "依頼を閉じました。"
	if detail["status"] == "closed" {
		record := Object{"reason": r["reason"], "source": map[bool]string{true: "token", false: "manual"}[fleet.Text(r["tokenId"]) != ""], "closedAt": detail["closedAt"], "updatedAt": s.now()}
		if r["route"] != nil {
			record["route"] = r["route"]
		}
		if e := s.save(ctx, "close_reasons", fmt.Sprintf("%s#%d", repo, n), record); e != nil {
			return e
		}
	}
	if e := s.saveClose(ctx, r); e != nil {
		return e
	}
	if e := s.postCloseNote(ctx, r); e != nil {
		return e
	}
	if pr, ok := number(fleet.ObjectOrEmpty(detail["pr"])["number"]); ok {
		pull := Object{}
		route := fmt.Sprintf("/repos/%s/pulls/%d", repo, pr)
		e := s.o.Gitea.Do(ctx, "GET", route, nil, &pull)
		if e == nil && pull["state"] == "open" {
			e = s.o.Gitea.Do(ctx, "PATCH", route, Object{"state": "closed"}, &pull)
		}
		if e != nil || pull["state"] != "closed" {
			r["prClose"] = Object{"number": pr, "result": "failed", "message": "PRの終了を確認できません。"}
		} else {
			r["prClose"] = Object{"number": pr, "result": "closed"}
		}
	}
	if e := s.saveClose(ctx, r); e != nil {
		return e
	}
	s.emit(repo, n)
	return nil
}

func (s *Service) launchClose(id string) {
	s.closing.Lock()
	defer s.closing.Unlock()
	if s.ctx.Err() != nil {
		return
	}
	if _, running := s.closeInFlight.LoadOrStore(id, true); running {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.closeInFlight.Delete(id)
		unlock := lock(&s.locks, "close:"+id)
		defer unlock()
		r, e := s.read(s.ctx, "close_requests", id)
		if e != nil || r == nil {
			return
		}
		if r["status"] == "reviewing" {
			_ = s.reviewClose(s.ctx, r)
		} else if r["status"] == "closing" {
			_ = s.executeClose(s.ctx, r, false)
		} else if r["status"] == "closed" {
			if !closeRecoveryDue(r, s.o.Now()) || !closeRecoveryPending(r) {
				return
			}
			repo := fleet.Text(r["project"])
			n, _ := number(r["number"])
			detail, e := s.Detail(s.ctx, repo, n, false, false)
			if e == nil && detail["state"] == "closed" {
				e = s.finishClose(s.ctx, r, detail)
			} else if e == nil {
				e = fmt.Errorf("close recovery: issue is not closed")
			}
			s.recordCloseRecovery(s.ctx, r, e)
		}
	}()
}

func (s *Service) Run(ctx context.Context) error {
	err := s.RunRecovery(ctx)
	if err == nil && ctx.Err() != nil {
		s.Close()
	}
	return err
}

func (s *Service) RunRecovery(ctx context.Context) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if e := s.recoverReplies(ctx); e != nil {
			return e
		}
		if e := s.recoverCloseCycle(ctx); e != nil {
			return e
		}
		select {
		case <-ctx.Done():
			return nil
		case <-s.ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
