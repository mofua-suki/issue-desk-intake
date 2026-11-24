// scripts/generate-desk-contract.ts が shared/src/desk-contract.ts から生成。直接編集しない。
export const deskContract = {
  protocol: 1,
  size: { files: 4, materialBytes: 240000, nearLimit: 0.8 },
  input: { chars: 524288, bytes: 1048576 },
  limits: {
    title: 200,
    body: 20000,
    readFiles: 12,
    questions: 2,
    rounds: 6,
    roundsAfterRaise: 2,
    chatTextMin: 1,
    chatText: 8000,
    conversation: 80000,
  },
  proposals: 25,
  retryDelays: [500, 1000, 2000],
  principles: {
    fields: {
      prevention: {
        principle: '可能な限り仕組み化する',
        meaning: '再発防止をテスト・検査・自動化で仕組みにする。',
        shape: '{kind: test|check|automation|none, detail}',
        kinds: ['test', 'check', 'automation', 'none'],
        writing: 'kind に方法の種類、detail に具体的な方法を書く。kind=none なら detail に防げない理由を書く。',
      },
      assumptions: {
        principle: '前提を疑う',
        meaning: '判断に使った前提を根拠と照合し、確認と推測を区別する。',
        shape: '[{assumption, evidence, verified}]',
        writing:
          'assumption に前提、evidence にファイル・行・コマンドの結果、verified に確認の成否を書く。verified=false は未確認と明記する。',
      },
      similar: {
        principle: 'ほかに同じ箇所がないか調べる',
        meaning: '同種の箇所を探し、対応と全リポジトリで止める検査を記録する。',
        shape: '{searched, found[], handled, guard}',
        writing:
          'searched に検索の語・コマンド、found に見つけた箇所の配列、handled に直した箇所・直さなかった理由、guard に全リポジトリで止める検査（テスト・lint）にしたか・しない理由を書く。',
      },
    },
    required: {
      remediation: ['prevention', 'assumptions', 'similar'],
      other: ['assumptions'],
      rule: '直し・調査・失敗の対応では prevention・assumptions・similar の3つとも必須、それ以外は assumptions だけが必須。',
    },
    placeholderValues: ['なし', '無し', '特になし', '該当なし', 'N/A', 'none', '-'],
    placeholderRule:
      '欄を「形だけ」の語で埋めず、根拠・方法・理由を具体的に書く。kind=none は detail に防げない理由があるときに使う。',
    intakeInstruction:
      '受付AI: 直しの依頼では、受入条件に再発防止の方法と同種の箇所の探し方を含め、確認済み事項に確かめた前提の根拠を書く。',
    requesterInstruction:
      '依頼側のAI: 直し・調査・失敗の対応では、3つの欄に当たる内容を報告する。それ以外も assumptions に当たる内容を報告する。',
  },
  writing: [
    '出典規則（本文・分割判断・heavyReason）: 「## 利用者の判断と根拠」に判断・根拠の事実（確認済み／推定）・確認コード／資料を記す。該当判断を支持する利用者発言の原文引用（「」付き）と日時がある場合だけ「利用者の判断」「利用者の指定」。他は「依頼者の指定」、根拠は「依頼文（依頼者のAI）」。無関係な引用、否定された指定（「分けるなとか言わない」）、過去事例・規則説明は裏付けにしない。',
    '受付AI: 「利用者の判断と根拠」・assumptionsをコード・資料と照合し basisConflicts: [{fact, reason, kind, resolution}] に返す。節なし・食い違いなしは[]。未確認を食い違いと扱わない。requester_fact・user_decision・不明・混在とも、依頼者のAIが同じ相談へ訂正と進め方を返信して再照合する（利用者の再判断を待たない。進め方が破壊的な操作・取り返しのつかない操作・新しい権限・上限超えの再試行に当たるときだけ利用者へ聞く）。resolutionは訂正を反映済みの食い違いだけ解決内容（保存・確定可）、他はnullで保留。',
    '実装先は利用者の明示Claude指定時だけ claude、他は codex（確認不要。create-issue.ts の --provider 省略時も codex）。',
    '細部は「細部は受付AIが決めてよい（利用者の判断は不要）」と任せる範囲を明記。決定済みの方針・値と判断が要る未決事項を分ける。',
    '目的・背景の根拠（Issue番号・数字・ログ行等）を付け、共通番号とGitea番号を区別。',
    '資料・対象ファイルは該当節・行範囲で名指し。探させず重複なく伝える。固定版に無いファイルは未確認として版・パスを報告。複数リポジトリは別依頼。',
    '質問は利用者の決定・資料・他の相談やIssueの状態・受付AIのおすすめからAIが答え、利用者の判断だけ聞く。',
  ],
  scripts: {
    request: 'create-issue.ts（新しい依頼。回答は --draft、読むだけなら --check）',
    todo: 'issue-desk-operations Skill（汎用 operation でTODO保存。相談を始めない）',
    editTodo: 'issue-desk-operations Skill（既存TODOの本文更新は汎用 operation）',
    read: 'issue-desk-issue-control Skill（汎用GETで単件・一覧、list-issues.ts --duplicatesで重複照合）',
    retry: 'issue-desk-issue-control Skill（指示があるか原因を解消したときだけ汎用 operation）',
    attention:
      'desk-alerts.ts と issue-desk-admin Skill（手動操作は汎用 operation、自動通知だけdesk-ops.ts。期限切れの見張りのお知らせは削除APIなし）',
  },
  basisConflicts: {
    kinds: ['requester_fact', 'user_decision'],
    requesterRecovery: {
      action: 'correct_requester_facts',
      guidance:
        '依頼者の事実だけの食い違いです。利用者の判断を取り直す必要はありません。依頼者が同じ相談へ訂正を返信し、受付AIが再照合してください。解消するまで確定できません。同じ依頼を自動再送しないでください。',
    },
  },
  errors: {
    DECISION_BASIS_MISMATCH: {
      action: 'reconfirm_decision',
      guidance:
        '分類不明・判断に影響する食い違いでは、依頼者のAIが訂正した事実を踏まえて利用者の目的に合う進め方を選び、訂正と選んだ進め方を同じ相談へ返信して再照合を受け、利用者には判断の記録で報告してください。利用者の回答を待つのは、進め方が破壊的な操作・取り返しのつかない操作・新しい権限・上限超えの再試行に当たるときだけです。解消まで確定できません。同じ依頼を自動再送しないでください。',
    },
    INPUT_TOO_LARGE: {
      action: 'check_consultation',
      guidance:
        '相談は資料を圧縮しても上限を超える場合だけ子へ分ける。分割禁止は共通の出典規則（「## 利用者の判断と根拠」の該当判断を支持する利用者の「」付き原文引用と日時）で裏付けた場合だけ。引用のない指定は判断材料。裏付けた禁止や子が目安・元の範囲に合わなければ元の相談を続け、収まらなければ未確認事項を残す。同じ相談で進行と子を読み、親は再送しない。起票前の比較・代理回答・見積もり・クローズの停止は理由・未確認事項を報告し、本文は切らない。',
    },
    TOO_MANY_PROPOSALS: {
      action: 'report',
      guidance:
        '受付は25件超の分割案を1回だけまとめ直して依存番号も再検査する。このコードは修正後も不合格の場合に返る。依頼者は理由を報告し、案や依存を勝手に削らず、同じ内容を自動再送しない。',
    },
    MORE_CONTEXT_REQUIRED: {
      action: 'check_consultation',
      guidance:
        '相談の読み足し回数切れでは自動の分割をせず、元の相談に未確認事項を残す。同じ相談を読んで続け、親を再送しない。',
    },
    CONFIRMATION_INCOMPLETE: {
      action: 'check_consultation',
      guidance:
        '実装とテストの確認不足は受付がまず1回読み足しを求め、なお不足なら自動の分割をせず元の相談に未確認事項を残す。固定版に無い資料は最新の既定ブランチへ切り替える。依頼者は相談の進行と未確認事項を読み、利用者の判断が必要な質問だけ回答する。入手できない資料を確認済みとせず、親を再送しない。',
    },
    TEMPORARY_UNAVAILABLE: {
      action: 'retry',
      guidance: '読取または送信IDで照合できる要求だけ、同じ内容・IDで規定回数待って再送する。',
    },
    SPLIT_CHILDREN: {
      action: 'show_children',
      guidance:
        '受付が保存・開始した子の相談を表示し、返信・編集・再送・優先度の変更は各子で続ける。親を再送したり子を新しい依頼として作り直したりしない。',
    },
    CONTRACT_MISMATCH: {
      action: 'update',
      guidance: 'PCのissue-deskを更新し、hostdのSkill配布とNASの受付更新を確認する。',
    },
    REQUEST_REJECTED: {
      action: 'report',
      guidance: '受付の理由を確認する。権限・状態を変えたり同じ内容を自動再送したりしない。',
    },
  },
};
export const deskContractVersion = '4cd4625bbadfe9da0aa2a3f0af5ead6a3c8b12da3ef27b3d1f751c2ca9759213';
export const deskContractHeader = 'x-issue-desk-contract';
export function contractError(code: string) {
  const known = Object.hasOwn(deskContract.errors, code) ? code : 'REQUEST_REJECTED';
  return {
    code: known,
    recovery: deskContract.errors[known],
    contractVersion: deskContractVersion,
  };
}
