package consultationai

import (
	"context"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/models"
	"issue-desk-intake/server/internal/usage"
	"path/filepath"
	"strings"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/consultations"
)

func (s *Service) Turn(ctx context.Context, original Record) (result Record, err error) {
	lock := s.lock(fleet.Text(original["id"]))
	lock.Lock()
	defer lock.Unlock()
	stage := "開始の確認"
	defer func() { err = consultations.AtStage(stage, err) }()
	d := copyRecord(original)
	if fleet.Text(d["id"]) == "" || fleet.Text(d["run"]) == "" || d["status"] != "thinking" {
		return nil, errors.New("相談の実行権がありません")
	}
	if len(fleet.Elements(d["consultationParts"])) > 0 || len(fleet.Elements(d["consultationChildren"])) > 0 {
		stage = "子相談の復旧"
		return s.resumeSplitChildren(ctx, d)
	}
	stage = "設定の読取"
	config, err := s.options.Config(ctx)
	if err != nil {
		return nil, err
	}
	stage = "使用量の確認"
	budget, err := s.budget(d, config)
	if err != nil {
		return nil, err
	}
	if err := s.reconcileMeasurement(ctx, d, budget); err != nil {
		return nil, err
	}
	if err := ensureMeasurement(d, budget); err != nil {
		return nil, err
	}
	ctx = context.WithValue(ctx, callBudgetKey{}, budget)
	if err := budget.check(); err != nil {
		return s.stopBudget(ctx, d, budget)
	}
	reads, err := budget.reads()
	if err != nil {
		return nil, err
	}
	stage = "共通番号の照合"
	if err := s.resolveReferences(ctx, d); err != nil {
		return nil, err
	}
	stage = "添付画像の準備"
	images, imageMeta, cleanup, err := s.imageInputs(ctx, d)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	stage = "実行先の決定"
	settings, err := s.resolve(ctx, config, map[string]any(d), "intake")
	if err != nil {
		return nil, err
	}
	stage = "会話の復旧情報の準備"
	session, err := s.turnSession(ctx, d, config, settings, len(images) > 0)
	if err != nil {
		return nil, err
	}
	// checked は検査に入った最後の回答。検査で断られて終わるターンでも、会話は保存済みの
	// 完結した返答で終わっているので残し、次の発言を差分だけで続ける。
	var checked Record
	defer func() {
		kept := result
		if kept == nil && replyCheckFailed(err) {
			kept = checked
		}
		if finishErr := s.finishSession(context.WithoutCancel(ctx), d, session, kept); err == nil && finishErr != nil {
			err = finishErr
		}
	}()
	stage = "資料の読取"
	m, err := s.prepareMaterials(ctx, d, config, session)
	if err != nil {
		return nil, err
	}
	trigger := initialCallTrigger(d, reads)
	repair := repairDetail{}
	if fleet.Object(reads.value["active"]) != nil && reads.value["answerPending"] == false && !reads.newInput() {
		trigger = "context_read"
	}
	if err := reads.drain(ctx, m); err != nil {
		return nil, err
	}
	stage = "回答スキーマの読取"
	schemaSource := filepath.Join(s.options.SchemaDir, "intake.schema.json")
	schema, schemaRaw, cleanupSchema, err := turnResponseSchema(s.options.RuntimeDir, schemaSource, false)
	if err != nil {
		return nil, err
	}
	defer cleanupSchema()
	var finalSchema string
	var finalSchemaRaw []byte
	extra := initialTurnNotice(reads)
	compact := false
	repairs := map[string]int{}
	previousExecutionProblem := ""
	final := false
	lenientFinal := false
	overlapUnread := false
	pending := Record(nil)
	if reads.value["answerPending"] == true && !reads.newInput() {
		pending = Record(fleet.Object(reads.value["lastReply"]))
	}
	if reads.beforeAnswer() && pending == nil {
		taken, e := reads.supplement(ctx)
		if e != nil {
			return nil, e
		}
		if !taken {
			taken, e = reads.take(ctx, "draft")
			if e != nil {
				return nil, e
			}
		}
		if !taken && reads.newInput() {
			// 新しい発言に答える呼び出しの枠が残っていない。前の返事を新しい回答として返さず、上限で止まったことを示す。
			return s.stopBudget(ctx, d, budget)
		}
		if !taken {
			return endAnswer(d, Record(fleet.Object(reads.value["lastReply"]))), nil
		}
		final = true
		overlapUnread = m.finishOverlapList(ctx)
	}
	for {
		stage = "入力の組み立て"
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := budget.check(); err != nil {
			return s.stopBudget(ctx, d, budget)
		}
		contextData, shown, bodies := m.data(compact)
		m.addDeliveredBodies(session, bodies)
		remaining := readRoundsLeft(m, final)
		turnExtra := extra
		callSchema, callSchemaRaw := schema, schemaRaw
		strictFinal := final && !lenientFinal
		if final {
			turnExtra += "\n追加読取の枠は終了しました。追加読取を求めず下書きと未確認事項を返してください。重なり候補の続きはコントローラーが読み足し済みです。読めた範囲で重複を判定してください。"
		}
		if strictFinal {
			if finalSchema == "" {
				stage = "最終回答スキーマの読取"
				var cleanupFinalSchema func()
				finalSchema, finalSchemaRaw, cleanupFinalSchema, err = turnResponseSchema(s.options.RuntimeDir, schemaSource, true)
				if err != nil {
					return nil, err
				}
				defer cleanupFinalSchema()
			}
			callSchema, callSchemaRaw = finalSchema, finalSchemaRaw
		}
		inputDraft, _, err := conversationInput(d)
		if err != nil {
			return nil, err
		}
		full := prompt(inputDraft, contextData, budget.prompt(), remaining, imageMeta, turnExtra)
		if !compact && nearInput(config, full, string(callSchemaRaw)) {
			compact = true
			extra += "\n" + compactNotice()
			if err := s.resetSession(ctx, d, session, "input_compacted"); err != nil {
				return nil, err
			}
			continue
		}
		var answer Record
		if pending != nil {
			answer = pending
			pending = nil
		} else {
			stage = "受付AIの呼び出し"
			budget.execution = &readExecution{Final: final, Reading: fleet.Number(reads.value["rounds"]) > 0 || final}
			input := resumedPrompt(full, inputDraft, m, session, shown, turnPromptState{Notes: turnExtra, RemainingReadRounds: remaining, Budget: budget.prompt()})
			response, callErr := s.call(context.WithValue(context.WithValue(ctx, repairDetailKey{}, repair), callTriggerKey{}, trigger), d, config, settings, usage.DraftStep, callSchema, input, full, session, budget, images, m.state.InitialDocuments)
			var normalized Record
			if callErr == nil {
				normalized, callErr = turnResponse(response.Value, strictFinal)
			}
			budget.execution = nil
			trigger = ""
			repair = repairDetail{}
			if callErr != nil {
				var failure *ai.Failure
				if errors.As(callErr, &failure) && failure.Kind == "format" {
					if ok, e := s.repair(ctx, d, config, repairs, "draft_format"); e != nil {
						return nil, e
					} else if ok {
						trigger = "format_repair"
						repair = repairDetail{kind: "draft_format", field: failure.Field}
						extra = "前の回答は指定形式に合いません。JSONスキーマだけを満たす回答へ修復してください。"
						continue
					}
				}
				if errors.Is(callErr, errReadEnd) {
					if err := reads.end(ctx); err != nil {
						return nil, err
					}
					return endAnswer(d, Record(fleet.Object(reads.value["lastReply"]))), nil
				}
				if errors.As(callErr, &failure) && failure.DeskCode == "CONSULTATION_BUDGET_REACHED" {
					return s.stopBudget(ctx, d, budget)
				}
				if errors.As(callErr, &failure) && failure.DeskCode == "INPUT_TOO_LARGE" {
					stage = "入力超過からの分割"
					return s.splitInput(ctx, d, config, settings, callErr)
				}
				return nil, callErr
			}
			stage = "回答の保存"
			answer = normalized
			logSplitDependencies(fleet.Text(d["id"]), answer)
			if err := s.rememberAnswer(ctx, d, m, session, reads, shown, bodies, answer); err != nil {
				return nil, err
			}
		}
		stage = "回答の検査"
		checked = answer
		consultations.MarkSplitCancellation(answer)
		if answer["escalate"] == true {
			raised, e := s.raise(ctx, d, config, m, fleet.Text(answer["escalateReason"]))
			if e != nil {
				return nil, e
			}
			if raised {
				if settings, err = s.switchRaisedTier(ctx, d, config, session); err != nil {
					return nil, err
				}
				trigger = "automatic_continuation"
				extra = "難易度を一段引き上げました。追加資料の確認を続けてください。"
				continue
			}
		}
		if requested := fleet.Elements(answer["readFiles"]); len(requested) > 0 {
			step, err := s.answerReadRequests(ctx, d, config, reads, m, repairs, answer, requested, final)
			if err != nil {
				return nil, err
			}
			if step.result != nil {
				return step.result, nil
			}
			trigger, repair, extra = step.trigger, step.repair, step.extra
			if step.final {
				final, overlapUnread, lenientFinal = true, step.overlapUnread, step.lenient
			}
			continue
		}
		if len(fleet.Elements(answer["splitIssues"])) > 25 {
			if ok, e := s.repair(ctx, d, config, repairs, "split_over_limit"); e != nil {
				return nil, e
			} else if ok {
				trigger = "format_repair"
				repair = repairDetail{kind: "split_over_limit"}
				extra = "分割案が25件を超えています。元の要件を落とさず25件以内へまとめ直してください。"
				continue
			}
			return nil, &ai.Failure{Kind: "proposal", DeskCode: "TOO_MANY_PROPOSALS", Message: "分割案を上限以内へまとめ直せませんでした。保存済みの相談で理由と元の要件を確認してください。"}
		}
		freshHandoff, handoffProblem := m.prepareHandoff(ctx, d, answer)
		if handoffProblem != "" {
			controllerQuestion(answer, "必要資料を取得できませんでした。\n"+handoffProblem, handoffProblem)
		} else if freshHandoff {
			trigger = "context_read"
			extra = "実装担当への引継ぎ資料を取得しました。本文と版を確認してください。"
			continue
		}
		executionProblem := s.executionPlanProblem(ctx, d, answer, m)
		completionProblem := completionPlanProblem(d, answer)
		if executionProblem != "" || completionProblem != "" {
			var corrections []string
			if executionProblem != "" {
				if executionProblem == previousExecutionProblem {
					return nil, &ai.Failure{Kind: "reply_check", Message: "実行計画を確定できません: " + executionProblem}
				}
				previousExecutionProblem = executionProblem
				ok, err := s.repair(ctx, d, config, repairs, "execution_plan")
				if err != nil {
					return nil, err
				}
				if !ok {
					return nil, &ai.Failure{Kind: "reply_check", Message: "実行計画を確定できません: " + executionProblem}
				}
				corrections = append(corrections, "全受入条件の実行分担を直してください。実機・履歴・他repoはcontroller、報告訂正はreporterへ分け、元条件を省かず、個別承認を代替しないでください。\n"+executionProblem+executionPlanAcceptanceList(answer))
			}
			if completionProblem != "" {
				ok, err := s.repair(ctx, d, config, repairs, "completion_plan")
				if err != nil {
					return nil, err
				}
				if ok {
					corrections = append(corrections, "起票前に完了までの担当・環境・検証・マージ・配置と承認を整理してください。利用者に形式の修正を求めません。"+completionProblem)
				} else if len(corrections) == 0 {
					return nil, &ai.Failure{Kind: "reply_check", Message: "完了計画の整理を完了できません: " + completionProblem}
				}
			}
			trigger = "format_repair"
			// 両方を直すときは、先に検査する実行計画の種類で記録する。
			repair = repairDetail{kind: "completion_plan"}
			if executionProblem != "" {
				repair = repairDetail{kind: "execution_plan"}
			}
			extra = strings.Join(corrections, "\n")
			continue
		}
		finishCompletionReply := prepareCompletionReply(d, answer)
		holdNewProject(answer)
		problem := reviewEvidenceProblem(d, answer, m, shown, bodies, session)
		if problem != "" {
			if ok, e := s.repair(ctx, d, config, repairs, "reviewed_unconfirmed"); e != nil {
				return nil, e
			} else if ok {
				trigger = "format_repair"
				repair = repairDetail{kind: "reviewed_unconfirmed"}
				extra = problem
				continue
			}
			controllerQuestion(answer, "実装とテストの該当箇所を確認できる資料・行範囲を指定してください。（"+problem+"）", problem)
		}
		if err := validateImages(imageMeta, answer); err != nil {
			controllerQuestion(answer, "添付画像の画面・箇所・変更内容を確認してください。", err.Error())
		}
		if answer["ready"] == true && s.options.Reference != nil && fleet.Text(d["project"]) != "" && fleet.Object(answer["newProject"])["needed"] != true {
			key := fullOpenRequestsKey(m.state.Projects[fleet.Text(d["project"])])
			if key == "" || fleet.Text(reads.value["answerOpenRequestsKey"]) != key {
				// A reply written without the full list cannot be used after a restart.
				reads.value["answerPending"] = false
				if err := reads.save(ctx); err != nil {
					return nil, err
				}
				// 読み足しの枠はAIの要求にかかる。照合資料はコントローラーの責任で最終回でも取得する。
				doc, _, err := m.read(ctx, ReadRequest{File: "open-requests", Mode: "full", Context: 5})
				if err != nil {
					return nil, err
				}
				if err := m.updateOpenRequestsNotice(ctx, doc.Reason != ""); err != nil {
					return nil, err
				}
				if final {
					overlapUnread = m.finishOverlapList(ctx)
				}
				if doc.Reason == "" && (!final || finalCallRoomLeft(budget)) {
					trigger = "context_read"
					extra = "既存依頼・TODOの照合資料を追加しました。重複を照合して回答し直してください。"
					continue
				}
				controllerQuestion(answer, "既存の依頼との重複照合を確認してください。", "既存依頼の照合資料を確認できません")
			}
		}
		if findings := s.environmentFindings(ctx, d, answer); len(findings) > 0 {
			details := environmentRequestDetails(findings)
			ok, e := s.repairWithinBudget(ctx, d, config, repairs, "environment_request", budget, reads, final)
			if e != nil {
				return nil, e
			}
			if ok {
				trigger = "automatic_continuation"
				extra = "実装環境で実行できない要求があります。満たせない条件は原則として「## マージ後の運用作業（自動化の対象外）」に移し、担当、実施手順、確認内容、自動化できない理由を明記して実装の完了条件から外してください。依頼の価値に不可欠な条件は移さずready=falseとし、範囲または完成物について具体的に質問してください。一部の担当PCだけが満たす場合も新規起票時にPCを固定できないため同様に扱ってください。既定ブランチへの追従と再検証はコントローラーが行うため依頼文に書かず、他の要求はコントローラーの担当とするか固定したリポジトリ資料のパス・行範囲やテストフィクスチャで代えてください。\n" + details
				continue
			}
			controllerQuestion(answer, "次の条件をマージ後の運用作業へ移しますか。それとも依頼の価値に不可欠な条件として残し、対象範囲または完成物を変更しますか。担当・手順と確認方法も指定してください。\n"+details, details)
		}
		if problems := acceptanceProblems(answer); len(problems) > 0 {
			details := strings.Join(problems, "\n")
			ok, e := s.repairWithinBudget(ctx, d, config, repairs, "acceptance", budget, reads, final)
			if e != nil {
				return nil, e
			}
			if ok {
				trigger = "format_repair"
				repair = repairDetail{kind: "acceptance"}
				extra = "受入条件の確認手段と期待結果を補完してください。利用者に形式の書き直しを求めません。\n" + details
				continue
			}
			controllerQuestion(answer, acceptanceQuestion(problems), acceptanceReason(problems))
		}
		if again, e := reads.checkIntakeSize(ctx, d, answer, config, final, m.state.Rounds); e != nil {
			return nil, e
		} else if again {
			if err := m.countRound(ctx); err != nil {
				return nil, err
			}
			trigger = "automatic_continuation"
			extra = fleet.Text(fleet.Object(reads.value["intakeSize"])["notice"])
			continue
		}
		if again, e := reads.checkHeavyReason(ctx, d, answer, final, m.state.Rounds); e != nil {
			return nil, e
		} else if again {
			if err := m.countRound(ctx); err != nil {
				return nil, err
			}
			trigger = "automatic_continuation"
			extra = fleet.Text(fleet.Object(reads.value["heavyReason"])["notice"])
			continue
		}

		if final && overlapUnread && answer["ready"] != true && len(fleet.Elements(answer["questions"])) == 0 {
			controllerQuestion(answer, overlapUnreadQuestion, "読取枠の終了後も重なり候補の続きを取得できませんでした")
		}
		holdUnreadHistory(d, answer)
		if err := s.crossApproval(ctx, d, answer, m); err != nil {
			return nil, &ai.Failure{Kind: "reply_check", Message: err.Error()}
		}
		if err := s.finalize(ctx, d, answer, m); err != nil {
			var scopeErr *scopeFormatError
			if errors.As(err, &scopeErr) {
				if ok, e := s.repair(ctx, d, config, repairs, "change_scope"); e != nil {
					return nil, e
				} else if ok {
					trigger = "format_repair"
					repair = repairDetail{kind: "change_scope"}
					extra = "変更範囲の形式を修正してください: " + err.Error()
					continue
				}
			}
			return nil, &ai.Failure{Kind: "reply_check", Message: err.Error()}
		}
		if problems := acceptanceProblems(answer); len(problems) > 0 {
			controllerQuestion(answer, acceptanceQuestion(problems), acceptanceReason(problems))
		}
		finishCompletionReply()
		clarifyIncompleteReply(answer)
		ensureQuestion(d, answer, questionlessQuestion, "回答が起票可でも質問でもありませんでした")
		stage = "代理回答"
		return s.proxy(ctx, d, answer, config, m, settings, len(images) > 0)
	}
}

