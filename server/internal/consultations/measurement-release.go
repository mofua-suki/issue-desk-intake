package consultations

import "issue-desk-intake/server/internal/fleet"

// maxMeasurementReleases は、1つの相談で使用量の保留を解除できる回数の上限。
// 解除は利用者の新しい返信だけで行うので、上限は繰り返しの時間切れで使用量が増え続けることを防ぐ。
const maxMeasurementReleases = 3

// timeoutDiagnosticKinds は、AIの出力が最後まで返らず使用量を取れなかった呼び出しの診断の種類。
// 認証・形式・使用量の保存の失敗などは含めない（それらは使用量の保留を従来どおり解除しない）。
var timeoutDiagnosticKinds = map[string]bool{"timeout_idle": true, "timeout_total": true, "canceled_by_server": true, "exit_nonzero": true}

// releaseUnmeasuredHold は、時間切れ・中断・異常終了で使用量を確定できなかった呼び出しの保留を、利用者の新しい返信で解除する。
// 保留が無く、完了の記録が無い呼び出し(pendingCall。受付の再起動などで途中で中断した呼び出し)だけが残っている場合も解除する。
// 使用量を0などで補わない。未確定の呼び出しの記録（measurementReleases）を残し、呼び出し回数の上限は数え続ける。
// 自動の再試行（retry）からは呼ばない。
func releaseUnmeasuredHold(next Record, messageID, at string) bool {
	hold := record(next["usageMeasurementHold"])
	diagnostic := record(hold["diagnostic"])
	kind := text(diagnostic["kind"])
	budget := clone(record(next["consultationBudget"]))
	if budget == nil {
		budget = Record{}
	}
	if !CanReleaseMeasurementHold(next) {
		return false
	}
	if hold == nil {
		kind = "interrupted"
		if record(budget["pendingCall"]) == nil {
			diagnostic = lastDiagnostic(next)
			kind = text(diagnostic["kind"])
		}
	}
	releases := fleet.Items(budget["measurementReleases"])
	budget["measurementReleases"] = append(append([]any{}, releases...), Record{
		"at": at, "messageId": messageID, "kind": kind, "callId": text(record(budget["pendingCall"])["id"]), "diagnosticId": text(diagnostic["id"]),
		"heldAt": text(hold["at"]), "step": text(hold["step"]), "pendingAt": text(record(budget["pendingCall"])["at"]),
	})
	budget["unknownCalls"] = float64(0)
	delete(budget, "pendingCall")
	next["consultationBudget"] = budget
	delete(next, "usageMeasurementHold")
	return true
}

// CanReleaseMeasurementHold is shared by the release operation and its user
// guidance so that non-releasable failures never promise a restart on reply.
func CanReleaseMeasurementHold(next Record) bool {
	hold := record(next["usageMeasurementHold"])
	diagnostic := record(hold["diagnostic"])
	kind := text(diagnostic["kind"])
	budget := record(next["consultationBudget"])
	if len(fleet.Items(budget["measurementReleases"])) >= maxMeasurementReleases {
		return false
	}
	if hold == nil {
		if record(budget["pendingCall"]) != nil {
			return true
		}
		// 時間切れの後の1回だけの再試行も時間切れで終わると、保留も途中の呼び出しも無く、
		// 未確定の件数だけが残る。最後の診断が時間切れならその呼び出しの欠測なので、返信で解除する。
		kind := text(lastDiagnostic(next)["kind"])
		return fleet.Number(budget["unknownCalls"]) > 0 && (kind == "timeout_idle" || kind == "timeout_total")
	}
	if !timeoutDiagnosticKinds[kind] {
		return false
	}
	if kind == "exit_nonzero" {
		reason := text(diagnostic["failureReason"])
		original := text(hold["originalFailure"])
		if (reason != "" && reason != "unknown") || text(hold["reason"]) == "record_failed" || (original != "" && original != "execution" && original != "error") {
			return false
		}
	}
	return true
}

// lastDiagnostic は下書きに残した呼び出しの診断（aiDiagnostics）の最後の1件。無ければ nil。
func lastDiagnostic(next Record) Record {
	history := fleet.Elements(next["aiDiagnostics"])
	if len(history) == 0 {
		return nil
	}
	return record(history[len(history)-1])
}
