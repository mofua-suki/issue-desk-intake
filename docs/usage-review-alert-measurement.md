# 利用量通知の計測

`BenchmarkUsageReviewAlertHTTP` は認証を含む `/api/usage-review/alerts` の HTTP ハンドラーと PostgreSQL を通す。専用の空スキーマに見直し100回（うち確認済み90回）と使用量1000行の合成データを作る。Goの `ns/op` はリクエスト生成からJSON応答の検証までの時間で、ネットワーク・本番負荷・本番のデータ分布は含まない。`usage-queries/op` は使用量DB接続のドライバーで観測した `SELECT json FROM usage_rows` の実行数で、読み取った行数や他のテーブルのクエリ数ではない。ゼロ件しか観測できなければ失敗する。

```sh
node scripts/with-test-postgres.ts -- go -C server test ./internal/httpapi -run '^$' -bench '^BenchmarkUsageReviewAlertHTTP$' -benchmem -count=5
```

比較時は変更前後のソースに同じ計測コードとテスト用フィクスチャを置き、同一PC・Go・PostgreSQL・設定・合成データ・同時負荷の条件でそれぞれ実行する。ビルドやDB初期化は計測時間に含めず、各サンプルと中央値、版、条件、確認済み比率を記録する。データ量や確認済み比率を変える場合は両版で同じ値を使い、結果を別に記録する。小さな差は繰り返しによるばらつきと区別する。この計測は別環境での本番相当データによる実測を代替しない。

運用環境での前後比較は運用担当者が隔離した本番相当データと同一設定・負荷の旧版／新版を用い、外部から認証済み通知APIへの応答時間を複数回測定する。データ件数・期間・確認済み割合、版、環境、同時負荷、測定回数、中央値と高パーセンタイルを記録し、同じ条件で比較する。DBの実読取回数は計測環境のクエリ統計等で同じ要求区間に集計し、推定値と区別する。測定結果には接続先、資格情報、個々の通知や使用量を含めない。応答時間の前後差は他の環境差や負荷の影響を受けるため、単独で因果効果と断定しない。

## 自動再実装の使用量判定

自動再実装の使用量上限判定では、欠測行を同じ工程の重み付き実測から補完する。時間付き実測の最大時間当たり使用量に欠測行の時間を掛け、工程の実測最大値を上限とする。時間なしの実測が1件でもある工程では、欠測を実測最大値で補う。時間なしの実測は時間当たりの使用量を比較できないため、割合の閾値を設けず保守的に扱う。欠測の時間や有効な時間当たり使用量がない場合も最大値を使う。対応する実測がない工程は補完できない。判定イベントの `estimate.steps` は従来の `maximum` に加え、工程の補完額 `estimated` と最大値をフォールバックとして使った行数 `fallbackRows` を持つ。補完不能または数値として表現できない補完額は `null` とする。

### 自動再実装の使用量判定の影響確認

十分な運用期間を置き、運用担当者がServerの共有DBへの読み取り専用接続で次を実行する。NASの配備設定では共有DBの `public.job_events` に判定イベントが保存されるため、検索パスを明示する。`start` は集計開始時刻、`end` は集計終了時刻（含まない）を UTC で指定する。期間と配備版を記録し、同じ長さの期間の結果も比較する。`job_events.at` は判定時刻であり、同じ依頼の再試行は別イベントとして数える。停止依頼数は期間内の異なる job 数で、イベント数とは異なる。初回・欠測なし等でイベントが作られない判定は分母に含まれない。

```sql
\set start '2026-11-06T00:00:00Z'
\set end '2026-12-06T00:00:00Z'
SET search_path TO public;
-- usage-impact-query-start
WITH events AS (
  SELECT e.seq, e.job_id, e.kind, e.data,
    CASE WHEN jsonb_typeof(e.data->'estimate'->'steps') = 'array'
      THEN e.data->'estimate'->'steps' ELSE '[]'::jsonb END AS steps
  FROM job_events e
  WHERE e.kind IN ('implementation_usage_estimate', 'implementation_usage_limit')
    AND e.at >= :'start'::timestamptz AND e.at < :'end'::timestamptz
), per_event AS (
  SELECT e.job_id, e.kind,
    COALESCE(bool_and(COALESCE(jsonb_typeof(s.step->'fallbackRows') = 'number', false)), false) AS known,
    COALESCE(sum(CASE WHEN jsonb_typeof(s.step->'fallbackRows') = 'number'
      THEN (s.step->>'fallbackRows')::numeric ELSE 0 END), 0) AS fallback_rows
  FROM events e LEFT JOIN LATERAL jsonb_array_elements(e.steps) AS s(step) ON true
  GROUP BY e.seq, e.job_id, e.kind
)
SELECT kind, count(*) AS events, count(DISTINCT job_id) AS jobs,
  count(*) FILTER (WHERE known AND fallback_rows > 0) AS fallback_events,
  count(DISTINCT job_id) FILTER (WHERE known AND fallback_rows > 0) AS fallback_jobs,
  sum(fallback_rows) AS fallback_rows,
  count(*) FILTER (WHERE NOT known) AS unknown_events
FROM per_event GROUP BY kind ORDER BY kind;
-- usage-impact-query-end
```

`implementation_usage_limit` は停止判定、`implementation_usage_estimate` は欠測を含む継続判定。各行の `events`・`jobs` と `fallback_events`・`fallback_jobs`、`fallback_rows` を両方の種別で確認する。`unknown_events` は内訳なし・旧形式を含み、ゼロ扱いしない。フォールバックには欠測側の時間なし等も含まれるため時間なし実測だけを原因とする件数ではない。イベントには反事実の比例補完額や全判定が残らず、この集計だけでは変更による増加数・停止率や因果関係は確定できない。必要なら保存済み使用量と当時の設定を別途照合する。

