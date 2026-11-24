package adminapi

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"net/http"
	"regexp"
	"strings"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/usage"
)

// queryMaps -> iso -> moment accepts years 0000..9999, but not infinities
// or other PostgreSQL years. Keep that range and its UTC millisecond precision.
// In particular 0001 BC is Go's year 0000 and must still be considered.
func expireSSHTickets(ctx context.Context, tx *repairTx, now time.Time, ttl int) error {
	_, err := tx.ExecContext(ctx, `UPDATE ssh_tickets SET status='expired'
 WHERE status='approved'
 AND decided_at >= TIMESTAMPTZ '0001-01-01 00:00:00+00 BC'
 AND decided_at < TIMESTAMPTZ '10000-01-01 00:00:00+00'
 AND date_trunc('milliseconds', decided_at, 'UTC') <= ?`, now.Add(-time.Duration(ttl)*time.Minute))
	return err
}

func sshView(row map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range row {
		out[key] = value
	}
	emergency, _ := integer(row["emergency"])
	if value, ok := row["emergency"].(bool); ok {
		out["emergency"] = value
	} else {
		out["emergency"] = emergency != 0
	}
	return out
}
func (s *service) ssh(r *http.Request, p auth.Principal) (any, int, error) {
	policy, e := s.repairPolicy()
	if e != nil {
		return nil, 0, e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := r.Context()
	tx, e := s.beginRepair(ctx)
	if e != nil {
		return nil, 0, e
	}
	defer tx.Rollback()
	now := s.Now()
	id, action, e := idRoute(strings.TrimPrefix(r.URL.Path, "/api/ssh-tickets"), uuidPattern, "経路が見つかりません。")
	if e != nil {
		return nil, 0, e
	}
	// Expiry is durable even when the requested transition is rejected.
	if e = expireSSHTickets(ctx, tx, now, policy.TTL); e != nil {
		return nil, 0, e
	}
	if e = tx.Commit(); e != nil {
		return nil, 0, e
	}
	tx, e = s.beginRepair(ctx)
	if e != nil {
		return nil, 0, e
	}
	defer tx.Rollback()
	if r.Method == http.MethodGet {
		return readSSHTickets(ctx, tx, id, action)
	}
	if r.Method != http.MethodPost {
		return nil, 0, bad(405, "このメソッドは使えません。")
	}
	requester, token := sshRequester(r, p)
	status := 200
	if id == "" {
		if id, e = s.createSSHTicket(ctx, tx, r, p, policy, requester, token, now); e != nil {
			return nil, 0, e
		}
		status = 201
		action = "requested"
	} else if e = s.transitionSSHTicket(ctx, tx, r, p, policy, requester, id, action, now); e != nil {
		return nil, 0, e
	}
	row, e := queryMap(ctx, tx, "SELECT * FROM ssh_tickets WHERE id=?", id)
	if e != nil {
		return nil, 0, e
	}
	if e = tx.Commit(); e != nil {
		return nil, 0, e
	}
	s.log("SSH申請の操作を保存しました: " + action + " " + id)
	s.notify("ssh-ticket", id)
	return sshTicketResponse(row, action, policy), status, nil
}

// readSSHTickets answers GET for the collection or one ticket.
func readSSHTickets(ctx context.Context, tx *repairTx, id, action string) (any, int, error) {
	if action != "" {
		return nil, 0, bad(405, "このメソッドは使えません。")
	}
	if id == "" {
		rows, e := queryMaps(ctx, tx, "SELECT * FROM ssh_tickets ORDER BY requested_at DESC,id")
		if e != nil {
			return nil, 0, e
		}
		out := []map[string]any{}
		for _, row := range rows {
			out = append(out, sshView(row))
		}
		return map[string]any{"tickets": out}, 200, nil
	}
	row, e := queryMap(ctx, tx, "SELECT * FROM ssh_tickets WHERE id=?", id)
	if e != nil {
		return nil, 0, e
	}
	if row == nil {
		return nil, 0, bad(404, "申請が見つかりません。")
	}
	return sshView(row), 200, nil
}

// sshRequester names who sends the request and, for a PC, the token recorded
// as the ticket's owner.
func sshRequester(r *http.Request, p auth.Principal) (requester string, token any) {
	requester = actor(p)
	if p.Kind == auth.PC {
		token = p.TokenID
		requester = "token:" + p.TokenID + ":" + p.Name
	} else if p.Kind == auth.Screen {
		csrf := r.Header.Get("X-CSRF-Token")
		requester = "screen:" + auth.Hash(csrf)
	}
	return requester, token
}

// createSSHTicket validates a new request, decides an automatic approval and
// saves the ticket. It returns the new ticket id.
var directPRHead = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (s *service) createSSHTicket(ctx context.Context, tx *repairTx, r *http.Request, p auth.Principal, policy *hostSSHPolicy, requester string, token any, now time.Time) (string, error) {
	data, e := body(r)
	if e != nil {
		return "", e
	}
	host, operation, reason, evidence := str(data["host"]), str(data["operation"]), str(data["reason"]), str(data["evidence"])
	emergency, emergencyOK := data["emergency"].(bool)
	if !policy.allows(host, operation) || !textValid(reason, 4000) || !textValid(evidence, 16000) || !emergencyOK {
		return "", bad(400, "申請の入力を確認してください。")
	}
	approver := ""
	if raw, present := data["directPR"]; present {
		proof, ok := parseDirectPRProof(raw)
		if !ok || !fleet.ValidRepo(proof.repo) || operation != "verify" || emergency {
			return "", bad(400, "直接PRの固定検証の申請を確認してください。")
		}
		if p.Kind != auth.PC || !p.Delegated || !policy.allowsDirectPRVerify(proof.repo, p.Name, host, operation) {
			return "", bad(403, "直接PRの固定検証の申請元または対象が許可されていません。")
		}
		if s.DirectPRBase == nil {
			return "", bad(503, "対象リポジトリの既定ブランチを確認できません。")
		}
		lookup, cancel := context.WithTimeout(ctx, 3*time.Second)
		base, err := s.DirectPRBase(lookup, proof.repo)
		cancel()
		if err != nil {
			return "", bad(503, "対象リポジトリの既定ブランチを確認できません。")
		}
		if base != proof.base {
			return "", bad(409, "対象リポジトリの既定ブランチが変わりました。")
		}
		approver = "policy:direct-pr:" + proof.repo
		evidence = "repo=" + proof.repo + " head=" + proof.head + " base=" + proof.base + " branch=" + proof.branch + " purpose=direct-pr-verify"
	}
	if approver == "" {
		approver, e = sshAutoApprover(ctx, tx, p, policy, host, operation, now)
		if e != nil {
			return "", e
		}
	}
	id := randomID()
	ticketStatus := "requested"
	var approvedBy, decided any
	if approver != "" {
		ticketStatus = "approved"
		approvedBy = approver
		decided = iso(now)
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO ssh_tickets(id,host,operation,reason,evidence,emergency,status,requester,requested_token,requested_at,approver,decided_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", id, host, operation, reason, evidence, emergency, ticketStatus, requester, token, iso(now), approvedBy, decided)
	if e != nil {
		return "", e
	}
	return id, nil
}

// sshAutoApprover returns the approver recorded when a request is approved
// without the screen: a registered read-only operation, or an active host-ssh
// grant. An empty result leaves the ticket requested.
func sshAutoApprover(ctx context.Context, tx *repairTx, p auth.Principal, policy *hostSSHPolicy, host, operation string, now time.Time) (string, error) {
	if !(p.Kind == auth.CLI || p.Kind == auth.PC && p.Delegated) {
		return "", nil
	}
	if contains(policy.ReadOnlyOperations, operation) {
		return "policy:status", nil
	}
	grants, e := queryMaps(ctx, tx, "SELECT * FROM permission_grants WHERE kind='host-ssh' AND target=? AND revoked_at IS NULL ORDER BY created_at,id FOR UPDATE", host+":"+operation)
	if e != nil {
		return "", e
	}
	for _, grant := range grants {
		if grantStatus(grant, now) == "active" {
			return "grant:" + str(grant["id"]), nil
		}
	}
	return "", nil
}

// transitionSSHTicket applies approve, deny, issue or result to a saved ticket.
func (s *service) transitionSSHTicket(ctx context.Context, tx *repairTx, r *http.Request, p auth.Principal, policy *hostSSHPolicy, requester, id, action string, now time.Time) error {
	row, e := queryMap(ctx, tx, "SELECT * FROM ssh_tickets WHERE id=? FOR UPDATE", id)
	if e != nil {
		return e
	}
	if row == nil {
		return bad(404, "申請が見つかりません。")
	}
	switch action {
	case "approve", "deny":
		return decideSSHTicket(ctx, tx, r, p, policy, row, requester, id, action, now)
	case "issue":
		return s.issueSSHTicket(ctx, tx, p, policy, row, id, now)
	case "result":
		return reportSSHTicket(ctx, tx, r, p, row, id, now)
	default:
		return bad(404, "経路が見つかりません。")
	}
}

// decideSSHTicket records the screen's approval or denial of a requested ticket.
func decideSSHTicket(ctx context.Context, tx *repairTx, r *http.Request, p auth.Principal, policy *hostSSHPolicy, row map[string]any, requester, id, action string, now time.Time) error {
	if p.Kind != auth.Screen || headerPresent(r, "X-Desk-Key") {
		return bad(403, "承認・却下は受付画面からだけ使えます。")
	}
	if row["status"] != "requested" {
		return bad(409, "申請済みの状態でだけ承認・却下できます。")
	}
	if action == "approve" && !policy.allows(str(row["host"]), str(row["operation"])) {
		return bad(409, "対象PCか操作が現在の方針で許可されていません。")
	}
	next := "approved"
	if action == "deny" {
		next = "denied"
	}
	_, e := tx.ExecContext(ctx, "UPDATE ssh_tickets SET status=?,approver=?,decided_at=? WHERE id=? AND status='requested'", next, requester, iso(now), id)
	return e
}

// issueSSHTicket lets the signer token issue an approved, unexpired ticket that
// the current policy still allows.
func (s *service) issueSSHTicket(ctx context.Context, tx *repairTx, p auth.Principal, policy *hostSSHPolicy, row map[string]any, id string, now time.Time) error {
	if p.Kind != auth.PC || p.Name != policy.Signer.Token {
		return bad(403, "発券は署名役のトークンだけ使えます。")
	}
	host, operation := str(row["host"]), str(row["operation"])
	if !policy.allows(host, operation) {
		return bad(409, "対象PCか操作が現在の方針で許可されていません。")
	}
	if str(row["approver"]) == "policy:status" && !contains(policy.ReadOnlyOperations, operation) {
		return bad(409, "読み取り専用の承認は現在の方針で許可されていません。")
	}
	if repo, directPR := strings.CutPrefix(str(row["approver"]), "policy:direct-pr:"); directPR {
		var requester string
		token := str(row["requested_token"])
		err := s.AppDB.QueryRowContext(ctx, bindSQL("SELECT t.name FROM api_tokens t JOIN api_token_grants g ON t.id=g.token_id WHERE t.id=?"), token).Scan(&requester)
		if err != nil || !policy.allowsDirectPRVerify(repo, requester, host, operation) || directPRTicketHead(row, repo) == "" {
			return bad(409, "直接PRの固定検証は現在の方針または運用代行で許可されていません。")
		}
	}
	decided, valid := moment(row["decided_at"])
	if row["status"] != "approved" || row["approver"] == nil || !valid || !decided.After(now.Add(-time.Duration(policy.TTL)*time.Minute)) || row["issued_at"] != nil {
		return bad(409, "承認済み・期限内・未発券の申請だけ発券できます。")
	}
	_, e := tx.ExecContext(ctx, "UPDATE ssh_tickets SET status='issued',issued_at=?,expires_at=? WHERE id=? AND status='approved' AND issued_at IS NULL", iso(now), iso(now.Add(time.Duration(policy.TTL)*time.Minute)), id)
	return e
}

// reportSSHTicket validates the requester's connection result and records it
// once on an issued ticket, with secrets removed from the summary.
func reportSSHTicket(ctx context.Context, tx *repairTx, r *http.Request, p auth.Principal, row map[string]any, id string, now time.Time) error {
	owned := p.Kind == auth.PC && row["requested_token"] == p.TokenID || p.Kind == auth.CLI && row["requester"] == "temporary-key" && row["requested_token"] == nil
	if !owned {
		return bad(403, "結果は申請元だけ記録できます。")
	}
	data, e := body(r)
	if e != nil {
		return e
	}
	result := str(data["connectionResult"])
	_, hasExit := data["exitCode"]
	exit, exitOK := integer(data["exitCode"])
	summary, summaryOK := data["summary"].(string)
	if !hasExit || result != "connected" && result != "failed" || data["exitCode"] != nil && (!exitOK || exit < -2147483648 || exit > 2147483647) || !summaryOK {
		return bad(400, "結果の入力を確認してください。")
	}
	if row["status"] != "issued" || row["reported_at"] != nil {
		return bad(409, "結果は発券済みの申請に1回だけ記録できます。")
	}
	var exitValue any
	if data["exitCode"] != nil {
		exitValue = exit
	}
	summary = usage.Redact(redactAuthorization(summary))
	if contract.UTF16Length(summary) > 2000 {
		summary = contract.ClipUTF16(summary, 2000)
	}
	_, e = tx.ExecContext(ctx, "UPDATE ssh_tickets SET status='reported',connection_result=?,exit_code=?,summary=?,reported_at=? WHERE id=? AND status='issued' AND reported_at IS NULL", result, exitValue, summary, iso(now), id)
	return e
}

// sshTicketResponse builds the saved ticket's view. An issued ticket also
// carries what the signer needs to sign the certificate.
func directPRTicketHead(row map[string]any, repo string) string {
	parts := strings.Fields(str(row["evidence"]))
	if len(parts) != 5 || parts[0] != "repo="+repo || parts[4] != "purpose=direct-pr-verify" {
		return ""
	}
	head, ok := strings.CutPrefix(parts[1], "head=")
	if !ok || !directPRHead.MatchString(head) {
		return ""
	}
	return head
}

func sshTicketResponse(row map[string]any, action string, policy *hostSSHPolicy) map[string]any {
	out := sshView(row)
	if action == "issue" {
		host := policy.Hosts[str(row["host"])]
		out["principal"] = host.Principal
		out["ttlMinutes"] = policy.TTL
		command := policy.Operations[str(row["operation"])]
		if repo, directPR := strings.CutPrefix(str(row["approver"]), "policy:direct-pr:"); directPR {
			command = "ISSUE_DESK_DIRECT_PR_HEAD=" + directPRTicketHead(row, repo) + " " + command
		}
		out["forceCommand"] = command
	}
	return out
}
