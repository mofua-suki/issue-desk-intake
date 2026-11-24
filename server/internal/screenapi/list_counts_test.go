package screenapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"math/rand/v2"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"sort"
	"testing"
	"time"
)

func closedConsultationCases() []Snapshot {
	cases := []Snapshot{}
	for _, parent := range []bool{false, true} {
		for _, cleanup := range []bool{false, true} {
			for _, references := range []Object{
				{},
				{"issue": Object{"number": 1}},
				{"issues": []any{Object{"number": 1}, Object{"number": 2}}},
				{"issue": Object{"number": 1}, "proposal": Object{"kind": "followup"}},
				{"issue": Object{"number": 1}, "splitIssues": []any{Object{"title": "saved proposal"}}, "messages": []any{Object{"images": []any{Object{"name": "saved image"}}}}},
			} {
				draft := with(references, Object{"id": "closed", "project": "owner/repo", "status": "closed", "questions": []any{"古い質問"}, "executionActive": true})
				if parent {
					draft["parentSplit"] = "parent"
				}
				if cleanup {
					draft["cleanup"] = Object{"reason": "finished"}
				}
				cases = append(cases, Snapshot{Rows: []Row{{Key: "draft:closed", Type: "draft", Item: draft}}, Aux: map[string]any{"reviews": []any{}, "stalls": []any{}, "fullTests": []any{}, "attentionAlerts": []any{}}})
			}
		}
	}
	return cases
}

func TestClosedConsultationsKeepHistoryWithoutRevivingWork(t *testing.T) {
	for index, snapshot := range closedConsultationCases() {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			rows := classifiedListRows(snapshot)
			if len(rows) != 1 || rows[0]["kind"] != "draft" || rows[0]["id"] != "closed" || rows[0]["status"] != "closed" {
				t.Fatalf("closed history was replaced: %v", rows)
			}
			for name, count := range countSnapshotGroups(listWorkGroups(snapshot), snapshot) {
				if fleet.Number(count) != 0 {
					t.Fatalf("closed consultation appeared in %s: %v", name, count)
				}
			}
			for _, live := range []struct {
				group string
				issue Object
			}{
				{"running", Object{"status": "running", "executionActive": true}},
				{"waiting", Object{"status": "queued", "executionScheduled": true}},
				{"stopped", Object{"status": "failed"}},
				{"attention", Object{"status": "waiting", "requiresUser": true, "nextAction": "対象の承認を判断する"}},
			} {
				issue := with(live.issue, Object{"project": "owner/repo", "number": 1, "intakeId": "closed"})
				withIssue := snapshot
				withIssue.Rows = append(append([]Row{}, snapshot.Rows...), Row{Key: "issue:1", Type: "issue", Item: issue})
				groups := listWorkGroups(withIssue)
				if len(fleet.Items(groups[live.group])) != 1 || fleet.ObjectOrEmpty(fleet.Items(groups[live.group])[0])["kind"] != "issue" {
					t.Fatalf("closed consultation hid %s Issue: %v", live.group, groups)
				}
				for group, raw := range groups {
					if group != live.group && len(fleet.Items(raw)) != 0 {
						t.Fatalf("closed consultation revived in %s: %v", group, groups)
					}
				}
			}
		})
	}
}

func parentConsultationCases() []Snapshot {
	cases := []Snapshot{}
	for _, status := range []string{"draft", "intake_waiting", "thinking", "submit_error", "submit_unknown", "submitted", "error", "closed", "project_handoff", "investigation_reported"} {
		for _, link := range []struct{ parent, child Object }{
			{Object{"hasChildren": true}, Object{}},
			{Object{"splitChildren": []any{"child"}}, Object{}},
			{Object{"consultationChildren": []any{"child"}}, Object{}},
			{Object{}, Object{"parentSplit": "parent"}},
			{Object{}, Object{"parentConsultation": "parent"}},
		} {
			parent := with(link.parent, Object{"id": "parent", "project": "owner/repo", "status": status, "questions": []any{"親の古い質問"}, "splitIssues": []any{Object{}}, "messages": []any{Object{"images": []any{Object{}}}}})
			child := with(link.child, Object{"id": "child", "project": "owner/repo", "status": "draft", "questions": []any{"子の判断"}})
			snapshot := Snapshot{Rows: []Row{
				{Key: "draft:parent", Type: "draft", Item: parent},
				{Key: "draft:child", Type: "draft", Item: child},
				{Key: "issue:1", Type: "issue", Item: Object{"project": "owner/repo", "number": 1, "intakeId": "parent", "status": "running", "executionActive": true}},
			}, Aux: map[string]any{"reviews": []any{}, "stalls": []any{Object{"id": "parent-stall", "draftId": "parent", "title": "親の止まり", "detail": "親の詳細"}}, "fullTests": []any{}, "attentionAlerts": []any{Object{"key": "parent-budget", "kind": "consultation_budget", "draftId": "parent", "requiresUser": true, "nextAction": "親の質問"}}}}
			cases = append(cases, snapshot)
			projected := snapshot
			projected.Rows = append([]Row{}, snapshot.Rows...)
			for i, row := range projected.Rows {
				projected.Rows[i].Item = Summarize(row.Type, row.Item)
			}
			cases = append(cases, projected)
			for _, base := range []Snapshot{snapshot, projected} {
				followup := base
				followup.Rows = append([]Row{}, base.Rows...)
				followup.Rows[0].Item = with(base.Rows[0].Item, Object{"proposal": Object{"kind": "followup"}})
				cases = append(cases, followup)
			}
		}
	}
	return cases
}

