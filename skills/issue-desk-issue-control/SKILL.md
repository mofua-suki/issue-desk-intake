---
name: issue-desk-issue-control
description: どのプロジェクトでも、Issueを読む・一覧/検索する・閉じる・再試行する・優先度や担当を変える・停止/再開する・返信する・許可の付与を登録するときに使う。直接PRの作成・マージは merge-manual-pr、共通の接続・送信前のIssue番号確認、起票・依頼文の作成・TODO保存は issue-desk-operations を使う。
---

<!-- skill-standard: 1 -->

# issue-deskの受付経由のIssue制御

Issueの読取・返信などの操作はこのSkillを使う。直接PRの作成・マージだけなら、このSkillの全文ではなく skills/merge-manual-pr/SKILL.md の該当節を読む。

共通の決まりは [issue-desk-operations](../issue-desk-operations/SKILL.md) の「共通の決まり」を参照する。A9 MAXの絶対パス・NAS本体の呼び出し・トークン接続も同章に従う。以下のコマンド例は同章の実行先ごとの呼び出し方を使う。起票・依頼文の作成・TODO保存と送信前のIssue番号確認も同Skillを参照する。

## 許可の付与を登録する

利用者が今のチャットで対象・操作・期間を具体的に述べたときだけ、[許可登録・確認・取消](refs/grants.md) を使う。発言は原文を引用し、必要最小のTTLで登録して用後に取り消す。曖昧な指示・別のチャットやIssue・AI自身の判断で登録せず、~/.claude/settings*.json 等の許可設定を直接書き換えない。

### DBを参照する（db-read）

対象ごとの期限付き `db-read` 許可が必要。恒久にはできずTTLは60〜86400秒。[DB照会手順](../issue-desk-api/refs/db-query.md) の登録・照会・打切り判定・用後取消に従う。資格情報の表・列を読めるようSQLを言い換えて迂回しない。

## 読む・一覧/検索する

[読取・一覧・重複照合](refs/read-issues.md) を読む。単件・通常一覧は既存の汎用GET、依頼・相談・TODOの重複照合は `list-issues.ts --duplicates` を使う。共通番号を勝手にGitea番号へ翻訳せず、応答の対象・状態を照合し、ページ途中の欠測を0件や完了にしない。

## 閉じる

[クローズ依頼・終了理由変更・NASの直接終了](refs/close-issue.md) を読む。Issueの内容と終了理由・根拠を確かめてから、固定IDで依頼し、実際に閉じたことまで確認する。確認待ちや処理中は完了ではない。権限の拒否を一時キーへ切り替えて回避せず、forceは利用者の明示なしに送らない。

## 再試行する

