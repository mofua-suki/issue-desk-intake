package consultationai

import (
	"context"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
	"strings"
	"time"
)

var overlapCursor = regexp.MustCompile(`続き: ([a-f0-9]{64}:[0-9]+:[0-9]+)`)

const (
	maxFinalContinuations = 40
	overlapUnreadQuestion = "既存の依頼との重複を一部照合できませんでした（重なり候補の続きを取得できません）。照合できなかった候補を未確認として本文に残したまま起票しますか。"
)

type readBudget struct {
	budget *callBudget
	value  Record
}

func (b *callBudget) reads() (*readBudget, error) {
	v := Record{}
	if saved := fleet.Object(b.value["contextRead"]); saved != nil {
		for k, x := range saved {
			v[k] = x
		}
	}
	if v["rounds"] == nil {
		minimum := fleet.Number(fleet.Object(b.draft["progress"])["rounds"])
		if carried := fleet.Number(b.draft["carriedRounds"]); carried > minimum {
			minimum = carried
		}
		v["rounds"] = minimum
		v["measuredFrom"] = Record{"at": b.service.options.Now().UTC().Format(time.RFC3339Nano), "calls": b.value["calls"], "minimum": minimum, "historicalUnknown": minimum > 0 || fleet.Number(b.value["calls"]) > 0}
	}
	rounds := fleet.Number(v["rounds"])
	if rounds < 0 || rounds != float64(int(rounds)) {
		return nil, errors.New("保存済みの読み足し枠が不正です")
	}
	if a := fleet.Object(v["active"]); a != nil {
		remaining := fleet.Number(a["remaining"])
		if remaining < 0 || remaining > 12 || remaining > float64(len(fleet.Elements(a["pending"]))) || remaining != float64(int(remaining)) {
			return nil, errors.New("保存済みの読み足し残件が不正です")
		}
	}
	return &readBudget{b, v}, nil
}
func (r *readBudget) save(ctx context.Context) error {
	r.budget.value["contextRead"] = r.value
	return r.budget.persist(ctx)
}

