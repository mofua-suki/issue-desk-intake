package consultationai

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"math"
	"os"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/contract"
)

type callBudget struct {
	service   *Service
	draft     Record
	config    map[string]any
	value     Record
	calls     int
	weighted  float64
	execution *readExecution
	// allowedUnknown is local to this execution; it is never persisted.
	allowedUnknown       int
	timeoutExceptionUsed bool
}

type callBudgetKey struct{}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&15 | 64
	b[8] = b[8]&63 | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
func (s *Service) budget(d Record, c map[string]any) (*callBudget, error) {
	v := Record{}
	for key, value := range fleet.Object(d["consultationBudget"]) {
		v[key] = value
	}
	for _, key := range []string{"calls", "weighted", "unknownCalls"} {
		n := fleet.Number(v[key])
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
			return nil, errors.New("相談全体の使用量が不正です")
		}
		v[key] = n
	}
	setting := fleet.Object(fleet.Object(c["intake"])["consultationBudget"])
	calls, weighted := 40, 6000000.0
	if value, ok := setting["calls"]; ok {
		n := fleet.Number(value)
		if n < 1 || n != float64(int(n)) {
			return nil, errors.New("相談の呼出上限が不正です")
		}
		calls = int(n)
	}
	if value, ok := setting["weighted"]; ok {
		weighted = fleet.Number(value)
		if weighted <= 0 || math.IsNaN(weighted) || math.IsInf(weighted, 0) {
			return nil, errors.New("相談の使用量上限が不正です")
		}
	}
	calls += int(fleet.Number(fleet.Object(v["added"])["calls"]))
	weighted += fleet.Number(fleet.Object(v["added"])["weighted"])
	return &callBudget{service: s, draft: d, config: c, value: v, calls: calls, weighted: weighted}, nil
}
func (b *callBudget) persist(ctx context.Context) error {
	return b.persistWith(ctx, Record{})
}

// persistWith commits related draft changes under the same run ownership check.
func (b *callBudget) persistWith(ctx context.Context, fields Record) error {
	b.value["limits"] = Record{"calls": b.calls, "weighted": b.weighted}
	b.value["updatedAt"] = b.service.options.Now().UTC().Format(time.RFC3339Nano)
	fields["consultationBudget"] = b.value
	return b.service.save(ctx, b.draft, fields)
}
func (b *callBudget) check() error {
	if b.value["stopped"] != nil && b.value["stopped"] != false || fleet.Number(b.value["calls"]) >= float64(b.calls) || fleet.Number(b.value["weighted"]) >= b.weighted {
		return &ai.Failure{Kind: "budget", Message: "相談全体の上限に達しました。保存済みの相談から続行を選んでください。", DeskCode: "CONSULTATION_BUDGET_REACHED"}
	}
	return nil
}

type readExecution struct {
	Final, Reading bool
	Attempts       int
}

var errReadEnd = errors.New("保存済みの情報で追加確認を終了します")

