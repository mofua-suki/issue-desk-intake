package consultationai

import (
	"context"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/usage"
)

const unmeasuredUsageKind = "usage_unmeasured"

const measurementMessage = "前のAI呼び出しの使用量またはその保存を確定できないため、自動の呼び出しを停止しました。未確定の記録を保持しています。時間切れ・中断・AIの異常終了が原因なら、相談へ返信すると再開します（1相談3回まで）。再試行だけでは再開しません。"

func measurementFailure() error {
	return &ai.Failure{Kind: unmeasuredUsageKind, Message: measurementMessage}
}

// measurementFailureWith は停止の理由（元の呼び出しの失敗や、中断の疑い）を併記する。停止そのものは変えない。
func measurementFailureWith(detail string) error {
	if detail == "" {
		return measurementFailure()
	}
	return &ai.Failure{Kind: unmeasuredUsageKind, Message: measurementMessage + detail}
}

// diagnosticLabels は診断の Kind の表示名。値は ai.Diagnostic.Kind の固定の識別子だけ。
var diagnosticLabels = map[string]string{
	"completed":          "正常終了（使用量だけ確定できなかった）",
	"timeout_idle":       "無出力が続いて時間切れ",
	"timeout_total":      "全体の制限時間切れ",
	"canceled_by_server": "サーバーの停止・再起動などで中断",
	"spawn_failed":       "AIの実行ファイルを起動できなかった",
	"output_overflow":    "出力が上限を超えた",
	"exit_nonzero":       "AIが異常終了した",
	"authentication":     "認証の失敗",
	"rate_limit":         "通信の一時制限",
	"usage_limit":        "利用上限",
	"format":             "回答の形式の不一致",
	"session":            "会話の復旧情報を保存できなかった",
	"input_invalid":      "送る内容が不正",
	"execution":          "実行の失敗",
}

// holdReasons は保留の理由（reason）の表示名。
var holdReasons = map[string]string{
	"record_failed": "使用量の記録を保存できなかった",
	"missing":       "使用量が返らなかった",
	"partial":       "使用量の一部が欠けていた",
}

// holdDetail は保留に保存した診断と原因から、元の失敗を読める短い文にする。どちらも無い古い保留は空にする。
func holdDetail(hold Record) string {
	diagnostic := fleet.Object(hold["diagnostic"])
	label, known := diagnosticLabels[fleet.Text(diagnostic["kind"])]
	recordCause, originalCause := fleet.Text(hold["recordCause"]), fleet.Text(hold["originalCause"])
	if !known && recordCause == "" && originalCause == "" {
		return ""
	}
	detail := ""
	if known {
		detail = fmt.Sprintf("元の呼び出し: %s（段階 %s", label, fleet.Text(diagnostic["stage"]))
		if ms := fleet.Number(diagnostic["elapsedMs"]); ms > 0 {
			detail += fmt.Sprintf("・%d秒", int(ms/1000))
		}
		if id := fleet.Text(diagnostic["id"]); id != "" {
			detail += "・診断ID " + id
		}
		detail += "）。"
	}
	if reason := holdReasons[fleet.Text(hold["reason"])]; reason != "" {
		detail += "確定できない理由: " + reason + "。"
	}
	if recordCause != "" {
		detail += "保存の失敗: " + recordCause + "。"
	}
	if originalCause != "" {
		detail += "呼び出しの失敗: " + originalCause + "。"
	}
	return detail
}

// pendingDetail は、完了の記録が残っていない呼び出しがあるときの理由。
func pendingDetail(value Record) string {
	if pending := fleet.Object(value["pendingCall"]); pending != nil {
		return fmt.Sprintf("完了の記録が無い呼び出しがあります（開始 %s）。サーバーの再起動などで途中で中断した可能性があります。", fleet.Text(pending["at"]))
	}
	if n := fleet.Number(value["unknownCalls"]); n > 0 {
		return fmt.Sprintf("使用量を確定できていない呼び出しが%d件あります。", int(n))
	}
	return ""
}
func isMeasurementFailure(err error) bool {
	var failure *ai.Failure
	return errors.As(err, &failure) && failure.Kind == unmeasuredUsageKind
}
func usageMeasurement(request ai.Request, u ai.Usage, config map[string]any) (any, bool) {
	row := map[string]any{"provider": request.Provider, "model": request.Model, "inputTokens": tokenValue(u.InputTokens), "outputTokens": tokenValue(u.OutputTokens), "cacheReadTokens": tokenValue(u.CacheReadTokens), "cacheWriteTokens": tokenValue(u.CacheWriteTokens)}
	amount := usage.Weighted(row, fleet.Object(config["usageWeights"]))["weighted"]
	return amount, amount != nil && (u.Partial == nil || !*u.Partial)
}
func ensureMeasurement(d Record, b *callBudget) error {
	if d["usageMeasurementHold"] != nil {
		hold := fleet.Object(d["usageMeasurementHold"])
		return measurementHoldFailure(hold, fleet.Object(d["consultationBudget"]))
	}
	if b != nil && (fleet.Number(b.value["unknownCalls"]) > float64(b.allowedUnknown) || b.value["pendingCall"] != nil) {
		if !consultations.CanReleaseMeasurementHold(Record{"consultationBudget": b.value, "aiDiagnostics": d["aiDiagnostics"]}) {
			return unreleasableMeasurementFailure(pendingDetail(b.value))
		}
		return measurementFailureWith(pendingDetail(b.value))
	}
	return nil
}

