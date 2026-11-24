# PR作成・マージ依頼のAPI手順

利用条件・検証・独立レビュー・個別承認は [Skill本文](../SKILL.md) に従う。ここはその条件を満たした操作だけで使う。通常の作成・マージは Skill本文『道具で送って結果まで待つ』の `scripts/direct-pr.ts` がこの手順どおりに送って待つ。ここは道具の判定の根拠と、取消など道具に無い操作の手順。[汎用窓口](../../issue-desk-api/SKILL.md) のJSON応答は `body` の中に受付の応答を持つ。`redacted: true` は値が秘匿された印なので、その値を元の値と照合済みにしない。不正なbodyや結果の欠測を正常と扱わない。

## 送信前にIDと本文を固定する

- IDはミリ秒の正の整数（16桁以内）・ハイフン・小文字16進8桁。初回だけ、標準の乱数機能で作る（例: Pythonの `f"{time.time_ns() // 1_000_000}-{secrets.token_hex(4)}"`）。同じ操作の再送で生成し直さない。
- 送信するJSON本文（ID・kind・repo・payload）を、ホーム下の作業ツリーの `.tmp/` へ送信前に保存する。`direct-pr.ts` が保存するのもこの本文だけで、接続先と送り手はそのPCの窓口（K2）で決まる。資格情報の値は保存しない。作成依頼とマージ依頼は別の操作なので、それぞれのIDと本文を持つ。
- `repo` は必須の `owner/name`。別リポジトリやIssueのPRへ流用しない。JSONを正しくエスケープし、重複キー・未知項目・型違いを除く。生成済みファイルは上書きせず、結果不明ならその同じファイルで照合する。

## ブランチ方式でPRを作る

同じリポジトリへpushしたブランチを指定する。別リポジトリの `owner:branch` は使わない。ブランチは先頭ハイフン・`..`・`//`・末尾 `/`・`.lock`・`codex/issue-` 接頭辞を含めない。titleは空でない1行・255 UTF-16コード単位以内、bodyは空でない60,000 UTF-16コード単位以内のPR本文。

```json
{"id":"1790000000000-0123abcd","kind":"pr_create","repo":"owner/repo","payload":{"branch":"chat/topic","title":"変更の題","body":"問題・変更・検証結果"}}
```

例のIDを再利用せず、初回に作った実IDと実値でJSONを保存して送る。NASの例で、Ubuntuでは `sh ~/.issue-desk/desk-pc` を `~/.issue-desk/desk` に替える。

```sh
sh ~/.issue-desk/desk-pc operation --method POST --path /api/v1/requests --body-file <作業ツリー>/.tmp/pr-create.json
```

HTTP 201は新規、200は同ID・同内容の再送。どちらも受理であり、作成完了とは限らない。応答 `body.request.id/kind/repo` とpayloadの送信した各入力項目を、保存した要求に照合する。同IDで異なる内容は409。通信断・結果不明なら下のGETで照合し、受付に記録があるか確認してから同ID・同一本文の再送を判断する。

## pushできないときのパッチ方式

新しい権限・認証を足さず、既定ブランチの祖先にある基点と、同じ基点からのテキストパッチを渡す。新規ファイルは意図したパスだけステージし、未追跡のまま落とさない。バイナリ・秘密ファイル・資格情報・ビルド成果物は含めない。

```sh
git merge-base HEAD refs/remotes/origin/trunk
git diff --numstat <基点SHA> -- <変更対象のパス>
git diff --no-ext-diff --no-textconv --no-color --full-index <基点SHA> -- <変更対象のパス> > <作業ツリー>/.tmp/change.patch
```

既定ブランチが異なる場合は実際の名前へ替える。全ての変更対象を同じ3コマンドへ含め、基点を混ぜない。numstatの `-` はバイナリ。`--binary` は使わない。パッチは空でない厳密UTF-8のテキスト、UTF-8で200,000 bytes以下、NUL・`GIT binary patch` を含まない。baseは小文字40桁SHA。`branch` は同時指定しない。patchSha256は送る文字列のUTF-8に対する小文字SHA256。

既に確かめた基点・repo・題を実値に替え、次のように標準ツールでパッチとPR本文をJSON化できる。初回だけ実行し、ファイルが存在したら止める。結果不明時に再生成しない。

```sh
python3 - <パッチの絶対パス> <PR本文の絶対パス> <要求JSONの絶対パス> <<'PY'
import hashlib, json, secrets, sys, time
from pathlib import Path
patch = Path(sys.argv[1]).read_bytes().decode('utf-8-sig')
body = Path(sys.argv[2]).read_bytes().decode('utf-8')
encoded = patch.encode('utf-8')
if not patch.strip() or len(encoded) > 200000 or '\0' in patch or 'GIT binary patch' in patch:
    raise SystemExit('パッチの形式・サイズを確認してください')
doc = {'id': f'{time.time_ns() // 1000000}-{secrets.token_hex(4)}',
       'kind': 'pr_create', 'repo': '<実際のowner/repo>',
       'payload': {'title': '<実際の題>', 'body': body, 'patch': patch,
                   'base': '<確認した40桁の基点SHA>',
                   'patchSha256': hashlib.sha256(encoded).hexdigest()}}
with Path(sys.argv[3]).open('x', encoding='utf-8') as out:
    json.dump(doc, out, ensure_ascii=False)
print(doc['id'])
PY
```

