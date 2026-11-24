// Package tierpolicy is the shared source for consultation guidance and filing decisions.
package tierpolicy

import "strings"

const ConsultationRules = "段は範囲の広さではなく中身の難しさで決める。heavy は次の重の条件のいずれかに当たる依頼: (1) この依頼の変更そのものが権限・認証・資格情報、データの削除や移行、不可逆な操作、自動マージや配布の判定基準を変える、(2) 新しいプロジェクトを要する、(3) 原因や方針の分岐を含め、利用者の判断が必要な未決事項がある。既存の安全判定を維持する・弱めない・実サービスに接続しないという制約は重の根拠にしない。「利用者の判断:」に書かれた決定済みの判断は未決事項ではない。「未確認」や verified=false でもコードやログで実装時に確かめられることは利用者の判断が必要な未決事項ではない。「相談で決める」「細部は任せる」「受付AIが決めてよい」は細部を任せた意味で未決事項の根拠にしない。standard は重の条件に当たらない原因と直し方が分かった修正や決まった型の機能追加。テストの追加だけ、資料だけ、検証の記録だけの依頼は他の重の条件に当たらない限り standard 以下。light は小さな修正・表示や文言の変更。ファイル数や文章の長さだけでは重にしない。軽と標準で迷えば standard。"
const ReasonRules = "当てはめた条件の番号と、その根拠になる依頼文の箇所の短い引用（「」付き）を300文字までで書く。heavy ならどの重の条件に当たるかとその箇所の引用、standard・light ならなぜ重の条件に当たらないかと判断の根拠にした箇所の引用。"

const (
	RequestedReason         = "依頼者の指定"
	StallReason             = "止まりの修正の依頼（原因の調べが要るため中）"
	UnknownScopeReason      = "変更範囲の宣言が不明"
	CrossProjectReason      = "複数プロジェクトの案"
	HeavyReasonPrefix       = "分けられない理由: "
	LightReason             = "変更範囲の宣言と受入条件あり"
	LightDisabledReason     = "変更範囲の宣言と受入条件あり（disableLightTier 設定で light 無効）"
	MissingScopeReason      = "変更範囲の宣言なし"
	MissingAcceptanceReason = "受入条件の節なし"
	EmptyAcceptanceReason   = "受入条件の箇条書きなし"
)

// Input contains facts already parsed from the draft; Markdown parsing stays with filing.
type Input struct {
	Requested       string
	StallFix        bool
	ScopeKnown      *bool
	CrossProject    bool
	HeavyReason     string
	HasAcceptance   bool
	AcceptanceCount int
	DisableLight    bool
}
type Decision struct{ Level, Source, Reason string }

func Valid(level string) bool { return level == "light" || level == "standard" || level == "heavy" }

// Decide preserves priority and ignores estimates, path counts, and acceptance counts beyond presence.
func Decide(in Input) Decision {
	if Valid(in.Requested) {
		return Decision{in.Requested, "user", RequestedReason}
	}
	if in.StallFix {
		return Decision{"standard", "desk", StallReason}
	}
	heavy := []string{}
	if in.CrossProject {
		heavy = append(heavy, CrossProjectReason)
	}
	if in.HeavyReason != "" {
		heavy = append(heavy, HeavyReasonPrefix+in.HeavyReason)
	}
	if len(heavy) > 0 {
		return Decision{"heavy", "desk", strings.Join(heavy, "・")}
	}
	if in.ScopeKnown != nil && *in.ScopeKnown && in.HasAcceptance && in.AcceptanceCount >= 1 {
		if in.DisableLight {
			return Decision{"standard", "desk", LightDisabledReason}
		}
		return Decision{"light", "desk", LightReason}
	}
	missing := []string{}
	if in.ScopeKnown == nil {
		missing = append(missing, MissingScopeReason)
	} else if !*in.ScopeKnown {
		missing = append(missing, UnknownScopeReason)
	}
	if !in.HasAcceptance {
		missing = append(missing, MissingAcceptanceReason)
	} else if in.AcceptanceCount == 0 {
		missing = append(missing, EmptyAcceptanceReason)
	}
	return Decision{"standard", "desk", strings.Join(missing, "・")}
}

const ImplementationGuidance = "実装難易度は起票時に受付のGoが機械判定する。相談難易度とは別で、相談で選んだ段を実装へ流用しない。優先順は、保存された明示指定（" + RequestedReason + "）→止まりの修正（standard）→heavy条件（" + CrossProjectReason + "・" + HeavyReasonPrefix + "具体的な理由）→範囲既知で受入条件の箇条書きが1件以上ならlight→その他standard。変更範囲の宣言が不明なだけではheavyにせずstandardにする。disableLightTier=trueは自動lightだけをstandardへ変える。ファイル数・行数・受入条件の件数の大小では上げない。コードフェンス内の箇条書きは数えない。"

func ImplementationRules() string { return ImplementationGuidance }

const SelectionRules = "新しい相談の難易度は依頼元のAIが内容から選び、--tier（TODOの/startではtier）を必ず指定して根拠を本文へ残す。受付は始めの難易度判定をせず、未指定はTIER_REQUIREDで断る。相談の段と根拠はAIが内容から選び、通常の選択を利用者へ質問しない。AIの見積もりや選択を利用者の明示指定として引用・保存しない。利用者の実際の指定がある場合は原文と日時を残し、その指定と既存の固定規則を維持する。モデル名と段の対応は現在の設定を使う。途中の難易度変更は既存のraiseTier/escalateと固定・承認の条件に従い、指定の上書きや新しい承認経路を作らない。今の段で分析が難しい場合だけ具体的理由を添えて引き上げを申告し、質問で解決する場合や高・固定値では申告しない。"
const HandoffRules = "資料の引継ぎには固定ref・正確なパス・読取範囲・本文の提供有無・確認済み事実・推定・未確認事項を記す。同じ固定版で受付へ本文提供済みの範囲は再読を省く。依頼者AIの既読申告だけで受付の本文確認済み記録を作らず、版違い・本文未提供・判断に必要な不足は必要な範囲だけ追加確認する。目次・索引・ファイル名やハッシュだけを本文の確認済みにしない。"
const FinalizationRules = "相談確定前に、目的と受入条件、必要な変更対象と変更範囲宣言、手順、確認手段と合格条件、検証・配置の環境、既存依頼との重複と成果物への依存、実装・検証・マージ・配置の完了計画を照合する。決定済みの利用者判断とAIが決められる細部、残る未確認事項を区別する。既存の出典・承認・起票条件を保持し、利用者の返答がないことや計画了承を個別操作の承認に代えない。"
const SplittingRules = "分割は費用見込みだけで決めず、依存・共有前提・単独で完了できる挙動を確かめる。分ける／分けない理由と推定費用を本文かreplyへ残す。大きさだけでheavyや分割を断定しない。"

const GuidanceText = "相談の難易度: " + ConsultationRules + "\n理由: " + ReasonRules + "\n" + SelectionRules + "\n" + ImplementationGuidance + "\n" + HandoffRules + "\n" + FinalizationRules + "\n" + SplittingRules

func Guidance() string { return GuidanceText }