// resolveReferences saves the issues and consultations that the common numbers in the
// consultation refer to.
func (s *Service) resolveReferences(ctx context.Context, d Record) error {
	if s.options.ResolveNumbers == nil {
		return nil
	}
	refs, err := s.options.ResolveNumbers(ctx, d)
	if err != nil {
		return err
	}
	return s.saveResolvedReferences(ctx, d, refs)
}

// turnSession restores the resumable session of the turn. A session cannot be resumed
// when the conversation input omits earlier messages, so it is dropped then.
func (s *Service) turnSession(ctx context.Context, d Record, config map[string]any, settings models.Settings, hasImages bool) (*sessionState, error) {
	session, err := s.session(ctx, d, config, settings, hasImages)
	if err != nil {
		return nil, err
	}
	_, omitted, err := conversationInput(d)
	if err != nil {
		return nil, err
	}
	if omitted && session.ID != "" {
		if err := s.dropSession(ctx, d, session); err != nil {
			return nil, err
		}
	}
	return session, nil
}

// readStep is how the turn goes on after the answer asked for more materials.
type readStep struct {
	// result is the answer that ends the turn. When it is nil, the turn calls the AI again.
	result  Record
	trigger string
	repair  repairDetail
	extra   string
	// final is set when the read rounds ended and the next call makes the final draft.
	final         bool
	overlapUnread bool
	// lenient keeps a read-only final reply from failing the format check.
	lenient bool
}

