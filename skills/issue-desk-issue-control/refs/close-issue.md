# Issueの終了と終了理由の表示変更

接続と応答の `status` / `body` / `redacted` は [共通API手順](../../issue-desk-api/SKILL.md) に従う。通常はNASの `sh ~/.issue-desk/desk-pc get|operation`、Ubuntuの `~/.issue-desk/desk get|operation` を使う。認証方式は送信前に選び、拒否を避けるため別の方式へ切り替えない。

## 閉じてよい条件と対象の照合

利用者が対象の終了を指示した場合、または記録・決まりから終了条件を確認できた場合だけ依頼する。GET `/api/issue?desk=<共通番号>&view=summary` でproject・数値number・title・現在の状態（`state`・`status`）を照合する。`actions` と `closeRefusal` はPC別トークンの応答には付かないので、閉じられるかは状態と受付の拒否理由で確かめる。送信のnumberは応答のGitea番号であり、共通番号ではない。処理中（`status`）・後続の有無・関連PR（`pr`）を確認し、欠測を終了条件の成立と扱わない。`description` や根拠を書くのに本文・会話が要るとき（`bodyTruncated` がtrueのときなど）は `view` を付けない全文のGETで読む。

理由は `voluntary`（取り下げ、後続がない）、`resolved`（マージ済みPRで解決）、`duplicate`（重複先の存在）、`other_route`（別経路のPR・コミットで対応）。`comment` に根拠を `owner/repo#番号` または `owner/repo@コミット` で記す。対象と内容の説明 `description` は空白以外1〜2000 UTF-16コード単位、commentは20000以内。`other_route` のPRを記録する `route` は `owner/repo#正のPR番号` で、他のreasonには送らない。

## PCトークンのクローズ依頼

初回送信前に小文字UUIDを生成し、同じ接続先・送り手・ID・対象・本文を作業ツリー内の `.tmp/` に保存する。`instructionAt` は利用者が終了を指示した会話日時のオフセット付きISO 8601（秒まで、実在し未来でない日時）。AIの実行時刻で置き換えない。PCトークンでは `force` を送らない。別PC担当には運用代行許可が必要。

```json
{"id":"12345678-1234-1234-1234-123456789abc","project":"owner/repo","number":12,"reason":"other_route","route":"owner/repo#34","description":"この依頼で求めた挙動の説明","instructionAt":"2026-11-13T10:00:00+09:00","comment":"owner/repo#34 のマージで対応済み"}
```

```sh
sh ~/.issue-desk/desk-pc operation --method POST --path /api/issues/close --body-file <本文JSONの絶対パス>
sh ~/.issue-desk/desk-pc get --path /api/close-requests/<送信前に固定したUUID>
```

受付AIの内容・根拠照合と担当Workerの終了結果を、同じIDのGETで追う。`reviewing` / `closing` は未完了。3秒程度の間隔・既定15分を目安に確認し、時間切れはIDと現在の状態を保持する。新しい依頼を重ねない。`closed` / `already_closed` だけを終了成功とし、`confirm`（未終了・判断待ち）、`declined`（閉じない判断）、`expired`（依頼後の再オープン）を区別する。HTTP 200/202だけで閉じたと報告しない。

応答不明ならまず同IDのGETで受理を照合し、受付に存在しないと確認できた場合だけ保存済み同一本文で再送する。失敗・読取拒否を未受理と推定しない。Issueが閉じてもPRの処理は別結果であり、`prClose.result` のclosed / already_closed / delegated / failed、mergedとnotedを読む。PR失敗でIssue未終了と扱わず、PRを閉じることとマージを混同しない。

### 確認待ちへの判断

GET `/api/close-requests/<id>` で対象・理由・message・actions・fingerprintを読み直す。終了理由の根拠と利用者の判断を確認できたときだけ、POST `/api/close-requests/<id>/close` へ `{"fingerprint":"<表示された指紋>","instructionAt":"<利用者の指示日時>"}` を送る。指紋が変われば409なので再照合する。新しい指紋へ過去の了承を機械的に流用しない。

