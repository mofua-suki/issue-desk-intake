---
name: issue-desk-stats
description: issue-deskの稼働実績、手戻りの内訳、検査の発動数を受付APIから読むときに使う。依頼別の使用量レポートはusage-per-issueを使う。
---

<!-- skill-standard: 1 -->

# 稼働実績を読む

集計は受付が行う。専用CLIを作らず、[issue-desk-api](../issue-desk-api/SKILL.md) と [共通の接続](../issue-desk-operations/refs/connection.md) に従い、既存の汎用GET窓口で必要な表を読む。要対応を確認する操作は含まない。

## 経路を選ぶ

| 読む内容 | GETの経路 |
| --- | --- |
| 稼働実績 | `/api/stats` |
| 手戻りとフォローアップの実行先別比較 | `/api/stats/rework` |
| 検査の発動数と見直し | `/api/stats?tables=stopChecks` |

使用できるquery・認証条件は `api/openapi/` の該当経路で確認する。チェックアウトが無ければ `/api/openapi.json?pathPrefix=%2Fapi%2Fstats` を同じGET窓口で読む。NASの窓口は一時キー、Ubuntuの窓口はPCトークンで、失敗時に自動切替しない。`/api/stats/rework` は運用代行なしのPCトークンでも読める。

```bash
~/.issue-desk/desk desk-get.ts GET '/api/stats?tables=usage,tiers&details=true'
~/.issue-desk/desk desk-get.ts GET '/api/stats/rework?from=2026-10-01&to=2026-10-14&project=owner%2Frepo'
~/.issue-desk/desk desk-get.ts GET '/api/stats?tables=stopChecks'
```

値は依頼に合わせて変え、queryはURLエンコードする。`&` を含む経路全体をシェルで引用する。期間比較の `beforeFrom`・`beforeTo`・`afterFrom`・`afterTo` は4項目を揃える。各値は `YYYY-MM-DD` または日本時間の `YYYY-MM-DDTHH:MM`（秒なし）で、1期間は92日以内。日付の終了は当日を含み、時刻の終了はその時刻を含まない。

## 応答を確認して報告する

- 両経路は既定で要約を返す。`details=true` で明細を取得し、`/api/stats` では `tables` で必要な表を選ぶ。検査だけなら `/api/stats` に `tables=stopChecks` を指定し、手戻り経路や明細指定と混ぜない。
- 汎用窓口はAPI応答そのものを返す。旧CLIの `{stats, failures}` という包みは無い。終了コード0とJSONオブジェクトであることを確認し、検査の取得なら `stopChecks` がオブジェクトであることも確かめる。不正応答・HTTP失敗・出力上限超過を取得成功と扱わない。
- 汎用窓口の出力上限は既定1 MiB。超えたら表・期間を絞るか、必要な範囲で `--max-bytes`（最大16 MiB）を指定する。資格情報を表示せず、401・403を別の認証で迂回しない。
- 要約では `usage` の明細・Issue一覧、`tiers`・`followups` の行、`implementation` のIssue一覧、`inputBudget` の明細が省略される。手戻り要約も `breakdown.issues`・`breakdown.followups.groups[].issues`・`implementation.issues` を省略する。省略・`missing`・`null` は0件ではない。
- 手戻りの `breakdown` が `null` で `unavailable` に理由があるときは、共有DBの読取失敗として報告する。検査行の `null` も未計測であり、未発動と断定しない。
- 原因や異常を述べる前に、欄の意味・実行先ごとの数え方・欠測・母集団の件数・期間に混在する設定や配置版を確認する。件数や大きさを揃えて比較し、単純な前後差を因果効果と断定しない。未確認の解釈は「未確認の仮説」と確かめ方を添える。
