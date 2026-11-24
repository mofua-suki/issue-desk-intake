# 利用者の許可を登録・確認・取り消す

[共通API手順](../../issue-desk-api/SKILL.md) の既存PCトークン窓口を使う。運用代行が必要で、拒否を別認証・別アカウントへ切り替えて回避しない。APIがあることを許可の根拠にしない。

この会話で利用者が具体的に「何を・どの対象に・どのくらいの期間」許した発言があるときだけ、その原文を引用して必要最小TTLで登録する。AIの要約・推測・別チャットの発言で作らない。自身のpermissions・allowルール・settings.jsonを直接広げない。用が済んだら期限を待たず取り消す。

## 登録の本文

送信前に32桁の小文字16進IDを生成し、接続先・送り手・ID・本文を作業ツリー内に保存する。POST `/api/permission-grants` の未知項目を除き、数値と真偽の型を保つ。

```json
{"id":"0123456789abcdef0123456789abcdef","kind":"db-read","target":"shared","ttlSeconds":600,"evidence":{"quote":"利用者の許可発言の原文","statedAt":"2026-11-13T10:00:00+09:00"}}
```

| kind | target |
| --- | --- |
| nas-claude-rule | usage-per-issue / docker-inspect / docker-stats / docker-compose-ps |
| hostd-repair | PC名:restore-hostd-files / set-node-path / quarantine-old-requests / send-log-tail / retry-failed-image |
| host-ssh | PC名:host-ssh.jsonに現在登録されている固定操作名 |
| implementation-limit-retry | owner/repo |
| db-read | shared または app |

hostd-repairの各修理IDにも `PC名:` を付ける。PC名とSSH操作名は英小文字・数字で始まる英小文字・数字・ハイフンで、SSHの登録・read-only自動承認の現行一覧も受付が照合する。単なる任意コマンドをread-onlyとみなさない。

evidence.quoteは空白以外1〜2000文字、NULを含めず実際の発言を使う。statedAtはオフセット付きの実在する日時で、未来でなく登録の7日以内。送信した時刻で代えない。ttlSecondsは整数60〜86400秒。利用者が恒久を明示したnas-claude-ruleだけ `permanent: true` を使え、ttlSecondsと併用しない。他種の恒久や推測による恒久を認めない。

```sh
sh ~/.issue-desk/desk-pc operation --method POST --path /api/permission-grants --body-file <本文JSONの絶対パス>
sh ~/.issue-desk/desk-pc get --path /api/permission-grants/<固定したID>
```

同ID・同内容は冪等。通信断では同IDのGETで受理を確認し、不明を未受理と扱わず、同じ本文で照合する。別内容で同IDを再利用しない。有効な付与は全体10件・同じkind/targetで1件まで。409で既存の付与や枠を避けるためID・種類・対象を変えない。

## 一覧・取り消し・報告

GET `/api/permission-grants?status=active`（既定）または `status=all`、必要時kindで絞る。応答 `body.grants` が配列であることを確かめ、欠測・読取失敗を0件としない。

POST `/api/permission-grants/<id>/revoke` に `{}` を送り、応答のid/statusと再読取で取り消しを確認する。取り消し済み・期限切れは変えず冪等。同じ対象でも期限切れの再発行は新たな利用者の許可がない限り行わない。

登録後は種類・対象・expiresAt（報告は日本時間）・取り消し方を伝える。appliedAtは反映の記録であり、実際のホスト操作成功や期限延長の許可ではない。利用する操作の条件も別に照合し、用後の取消結果まで確認する。
