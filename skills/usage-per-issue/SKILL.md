---
name: usage-per-issue
description: NASの依頼別使用量レポートを期間やリポジトリを指定して取得・確認するときに使う。
---

<!-- skill-standard: 1 -->

# 依頼別使用量レポート

依頼別の使用量、工程、手戻りや集計を求められたときに使う。Issueの起票・TODO保存は issue-desk-operations、Issueの読み取り・制御は issue-desk-issue-control、要対応などの管理操作は issue-desk-admin の担当。このSkillは読み取りレポートだけを扱う。

## 実行前

- 利用者の依頼と、実行環境での直接Go管理CLIの実行許可を確認する。NASのチャットでは許可の付与 `nas-claude-rule`／`usage-per-issue` が、ホストから `sh ~/Workspace/issue-desk-worker/deploy/nas/intake.sh exec -T /usr/local/bin/intake admin --config /state/config.local.json --application-dir /app usage-per-issue report --from …` を呼ぶ形だけを許す（`deploy/nas/permission-grant-rules.json`）。許可が確認できなければ実行せず、その理由を伝える。認証や権限を変更したり、別経路で回避したりしない。
- NASの受付で、共有DB接続環境とアプリ用PostgreSQLへの接続が既に整っている環境で実行する。設定値、トークン、資格情報を表示・報告しない。取得結果も必要な範囲だけ共有する。
- この操作は読み取りであり送信IDを使う更新操作を含まない。403、409、通信失敗、DB読取失敗や不明な結果は成功として報告しない。

## 引数と実行

NAS受付コンテナ内の `/usr/local/bin/intake` を直接使う。以下はCLIの直接実行が既に許可され、実行環境が整っている場合の例。`/state/config.local.json` と `/app` はNAS配置の例で、実際の設定・アプリの場所に合わせる。値はシェルで安全に引用する。

```sh
/usr/local/bin/intake admin --config /state/config.local.json --application-dir /app usage-per-issue report --from 2026-11-01 --to 2026-11-30 --repo owner/repo --format json --population all
```

| 元Scriptの引数 | 直接CLIでの扱い |
| --- | --- |
| `--from`、`--to` | 必須、既定値なし。実在する `YYYY-MM-DD`。日本時間の開始日から終了日を含め1〜92日。逆順や範囲外はエラー。 |
| `--repo` | 任意、省略時は全リポジトリ。指定時は `owner/repo` 形式。 |
| `--format` | 任意。`table`、`json`、`both` のいずれか。既定は `both`（表の後にJSON）。 |
| `--population` | 任意。`merged`（既定）または `all`。`all` はJSONに未マージを含む `detail` を追加し、表や通常の集計対象を変えない。 |
| `--config` | 任意。元Scriptは `ISSUE_DESK_CONFIG`、未設定または空なら `/state/config.local.json` を使う。直接CLIでは `admin` の直後、サブコマンドより前に指定し、省略時は `config.local.json`。 |
| `--sqlite` | 元Scriptは値を受け付けCLIへ転送するが、現行CLIには定義がなく拒否される。指定しない。 |
| `--help` | 元Scriptでは単独指定時だけ使い方を表示して終了する。CLIのレポート引数として利用できない。ここに記した引数を参照する。 |

レポート引数はそれぞれ高々1回指定する。未知の引数、重複、値の欠落、不正な形式や余分な引数はエラー。元Scriptも値が必要な引数を重複なく受け付ける。直接CLIには `--application-dir` を `admin` のオプションとして指定し、省略時はカレントディレクトリになる。

全リポジトリの表だけを見る例:

```sh
/usr/local/bin/intake admin --config /state/config.local.json --application-dir /app usage-per-issue report --from 2026-11-01 --to 2026-11-01 --format table
```

## 結果を確認する

終了状態とエラーを確認する。引数エラーなら指定を修正する。共有DBや下書き保存先の接続・読取に失敗した場合は結果を成功や0件とせず、権限を変えずに環境の管理者へ状況を伝える。

表の冒頭の日本時間の期間とマージ件数、読めない行、分割相談の帰属行数を確認する。各依頼の工程別の記録数・途中の記録・不明件数、および集計の既知・不明・件数を見る。`不明` や欠測をゼロと扱わない。表は期間内にマージされた依頼の集計で、マージ後の使用量を含めない。下書きの相談使用量は最初の起票先へ集約される。

JSONでは `period.fromDate`・`period.toDate`、`total.count`、`skipped`、`attribution.splitRows`、各 `issues` の `total.partialRows` と `unknown`、集計の `known`・`unknown` を確認する。`all` の `detail.issues` は未マージも含み、`breakdown` の `missingRows` と `limitTally` の `unreadable`・`resolved`・`unresolvedSteps` も確認する。詳細は同じ依頼の使用量行を対象とし、下書き経由の相談行を含まないため通常集計と同一ではない。`both` は表とJSONが連続するため、JSONだけを機械処理するときは `--format json` を使う。欠測や読めない行があればその件数・状態を結果に添え、完全な集計と断定しない。
