# TODOの作成・本文更新・相談開始

汎用窓口の接続・`body`・`redacted`・終了コードは [共通API手順](../../issue-desk-api/SKILL.md) の「汎用窓口の応答」に従う。

新規TODOの作成・本文保存・既存本文の更新は以下の汎用窓口を使う。TODOを作り直さず、受付AI・Giteaを呼ばない。事前に本文中のIssueを共通番号と照合する。

## 新規TODOを作成して本文を保存する

保存だけを頼まれたときは、この2段階で終え、相談や起票を始めない。自分のTODOの作成・本文保存POSTは既存PCトークンで使え、運用代行の追加許可は要らない。他所有者の保存と、GETの詳細・一覧には運用代行を許したトークンが必要。本文のIssue番号を照合した後、送信前に小文字UUIDを2つ作る。1つはTODO ID、もう1つは本文保存の送信IDで、対象・作成body・保存bodyとともに作業ツリーの `.tmp/` へ記録する。既存のTODOを編集するときは新規作成を使わない。

1. POST `/api/todos` のbodyを保存する。projectは許可された `owner/repo`、providerは相談開始後の実装先で既定codex。consultationProviderは相談元で、Codexならcodex、Claudeならclaude。省略した値も再送時に変えない。

```json
{"id":"初回に固定したTODO UUID","project":"owner/repo","provider":"codex","consultationProvider":"codex"}
```

```sh
sh ~/.issue-desk/desk-pc operation --method POST --path /api/todos --body-file <作業ツリー>/.tmp/todo-create.json
```

2. 応答 `body.id`・project・provider・consultationProviderを保存した要求に照合する。consultationProviderの省略はcodexというAPIの既定値で照合する。初回の未保存TODOはrevision 0、本文は空。この作成成功だけを「TODOを保存した」と報告しない。初回本文保存のbodyを次の形で固定して同じTODOへ送る。textは空白だけを不可とし、全体で8000 UTF-16コード単位以内。新規本文には画像を付けず、provider/projectは保存bodyで変更しない。

```json
{"id":"初回に固定した保存UUID","revision":0,"text":"照合済みのメモ本文"}
```

```sh
sh ~/.issue-desk/desk-pc operation --method POST --path /api/todos/<保存したTODO ID>/save --body-file <作業ツリー>/.tmp/todo-save.json
```

本文保存の応答 `body.id`・`body.requestId`・`body.revision`・本文の `body.body`・期限・確認条件を照合し、本文が保存されたことを確認して完了とする。既存の運用代行許可がある場合はGET詳細でも照合する。画像の省略は保持であり、空配列で消さない。期限・条件は下記の規約で初回bodyへ含める。

通信断などで結果が不明なら、運用代行がある場合はGETで同じTODOと保存IDを確認する。代行の無い所有者の保存では、同じ作成・保存POSTのうち結果不明の段を、保存済みID・同一bodyで照合する。GETが403でも認証を切り替えて回避しない。新しいIDを作らず、必要な場合だけ各固定bodyで照合する。既存の同ID作成は同じproject・送り手・provider・consultationProviderでしか再送しない。再送時にrevisionを現在値へ更新したり、期限・本文・保存IDを作り直したりしない。保存後に別編集があった場合や409は現状を読み、勝手に上書きしない。失敗して空TODOだけが残っても相談開始や別TODO作成で回避せず、未保存であることを区別する。旧CLIのTODO IDからSHA256で保存IDを導出する規約はAPIの必須条件ではなく、2つのUUIDとbodyを固定して同じ要求を照合すればよい。

## 版・送信ID・画像を保って更新する

詳細GETには運用代行を許したPCトークンが必要。詳細を読み、対象IDと現在の `body.revision` を確認する。NASの例で、Ubuntuは `sh ~/.issue-desk/desk-pc` を `~/.issue-desk/desk` に替える。

```sh
sh ~/.issue-desk/desk-pc get --path /api/todos/<TODO ID>
```

送信前にUUIDを1つ生成して保存し、作業ツリー内の一時JSONに次のbodyを作る。本文は空にしない。読み取ったrevisionを数値で入れる。provider・project・consultationProviderは既存編集では変えない。

```json
{"id":"保存した送信UUID","revision":3,"text":"更新する本文"}
```

トークンでは `images` を省略し、受付に既存画像を引き継がせる。空配列で画像を消さない。一時キーを既存の許可済み経路で使う場合も省略で保持でき、明示するなら直前GETで読んだ画像IDをすべて保持する。

```sh
sh ~/.issue-desk/desk-pc operation --method POST --path /api/todos/<TODO ID>/save --body-file <JSONの絶対パス>
```