// saveContinuation saves the checkpoint of a reply that is replaced by a read
// continuation. An explicit cancellation of the split issues in that reply is
// preserved by saving it together with the checkpoint.
func (r *readBudget) saveContinuation(ctx context.Context, answer Record) error {
	if answer["_cancelSplitIssues"] == true && fleet.Elements(answer["splitIssues"]) != nil && len(fleet.Elements(answer["splitIssues"])) == 0 {
		r.budget.value["contextRead"] = r.value
		return r.budget.persistWith(ctx, Record{"splitIssues": []any{}})
	}
	return r.save(ctx)
}
func (r *readBudget) remaining() int {
	return r.budget.calls - int(fleet.Number(r.budget.value["calls"]))
}
func (r *readBudget) ending() bool {
	return r.value["ended"] == true || fleet.Number(r.value["rounds"]) >= 12 || r.remaining() <= 3
}
func (r *readBudget) beforeAnswer() bool {
	return r.value["ended"] == true || fleet.Number(r.value["rounds"]) >= 12 || fleet.Object(r.value["final"])["draft"] == true || fleet.Number(r.value["rounds"]) > 0 && r.remaining() <= 1
}
func (r *readBudget) notice() string {
	if fleet.Number(r.value["rounds"]) >= 10 {
		return "相談全体の追加資料確認は残り少数です。確認済みの根拠で下書きを作り、残る未確認事項を明記してください。"
	}
	return ""
}
func (r *readBudget) inputs() []any {
	seen := map[string]bool{}
	out := []any{}
	for _, item := range fleet.Elements(r.budget.draft["messages"]) {
		m := fleet.Object(item)
		if m["role"] == "user" {
			body := strings.TrimSpace(fleet.Text(m["text"]))
			key := digest(body)
			if body != "" && !seen[key] {
				seen[key] = true
				out = append(out, key)
			}
		}
	}
	return out
}
func (r *readBudget) newInput() bool {
	if _, ok := r.value["inputs"]; !ok {
		return false
	}
	seen := map[string]bool{}
	for _, v := range fleet.Elements(r.value["inputs"]) {
		seen[fleet.Text(v)] = true
	}
	for _, v := range r.inputs() {
		if !seen[fleet.Text(v)] {
			return true
		}
	}
	return false
}
func (r *readBudget) seenInputs() []any {
	out := []any{}
	seen := map[string]bool{}
	for _, v := range append(append([]any{}, fleet.Elements(r.value["inputs"])...), r.inputs()...) {
		if !seen[fleet.Text(v)] {
			seen[fleet.Text(v)] = true
			out = append(out, v)
		}
	}
	return out
}
func (r *readBudget) remember(ctx context.Context, answer Record) error {
	r.value["lastReply"] = answer
	r.value["answerPending"] = true
	r.value["generation"] = fleet.Number(r.value["generation"]) + 1
	r.value["inputs"] = r.seenInputs()
	return r.save(ctx)
}
func (r *readBudget) begin(ctx context.Context, requests []any) error {
	if len(requests) == 0 {
		return nil
	}
	if r.ending() {
		return errors.New("追加資料確認の枠を使い切りました")
	}
	if len(requests) > 12 {
		return errors.New("一度の資料確認は12要求以内です")
	}
	r.value["rounds"] = fleet.Number(r.value["rounds"]) + 1
	r.value["answerPending"] = false
	r.value["active"] = Record{"pending": requests, "remaining": len(requests), "generation": r.value["generation"], "batch": requests}
	return r.save(ctx)
}
func (r *readBudget) checkpoint(ctx context.Context, pending []any) error {
	prior := fleet.Object(r.value["active"])
	if prior == nil {
		prior = Record{}
	}
	prior["pending"], prior["remaining"] = pending, len(pending)
	r.value["active"] = prior
	return r.save(ctx)
}
func (r *readBudget) end(ctx context.Context) error {
	r.value["ended"] = true
	if r.value["inputs"] == nil {
		r.value["inputs"] = r.seenInputs()
	}
	return r.save(ctx)
}
func (r *readBudget) take(ctx context.Context, kind string) (bool, error) {
	final := fleet.Object(r.value["final"])
	if final == nil {
		final = Record{}
	}
	if final[kind] == true || r.remaining() <= 1 {
		return false, nil
	}
	final[kind] = true
	r.value["final"] = final
	r.value["inputs"] = r.seenInputs()
	return true, r.save(ctx)
}

// supplement は、読み足しの枠を終えた後に届いた利用者の新しい発言へ、追加読取なしの回答を1回だけ割り当てる。
// 枠の終わり方（ended・回数・最終回の使用済み）は問わない。同じ発言では2回目を割り当てない（入力digestで判定）。
func (r *readBudget) supplement(ctx context.Context) (bool, error) {
	if !r.newInput() || r.remaining() <= 1 {
		return false, nil
	}
	r.value["inputs"] = r.seenInputs()
	final := fleet.Object(r.value["final"])
	if final == nil {
		final = Record{}
	}
	final["draft"] = true
	r.value["final"] = final
	return true, r.save(ctx)
}
func (r *readBudget) invalidNotice(notice string) {
	if previous := fleet.Text(r.value["invalidReadNotice"]); previous != "" {
		notice = previous + "\n" + notice
	}
	r.value["invalidReadNotice"] = notice
}
func (r *readBudget) drain(ctx context.Context, m *materials) error {
	pending := append([]any{}, fleet.Elements(fleet.Object(r.value["active"])["pending"])...)
	for len(pending) > 0 {
		item, err := request(pending[0])
		if err != nil {
			pending = pending[1:]
			r.invalidNotice("保存された資料要求を取り除きました: " + err.Error() + "。正しい形式で再要求してください。")
			r.value["answerPending"] = false
			if err := r.checkpoint(ctx, pending); err != nil {
				return err
			}
			continue
		}
		if _, _, err := m.read(ctx, item); err != nil {
			if !errors.Is(err, errProjectNotAllowed) {
				return err
			}
			pending = pending[1:]
			r.invalidNotice("保存された資料要求を取り除きました: " + err.Error() + "。同じプロジェクトの相対パスをfileに指定し、行範囲はmode、start、endで指定して再要求してください。")
			r.value["answerPending"] = false
			if err := r.checkpoint(ctx, pending); err != nil {
				return err
			}
			continue
		}
		pending = pending[1:]
		if err := r.checkpoint(ctx, pending); err != nil {
			return err
		}
	}
	m.state.Pending = nil
	m.state.Remaining = 0
	return m.persist(ctx)
}