// finalAnswerStep は資料要求の直しを使い切ったとき、読取だけの仮応答で相談を終えず、
// その回だけ最終回として既読の根拠で下書きを作らせる。読取の枠は閉じず、次の発言で読み足せる。
// AIがそれでも読取だけを返したときは形式の失敗にせず、従来どおり仮応答で終える（lenient）。
func finalAnswerStep(ctx context.Context, m *materials, reason string) readStep {
	return readStep{final: true, lenient: true, overlapUnread: m.finishOverlapList(ctx), trigger: "automatic_continuation", extra: reason + "これ以上の資料は読まず、保存済みの根拠で作れる下書きと残る未確認を具体化してください。"}
}

// answerReadRequests handles the materials the answer asked for: it rejects malformed and
// repeated requests, ends the read rounds at their limit, or reads the fresh requests.
func (s *Service) answerReadRequests(ctx context.Context, d Record, config map[string]any, reads *readBudget, m *materials, repairs map[string]int, answer Record, requested []any, final bool) (readStep, error) {
	if final {
		return readStep{result: endAnswer(d, answer)}, nil
	}
	if len(requested) > 12 {
		return readStep{}, errors.New("一度の追加資料要求が12件を超えています")
	}
	valid, problem := splitReadRequests(requested)
	if problem != "" {
		if e := reads.rejectRequests(ctx, problem); e != nil {
			return readStep{}, e
		}
	}
	fresh, e := m.fresh(valid)
	if e != nil {
		return readStep{}, e
	}
	if problem != "" && len(fresh) == 0 {
		if ok, e := s.repair(ctx, d, config, repairs, "invalid_read"); e != nil {
			return readStep{}, e
		} else if ok {
			return readStep{trigger: "automatic_continuation", extra: fleet.Text(reads.value["invalidReadNotice"])}, nil
		}
		if reads.remaining() > 1 {
			return finalAnswerStep(ctx, m, "資料要求の訂正を完了できませんでした: "+problem+"。"), nil
		}
		answer = Record(fleet.Object(fleet.Clone(answer)))
		answer["reply"] = strings.TrimSpace(fleet.Text(answer["reply"])) + "\n資料要求の訂正を完了できませんでした: " + problem
		return readStep{result: endAnswer(d, answer)}, nil
	}
	if len(fresh) == 0 {
		if ok, e := s.repair(ctx, d, config, repairs, "repeated_read"); e != nil {
			return readStep{}, e
		} else if ok {
			return readStep{trigger: "format_repair", repair: repairDetail{kind: "repeated_read"}, extra: "同じ資料要求が反復されました。既読の資料から回答するか、別の行範囲・検索語を指定してください。"}, nil
		}
		if reads.remaining() > 1 {
			return finalAnswerStep(ctx, m, "同じ資料要求が直したあとも反復されました。"), nil
		}
		return readStep{result: endAnswer(d, answer)}, nil
	}
	if reads.ending() || m.state.Rounds >= 6 {
		if err := reads.end(ctx); err != nil {
			return readStep{}, err
		}
		take, e := reads.take(ctx, "draft")
		if e != nil {
			return readStep{}, e
		}
		if !take {
			return readStep{result: endAnswer(d, answer)}, nil
		}
		return readStep{final: true, overlapUnread: m.finishOverlapList(ctx), trigger: "automatic_continuation", extra: "追加確認を終了します。保存済みの根拠で作れる下書きと残る未確認を具体化してください。"}, nil
	}
	if err := reads.readRound(ctx, m, fresh); err != nil {
		return readStep{}, consultations.AtStage("資料の読取", err)
	}
	return readStep{trigger: "context_read", extra: reads.notice() + "\n" + fleet.Text(reads.value["invalidReadNotice"])}, nil
}