func TestParentsStayOutOfWorkAndKeepChildren(t *testing.T) {
	for index, snapshot := range parentConsultationCases() {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			groups := listWorkGroups(snapshot)
			if counts := countSnapshotGroups(groups, snapshot); fleet.Number(counts["followups"]) != 0 {
				t.Fatalf("parent appeared in followup count: %v", counts)
			}
			if len(fleet.Items(groups["attention"])) != 1 || fleet.ObjectOrEmpty(fleet.Items(groups["attention"])[0])["id"] != "child" || len(fleet.Items(groups["running"])) != 1 || len(fleet.Items(groups["waiting"])) != 0 || len(fleet.Items(groups["stopped"])) != 0 {
				t.Fatalf("parent replaced or duplicated child work: %v", groups)
			}
			for _, row := range classifiedListRows(snapshot) {
				if row["id"] == "parent" || row["draftId"] == "parent" {
					t.Fatalf("parent appeared in a list: %v", row)
				}
			}
		})
	}
	for _, extra := range []Object{{}, {"hasChildren": false}, {"splitChildren": []any{}, "consultationChildren": []any{}}, {"splitChildren": []any{""}}} {
		draft := with(extra, Object{"id": "proposal", "status": "draft", "questions": []any{"未作成の案を確認する"}, "splitIssues": []any{Object{}, Object{}}})
		snapshot := Snapshot{Rows: []Row{{Type: "draft", Item: draft}}, Aux: map[string]any{}}
		groups := listWorkGroups(snapshot)
		if len(fleet.Items(groups["attention"])) != 1 || fleet.ObjectOrEmpty(fleet.Items(groups["attention"])[0])["id"] != "proposal" {
			t.Fatalf("uncreated split proposal was hidden: %v", groups)
		}
	}
}