応答 `body.id`・revision・本文・期限・確認条件を照合する。通信断などで結果不明なら同じ送信ID・同内容で照合し、新規UUIDで重ねない。再送時は最新revisionを読み取っても、本文・dueAt/check等の保存内容と送信IDは変えない。受付が保存済み送信IDと内容を照合して同じ応答・版を返す。409は同時編集・異内容再送・相談開始済みなので、現状を再取得して照合し、無条件に上書きしない。403は既存トークンの運用代行許可を確認し別経路へ迂回しない。一時JSONは用後に消す。

## 期限と確認条件

- 更新で `dueAt`・`check` を省略すれば既存値を維持する。初回本文保存で期限を省くと保存時刻の7日後。期限は実在するUTC ISO 8601（末尾 `Z`）で、通常保存は保存時刻の1〜30日先。
- 同ID・同内容の再送では期限を現在に合わせて変えない。既受理なら元の応答を返し、未使用の送信IDなら1〜30日の範囲を受付が検査する。
- checkは `{"problem":"期限が読めない","resolved":"期限を確認できる","methods":["ファイルあり: README.md"]}`。problem/resolvedは空白だけを除いて各1〜2000文字、methodsは最大10行、各1〜300文字。
- methodsの形式は「ファイルあり/なし: 相対パス」「文字あり/なし: 相対パス :: 語（200文字以内）」「Issue完了/未完了: 共通番号N」「コマンド: 文（実行せず記録だけ）」。パスの `..`・絶対パス・ワイルドカードは禁止。`check: null` は条件を解除するので、利用者が解除を依頼したときだけ指定する。
- 日時は報告時にJSTへ換算し、欠測を未設定と推定しない。作成と本文保存のそれぞれのID・bodyを、再送時にも保持する。

## TODOの一覧と相談開始

一覧は運用代行を許したトークンの窓口の `get` で `/api/todos` を読む。projectで絞り込む場合も同じ窓口を使う。project値はURLエンコードし、クエリ全体をシェルで引用する（例は `owner/repo` をエンコード）。応答の `body.hasMore` が false になるまで同じ条件で `page` を増やす。読取失敗や `hasMore` の欠落を全件確認済みとしない。

```sh
~/.issue-desk/desk get --path '/api/todos?project=owner%2Frepo&page=1&limit=100'
sh ~/.issue-desk/desk-pc get --path '/api/todos?project=owner%2Frepo&page=1&limit=100'
```

TODO保存後は自動で起票しない。利用者の指示で相談を始めるときは、運用の代行を許したPCトークンを使い、まず詳細の応答 `body.revision` を読む。NASでは次のように読み、その版の数値を本文に入れて開始する（`<TODO ID>` と `<読んだ版>` は実際の値に置き換える）。窓口には `--config` を付けない。

```sh
sh ~/.issue-desk/desk-pc get --path /api/todos/<TODO ID>
printf '%s' '{"revision":<読んだ版>,"consultationProvider":"codex","tier":"light"}' | sh ~/.issue-desk/desk-pc operation --method POST --path /api/todos/<TODO ID>/start --body-file -
```

Ubuntuではそれぞれ `~/.issue-desk/desk get`、`~/.issue-desk/desk operation` を使う。NASでproject絞り込みを行うときも `sh ~/.issue-desk/desk-pc get` を使う。`tier` は相談の難易度（light・standard・heavy）で必須。依頼の難しさで依頼元が選ぶ（[基準](tier-and-splitting.md)。通常は `light`）。無いと400（`TIER_REQUIRED`）。`consultationProvider` は相談元で、Codexのチャットでは `codex`、Claudeでは `claude`。`provider` は独立した実装先で、指定するときはPOST本文に `"provider":"codex"` または `"provider":"claude"` を追加する。

開始は非同期で、応答 `body.id` は下書きID。続きの回答と、結果が不明なときの送り直し前の確認は、NASでも一時キー窓口を使う。

```sh
~/.issue-desk/desk create-issue.ts --draft <下書きID> --text <回答>
~/.issue-desk/desk create-issue.ts --check <下書きID>
```

409の版違いはTODOを再取得して内容と版を確認する。開始の応答を受け取れなかった場合もTODOと同じIDの下書きを確認する。窓口の終了コードは成功0、HTTP拒否1、手元・通信失敗2。403なら代行を許したトークンか確認し、回り込まず利用者に許可または画面からの操作を頼む。

相談元は作成bodyの `consultationProvider`、実装先は別の `provider`。本文の保存だけで相談を始めない。一時JSONは結果を記録した後に片付ける。
