# Issueの単件取得・一覧・重複照合

接続・応答の `status` / `body` / `redacted` と失敗の扱いは [汎用API手順](../../issue-desk-api/SKILL.md) に従う。NASは既存の `sh ~/.issue-desk/desk-pc get`、Ubuntuは `~/.issue-desk/desk get` を使う。認証を切り替えて拒否を回避しない。GETは操作許可ではない。

## 単件を読む

```sh
sh ~/.issue-desk/desk-pc get --path '/api/issue?desk=4553'
sh ~/.issue-desk/desk-pc get --path '/api/issue?project=owner%2Frepo&number=12'
sh ~/.issue-desk/desk-pc get --path '/api/issue?desk=4553&view=summary'
```

`desk` は受付の共通番号で、`number` はGitea番号。どちらも正の整数。共通番号で読んだ応答の `project`・数値 `number`・`desk`・空でない `title` を照合し、プロジェクトも指定された場合は一致を確かめる。query値はURLエンコードする。欠測・不一致・401/403/404・不正応答を対象なしや成功にしない。

`view=summary` は要約であり、本文・会話の全取得ではない。状態（`state`・`status`）、要対応、`bodyExcerpt` / `bodyTruncated`、コメント数、`latestAsk`、`contentRevision` を使って読む範囲を決める。対象の照合と、停止・再開・担当変更・優先度・終了の前後の状態確認は要約で足りる。`actions` と `closeRefusal` は画面・一時キーの応答だけに付き、PC別トークンの応答には無い。送信・判断に本文や会話が必要なら `view` を付けないGETを行い、log以外のコメント、前回の回答・取消し、承認対象を照合する。要約だけで修正指示や承認を送らない。

## 通常の一覧・件数

```sh
sh ~/.issue-desk/desk-pc get --path '/api/issues/search?project=owner%2Frepo&state=open&page=1&limit=100'
sh ~/.issue-desk/desk-pc get --path '/api/issues/search?project=owner%2Frepo&status=waiting&sort=created&order=asc&page=1&limit=100'
sh ~/.issue-desk/desk-pc get --path '/api/issues/search?project=owner%2Frepo&countOnly=1'
```

APIの正本は `api/openapi/50-issues-drafts.yaml` の `/api/issue` と `/api/issues/search`。使うqueryの型と値を確認して指定する。

- `q` は題名の検索語。`host` は行のhost、`mine=1` はPCトークンの送り手名による絞り込み。`host` / `mine` は保存リースや既定PCによる表示上のhostで、担当表の持ち分・PCの実行待ち数とは異なる。`mine` と `host` を併用しない。担当表は `assignments.json` で照合する。
- `state` は `open` / `closed`、`status` は状態のカンマ区切り。`status=waiting` は作業待ち（pending・queued・untracked）の検索で、実装AIへの回答待ちとは限らない。
- `priority` は high/normal/auto、`prioritySource` は user/ai。`sort` は created/updated/closed、`order` は asc/desc。終了日時のない行は終了日時順の末尾になる。
- `createdFrom/To`、`closedFrom/To`、`updatedFrom/To` は実在する日時をオフセット付きISO 8601で送る。fromを含みtoを含まない。fromはtoより前にし、不正な日付の自動繰越を許さない。
- 通常検索と `countOnly=1` の成功応答の両方で `body.incomplete` と `body.unreadProjects` を確認する。`project` 未指定で `incomplete=true` のとき、`total` は読めたリポジトリの行だけの暫定件数として伝え、`unreadProjects` の読めなかったリポジトリと理由も示す。`project` 指定時は、他候補が `unreadProjects` にあっても指定先を読めていれば指定先の件数はそろっている。指定先自体または全候補を読めない場合はエラーとして扱い、0件や成功としない。
- `body.total` は読めた対象の条件に合う件数、通常検索の `body.issues` はそのページの行、`body.hasMore` は続きの有無。`page` は1以上、`limit` は1〜100。既定は更新が新しい順で100件。件数だけの依頼では同じ検索条件で `countOnly=1` を1回使う。一覧が必要なら同じ条件でページを読み、全件が必要ならpageを増やして `hasMore=false` まで読む。各ページでも欠測欄を確認し、途中の失敗・重複行・件数の変動を全件取得としない。
- `countOnly=1` は `issues` や状態の詳細を取得した扱いにしない。依頼・相談・TODOの重複不存在の確認には使わない。