[汎用窓口による再試行](refs/api-operations.md#再試行) を読む。原因と再試行で変わる条件を根拠から確認し、現在の `retryable` を照合して、固定した送信IDとJSONで `/api/comment` に `action: "retry"` を送る。通常・再検証・既定ブランチからのやり直し・上限承認・期限付き許可の条件を混同しない。結果不明は同ID同内容で照合し、成功後の読取失敗では再送しない。明示した送信間隔は同じ系列の `desk-paced.ts` で守る。

## Issueの優先度を変える

優先度変更の条件・手順・確認は [priority-issue](../priority-issue/SKILL.md) を参照する。

## Issueへ返信する（回答・修正指示・追加指示）

画面の返信（画像付きも含む）とreply-issue.tsはasync=trueで返信を保存し、HTTP 202のaccepted=trueと送信IDを受け取ってから、GET /api/comment?id=<送信ID>で結果を待つ。queuedは未完了、doneかつresultがあるときだけ返信の適用完了。failedは確定した拒否で、error/errorCodeを読み、別IDで再投稿しない。通信障害・5xx等の結果不明はerrorを残したqueuedで同じIDの照合を続ける。通信断・CLIの待機期限後は同じ送信IDを汎用GETで照会する。404で未受理と確認できた場合だけ、同ID・同本文で送信する。汎用operationから返信する場合もasync=trueを付ける（省略時の同期応答は旧利用側との互換用）。返信の適用完了と実装の完了は別で、その後は共通番号のfollow-issues.tsで追跡する。

本文作成前に `reply-issue.ts --project <owner/repo> --number <Issue番号> --desk <共通番号> --prepare --prepared <準備ファイル>` を実行し、表示された本文と非logコメントを時系列で読む。秘匿や欠落で未確認なら送らず、別の安全な方法で資料を確認する。有効な条件と取消しを照合してから本文を作り、同じ対象に `--prepared <準備ファイル>` を付けて送る。条件を置換・取消しするときは対象コメントのIDまたは日時と条件、置換内容、維持する条件を書く。準備結果は利用者の承認ではない。

送信直前に本文・コメントの変更を検出すると送らないので、再準備して本文を見直す。応答喪失時は同じ準備ファイル・送信ID・本文・承認指定で再送して受付に照合させる。新しいIDや書き直した本文では再送しない。最後の読取と送信の間の変更は検出できず、条件の意味の判断は本文作成者が行う。

利用者の判断を受けて送る操作は、正本 [chat-instructions.md](../../chat-instructions.md) の箇条 (b)「根拠が変わったとき」を参照する。

1. 送信前に、回答・修正指示・追加指示の本文について、[issue-desk-operations](../issue-desk-operations/SKILL.md) の「送信前のIssue番号確認」の手順を行う。
2. 照合して書き直した本文を、次のコマンドで送る。

```
~/.issue-desk/desk reply-issue.ts --project <owner/repo> --number <Issue番号> --text "<本文>"
~/.issue-desk/desk reply-issue.ts --project <owner/repo> --number <Issue番号> --file <本文.md> --id <送信ID>
node scripts/reply-issue.ts --url <受付のURL> --token-file <トークンのファイル> --project <owner/repo> --number <Issue番号> --file <本文.md>
```

間隔を約束した返信・既存相談への回答も再試行と同じ系列で送る。既存の承認条件と送信IDの扱いはそのまま。待機中の停止では送らず、実行中の停止は子CLIの終了確認まで排他を保持する。終了不明・壊れた記録・所有者不明の排他は自動解除しない。送信IDで受付の実状態と終了を照合する。`~/.issue-desk/paced/<系列IDのSHA-256>/state.json` の終了状態・単調時計の待機時間を確かめ、読めない場合や目視の時刻比較だけで遵守済みと報告しない。

```
~/.issue-desk/desk desk-paced.ts --sequence corrections-20261028 --interval 180 -- reply-issue.ts --project mofua-suki/issue-desk-intake --number 275 --file ~/.issue-desk/requests/answer.md --id <送信ID>
~/.issue-desk/desk desk-paced.ts --sequence corrections-20261028 --interval 180 -- create-issue.ts --draft <下書きID> --text "<回答>"
```


- 実装AIの質問への回答（実装の回答待ち `waiting`）・レビュー保留への修正指示（`review_held`）・追加指示を、受付画面のIssue画面と同じ経路で送る。Giteaへ直接コメントしない。どれになるかは受付の判定どおりで、表示に「回答」「修正指示」「追加指示」と出る。
- 使用量の上限を超えた質問待ちには、上の条件を満たす `--over-limit-statement` を付けた返信を送れる。受付は再試行として適用する。承認のない返信は理由を示して断る。
- 本文は `--text` か `--file` のどちらか1つで、8000 UTF-16コード単位まで。先頭が `/codex` のコマンドの本文は送る前に断られる（再試行は上の「再試行する」、優先度の変更は [priority-issue](../priority-issue/SKILL.md)、停止・再開は下の「Issueの停止・再開・担当を変える」）。画像は付けられない。
- 実装環境で利用できない履歴管理操作や許可されていない外部取得を要求せず、必要な差分・資料本文は依頼者側またはコントローラーが確認して貼って渡す。分けて送る場合も各本文に必要な根拠を含める。
- 既知の操作への実行要求は接続前に検査する。禁止や過去の説明は実行要求と区別するが、自然言語の全表現を判定するものではない。誤検出した場合は `--allow-worker-commands <理由>` で解除できる。これは実装環境の権限や受付の認証・承認、回数・使用量の制限を変更しない。
- 答えるのは、利用者の決定・Issueやほかの資料の記録で答えられるものだけ。利用者の判断が要る質問は利用者に回す。
- 送る前に送信IDが表示される。通信が切れたら、同じ引数に `--id <送信ID>` を付けて送り直す（別の送信IDで送り直さない）。
- 接続と403の扱いは上の「再試行する」と同じ（403なら認証区分・運用代行・送り手を確認し、認証や権限を変えて回り込まない）。

## Issueの停止・再開・担当を変える

[汎用窓口による停止・再開・担当変更](refs/api-operations.md#停止再開担当変更) を読む。利用者の指示または既存の割り込み条件に当てはまるときだけ、対象と状態（`state`・`status`）を確認して送る。停止・再開は `/api/comment`、未着手依頼の担当変更は `/api/handover`。再開理由・担当表のホスト名・同ID再送・操作後確認の条件は参照先に従う。

## 削除・権限・未了の承認を送る

[チャット承認の共通規約と操作別のJSON](refs/api-operations.md#チャット承認の共通規約) を読む。対象head/指紋・利用者の原文または実際に見せた質問文と記号・会話上の発言日時を揃え、既存の運用代行PCトークンで送る。相談の上限後の続行・代理回答取消・組の了承・新プロジェクト作成同意も同参照先にある。AIの判断や時間切れを承認にしない。

## PRを作る（直接PR・AIセッションのPR）

利用者が同じ対象の直接PRを明示した場合は [merge-manual-pr](../merge-manual-pr/SKILL.md) に従い、全検証・独立レビュー・汎用APIでの作成・通常マージ・必要な個別承認・反映確認まで行う。通常の依頼は issue-desk-operations から受付へ送る。直接PRや個別承認は時間切れやAIの判断で代えない。

## 実装と分離した受入条件の確認記録

Issue詳細の `executionProgress` にある条件をコントローラーが実際に確認したときは、`~/.issue-desk/desk record-execution-check.ts --desk <共通番号> --file <確認結果.json> [--id <送信ID>]` で記録する。実装の再開・使用量上限の追加・承認の付与は行わない。PCトークンは運用代行の許可が必要。実装AIの完了申告を実測に変えず、確認前は記録しない。

JSONは `requirement`（条件ID）、`planSHA`、`head`、`generation`、`status`（passed/failed）、`source`（実際に読んだAPI・資料・操作の出典）、`observedAt`（観測日時）、`detail`（観測結果と期待値との照合）を含める。対象SHA・世代はIssue詳細の現在値と照合する。外部確認は実行権限のある既存経路だけを使い、個別承認を代替しない。報告担当の条件ではdetailを訂正文として受付が投稿し、その実在する投稿から確認記録を作る。応答不明は表示済みの同じ送信ID・同じ内容で照合する。