// finishOverlapList follows every cursor of the overlap list already read.
// The list is the controller's filing prerequisite, so the AI's read allowance
// does not apply; losing it would leave ready=false with no way forward.
// It reports whether any candidate stayed unread.
func (m *materials) finishOverlapList(ctx context.Context) bool {
	project := fleet.Text(m.draft["project"])
	if project == "" || m.service.options.Reference == nil {
		return false
	}
	p := m.state.Projects[project]
	if p == nil {
		return false
	}
	unread := false
	attempted := map[string]bool{}
	for count := 0; ; count++ {
		next := ""
		known := map[string]bool{}
		for _, doc := range p.Reads {
			if isContinuation(doc.Path, doc.Request) {
				known[doc.Request.Keyword] = true
				unread = unread || doc.Reason != ""
			}
		}
		for _, doc := range p.Reads {
			if doc.Path != "open-requests" || doc.Reason != "" {
				continue
			}
			for _, match := range overlapCursor.FindAllStringSubmatch(doc.Text, -1) {
				if !known[match[1]] && !attempted[match[1]] && next == "" {
					next = match[1]
				}
			}
		}
		if next == "" {
			return unread
		}
		if count >= maxFinalContinuations {
			return true
		}
		attempted[next] = true
		item, err := request(map[string]any{"file": "open-requests", "mode": "keyword", "keyword": next})
		if err == nil {
			_, _, err = m.read(ctx, item)
		}
		if err != nil {
			unread = true
		}
	}
}
func (m *materials) fresh(reads []any) ([]any, error) {
	out := []any{}
	seen := map[string]bool{}
	for _, value := range reads {
		r, err := request(value)
		if err != nil {
			return nil, err
		}
		project, file := fleet.Text(m.draft["project"]), r.File
		if before, after, ok := strings.Cut(file, ":"); ok && projectName.MatchString(before) {
			project, file = before, after
		}
		key := requestKey(r)
		if !referenceNames[file] && file != "repo-index" && file != "repo-search" {
			rKey := r
			rKey.File = project + ":" + file
			key = pinnedRequestKey(rKey)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		found := false
		if p := m.state.Projects[project]; p != nil {
			_, found = m.readBefore(p, project, file, r)
		}
		if !found {
			out = append(out, value)
		}
	}
	return out, nil
}
func (s *Service) stopBudget(ctx context.Context, d Record, b *callBudget) (Record, error) {
	stop := Record{"at": s.options.Now().UTC().Format(time.RFC3339Nano), "calls": b.value["calls"], "weighted": b.value["weighted"], "limits": Record{"calls": b.calls, "weighted": b.weighted}, "questions": d["questions"], "reply": d["reply"]}
	if existing := fleet.Object(b.value["stopped"]); existing != nil {
		stop = existing
	} else {
		b.value["stopped"] = stop
		b.value["stops"] = append(fleet.Elements(b.value["stops"]), stop)
		if err := b.persist(ctx); err != nil {
			return nil, err
		}
	}
	return Record{"ready": false, "questions": d["questions"], "consultationBudget": b.value, "reply": "相談全体の呼び出し上限に達したため、保存済みの下書きと未確認事項を残して停止しました。続行の操作で追加枠を予約できます。"}, nil
}