func TestListCountsMatchScreenTabs(t *testing.T) {
	empty := func() Snapshot {
		return Snapshot{Rows: []Row{}, Aux: map[string]any{"reviews": []any{}, "stalls": []any{}, "fullTests": []any{}, "attentionAlerts": []any{}}}
	}
	makeSnapshot := func(issues, drafts, closes []Object) Snapshot {
		s := empty()
		for _, group := range []struct {
			kind  string
			items []Object
		}{{"issue", issues}, {"draft", drafts}, {"closeRequest", closes}} {
			for i, item := range group.items {
				s.Rows = append(s.Rows, Row{Key: fmt.Sprintf("%s:%d", group.kind, i), Type: group.kind, Item: item})
			}
		}
		return s
	}
	budget := empty()
	budget.Aux["attentionAlerts"] = []any{Object{"key": "budget:draft", "kind": "consultation_budget", "requiresUser": true, "nextActor": "user", "nextAction": "相談の上限を足して続行するか判断する"}, Object{"key": "usage:codex", "kind": "usage_limit_wait", "requiresUser": true, "nextActor": "user", "nextAction": "再試行するか判断する"}}
	gist := empty()
	gist.Aux["attentionAlerts"] = []any{Object{"key": "job-recovery:owner/repo#1", "kind": "job_recovery", "project": "owner/repo", "requiresUser": true, "gist": "対象データの削除を判断してください。"}}
	pending := makeSnapshot([]Object{{"project": "owner/repo", "number": 7, "status": "completed", "executionProgress": Object{"pending": 1}}}, nil, nil)
	pending.Aux["attentionAlerts"] = []any{Object{"key": "job-recovery:owner/repo#7", "kind": "job_recovery", "project": "owner/repo", "number": 7, "requiresUser": true, "nextActor": "user", "nextAction": "上限超えを判断する"}}
	closedPending := makeSnapshot([]Object{{"project": "owner/repo", "number": 7, "state": "closed", "status": "completed", "executionProgress": Object{"pending": 1}}}, nil, nil)
	closedPending.Aux["attentionAlerts"] = pending.Aux["attentionAlerts"]
	corpus := []Snapshot{gist, budget, pending, closedPending,
		makeSnapshot([]Object{
			{"project": "owner/repo", "number": 21, "state": "closed", "status": "completed", "executionProgress": Object{"pending": 1}},
			{"project": "owner/repo", "number": 22, "state": "closed", "status": "completed", "executionProgress": Object{"pending": 1, "checks": []any{Object{"status": "failed"}}}},
			{"project": "owner/repo", "number": 23, "state": "closed", "status": "completed", "executionProgress": Object{"unavailable": "読取失敗"}},
			{"project": "owner/repo", "number": 24, "state": "closed", "status": "completed", "executionActive": true, "executionProgress": Object{"pending": 1}},
			{"project": "owner/repo", "number": 25, "state": "closed", "status": "completed", "executionScheduled": true, "executionProgress": Object{"pending": 1}},
			{"project": "owner/repo", "number": 26, "state": "closed", "status": "completed", "handoff": true, "executionProgress": Object{"pending": 1}},
			{"project": "owner/repo", "number": 27, "state": "closed", "status": "completed", "requiresUser": true, "executionProgress": Object{"pending": 1}},
			{"project": "owner/repo", "number": 28, "state": "closed", "status": "completed", "recoveryState": "stopped", "executionProgress": Object{"pending": 1}},
		}, nil, nil),
		makeSnapshot([]Object{{"project": "owner/repo", "number": 9, "state": "closed", "status": "review_held", "requiresUser": true, "nextAction": "上限超えを判断する", "executionProgress": Object{"pending": 1}}, {"project": "owner/repo", "number": 7, "state": "closed", "status": "review_held", "executionProgress": Object{"pending": 1}}, {"project": "owner/repo", "number": 8, "state": "closed", "status": "closed", "executionProgress": Object{"pending": 1}}}, nil, nil),
		makeSnapshot([]Object{{"project": "owner/repo", "number": 1, "status": "completed", "executionProgress": Object{"pending": 1}}, {"project": "owner/repo", "number": 2, "status": "completed", "executionProgress": Object{"unavailable": "読取失敗"}}, {"project": "owner/repo", "number": 3, "status": "completed", "executionProgress": Object{"pending": 0}}}, nil, nil),
		empty(),
		makeSnapshot([]Object{{"project": "owner/repo", "number": 1, "status": "failed"}}, []Object{{"id": "parent", "project": "owner/repo", "status": "submitted", "issues": []any{Object{"number": 1}}, "familyProgress": Object{"total": 2, "completed": 1, "remaining": 1}}}, nil),
		makeSnapshot([]Object{{"project": "owner/repo", "number": 1, "status": "running", "intakeId": "draft"}}, []Object{{"id": "draft", "project": "owner/repo", "status": "submit_error", "splitIssues": []any{Object{}}, "issues": []any{Object{"number": 1}}}}, nil),
		makeSnapshot(nil, []Object{{"id": "draft", "project": "owner/repo", "status": "investigation_reported", "questions": []any{"判断は？"}, "investigations": []any{Object{"project": "owner/repo", "number": 1}}}}, nil),
		makeSnapshot(nil, []Object{{"id": "p", "status": "draft", "splitIssues": []any{}, "actions": []any{"finish"}, "familyProgress": Object{"total": 2, "completed": 1, "closed": 1, "remaining": 0, "unknown": 0}}}, nil),
	}
	corpus = append(corpus, makeSnapshot([]Object{
		{"project": "owner/repo", "number": 1, "status": "completed", "followup": true, "executionProgress": Object{"pending": 1}},
		{"project": "owner/repo", "number": 2, "status": "completed", "followup": true, "executionProgress": Object{"unavailable": "受入確認を読めません"}},
		{"project": "owner/repo", "number": 3, "status": "closed", "followup": true, "executionProgress": Object{"pending": 1}},
		{"project": "owner/repo", "number": 4, "status": "completed", "executionProgress": Object{"pending": 1}},
	}, []Object{
		{"id": "closed-followup", "status": "closed", "proposal": Object{"kind": "followup"}, "executionProgress": Object{"pending": 1}},
		{"id": "completed-followup", "status": "completed", "proposal": Object{"kind": "followup"}, "executionProgress": Object{"pending": 1}},
	}, nil))
	corpus = append(corpus, closedConsultationCases()...)
	corpus = append(corpus, parentConsultationCases()...)
	for _, status := range []string{"error", "stalled", "submit_error", "submit_unknown", "thinking"} {
		for _, extra := range []Object{{}, {"executionActive": true}, {"executionScheduled": true}, {"retryAt": "2026-11-23T12:00:00Z"}} {
			draft := with(extra, Object{"id": "followup-stopped", "status": status, "turnStopped": true, "proposal": Object{"kind": "followup"}})
			s := makeSnapshot(nil, []Object{draft}, nil)
			s.Aux["attentionAlerts"] = []any{Object{"key": "notice", "kind": "job_recovery", "draftId": "followup-stopped", "handoff": true, "reason": "再開を確認"}}
			corpus = append(corpus, s)
		}
	}
	for _, text := range []string{"\u0085", "\ufeff", "\v原因\ufeff", "　同じ\n原因　"} {
		s := makeSnapshot(nil, []Object{{"id": "question", "status": "draft", "questions": []any{text}}}, nil)
		s.Aux["attentionAlerts"] = []any{Object{"key": "a", "project": "owner/repo", "kind": "job_recovery", "handoff": true, "reason": text}, Object{"key": "a", "project": "owner/repo", "kind": "job_recovery", "handoff": true, "reason": text}}
		corpus = append(corpus, s)
	}
	random := rand.New(rand.NewPCG(123, 456))
	statuses := []string{"pending", "queued", "running", "publishing", "releasing", "deploying", "scope_waiting", "project_waiting", "dependency_waiting", "prerequisite_waiting", "failed", "review_held", "waiting", "stopped", "untracked", "completed", "closed"}
	draftStatuses := []string{"draft", "intake_waiting", "thinking", "submitted", "error", "stalled", "submit_error", "submit_unknown", "investigation_waiting", "investigation_reported", "investigation_ended", "project_handoff", "closed"}
	kinds := []string{"job_recovery", "consultation_stop", "consultation_budget", "usage_limit_wait", "main_broken", "ops_watch", "failed", "review_held", "prerequisite_missing", "unknown"}
	for n := 0; n < 500; n++ {
		issues := []Object{}
		drafts := []Object{}
		closes := []Object{}
		for i := 0; i < 6; i++ {
			issue := Object{"project": "owner/repo", "number": i + 1, "status": statuses[random.IntN(len(statuses))], "intakeId": fmt.Sprintf("d%d", i), "executionActive": random.IntN(3) == 0, "executionScheduled": random.IntN(3) == 0}
			draft := Object{"id": fmt.Sprintf("d%d", i), "project": "owner/repo", "status": draftStatuses[random.IntN(len(draftStatuses))], "executionActive": random.IntN(3) == 0, "executionScheduled": random.IntN(3) == 0}
			switch random.IntN(8) {
			case 0:
				issue["followup"] = true
			case 1:
				issue["requiresUser"] = true
			case 2:
				issue["permissionApproval"] = Object{}
			case 3:
				issue["pendingFinalApprovals"] = []any{Object{"id": "approval"}}
			case 4:
				issue["state"] = "closed"
			case 5:
				issue["causeKey"] = "shared"
			case 6:
				issue["interruptDelay"] = Object{}
			case 7:
				issue["deletionApproval"] = Object{}
			}
			switch random.IntN(9) {
			case 0:
				draft["parentSplit"] = "parent"
			case 1:
				draft["questions"] = []any{" ", "判断は？"}
			case 2:
				draft["issue"] = Object{"number": i + 1}
			case 3:
				draft["splitIssues"] = []any{Object{}}
			case 4:
				draft["familyProgress"] = Object{"total": 2, "completed": 1, "remaining": 1}
			case 5:
				draft["messages"] = []any{Object{"images": []any{Object{}}}}
			case 6:
				draft["proposal"] = Object{"kind": "followup"}
			case 7:
				draft["turnStopped"] = true
			case 8:
				draft["investigations"] = []any{Object{"project": "owner/repo", "number": 9}}
			}
			issues = append(issues, issue)
			drafts = append(drafts, draft)
		}
		for i, status := range []string{"confirm", "reviewing", "closing", "closed"} {
			closes = append(closes, Object{"id": fmt.Sprintf("c%d", i), "project": "owner/repo", "number": i + 1, "status": status})
		}
		s := makeSnapshot(issues, drafts, closes)
		s.Aux["reviews"] = []any{Object{"round": n % 3, "requiresUser": n%2 == 0}}
		s.Aux["fullTests"] = []any{Object{"id": "full-test", "title": "テスト", "requiresUser": n%2 == 0, "causeKey": "same"}}
		s.Aux["stalls"] = []any{Object{"id": "stall", "title": "止まり", "detail": "詳細", "requiresUser": n%3 == 0, "causeKey": "same"}}
		alerts := []any{}
		for i := 0; i < 4; i++ {
			a := Object{"key": "same", "kind": kinds[random.IntN(len(kinds))], "reason": "　同じ\n原因　", "number": i%2 + 1, "project": "owner/repo"}
			switch random.IntN(6) {
			case 0:
				a["requiresUser"] = true
			case 1:
				a["requiresUser"] = false
			case 2:
				a["handoff"] = true
			case 3:
				a["state"] = "action_failed"
			case 4:
				a["causeKey"] = "same"
			case 5:
				a["state"] = "closed"
			}
			alerts = append(alerts, a)
		}
		s.Aux["attentionAlerts"] = alerts
		corpus = append(corpus, s)
	}

	for _, reverse := range []bool{false, true} {
		a := Object{"key": "stop", "kind": "job_recovery", "causeKey": "same", "project": "owner/repo", "number": 1, "handoff": true, "reason": "復旧できず"}
		b := Object{"key": "decision", "kind": "job_recovery", "causeKey": "same", "project": "owner/repo", "number": 1, "requiresUser": true, "nextAction": "新しい対象を承認するか判断する"}
		alerts := []any{a, b}
		if reverse {
			alerts = []any{b, a}
		}
		for _, issues := range [][]Object{nil, {{"project": "owner/repo", "number": 1, "status": "running", "executionActive": true}}} {
			s := makeSnapshot(issues, nil, nil)
			s.Aux["attentionAlerts"] = alerts
			corpus = append(corpus, s)
		}
	}
	stopDraft := makeSnapshot(nil, []Object{{"id": "stopped-draft", "project": "owner/repo", "status": "stalled"}}, nil)
	stopDraft.Aux["attentionAlerts"] = []any{Object{"key": "consultation-stop:stopped-draft", "kind": "consultation_stop", "project": "owner/repo", "draftId": "stopped-draft", "state": "stopped", "handoff": true, "requiresUser": false, "reason": "前提のマージ待ち"}}
	corpus = append(corpus, stopDraft)
	startCases := len(corpus)
	checks := []struct {
		issue  Object
		alerts []any
		state  string
	}{
		{Object{"status": "running", "executionActive": true, "executionUntil": "2099-01-01T00:00:00Z"}, []any{Object{"kind": "incident", "reason": "作業の遅延", "createdAt": "2000-01-01T00:00:00Z"}}, "running"},
		{Object{"status": "running", "executionActive": true}, []any{Object{"kind": "incident", "reason": "別の表現"}}, "running"},
		{Object{"status": "running", "executionActive": true, "permissionApproval": Object{}}, []any{Object{"kind": "incident", "reason": "遅延"}}, "attention"},
		{Object{"status": "running", "executionActive": true}, []any{Object{"kind": "incident", "requiresUser": true, "nextAction": "どちらを選びますか", "reason": "遅延"}}, "attention"},
		{Object{"status": "running", "executionActive": true}, []any{Object{"kind": "incident", "state": "stopped"}}, "stopped"},
		{Object{"status": "running", "executionActive": true}, []any{Object{"kind": "incident", "recoveryState": "action_failed", "reason": "復旧失敗"}}, "stopped"},
		{Object{"status": "running", "executionActive": true}, []any{Object{"kind": "incident", "handoff": true, "reason": "引継ぎ"}}, "stopped"},
		{Object{"status": "dependency_waiting", "executionScheduled": true}, []any{Object{"kind": "incident", "reason": "遅延"}}, "waiting"},
		{Object{"status": "running"}, []any{Object{"kind": "incident", "reason": "遅延"}}, "stopped"},
		{Object{"status": "review_held"}, []any{Object{"kind": "incident", "reason": "遅延"}}, "stopped"},
		{Object{"status": "running", "executionActive": true, "finalApprovalsUnavailable": "承認情報を取得できません"}, []any{Object{"kind": "incident", "reason": "遅延"}}, "stopped"},
		{Object{"status": "running", "executionActive": true}, []any{Object{"kind": "incident", "reason": "遅延"}, Object{"kind": "incident", "state": "stopped", "reason": "停止"}}, "stopped"},
	}
	for _, check := range checks {
		s := makeSnapshot([]Object{with(check.issue, Object{"project": "owner/repo", "number": 101})}, nil, nil)
		alerts := []any{}
		for i, raw := range check.alerts {
			alerts = append(alerts, with(fleet.ObjectOrEmpty(raw), Object{"key": fmt.Sprintf("case-%d", i), "project": "owner/repo", "number": 101}))
		}
		s.Aux["attentionAlerts"] = alerts
		corpus = append(corpus, s)
	}
	raw, err := json.Marshal(corpus)
	if err != nil {
		t.Fatal(err)
	}
	// Normalize fixture numbers/arrays exactly as production JSON readers do.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err = decoder.Decode(&corpus); err != nil {
		t.Fatal(err)
	}
	script := `import {registerHooks} from 'node:module';
registerHooks({resolve(specifier,context,next){try{return next(specifier,context);}catch(error){if(specifier.endsWith('.js')&&context.parentURL?.startsWith('file:'))return next(new URL(specifier.slice(0,-3)+'.ts',context.parentURL).href,context);throw error;}}});
const {requestGroupsWithAux}=await import('../../../client/src/ui/request-list.ts');
const chunks=[];for await(const chunk of process.stdin)chunks.push(chunk);
const input=Buffer.concat(chunks).toString('utf8');
console.log(JSON.stringify(JSON.parse(input).map(s=>{const get=type=>s.Rows.filter(r=>r.type===type).map(r=>r.item);const g=requestGroupsWithAux({issues:get('issue'),drafts:get('draft'),todos:[],closes:get('closeRequest'),reviews:s.Aux.reviews,stalls:s.Aux.stalls,fullTests:s.Aux.fullTests,attentionAlerts:s.Aux.attentionAlerts});return {attention:g.attention.length,running:g.issues.length,waiting:g.waiting.length,stopped:g.stopped.length,inProgress:g.issues.length,followups:g.followups.length,workGroups:Object.fromEntries([['attention','attention'],['running','issues'],['waiting','waiting'],['stopped','stopped']].map(([key,tab])=>[key,g[tab].map(r=>({kind:r.kind,project:r.project??null,number:r.number??null,id:r.id??null,work:r.work,related:(r.relatedAlerts??[]).map(a=>({key:a.key??null,reason:a.reason??null}))})).sort((a,b)=>JSON.stringify(a).localeCompare(JSON.stringify(b)))]))};})));`
	cmd := exec.Command("node", "--input-type=module", "-e", script)
	cmd.Stdin = bytes.NewReader(raw)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("画面の分類の実行に失敗: %v %s", err, out)
	}
	var expected []Object
	if err = json.Unmarshal(out, &expected); err != nil {
		t.Fatal(err, string(out))
	}
	for i, s := range corpus {

		groups := listWorkGroups(s)
		if i >= startCases {
			check := checks[i-startCases]
			rows := fleet.Items(groups[check.state])
			if len(rows) != 1 {
				t.Fatalf("事例%d: expected %s: %v", i, check.state, groups)
			}
			row := fleet.ObjectOrEmpty(rows[0])
			related := fleet.Items(row["relatedAlerts"])
			if len(related) != len(check.alerts) {
				t.Fatalf("事例%d: related alerts lost: %v", i, row)
			}
			for _, alert := range related {
				item := fleet.ObjectOrEmpty(alert)
				var index int
				if _, err := fmt.Sscanf(fleet.Text(item["key"]), "case-%d", &index); err != nil || index >= len(check.alerts) || item["reason"] != fleet.ObjectOrEmpty(check.alerts[index])["reason"] {
					t.Fatalf("事例%d: related reason lost: %v", i, related)
				}
			}
		}
		for _, name := range []string{"attention", "running", "waiting", "stopped"} {
			actual := []Object{}
			for _, raw := range fleet.Items(groups[name]) {
				r := fleet.ObjectOrEmpty(raw)
				actual = append(actual, Object{"kind": r["kind"], "project": r["project"], "number": r["number"], "id": r["id"], "work": r["work"], "related": func() []Object {
					v := []Object{}
					for _, a := range fleet.Items(r["relatedAlerts"]) {
						alert := fleet.ObjectOrEmpty(a)
						v = append(v, Object{"key": alert["key"], "reason": alert["reason"]})
					}
					return v
				}()})
			}
			expectedRows := fleet.Items(fleet.ObjectOrEmpty(expected[i]["workGroups"])[name])
			actualJSON, expectedJSON := []string{}, []string{}
			for _, row := range actual {
				raw, _ := json.Marshal(row)
				actualJSON = append(actualJSON, string(raw))
			}
			for _, row := range expectedRows {
				raw, _ := json.Marshal(row)
				expectedJSON = append(expectedJSON, string(raw))
			}
			sort.Strings(actualJSON)
			sort.Strings(expectedJSON)
			if !reflect.DeepEqual(actualJSON, expectedJSON) {
				t.Fatalf("事例%dの%sの分類・担当・行動が不一致: API=%v 画面=%v", i, name, actual, expectedRows)
			}
		}

		got := countSnapshotGroups(listWorkGroups(s), s)
		for _, key := range []string{"attention", "running", "waiting", "stopped", "inProgress", "followups"} {
			if fleet.Number(got[key]) != fleet.Number(expected[i][key]) {
				fixture, _ := json.Marshal(s)
				t.Fatalf("事例%dの%sが不一致: API=%v 画面=%v 入力=%s", i, key, got, expected[i], fixture)
			}
		}
	}
}

