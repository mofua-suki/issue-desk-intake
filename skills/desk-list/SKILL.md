---
name: desk-list
description: 受付画面の「要対応」「進行中」「フォローアップ」の件数・定義を確認するときに使う。同じ一覧APIで取得し、取得範囲や欠測による画面との認識齟齬を防ぐ。対処や定期監視はdesk-attentionを使う。
---

<!-- skill-standard: 1 -->

# 要対応・実行中・待機中・停止中・フォローアップを確認する

4分類の件数は画面と共通の分類で数える**独立した仕事の数**。フォローアップの件数は専用タブの未完了の表示行数。分類の正本は `shared/src/desk-attention.mts` の `workState` と `client/src/ui/request-list.ts` の `requestGroupsWithAux`。受付は画面との実行照合テストで検証する。

| タブ・返却欄 | 定義・必須の根拠 |
| --- | --- |
| 要対応・`counts.attention` | 利用者への具体的な質問・個別承認・上限判断がある。誰が何を判断するかを示す。失敗・停止という状態名だけでは入れない。 |
| 実行中・`counts.running` | Workerの有効なlease、または受付が所有する実際のターンを取得時点で確認している。保存されたrunning・thinkingだけでは入れない。 |
| 待機中・`counts.waiting` | 次に進む条件と、自動で再開する担当・仕組みを確認できる。配布待ち、依存待ち、自動再試行待ち、紐付いた調査報告待ち。 |
| 停止中・`counts.stopped` | 未完了だが自動再開予定がない、または実処理・再開の根拠が確認できない。原因と再開に必要な確認を示す。 |
| フォローアップ・`counts.followups` | 自動の後続案の相談・Issue・クローズ確認のうち、まだ処理が残る表示行。相談・確認自身が完了・クローズ済みなら対応する履歴タブへ移し、数えない。対象Issueが終了していても、未処理のクローズ確認は残す。Issueは`status=completed`でも受入確認が未了・読取不能なら数え、`status=closed`は履歴へ移す。着手が遅れた割り込みIssueは4分類へ入るため、この件数には含めない。 |

複数の条件が重なる仕事は要対応→停止中→実行中→待機中を優先し、理由と関連通知を保持する。完了・クローズ・未完了フォローアップ・TODOは別分類。実在する子相談を持つ親相談は、状態によらず一覧に表示・計上しない。保存済みの親子リンクで判定し、子がまだ作られていない分割案は独立した相談として残す。独立した子相談・子Issueを数え、同じ仕事に対する補助通知はその仕事へ紐付ける。個別承認の対象は集約して失わない。

`view=work` の分類済み行の `work` は `state`・`reason`・`nextActor`・`nextAction`、待機中は `waitCondition`・`resumeBy`。実処理未確認は理由に明示する。表示の分類は再試行・回答・承認・クローズの許可ではない。

## 取得する

共通の接続は [issue-desk-operations](../issue-desk-operations/SKILL.md) の `refs/connection.md` に従う。NAS・Ubuntuでは既存窓口を使う。

```sh
~/.issue-desk/desk desk-get.ts GET '/api/list?view=counts'
```

APIは `GET /api/list?view=counts`。画面のCookieセッション、NASのローカル一時キー、運用代行を許した既存PCトークンで読める。新しいトークン・権限を用意しない。`since` は併用しない。応答は `counts`（`attention`・`running`・`waiting`・`stopped`・`followups`）、`checkedAt`、`scope: active`、`partial`、`unavailable`。TODOを除く範囲で4分類とフォローアップの入力を取得する。

- フォローアップも同じ件数APIを使い、`counts.followups`を読む。欄のない旧版は未確認とし、別の件数で代用しない。終了した相談を残す旧版の画面とは定義が異なるため、稼働版も照合する。
- `counts.inProgress` は旧クライアント向けの非推奨の互換名で、`running` と同値。未完了全体として足したり報告したりしない。旧「進行中」の件数とは定義が変わっている。
- `partial=false` と読取成功を確認してから件数を答える。取得時刻は必要に応じ日本時間へ直す。
- `partial=true` は取得できた範囲の暫定値。欠測経路を添え、完全な件数や0件と断定しない。取得エラー・不正な応答も0件にしない。
- 画面との不一致は、表示時点・画面の保持データ・稼働版・取得範囲・欠測を照合してから原因を述べる。取得時点の異なる数字だけで不具合と断定しない。
- 件数を聞かれた回は取得して答える。状態確認を対処・返信・再試行の許可に読み替えない。

内訳はNASで `~/.issue-desk/desk desk-get.ts GET '/api/list?view=work' --max-bytes 16777216` を使う。`counts` と同じ入力で分類済みの `groups.attention`・`groups.running`・`groups.waiting`・`groups.stopped`、画面には表示しない親の参照情報 `families` を返す。各行の `work` が関連通知を反映した分類・担当・行動であり、元の状態名だけで数え直さない。`view=work` は画面とNASローカル一時キーで読め、PCトークンは対象外。生の `GET /api/list?tabs=active` は画面用の入力であり、独自の再分類には使わない。要対応の原因調査・操作・監視は [desk-attention](../desk-attention/SKILL.md) に従う。そのCLIの `summary.total`・`summary.cases` や `monitoring.length` を画面の4分類の件数に読み替えない。