受付が基点・サイズ・内容・SHA256を再検査し、担当Workerが専用ブランチ・コミット・push・PR作成を行う。パッチ内の秘密を受付へ送って検査させない。自分で内容を読む。基点不在・祖先でない場合は取得済み既定ブランチに載せ直し、検証後に別操作として送る。

## 同IDで結果を読み、必要な場合だけ取り消す

```sh
sh ~/.issue-desk/desk-pc get --path /api/v1/requests/<保存した依頼ID>
```

`body.request` のID・repo・kindと、payloadに送信した各入力項目が保存した要求と一致することを確認する。運用代行PCの承認付きの作成・マージでは、受付が認証結果から `payload.approvalDelegated: true` を追加する。`approvalRequest` を付けたマージでは、受付が作成依頼から写した `approval`・`instructionAt`・`approvalDelegated: true` を追加する。この欄は送信JSONへ自分で追加せず、元の承認発言・日時と認証条件に対応する受付の追加記録として別に確認・保存する。これ以外の未知の差分や、送信項目の欠測・変更を無条件に許さない。状態の意味は以下。

| request.status | 判定 |
| --- | --- |
| queued | 受付の順番待ち。未完了。別IDで重ねない。 |
| claimed | `claimedBy` の担当Workerが処理中。未完了。担当PCが欠測なら処理先未確認として扱う。 |
| done | `result.status` を確認する。result欠測・未知の値は読取失敗であり成功ではない。 |

作成成功は `result.status: "created"`。正の整数 `number`・PRのURL・小文字40桁 `head` を記録し、差分と検証・レビュー対象へ照合する。パッチ方式では `patchSha256` も送信したものと照合し、欠測や不一致なら照合完了としない。`permissionHold` の対象がある場合は、保護対象操作とheadについて個別承認を満たすまでマージしない。

`refused` は拒否、`failed` は処理失敗、`cancelled` は取消済み。`message` 等の理由を読み、未知や欠測を成功・処理待ち・対象なしへ丸めない。ローカル待機を終えても依頼は残るので、再開時は同IDを読む。接続失敗時は読取だけを再実行し、401/403で別認証へ切り替えない。

取り消す必要がある場合だけ、同じ依頼IDへ空JSONでPOSTする。

```sh
printf '%s' '{}' | sh ~/.issue-desk/desk-pc operation --method POST --path /api/v1/requests/<保存した依頼ID>/cancel --body-file -
```

送り手のqueued依頼だけ取り消せる。`body.cancelled: true` とその依頼の状態を確認する。false・409・読取失敗を取消成功としない。claimedをcancelで止められると推定しない。受付内部のnext/result/releaseはチャットから呼ばない。

## マージを依頼する

作成済みPRの実番号とレビュー・全検証済みの実headを入れ、新しいマージ依頼IDを送信前に固定する。

```json
{"id":"1790000000001-89abcdef","kind":"merge_manual","repo":"owner/repo","payload":{"number":42,"head":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
```

個別承認が必要な場合だけ、同じpayloadへ `approval` と `instructionAt` を追加する。発言・日時・対象SHAの条件はSkill本文に従う。通常マージの許可や一般的な「進めて」を、保護対象の個別承認の代わりにしない。`approvalDelegated` は受付が認証から付ける情報なので、チャットの要求へ足さない。

### 事前承認（PR番号が決まる前の承認）

PRを作る前に利用者が保護対象のマージを承認したときは、作成依頼（`pr_create`、ブランチ方式・パッチ方式とも）の payload に `approval` と `instructionAt` を付けて送る。運用代行を許したトークンから送ったときだけ、受付が `approvalDelegated: true` を付けて保存する。PRができたら、マージ依頼には承認の代わりにその作成依頼のIDを `approvalRequest` に入れる。

```json
{"id":"1790000000002-89abcdef","kind":"merge_manual","repo":"owner/repo","payload":{"number":42,"head":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","approvalRequest":"1790000000000-01234567"}}
```

受付は、その依頼が同じrepo・同じ送り手の `pr_create` で、`status: "done"`・`result.status: "created"`・`result.number`/`result.head` がマージ依頼の `number`/`head` と同じで、運用代行トークンの承認を持つことを確かめ、合えば `approval`・`instructionAt`・`approvalDelegated: true` をマージ依頼へ写して保存する。合わなければ400で理由を返し、依頼を保存しない。`approval` と `approvalRequest` を同時に付けると400。ワーカーは、PR本文の作成依頼の印（`<!-- issue-desk-pr-request:<ID> -->`）がちょうど1つで `approvalRequest` と同じときだけ、PR番号の無い承認で保護対象をマージし、PRの承認の記録に出どころ（作成依頼ID）を残す。headが作成時から変わったPRには使えない（作成結果のheadと照合するため）。

```sh
sh ~/.issue-desk/desk-pc operation --method POST --path /api/v1/requests --body-file <作業ツリー>/.tmp/pr-merge.json
sh ~/.issue-desk/desk-pc get --path /api/v1/requests/<保存したマージ依頼ID>
```

`request.status: "done"` かつ `result.status: "merged"` だけがマージ成功。PR番号・headと結果を照合して報告する。queued/claimed、HTTP受理、待ち時間到達、結果の欠測は成功ではない。`refused/failed/cancelled` はそれぞれの理由と状態を保存し、条件を直すまで重ねて送らない。用後はローカルの一時JSON・パッチだけを片付ける。