func TestListCountsReadOnlyContractAndMissingInputs(t *testing.T) {
	f := newFixture(t)
	f.s.o.Lists["issue"] = func(context.Context, string) ([]Object, error) {
		return []Object{{"project": "owner/repo", "number": 1, "status": "running"}, {"project": "owner/repo", "number": 2, "status": "waiting"}, {"project": "owner/repo", "number": 3, "status": "queued", "followup": true}}, nil
	}
	f.s.o.Lists["draft"] = func(context.Context, string) ([]Object, error) {
		return []Object{{"id": fixtureID, "project": "owner/repo", "status": "submitted", "issues": []any{Object{"number": 1}}, "familyProgress": Object{"remaining": 1}}}, nil
	}
	f.s.o.Lists["todo"] = func(context.Context, string) ([]Object, error) {
		t.Fatal("件数取得でTODOを読んだ")
		return nil, nil
	}
	var first Object
	for _, actor := range []string{"screen", "token", "cli"} {
		var code int
		var value any
		if actor == "cli" {
			key := "counts-read-key"
			if err := f.db.AddCLIKey(context.Background(), auth.Hash(key), f.now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("GET", "http://127.0.0.1:8787/api/list?view=counts", nil)
			r.RemoteAddr = "127.0.0.1:1234"
			r.Header.Set("X-Desk-Key", key)
			w := httptest.NewRecorder()
			f.mux.ServeHTTP(w, r)
			code = w.Code
			if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
				t.Fatal(err)
			}
		} else {
			code, value = f.call("GET", "/api/list?view=counts", actor, nil)
		}
		body := fleet.ObjectOrEmpty(value)
		if code != 200 || !reflect.DeepEqual(body["counts"], Object{"attention": float64(0), "running": float64(0), "waiting": float64(0), "stopped": float64(2), "inProgress": float64(0), "followups": float64(1)}) || body["partial"] != false || body["scope"] != "active" || fleet.Text(body["checkedAt"]) == "" {
			t.Fatal(code, value)
		}
		if _, exists := body["rows"]; exists {
			t.Fatal("件数取得で一覧全体を返した", body)
		}
		if first != nil && !reflect.DeepEqual(first, body) {
			t.Fatal("画面と運用代行で集計が異なる", first, body)
		}
		first = body
	}
	code, meta := f.call("GET", "/api/meta", "screen", nil)
	measured := fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(meta)["listMeasurements"])
	if code != 200 || len(fleet.Items(measured["delta"])) != 0 || len(fleet.Items(measured["full"])) != 0 {
		t.Fatal("件数取得を一覧の全体・差分の計測に混ぜた", code, meta)
	}
	if _, err := f.db.SetDelegated(context.Background(), fixtureID, false, f.now); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.call("GET", "/api/list?view=counts", "token", nil); code != 403 {
		t.Fatal("運用代行なしのトークンを許可した", code)
	}
	for _, query := range []string{"view=counts&since=anything", "view=counts&view=counts", "view=other", "view=counts&project=owner/repo"} {
		if code, _ := f.call("GET", "/api/list?"+query, "screen", nil); code != 400 {
			t.Fatal("不正なクエリを許可した", query, code)
		}
	}
	f.s.o.Aux["attentionAlerts"] = func(context.Context) (any, error) { return nil, errors.New("unavailable attention") }
	code, value := f.call("GET", "/api/list?view=counts", "screen", nil)
	body := fleet.ObjectOrEmpty(value)
	if code != 200 || body["partial"] != true || !reflect.DeepEqual(body["unavailable"], []any{"attentionAlerts"}) {
		t.Fatal("欠測を完全な値にした", code, body)
	}
	f.s.o.Lists["draft"] = func(context.Context, string) ([]Object, error) { return nil, errors.New("unavailable drafts") }
	if code, value := f.call("GET", "/api/list?view=counts", "screen", nil); code != 503 {
		t.Fatal("基礎一覧の失敗を0件にした", code, value)
	}
}