func (b *callBudget) reserveCall(ctx context.Context, id string) error {
	if err := ensureMeasurement(b.draft, b); err != nil {
		return err
	}
	if e := b.execution; e != nil {
		remaining := b.calls - int(fleet.Number(b.value["calls"]))
		// 最終回の呼び出しも、時間切れなどの一時的な失敗は枠が残る限り再試行する。断ると読取だけの
		// 仮応答で読取の枠を閉じ、以後の発言がすべて最終回になって相談が止まる。
		if e.Reading && remaining <= 1 || e.Attempts > 0 && remaining <= 3 {
			return errReadEnd
		}
		e.Attempts++
	}
	if err := b.check(); err != nil {
		return err
	}
	var err error
	before := map[string]float64{}
	for _, key := range []string{"calls", "weighted", "unknownCalls"} {
		before[key] = fleet.Number(b.value[key])
	}
	b.value["calls"] = fleet.Number(b.value["calls"]) + 1
	b.value["unknownCalls"] = fleet.Number(b.value["unknownCalls"]) + 1
	b.value["pendingCall"] = Record{"id": id, "at": b.service.options.Now().UTC().Format(time.RFC3339Nano)}
	if b.draft["_ephemeral"] == true {
		return b.persist(ctx)
	}
	b.value["limits"] = Record{"calls": b.calls, "weighted": b.weighted}
	b.value["updatedAt"] = b.service.options.Now().UTC().Format(time.RFC3339Nano)
	_, err = b.service.options.Records.Change(ctx, "drafts", fleet.Text(b.draft["id"]), func(_ *sql.Tx, current Record) (Record, error) {
		if current == nil || current["status"] != "thinking" || current["run"] != b.draft["run"] {
			return nil, errors.New("相談の実行権を失いました")
		}
		stored := fleet.Object(current["consultationBudget"])
		if err := ensureMeasurement(current, &callBudget{value: stored, allowedUnknown: b.allowedUnknown}); err != nil {
			return nil, err
		}
		if err := (&callBudget{value: stored, calls: b.calls, weighted: b.weighted}).check(); err != nil {
			return nil, err
		}
		for key, value := range before {
			if fleet.Number(stored[key]) != value {
				return nil, measurementFailureWith("保存された使用量が変更されました。")
			}
		}
		current["consultationBudget"] = b.value
		return current, nil
	})
	if err == nil {
		b.draft["consultationBudget"] = b.value
	}
	return err
}
func tokenValue(v *int64) any {
	if v == nil {
		return nil
	}
	return float64(*v)
}
func (b *callBudget) measured(ctx context.Context, request ai.Request, u ai.Usage) error {
	before := map[string]float64{}
	for _, key := range []string{"calls", "weighted", "unknownCalls"} {
		before[key] = fleet.Number(b.value[key])
	}
	pendingID := fleet.Text(fleet.Object(b.value["pendingCall"])["id"])
	amount, complete := usageMeasurement(request, u, b.config)
	if amount != nil {
		b.value["weighted"] = fleet.Number(b.value["weighted"]) + fleet.Number(amount)
	}
	if complete {
		b.value["unknownCalls"] = math.Max(0, fleet.Number(b.value["unknownCalls"])-1)
	}
	delete(b.value, "pendingCall")
	if b.draft["_ephemeral"] == true {
		return b.persist(ctx)
	}
	b.value["limits"] = Record{"calls": b.calls, "weighted": b.weighted}
	b.value["updatedAt"] = b.service.options.Now().UTC().Format(time.RFC3339Nano)
	_, err := b.service.options.Records.Change(ctx, "drafts", fleet.Text(b.draft["id"]), func(_ *sql.Tx, current Record) (Record, error) {
		if current == nil || current["status"] != "thinking" || current["run"] != b.draft["run"] {
			return nil, errors.New("相談の実行権を失いました")
		}
		stored := fleet.Object(current["consultationBudget"])
		if pendingID == "" || fleet.Text(fleet.Object(stored["pendingCall"])["id"]) != pendingID {
			return nil, measurementFailureWith("呼び出しの予約が変更されました。")
		}
		for key, value := range before {
			if fleet.Number(stored[key]) != value {
				return nil, measurementFailureWith("保存された使用量が変更されました。")
			}
		}
		current["consultationBudget"] = b.value
		return current, nil
	})
	if err == nil {
		b.draft["consultationBudget"] = b.value
	}
	return err
}
func (b *callBudget) prompt() any {
	if fleet.Number(b.value["calls"]) < float64(b.calls)*.8 && fleet.Number(b.value["weighted"]) < b.weighted*.8 {
		return nil
	}
	return Record{"used": Record{"calls": b.value["calls"], "weighted": b.value["weighted"]}, "limits": Record{"calls": b.calls, "weighted": b.weighted}, "remaining": Record{"calls": math.Max(0, float64(b.calls)-fleet.Number(b.value["calls"])), "weighted": math.Max(0, b.weighted-fleet.Number(b.value["weighted"]))}, "notice": "相談全体の上限に近づいています。確定できる範囲と未確認事項を具体的に整理してください。"}
}
func (s *Service) inputCheck(ctx context.Context, d Record, c map[string]any, request ai.Request, full string) error {
	schema, err := os.ReadFile(request.SchemaFile)
	if err != nil {
		return err
	}
	settings := fleet.Object(c["intake"])
	chars, bytes := 524288, 1048576
	if value, exists := settings["maxInputChars"]; exists {
		chars = int(fleet.Number(value))
	}
	if value, exists := settings["maxInputBytes"]; exists {
		bytes = int(fleet.Number(value))
	}
	if chars < 1 || bytes < 1 {
		return errors.New("AI入力の上限が不正です")
	}
	size := func(input string) Record {
		combined := input + "\n" + string(schema)
		return Record{"chars": contract.UTF16Length(combined), "bytes": len(combined)}
	}
	actual, whole := size(request.Input), size(full)
	id, err := newID()
	if err != nil {
		return err
	}
	prior := fleet.Object(d["inputBudget"])
	sequence := int(fleet.Number(prior["total"])) + 1
	event := Record{"v": 1, "checkId": id, "sequence": sequence, "at": s.options.Now().UTC().Format(time.RFC3339Nano), "stage": "consultation", "draft": d["id"], "turn": d["run"], "phase": "pre_send", "transport": request.Provider, "mode": "full", "limits": Record{"chars": chars, "bytes": bytes}, "full": Record{"total": whole}, "sent": actual}
	if request.Resume {
		event["mode"] = "resumed"
		event["delta"] = Record{"total": actual}
	}
	records := append(append([]any{}, fleet.Elements(prior["records"])...), event)
	if len(records) > 64 {
		records = records[len(records)-64:]
	}
	observation := Record{"v": 1, "total": sequence, "dropped": sequence - len(records), "firstSequence": sequence - len(records) + 1, "lastSequence": sequence, "firstAt": fleet.Object(records[0])["at"], "lastAt": event["at"], "records": records}
	if err := s.save(ctx, d, Record{"inputBudget": observation}); err != nil {
		return err
	}
	for _, count := range []Record{actual, whole} {
		if int(fleet.Number(count["chars"])) > chars || int(fleet.Number(count["bytes"])) > bytes {
			return &ai.Failure{Kind: "input_size", DeskCode: "INPUT_TOO_LARGE", Message: "AIへ送る全体が入力上限を超えています。固定指示・資料・会話・スキーマを該当範囲へ絞ってください。"}
		}
	}
	return nil
}