## 状態から次の担当を判断する

専用CLIによる `nextActor`・`nextAction`・`evidence`・`nextUrl` の補完は行わない。APIが返した記録を優先し、欠けた値を確認済みの事実として補わない。状態名だけで操作を許可しない。

- 質問・レビュー指摘・失敗の原因と、現在の状態（`state`・`status`）を読む。AIが対処できる回答・原因解消後の再試行は [操作手順](api-operations.md) とSkill本文の条件に従う。失敗状態だけで再試行しない。
- 削除・権限・最終承認の保留、明示的な停止は利用者の判断に属する。記録・対象SHA/指紋と会話の許可を照合し、AIの判断で承認を作らない。
- `finalApprovalsUnavailable` があれば最終承認の記録を読めない原因を調べ、承認不要と扱わない。`pendingFinalApprovals` があれば対象の差分・SHA・承認条件を確認して利用者の判断を受ける。既存の削除・権限の承認と混同しない。
- `executionProgress.unavailable` は受入条件の実行計画・確認証拠の読取失敗であり、条件の達成ではない。`status=completed` でも `executionProgress.pending > 0` なら未完了条件を担当へ引き継ぎ、現在の `planSHA`・`head`・`generation` に対応した実行証拠を確認する。要約には `checks` が含まれないため、条件ごとの照合は単件の全取得で行う。実装AIの完了申告や古い版の証拠を達成の根拠にしない。実際に確認した結果の記録はSkill本文「実装と分離した受入条件の確認記録」に従う。
- `status=queued` で `dispatchSkip.reason` が `capability` / `execution_protocol` / `execution_plan` の場合は、必要能力・実行計画の対応版・入力資料を確認する。能力不足や計画の不備を単なる順番待ちと扱わず、権限のある既存経路で実行できる担当へ渡す。担当の変更・再試行は各操作の許可条件を別途満たす。
- 実行中・公開中・配置中、前提のPRマージや成果物待ちは、その処理の完了を確認する。移管先への返信・再送が必要という記録がある場合は待つだけにせず、その記録と操作条件を読む。待ちを伝えるときは誰が何をするのかを書く。
- 未知の状態や必要な情報の欠測は未確認と報告する。APIの `url` はそのまま使う。受付画面へのリンクが必要なら実際の受付URLと確認済み `desk` から `/#/issues/<desk>` を作り、Gitea番号で代用しない。

## 依頼・相談・TODOの重複を照合する

単純なq検索の代わりに、残す機械処理を呼ぶ。

```sh
~/.issue-desk/desk list-issues.ts --duplicates --project owner/repo --query '<題名の語>'
```

このCLIは変更操作を送らない。依頼はopen全ページ、相談とTODOは対象一覧の全ページを読み、題名をNFKC正規化・大小文字無視の部分一致で照合する。相談の提出済みと未割当を除き、TODOの題名は本文の最初の空でない行を使う。

出力の `issues` / `drafts` / `todos` をすべて確認する。`confirmed=false` の `reason` は読取失敗・応答不正・項目不正・途中欠落を区別する。`confirmed=true` でも `skipped>0` は空題名で未照合の行がある。候補が空というだけで重複なしとせず、候補の内容・状態と未確認範囲を照合する。読取が一部失敗した場合もCLI終了コード0になり得るため、終了コードだけで完全性を決めない。NAS交代中の専用終了75は元の読取を再開するための終了であり、重複なしを意味しない。