func TestWorkViewReturnsEffectiveTasksAndParentReferences(t *testing.T) {
	f := newFixture(t)
	f.s.o.Lists["issue"] = func(context.Context, string) ([]Object, error) {
		return []Object{
			{"project": "owner/repo", "number": 1, "status": "running", "executionActive": true},
			{"project": "owner/repo", "number": 2, "status": "waiting", "requiresUser": true},
		}, nil
	}
	f.s.o.Lists["draft"] = func(context.Context, string) ([]Object, error) {
		return []Object{{"id": fixtureID, "project": "owner/repo", "status": "submitted", "familyProgress": Object{"remaining": 2}, "issues": []any{Object{"number": 1}, Object{"number": 2}}}}, nil
	}
	f.s.o.Aux["attentionAlerts"] = func(context.Context) (any, error) {
		return []any{Object{"key": "handoff", "kind": "job_recovery", "project": "owner/repo", "number": 1, "handoff": true, "reason": "復旧できません"}}, nil
	}
	code, value := f.call("GET", "/api/list?view=work", "screen", nil)
	body := fleet.ObjectOrEmpty(value)
	groups := fleet.ObjectOrEmpty(body["groups"])
	if code != 200 || len(fleet.Items(groups["attention"])) != 1 || len(fleet.Items(groups["stopped"])) != 1 || len(fleet.Items(groups["running"])) != 0 || len(fleet.Items(body["families"])) != 1 {
		t.Fatal(code, body)
	}
	task := fleet.ObjectOrEmpty(fleet.Items(groups["stopped"])[0])
	if task["number"] != float64(1) || len(fleet.Items(task["relatedAlerts"])) != 1 || fleet.ObjectOrEmpty(task["work"])["state"] != "stopped" || task["countWork"] != nil {
		t.Fatal("通知集約の結果と根拠を返す", task)
	}
	code, counts := f.call("GET", "/api/list?view=counts", "screen", nil)
	if code != 200 || !reflect.DeepEqual(body["counts"], fleet.ObjectOrEmpty(counts)["counts"]) {
		t.Fatal(code, body, counts)
	}
	if code, _ := f.call("GET", "/api/list?view=work", "token", nil); code != 403 {
		t.Fatal("分類済み一覧のPC認証を広げた", code)
	}
	if code, _ := f.call("GET", "/api/list?view=work&since=cursor", "screen", nil); code != 400 {
		t.Fatal(code)
	}
}