閉じないという利用者の判断、または対象・説明の取り違えが記録から明らかなときだけ、同じ経路の末尾 `/keep` へ `{}` を送る（instructionAtは付けない）。判断できなければどちらも送らない。closeが `closing` を返したら再びGETで追い、終了まで成功扱いしない。一覧はGET `/api/close-requests?page=1&limit=200` をhasMore=falseまで読み、欠測・重複・totalとの不一致を部分成功としない。

## 閉じたIssueの表示理由だけを変更

運用代行を許したPCトークンで使える。Giteaには書かず、受付の表示記録だけを変える。事前のGET（`view=summary` で足りる）でproject・numberが一致し、`status` と `state` がどちらもclosed、実在する `closedAt` があることを確認する。現在理由→変更後理由と対象を照合してから、読み取ったclosedAtをそのまま送る。

```json
{"project":"owner/repo","number":12,"reason":"other_route","route":"owner/repo#34","closedAt":"<GETで取得した閉鎖日時>"}
```

```sh
sh ~/.issue-desk/desk-pc operation --method POST --path /api/close-reason --body-file <本文JSONの絶対パス>
```

理由は上記4種、routeはother_routeの場合だけ。保存応答のproject・number・closeReason.reasonとroute.repo/prを照合する。409は再オープン等によるclosedAtの変化として読み直し、古い条件で再送しない。通信断ではGETで表示記録を照合してから判断する。送信IDのない操作なので無条件に再送しない。

## NASの既存一時キーによる直接終了

`POST /api/issues/close` は一時キーなら受付画面と同じ直接終了を行う。PCトークンの確認依頼とは別の認証境界であり、PCの拒否を回避するため使わない。画面専用 `/api/issue/close` をトークン・一時キーから呼ばない。

処理中の対象は通常断られる。`force: true` は利用者がその対象の強制終了を明示的に指示したときだけで、AIの判断や時間切れでは付けない。forceも運用代行トークンからは使えない。通常の終了依頼で足りる場合は上のPC経路を選ぶ。

既存のNAS入口は稼働中の書き手を一意に確認する `deploy/nas/intake.sh exec` と、イメージ内の読み込み役 `chat-local.ts`。以下はNASのbashで、保存済み本文JSONの絶対パスを `CLOSE_BODY_FILE` に設定して使う固定呼出しである。本文はproject・数値number・reason・comment、必要時のみrouteと明示承認済みforceを含める。接続先やキーを本文に含めない。モジュールが要求ごとにキーを発行・回収し、資格情報を出力しない。新しいトークン・許可設定を作らない。

```sh
: "${CLOSE_BODY_FILE:?本文JSONの絶対パスを指定してください}"
sh "$HOME/Workspace/issue-desk-intake/deploy/nas/intake.sh" exec -T node --import /app/deploy/nas/chat-local.ts --input-type=module -e '
import { readFileSync } from "node:fs";
import { redactAll } from "/app/shared/src/io-redact.ts";
try {
  const payload = JSON.parse(readFileSync(0, "utf8"));
  const result = await globalThis.issueDeskLocal().call("POST", "/api/issues/close", payload);
  const { project, number, state, status, closedAt, alreadyClosed, closeReason, prClose, url } = result;
  console.log(redactAll(JSON.stringify({ project, number, state, status, closedAt, alreadyClosed, closeReason, prClose, url })));
} catch (error) {
  console.error(redactAll(error.message));
  process.exitCode = 1;
}
' < "$CLOSE_BODY_FILE"
```

保存先・稼働色を照合できない、拒否、通信断は成功にせず、対象GETで実際の状態を確認する。PCの依頼IDによる照合とは区別し、結果不明の直接終了を無条件に再送しない。forceを追加しての再送は別の判断として扱う。直接終了でもPR失敗とIssueの終了は区別する。用後に本文JSONを消す。