// initialTurnNotice collects the read and environment notices that the first call of the turn repeats.
func initialTurnNotice(reads *readBudget) string {
	extra := reads.notice() + "\n" + fleet.Text(reads.value["invalidReadNotice"])
	if state := fleet.Object(reads.value["intakeSize"]); state["pending"] == true {
		extra += "\n" + fleet.Text(state["notice"])
	}
	if state := fleet.Object(reads.value["heavyReason"]); state["pending"] == true {
		extra += "\n" + fleet.Text(state["notice"])
	}
	return extra
}

// rememberAnswer records which materials the answered call delivered and saves the answer
// so that a resumed turn neither resends them nor repeats the call.
func (s *Service) rememberAnswer(ctx context.Context, d Record, m *materials, session *sessionState, reads *readBudget, shown, bodies map[string]bool, answer Record) error {
	if session.Sent == nil {
		session.Sent = map[string]bool{}
	}
	for key := range shown {
		session.Sent[key] = true
	}
	if err := m.rememberDelivered(ctx, shown); err != nil {
		return err
	}
	m.addDeliveredBodies(session, bodies)
	m.addEvidenceShown(shown, bodies)
	session.Context = m.identity()
	key := fullOpenRequestsKey(m.state.Projects[fleet.Text(d["project"])])
	if shown[key] {
		session.LastOpenRequestsKey = key
	}
	if session.ID != "" {
		if err := s.rememberSession(ctx, d, session, session.ID); err != nil {
			return err
		}
	}
	delete(reads.value, "invalidReadNotice")
	if shown[key] {
		reads.value["answerOpenRequestsKey"] = key
	} else {
		delete(reads.value, "answerOpenRequestsKey")
	}
	return reads.remember(ctx, answer)
}