func TestConsultationStopRelatedDraft(t *testing.T) {
	notice := Object{"key": "consultation-stop:stopped-draft", "kind": "consultation_stop", "project": "owner/repo", "draftId": "stopped-draft", "state": "stopped", "handoff": true, "requiresUser": false, "reason": "停止理由・操作者・停止日時・経過時間"}
	snapshot := Snapshot{Rows: []Row{{Key: "draft:stopped-draft", Type: "draft", Item: Object{"id": "stopped-draft", "project": "owner/repo", "status": "stalled"}}}, Aux: map[string]any{"attentionAlerts": []any{notice}}}
	check := func(want int) []any {
		t.Helper()
		groups := listWorkGroups(snapshot)
		rows := fleet.Items(groups["stopped"])
		if len(rows) != want || len(fleet.Items(groups["attention"])) != 0 || countWorkGroups(groups)["stopped"] != want {
			t.Fatalf("groups=%v", groups)
		}
		return rows
	}
	rows := check(1)
	draft := fleet.ObjectOrEmpty(rows[0])
	related := fleet.Items(draft["relatedAlerts"])
	if draft["kind"] != "draft" || draft["id"] != "stopped-draft" || len(related) != 1 || fleet.ObjectOrEmpty(related[0])["key"] != notice["key"] || fleet.ObjectOrEmpty(related[0])["reason"] != notice["reason"] {
		t.Fatalf("draft=%v", draft)
	}
	snapshot.Rows[0].Item["project"] = "other/repo"
	check(2)
	snapshot.Rows[0].Item["project"] = nil
	snapshot.Rows[0].Item["unassigned"] = true
	notice["project"] = nil
	rows = check(1)
	if len(fleet.Items(fleet.ObjectOrEmpty(rows[0])["relatedAlerts"])) != 1 {
		t.Fatalf("unassigned=%v", rows)
	}
	snapshot.Rows = nil
	rows = check(1)
	if fleet.ObjectOrEmpty(rows[0])["key"] != notice["key"] {
		t.Fatalf("orphan=%v", rows)
	}
	notice["project"] = "owner/repo"
	snapshot.Rows = []Row{{Key: "draft:stopped-draft", Type: "draft", Item: Object{"id": "stopped-draft", "project": "owner/repo", "status": "stalled"}}}
	snapshot.Aux["attentionAlerts"] = []any{}
	rows = check(1)
	if len(fleet.Items(fleet.ObjectOrEmpty(rows[0])["relatedAlerts"])) != 0 {
		t.Fatalf("cleared=%v", rows)
	}
}
