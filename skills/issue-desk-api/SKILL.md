---
name: issue-desk-api
description: issue-desk の受付 API を、既存スクリプトのない操作で直接呼ぶときに使う。仕様書の参照、tailnet の接続と認証、資格情報を表示しない curl/fetch の手順。
---

<!-- skill-standard: 1 -->

# 受付 API を呼ぶ

普段は issue-desk の `scripts/*.ts` を使う。起票・読取・返信・クローズ依頼・再試行・TODO・運用代行の手順は、同じ配置先の [issue-desk-operations](../issue-desk-operations/SKILL.md) を読む。スクリプトのない操作だけ API を直接呼ぶ。API の存在は操作の実行許可を意味しない。

スクリプトのない読取は `~/.issue-desk/desk desk-get.ts GET '/api/drafts?view=summary&limit=20'` の1行で呼べる。GET専用、出力は既定1 MiB、`--max-bytes` は最大16 MiB。応答中の資格情報に相当する項目は表示しない。経路の `security` を確認し、一時キーで読めない経路はPCトークン接続を使う。

## 汎用窓口の応答

NASは `sh ~/.issue-desk/desk-pc get --path <APIパス>` / `operation --method <POST|PUT|PATCH|DELETE> --path <APIパス> --body-file <JSONの絶対パス>`、Ubuntuは `~/.issue-desk/desk get` / `operation` を使う。窓口が既存K2を読むため `--config` や資格情報を追加しない。`--body-file -` は標準入力のJSON。正規化された同じServerの `/api/` 配下だけを指定する。

応答は `{status, body, bodyType, redacted}`。終了コード0はHTTP成功、1はHTTP拒否、2は手元・通信失敗で、0だけで本文の妥当性を確定しない。JSON応答は `bodyType: "json"` とbodyの必須項目を照合する。204は `body: null`・`bodyType: "empty"`。`redacted: true` は秘密に相当する値・項目を秘匿した印なので、表示値を生値として比較・集計せず、必要な照合項目が伏せられた場合は未確認にする。成功でも業務上必要な現状を再GETして確かめる。

要求本文は1 MiB、応答は8 MiB、期限は30秒。窓口は自動再送・ID書換・リダイレクト追従・画面認証への切替をしない。再送の可否は各操作の手順に従い、同ID照合がある操作では保存した同一本文を使う。

## 仕様と接続

Issue詳細の `permissionApproval` は権限保留対象の照合情報（コミット・PR・完全なファイル集合、または `unavailable` の理由）で、詳細とsummaryで同じ形。GETは承認・再キューを行わない。承認は `POST /api/comment` の `action: "approve-permissions"`（`head`＝permissionApproval.head の40桁、`statement`＝利用者の承認発言）で、Issue詳細の `actions` に `approve_permissions` があるときだけ受け付ける。画面は有効なセッション・同一Origin・CSRF、トークンは運用の代行を許したものだけ（[汎用窓口の承認手順](../issue-desk-issue-control/refs/api-operations.md)）。一時キー・未許可トークンは403、対象の不一致・保留でない状態は409（読み直して確認）、監査投稿が未完了の503は同じ送信IDで再送する。利用者がその対象の権限変更を明示的に承認したときだけ送る。既存の削除承認・再試行で代用しない。Issueの未了の最終承認（`pendingFinalApprovals`、画面と運用代行トークンの詳細に出る）は `action: "approve-final"`（`approvalId`・`head`・`approvalFingerprint`・`statement`）で、トークンでは承認IDとhead SHAを含む利用者の承認の発言が要る（同じ汎用窓口の承認手順）。

- APIの正本は `mofua-suki/issue-desk-intake` の `api/openapi/`。経路の文字列を検索して該当ファイルのメソッド・query・body・応答を読む。共通認証は `00-header.yaml`、参照先定義は `98-components.yaml`・`99-components-usage.yaml`・`99-components.yaml`、結合版は `api/openapi.json`。窓口と配布は `docs/chat-operations.md`。チェックアウトが無い場合は認証付き GET `/api/openapi.json` で結合版を取得し、`?pathPrefix=%2Fapi%2Fissues` のように経路の前方一致で絞る。部分取得は一致する paths と参照用 components を返す。PC別トークン・一時キー・画面セッションを利用でき、tailnet内だけで接続する。値を推測せず、必要に応じて現行の実装とも照合する。
- URL は `https://issue-desk.example.ts.net/`。設定した `ISSUE_DESK_URL` を使ってもよいが、接続先を先に確認する。受付は tailnet 内だけで使い、Funnelや公開インターネットに出さない。
- PC別トークンのファイルは `ISSUE_DESK_TOKEN_FILE` または既存スクリプトの `--token-file` の指定を使う。例は `~/.config/issue-desk/token`。NAS本体では `~/.issue-desk/token`（update.sh が監視の volume から写す）。ファイルのパスだけを指定し、値をコマンド引数・URL・チャット・Gitへ書かない。Linuxでは所有者のみ読み書きできる権限にする。

## 認証を選ぶ