// splitReadRequests separates well-formed read requests from the problems of the malformed ones.
func splitReadRequests(requested []any) ([]any, string) {
	valid := make([]any, 0, len(requested))
	problem := ""
	for _, item := range requested {
		if _, e := request(item); e != nil {
			problem += "\n" + e.Error()
		} else {
			valid = append(valid, item)
		}
	}
	return valid, problem
}

// fullOpenRequestsKey is the identity of the current full list, including its body.
func fullOpenRequestsKey(p *projectState) string {
	if p != nil {
		for i := len(p.Reads) - 1; i >= 0; i-- {
			doc := p.Reads[i]
			if doc.Path == "open-requests" && doc.Request.Mode == "full" && doc.Reason == "" {
				return documentKey(doc)
			}
		}
	}
	return ""
}

// readRoundsLeft is the number of additional read rounds the prompt offers.
func readRoundsLeft(m *materials, final bool) int {
	remaining := 6 - m.state.Rounds
	if remaining < 0 {
		remaining = 0
	}
	if final {
		remaining = 0
	}
	return remaining
}

// resetSession drops the resumable provider session and logs why when one existed.
func (s *Service) resetSession(ctx context.Context, d Record, session *sessionState, reason string) error {
	if session.ID != "" {
		logSessionReset(d, reason)
		session.ResetReason = reason
	}
	return s.dropSession(ctx, d, session)
}

