# 一般監視と長い処理の見張り

## 一般監視

`~/.issue-desk/desk monitor-watch.ts start --file <記録の絶対パス>` に `{"config": {...}}` を標準入力で渡す。新しい記録を使い、以降の `next`・`result`・`status`・`cancel` には同じパスを指定する。観測・報告・操作には現在のセッションで確認した既存ツールを使う。

| configの項目 | 指定する値 |
| --- | --- |
| `targets`、`predicate` | `[{"id":"対象","conditions":["完了条件"]}]` と `"all"`。対象と条件は空にせず、IDと各対象内の条件名を一意にする。 |
| `mode` | `"periodic"` は定期報告、`"conditional"` は条件成立時に報告する。 |
| `observer`、`reporter` | それぞれ下記のツール情報を指定する。 |
| `end`、`cancellation` | `{"description":"終了条件","onSatisfied":true}` と解除方法の文字列。`onSatisfied` は真偽値。期限を定めるときは `end.at` にUnix時刻をミリ秒で指定する。 |
| `continuity` | `{"scope":"session","constraint":"このセッションでの継続条件"}`。外部登録を確認した場合のみ `registration` に `id`、`tool`、`evidence`、`accepted: true` を記す。受付済みの外部登録IDを使う。 |
| 任意の項目 | `observeMs` と `reportMs` は正の整数のミリ秒（既定120000、600000）。`handler` は止まりを扱うツール情報。`action` は条件成立後の許可済み操作で、`description`、既存の許可を示す `authorization`、ツール情報 `capability` を指定する。 |

ツール情報には `tool`、`specification`、`checkedAt`（確認時のUnix時刻、ミリ秒）、`available`、`kind`、`destination` を含める。開始時点の過去10分以内に仕様・利用可否・宛先を確かめて `available: true` とする。報告先は利用者に届く `kind`（`user-conversation` または `user-notification`）に限る。`start` の結果が `unavailable` なら `limitations` を確認する。実行IDだけで会話を再開できるとは判断しない。

`next` と `status` は標準入力不要。`result` の標準入力は `{"receipt": {...}}`、`cancel` は `{"reason":"解除理由"}`。以下の `observedAt` もUnix時刻のミリ秒で指定する。

1. `start` 後、`next` が返す `pending` の `kind`・`tool`・`destination`・`payload` に従って、確認済みの既存ツールを呼ぶ。`observe` の結果は `observation: {samples: [{target, observedAt, values, expectedTargets?, blocked?}], error?}`。`values` は条件名から真偽値への対応。`blocked` は任意の `{actor: "user" | "ai" | "system", reason: "理由", action: "対処"}`。
   `blocked` のある見本は、すべての条件が真でも成立とはみなさない。欠測や古い観測、条件の値が null の場合は読取失敗になる。
2. `result` に `{requestId, tool, destination, callId, evidence, outcome, observation?}` を渡す。ID・ツール・宛先の一致、一意な呼出IDと根拠が必要。`outcome` は `confirmed`、`failed`、または通知などの `accepted`。`handle` と `action` は `accepted` だけでは結果にならない。
3. `next` が `handle` を返したら payload の対象・止まり・観測の根拠に従い、検証済み handler を呼んで結果を返す。結果が返るまでは次の観測は出ない。対処失敗や handler が無い場合は `report` を要求する。user の止まりも理由と対処を `report` し、system は記録のみ。同じ止まりの再要求は `reportMs` ごと。報告は利用者に届くツールで実際に送り、その結果を返す。
4. `next` と `result` を繰り返し、成立後の `action` とその結果報告も回収する。`status` は保存状態を読む。`cancel` は待機中の要求を解除し、期限到達時も新しい操作を開始しない。記録の stdout 出力は利用者への通知完了ではない。

## Stage Watch (stage-watch)

長い処理は `node scripts/stage-watch.ts --name <実行名> -- <コマンド> [引数…]` で包む。終了予定を監視するときは区切りの `--` の前に `--expect-end <タイムゾーン付きISO日時>`、必要なら `--grace-min <分>`（既定10分）を指定する。出力数を検査するときは `--outputs <ディレクトリ>` と `--min-outputs <最小数>` を併用する。直下の通常ファイルを数える。

子の失敗、出力不足、遅延終了、期限超過で実行中の場合に受付へ異常を通知する。期限を過ぎても子は継続する。終了コードは子の終了結果に従い、正常終了後の出力不足は3、引数の誤りは2となる。異常の詳細と送信結果は既定で `~/.issue-desk/stage-watch/<実行名>.json` に残る。異常のない終了では通知せず、前回の同名結果を片付ける。

返答後に成功・失敗を同じ会話へ届けるには、その経路の仕様と登録結果を別に確かめる。受付への通知や結果ファイルだけを確認した場合は、このセッションで終了を待ち、依頼された成果を確認して報告する。