`security` の配列は候補のOR、1候補内のキーはAND。`x-issue-desk-delegate` は `true` ならPC別トークンに運用代行の許可が必要、`false` なら不要、文字列ならその条件で必要という意味。

- `pcToken`: `Authorization: Bearer` のPC別トークン（`idk_`）。v1では名前が送り手PC。自分の相談・TODOなど所有者の制限がある。運用代行の許可は利用者が受付画面で付けたものだけを使う。
- `hostdToken`: hostd 専用の `idh_`。自分のイメージ更新・アプリ配備・更新中の停止印だけ。PC別トークンと交換して使わない。hostd の仕事の引取り・結果報告をチャットの読取の代用にしない。
- `screenSession` と `csrfToken`: 利用者の画面セッション。GET以外は同一 `Origin` と `x-csrf-token` が必要。利用者の承認が要る操作はAIの判断で送らない。トークンから送れる承認（削除・権限・相談の続行・代理回答の取消・プロジェクトの作成同意など）は、利用者がチャットで対象を特定して承認した発言を statement に添え、その発言の日時を `instructionAt`（UTCオフセット付きISO 8601、例 `2026-11-11T11:58:00+09:00`。会話にある利用者の発言の時刻）に必ず入れる（欠けると400。決まりは docs/chat-operations.md『チャットの承認の証跡』）。
- `cliKey`: 受付と同じPC（NASなら受付コンテナ）で既存スクリプトが発行する1回限りの `x-desk-key`。POST等は同一Originも必要。別PCでキーを作らず、通常はトークン接続を使う。

Authorization付きの要求は、失敗しても画面セッション・一時キーへ戻らない。401は資格情報と失効、403は認証区分・運用代行・送り手・Originを確認する。権限を変えて迂回しない。

## curl（Bash、読取の例）

PC別トークンで自分のclient設定を読む。トークンをcurlの引数に入れず、標準入力のヘッダーから渡す。シェルのトレースを先に無効にする。`--verbose`・`--trace`・リダイレクト追従を付けない。

```bash
set +x
: "${ISSUE_DESK_TOKEN_FILE:?トークンファイルのパスを設定してください}"
{ printf 'header = "Authorization: Bearer '; tr -d '\r\n' < "$ISSUE_DESK_TOKEN_FILE"; printf '"\n'; } | \
  curl --silent --show-error --fail-with-body -K - 'https://issue-desk.example.ts.net/api/v1/client-config'
```

ヘッダーはパイプでcurlだけに渡る。トークンファイルは発行されたPCトークン形式のものを使う。途中のヘッダーを表示・保存しない。例の応答には資格情報が含まれない。画面の接続コード・csrfなどを返す経路の応答は表示しない。

## Node.js fetch（読取の例）

```javascript
import { readFileSync } from 'node:fs';
const base = new URL('https://issue-desk.example.ts.net/');
const token = readFileSync(process.env.ISSUE_DESK_TOKEN_FILE, 'utf8').trim();
if (!/^idk_[A-Za-z0-9_-]{43}$/.test(token)) throw new Error('PC別トークンの形式が違います');
const response = await fetch(new URL('/api/v1/client-config', base), {
  redirect: 'error',
  headers: { Authorization: `Bearer ${token}` },
});
if (!response.ok) throw new Error(`受付が応答しました: HTTP ${response.status}`);
const config = await response.json();
console.log(JSON.stringify(config));
```

## 稼働実績の要約と明細

稼働実績・手戻り・検査の発動数は [issue-desk-stats](../issue-desk-stats/SKILL.md) に従い、汎用GET窓口で読む。経路・query・欠測と要約の扱いは同Skillを参照する。

変更する操作は利用者の依頼範囲を確認してから、仕様に合わせて `method` と `Content-Type: application/json`、`body: JSON.stringify(input)` を指定する。呼出し設定・headers・tokenをログに出さない。204は本文を読まない。409は対象の版・状態・leaseを読み直す。通信失敗では、同じ送信ID・epoch・revisionで照合する経路かを先に確認し、引取りや更新を無条件に繰り返さない。画面APIのエラーは `retryable`・`safeToChange` に従う。

Skillを更新した後は、NASは通常の更新で配る。リポジトリの仕様書も同じ版に更新する。

## 一覧のページ規約

`GET /api/drafts` は `project`、カンマ区切りの `status`・`excludeStatus`（除外が優先）で絞り込み、`view=summary` で会話を除いた要約の行を返す（全文の一覧は1ページで数MBになる）。`page`（1以上、既定1）と `limit`（1〜200、既定100）を受け付ける。不正な指定は400。応答は `{items,total,page,limit,hasMore}` で、`total` はアクセス可能かつ絞り込み後の件数。全件が必要な読み手は `hasMore` が false になるまで次ページを取得し、途中の失敗を空一覧や完了として扱わない。

## 許可付きDB参照

DBを読み取り専用で照会するときは [許可付きDB参照](refs/db-query.md) を読む。事前の対象別db-read付与、SQLと応答の検査、失敗時の再送制限、用後の許可取消まで行う。