// switchRaisedTier restarts the session on the settings of the raised difficulty.
func (s *Service) switchRaisedTier(ctx context.Context, d Record, config map[string]any, session *sessionState) (models.Settings, error) {
	if err := s.resetSession(ctx, d, session, "difficulty_raised"); err != nil {
		return models.Settings{}, err
	}
	settings, err := s.resolve(ctx, config, map[string]any(d), "intake")
	if err != nil {
		return models.Settings{}, err
	}
	session.Provider, session.Key = settings.Provider, sessionKey(settings, config)
	return settings, nil
}

// rejectRequests saves the correction notice for malformed read requests and asks for a new answer.
func (r *readBudget) rejectRequests(ctx context.Context, problem string) error {
	r.value["invalidReadNotice"] = "資料要求が不正です。訂正してください:" + problem
	if strings.Contains(problem, "repo-search") {
		r.value["invalidReadNotice"] = fleet.Text(r.value["invalidReadNotice"]) + `
全体検索は文字列でなく {"file":"repo-search","mode":"keyword","keyword":"調べる識別子","start":null,"end":null,"context":5} の形式です。検索語は依頼と確認済み資料から決めてください。通常ファイルの文字列指定は有効な全文要求です。`
	}
	r.value["answerPending"] = false
	return r.save(ctx)
}