// holdMeasurement は使用量を確定できない呼び出しの後に、自動の呼び出しを止める印を保存する。
// 元の失敗（診断）を併記して保存する。使用量を0などで補って止めない扱いにはしない。
func (s *Service) holdMeasurement(ctx context.Context, d Record, request ai.Request, reason string, answer ai.Answer, callErr, recordErr error) error {
	hold := Record{"at": s.options.Now().UTC().Format(time.RFC3339Nano), "provider": request.Provider, "model": request.Model, "step": request.Step, "reason": reason}
	if answer.Diagnostic.ID != "" {
		hold["diagnostic"] = answer.Diagnostic.Map()
	}
	var failure *ai.Failure
	if errors.As(callErr, &failure) {
		hold["originalFailure"] = failure.Kind
	} else if callErr != nil {
		hold["originalFailure"] = "error"
	}
	if callErr != nil {
		hold["originalCause"] = consultations.FailureCause(d, callErr)
	}
	if recordErr != nil {
		hold["recordCause"] = consultations.FailureCause(d, recordErr)
	}
	if err := s.save(ctx, d, Record{"usageMeasurementHold": hold}); err != nil {
		return err
	}
	return measurementHoldFailure(hold, fleet.Object(d["consultationBudget"]))
}

// unreleasableMeasurementFailure は返信で解除できない停止。再開を約束する案内を出さない。
func unreleasableMeasurementFailure(detail string) error {
	return &ai.Failure{Kind: unmeasuredUsageKind, Message: "前のAI呼び出しの使用量またはその保存を確定できないため、自動の呼び出しを停止しました。未確定の記録を保持しています。この保留は返信では解除できません。元の失敗原因の修正が必要です。" + detail}
}

func measurementHoldFailure(hold Record, budget Record) error {
	var err error
	if fleet.Text(fleet.Object(hold["diagnostic"])["kind"]) != "" && !consultations.CanReleaseMeasurementHold(Record{"usageMeasurementHold": hold, "consultationBudget": budget}) {
		err = unreleasableMeasurementFailure(holdDetail(hold))
	} else {
		err = measurementFailureWith(holdDetail(hold))
	}
	if fleet.Text(fleet.Object(hold["diagnostic"])["kind"]) == "timeout_idle" {
		return &ai.IdleTimeoutError{Err: err}
	}
	return err
}

// diagnosticHistoryLimit は下書きの記録に残す診断の件数の上限。古いものから捨てる。
const diagnosticHistoryLimit = 20

// rememberDiagnostic は、成功しなかった呼び出しの診断を、相関情報（実行先・モデル・effort・工程・試行・ターン）と
// 時刻を添えて下書きの記録に残す。再起動のあとも読める。診断の保存の失敗は呼び出しの流れを変えない。
func (s *Service) rememberDiagnostic(ctx context.Context, d Record, request ai.Request, answer ai.Answer) {
	if answer.Diagnostic.ID == "" || answer.Diagnostic.Kind == "completed" {
		return
	}
	entry := Record(answer.Diagnostic.Map())
	entry["calls"] = fleet.Number(fleet.Object(d["consultationBudget"])["calls"])
	entry["at"] = s.options.Now().UTC().Format(time.RFC3339Nano)
	entry["provider"], entry["model"], entry["effort"] = request.Provider, request.Model, request.Effort
	entry["step"], entry["attempt"], entry["turn"] = request.Step, float64(request.Attempt), float64(request.Turn)
	history := append([]any{}, fleet.Elements(d["aiDiagnostics"])...)
	history = append(history, map[string]any(entry))
	if len(history) > diagnosticHistoryLimit {
		history = history[len(history)-diagnosticHistoryLimit:]
	}
	contract.LogFailure("診断の保存 "+fleet.Text(d["id"]), s.save(ctx, d, Record{"aiDiagnostics": history}))
}