// readRound starts a read round for the fresh requests and reads them into the materials.
func (r *readBudget) readRound(ctx context.Context, m *materials, fresh []any) error {
	if err := r.begin(ctx, fresh); err != nil {
		return err
	}
	m.state.Rounds++
	m.state.Pending = nil
	if err := m.persist(ctx); err != nil {
		return err
	}
	return r.drain(ctx, m)
}

// executionPlanProblem checks the execution plan against the materials and then the
// capabilities of the implementation environments.
func (s *Service) executionPlanProblem(ctx context.Context, d, answer Record, m *materials) string {
	materialAnswer := copyRecord(answer)
	if fleet.Text(materialAnswer["project"]) == "" {
		materialAnswer["project"] = d["project"]
	}
	planProblem := executionPlanWithMaterialsProblem(materialAnswer, m)
	if planProblem == "" {
		planProblem = s.executionPlanCapabilityProblem(ctx, d, answer)
	}
	return planProblem
}

// prepareCompletionReply prepares the completion plan, resolves the completion questions the
// user already confirmed and returns the step that finishes the reply after the checks.
func prepareCompletionReply(d, answer Record) func() {
	prepareCompletionPlan(d, answer)
	completionQuestionsBefore := append([]any{}, fleet.Elements(answer["questions"])...)
	resolvedCompletionQuestion := resolveConfirmedCompletionQuestions(d, answer)
	return completionReplyFinalizer(answer, resolvedCompletionQuestion, completionQuestionsBefore)
}

// reviewEvidenceProblem checks that the files the answer reviewed and the evidence ranges it
// cites were actually delivered.
func reviewEvidenceProblem(d, answer Record, m *materials, shown, bodies map[string]bool, session *sessionState) string {
	problem := reviewedProblem(d, answer, bodies)
	if problem == "" {
		problem = m.evidenceRangeProblem(d, answer, shown, session)
	}
	return problem
}

// finalCallRoomLeft reports whether the final round can still afford another call.
func finalCallRoomLeft(budget *callBudget) bool {
	return budget.check() == nil && budget.calls-int(fleet.Number(budget.value["calls"])) > 1
}

// repairWithinBudget asks for a repair only while a reading round can still afford the call.
func (s *Service) repairWithinBudget(ctx context.Context, d Record, config map[string]any, repairs map[string]int, kind string, budget *callBudget, reads *readBudget, final bool) (bool, error) {
	canCall := budget.check() == nil && !((fleet.Number(reads.value["rounds"]) > 0 || final) && budget.calls-int(fleet.Number(budget.value["calls"])) <= 1)
	if !canCall {
		return false, nil
	}
	return s.repair(ctx, d, config, repairs, kind)
}

// countRound counts a controller continuation as a read round and saves it.
func (m *materials) countRound(ctx context.Context) error {
	m.state.Rounds++
	return m.persist(ctx)
}

func endAnswer(d, answer Record) Record {
	if answer == nil {
		answer = Record{"title": d["title"], "body": d["body"], "questions": d["questions"]}
	}
	consultations.MarkSplitCancellation(answer)
	holdNewProject(answer)
	answer["ready"] = false
	unread := describeReadRequests(answer["readFiles"])
	answer["readFiles"] = []any{}
	answer["reply"] = strings.TrimSpace(fleet.Text(answer["reply"]) + "\n追加資料の確認枠を終了しました。確認できない事項は保存済みの下書きに残し、同じ要求を繰り返しません。")
	// 質問の無いまま終えると相談が止まり（stopQuestionlessOutcome）、依頼者は何を渡せば進むか分からない。
	ensureQuestion(d, answer, endQuestion(unread), "追加資料の確認枠が終了し、起票に足りない確認事項が残っています")
	return answer
}

// questionlessQuestion asks the requester for what a not-ready answer left unsaid.
const questionlessQuestion = "受付AIは起票できるかを確定できず、質問も返しませんでした。足りない確認事項（資料のパスと行範囲、前提の訂正など）を回答するか、保存済みの本文のまま起票してよいかを回答してください。"

// ensureQuestion keeps a not-ready answer actionable: the draft the engine would stop for
// lack of questions (stopQuestionlessOutcome) gets one controller question instead. Drafts
// the engine does not stop (hold, split children, new projects) are left unchanged.
func ensureQuestion(d, answer Record, question, reason string) {
	if answer["ready"] == true || hasQuestion(answer["questions"]) || d["hold"] == true || fleet.Text(d["parentSplit"]) != "" {
		return
	}
	if Record(fleet.Object(answer["newProject"]))["needed"] == true {
		return
	}
	answer["questions"] = []any{}
	// 前の回の理由が残らないよう、この回の質問の理由だけにする。
	answer["questionReasons"] = []any{}
	controllerQuestion(answer, question, reason)
}

// hasQuestion reports whether the answer keeps at least one non-blank question.
func hasQuestion(value any) bool {
	for _, q := range fleet.Items(value) {
		if strings.TrimSpace(fleet.Text(q)) != "" {
			return true
		}
	}
	return false
}

// describeReadRequests names the materials the answer still asked for, one per request.
func describeReadRequests(value any) []string {
	var out []string
	for _, item := range fleet.Items(value) {
		if name := strings.TrimSpace(fleet.Text(item)); name != "" {
			out = append(out, name)
			continue
		}
		request := Record(fleet.Object(item))
		file := strings.TrimSpace(fleet.Text(request["file"]))
		if file == "" {
			continue
		}
		start, end := fleet.Number(request["start"]), fleet.Number(request["end"])
		keyword := strings.TrimSpace(fleet.Text(request["keyword"]))
		switch {
		case start > 0 && end >= start:
			file += fmt.Sprintf(":%d-%d", int64(start), int64(end))
		case keyword != "":
			file += "（検索語: " + keyword + "）"
		}
		out = append(out, file)
	}
	return out
}

// endQuestion asks the requester for what the closed read rounds could not cover.
func endQuestion(unread []string) string {
	if len(unread) > 0 {
		return "追加資料の確認枠を終えたため、次の資料を読めていません: " + strings.Join(unread, "、") + "。該当箇所の本文を回答に貼るか、読まずに保存済みの根拠で起票してよいかを回答してください。"
	}
	return "追加資料の確認枠を終えましたが、起票に足りない確認事項を特定できませんでした。補う資料（パスと行範囲）を回答するか、保存済みの本文のまま起票してよいかを回答してください。"
}

// prepareMaterials restores the carried materials, reads the initial documents of every
// project in the consultation and drops the resumable session when the materials changed.
func (s *Service) prepareMaterials(ctx context.Context, d Record, config map[string]any, session *sessionState) (*materials, error) {
	freshMaterials := fleet.Object(d["progress"]) == nil && fleet.Object(d["carriedContext"]) == nil
	m, err := s.newMaterials(d, config)
	if err != nil {
		return nil, err
	}
	if err := s.restoreCarried(ctx, d, m, session); err != nil {
		return nil, err
	}
	if freshMaterials {
		m.state.InitialDocumentsCollecting = true
		if err := m.persist(ctx); err != nil {
			return nil, err
		}
	}
	projects := []string{}
	for repo := range m.state.Projects {
		projects = append(projects, repo)
	}
	if repo := fleet.Text(d["project"]); repo != "" && m.state.Projects[repo] == nil {
		projects = append(projects, repo)
	}
	for _, repo := range projects {
		if err := m.bootstrap(ctx, repo); err != nil {
			return nil, err
		}
	}
	if err := m.bootstrapGroup(ctx); err != nil {
		return nil, err
	}
	if err := m.seed(ctx); err != nil {
		return nil, err
	}
	if err := m.ensureOpenRequests(ctx); err != nil {
		return nil, err
	}
	if session.Context != "" && session.Context != m.identity() {
		if session.ID != "" {
			logSessionReset(d, "material_identity_changed")
		}
		if err := s.dropSession(ctx, d, session); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// replyCheckFailed は、受付AIの回答を保存した後の検査で断ってターンを終えたかを返す。
// reply_check（計画・承認・変更範囲の検査）と proposal（分割案の上限）が該当し、どちらも
// 「回答の検査」の段で checked に入れた回答を断ったときだけ返る。12件を超える資料要求は
// 種類の無いエラーで、ほかの失敗と区別できないため対象にしない。
func replyCheckFailed(err error) bool {
	var failure *ai.Failure
	return errors.As(err, &failure) && (failure.Kind == "reply_check" || failure.Kind == "proposal")
}
