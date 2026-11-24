---
name: provider-disable
description: 利用者が実行先（Codex・Claude）の一時的な無効の設定・取消、またはその状態の確認を求めたときに使う。片方の実行先の全工程を他方へ移す操作で、設定と取消は利用者の指示があるときだけ行う。
---

<!-- skill-standard: 1 -->
<!-- provider-disable-reference-time: 2026-11-12T09:00:00+09:00 -->
<!-- provider-disable-jst: 2026-11-13 09:00 = 2026-11-13T09:00+09:00 -->

# 実行先の一時的な無効

「Codex が切れたから Claude にして」などの利用者の指示で、片方の実行先（`codex` か `claude`）を期限付きで無効にし、その実行先の全工程を他方へ移す。専用のScriptは無く、受付APIを既存の窓口から呼ぶ。その他の運用管理は [issue-desk-admin](../issue-desk-admin/SKILL.md) に従う。

例が0以外で終わったとき（4xx・5xx・通信失敗・窓口自身の失敗）と、応答を検査できなかったときは、成功と扱わず、同じ要求を再送しない。そこで停止し、例が標準エラーへ出した拒否理由（`status`・`body`）と未確認の点を利用者に報告する。理由を見直すために同じ例を流し直さない。403は窓口のトークンに運用の代行が許されていない拒否で、権限を追加しない、認証を切り替えない、別の経路で迂回しない。利用者には、受付画面でそのトークンに運用の代行を許すか、受付画面で操作するかを伝える。

## 使う条件と窓口

- 一覧は読取で、利用者が状態を尋ねたときと、設定・取消の前後の確認に使う。設定と取消は変更で、利用者が実行先と操作を指示したときだけ送る。指示が無ければ送らない。
- NASでは `sh ~/.issue-desk/desk-pc`、Ubuntuでは `~/.issue-desk/desk` の `get`（読取）と `operation`（変更）を使う。NASの `~/.issue-desk/desk` はスクリプト名だけを受け付けるので、この操作には使わない。
- 窓口は稼働中の監視の設定（K2）から接続先とPC別トークンを自分で読む。`--config`・接続先・トークンを引数や環境変数で渡さず、トークンの値を引数・ログ・本文・報告に書かない。
- PC別トークンでは、一覧にも設定・取消にも運用の代行の許可が要る。許可は利用者が受付画面でそのトークンに与える。
- 作業ディレクトリはホームの下にする。本文は標準入力（`--body-file -`）で渡す。ファイルで渡すときはホームの下に置く。
- 窓口の終了コードは、成功が0。受付の拒否（HTTPの4xx・5xx）は1で、拒否の内容は標準エラーではなく標準出力に出る。1は窓口自身が受付を呼ぶ前に止まったとき（作業ディレクトリや本文ファイルがホームの外、docker が無い、監視のコンテナや設定を確認できない）にも返り、このときは標準出力が空で理由は標準エラーに出る。受付を呼んだ後の手元・通信の失敗と引数の誤りは2。終了コードだけで受付の拒否と決めず、出力を読む。
- 窓口の標準出力は `{"status":…,"body":…,"bodyType":…,"redacted":…}` で、受付の応答は `body` に入る。下の例は窓口の終了コードと出力を受け取り、終了コード0で形が正しいときだけ `body` を標準出力へ表示する。それ以外は `status`・`body`・`redacted`（解析できない出力はそのまま）を標準エラーへ出して1で終わる。`redacted` が true のときは成功でも標準エラーへその旨を出す。窓口が無いPCではこの例を使わず、[共通接続](../issue-desk-operations/refs/connection.md) と [受付APIの認証](../issue-desk-api/SKILL.md) に従う。

## 入力

| 入力 | 必須 | 既定 | 制約 |
| --- | --- | --- | --- |
| 実行先（経路の `{provider}`） | 設定・取消で必須 | なし | `codex` か `claude`。無効にする側を指定する |
| `until`（設定の本文） | 任意 | 受付が受け取った時点から24時間 | オフセット付きISO日時。受付時点より未来で7日以内 |
| `reason`（設定の本文） | 任意 | なし | 200 UTF-16コード単位以内、制御文字なし |

- 取消の本文は `{}` だけ。一覧に本文は無い。取消と一覧に `until`・`reason` を混ぜない。
- `until` は `2026-11-13T09:00+09:00` のように分まで（秒・ミリ秒も可）とオフセット（`Z` も可）を書く。利用者が日本時間で「2026-11-13 09:00」と言ったら、その日付と時刻が実在することを確かめ、`2026-11-13T09:00+09:00` に直して送る。直した値を利用者の指示と照らし、今より未来で7日以内であることを送る前に確かめる。例の日時は 2026-11-12 09:00（日本時間）に送る想定なので、実行時の日時に置き換える。
- JSONは手で連結せず、下の例のように `JSON.stringify` で作る。理由の文字はそのまま引数に渡し、シェルの単一引用符で囲む。理由に単一引用符（`'`）が入るときは、その1文字を `'\''` と書く（例: `'Codex'\''s limit'`）。
- 期限だけを送るときは `JSON.stringify({until:process.argv[1]})`、理由だけを送るときは `JSON.stringify({reason:process.argv[1]})` にして、引数を1つだけ渡す。使わない項目に空文字を渡さない。空文字の `until` は400で断られ、空文字の `reason` は空の理由として保存される。

## 一覧を読む

`GET /api/provider-disables`。読むだけで、設定は変わらない。例は応答の `disables` が配列であることを確かめてから表示する。`disables` の欠測、JSONでない本文、配列でない値は空一覧と扱わない。未確認として停止し、報告する。

### NAS

```sh
code=0; result=$(sh ~/.issue-desk/desk-pc get --path /api/provider-disables) || code=$?
printf '%s' "$result" | node -e '
const code=process.argv[1],stop=m=>{console.error(m);process.exit(1)},raw=require("fs").readFileSync(0,"utf8");
let r;try{r=JSON.parse(raw)}catch{stop("窓口の終了コード "+code+"。出力が無いかJSONとして解析できない。未確認: "+raw.trim().slice(0,2000))}
const env=r!==null&&typeof r==="object"&&r.bodyType!==undefined,shown=JSON.stringify(env?{status:r.status,body:r.body,redacted:r.redacted}:r);
if(code!=="0")stop("窓口の終了コード "+code+"。受付の拒否または窓口の失敗: "+shown);
const v=env?(r.bodyType==="json"?r.body:null):r;
if(!v||!Array.isArray(v.disables))stop("応答の形を確かめられない。未確認: "+shown);
if(env&&r.redacted)console.error("redacted=true: 応答の一部が伏せられている。伏せられた値は未確認");
console.log(JSON.stringify(v,null,2))' "$code"
```

### Ubuntu

```sh
code=0; result=$(~/.issue-desk/desk get --path /api/provider-disables) || code=$?
printf '%s' "$result" | node -e '
const code=process.argv[1],stop=m=>{console.error(m);process.exit(1)},raw=require("fs").readFileSync(0,"utf8");
let r;try{r=JSON.parse(raw)}catch{stop("窓口の終了コード "+code+"。出力が無いかJSONとして解析できない。未確認: "+raw.trim().slice(0,2000))}
const env=r!==null&&typeof r==="object"&&r.bodyType!==undefined,shown=JSON.stringify(env?{status:r.status,body:r.body,redacted:r.redacted}:r);
if(code!=="0")stop("窓口の終了コード "+code+"。受付の拒否または窓口の失敗: "+shown);
const v=env?(r.bodyType==="json"?r.body:null):r;
if(!v||!Array.isArray(v.disables))stop("応答の形を確かめられない。未確認: "+shown);
if(env&&r.redacted)console.error("redacted=true: 応答の一部が伏せられている。伏せられた値は未確認");
console.log(JSON.stringify(v,null,2))' "$code"
```

## 設定する

`POST /api/provider-disables/{provider}` に、任意の `until`・`reason` だけを持つJSONを送る。期限も理由も無ければ `{}`。例は応答の `disables` が配列で、`set.provider` があることを確かめてから表示する。

結果が不明なとき（通信失敗、窓口の異常終了、応答の検査の失敗）は、同じ要求を再送しない。上の例で一覧を読み直し、`provider`・`until`・`by`・`reason` を送った内容と照合する。期限を指定したときは `until` が送った値と同じであること、期限を省略したときは `setAt` が送信の前後の時刻の範囲に入り、`until` が `setAt`+24時間であることを確かめる。`by` は設定者（窓口が使ったトークンの名前）、`reason` は送った理由で、省略したなら欠けている。照合できなければ未確認として停止し、利用者に報告する。このAPIに送信IDによる照合は無く、一覧の状態だけで要求の受領を証明できないときも未確認とする。期限を省略した設定は送り直すたびに期限が延びるので、自動では送らない。

### NAS（期限と理由を省略）

```sh
code=0; result=$(printf '%s' '{}' | sh ~/.issue-desk/desk-pc operation --method POST --path /api/provider-disables/codex --body-file -) || code=$?
printf '%s' "$result" | node -e '
const code=process.argv[1],stop=m=>{console.error(m);process.exit(1)},raw=require("fs").readFileSync(0,"utf8");
let r;try{r=JSON.parse(raw)}catch{stop("窓口の終了コード "+code+"。出力が無いかJSONとして解析できない。未確認: "+raw.trim().slice(0,2000))}
const env=r!==null&&typeof r==="object"&&r.bodyType!==undefined,shown=JSON.stringify(env?{status:r.status,body:r.body,redacted:r.redacted}:r);
if(code!=="0")stop("窓口の終了コード "+code+"。受付の拒否または窓口の失敗: "+shown);
const v=env?(r.bodyType==="json"?r.body:null):r;
if(!v||!Array.isArray(v.disables)||!v.set||typeof v.set.provider!=="string")stop("応答の形を確かめられない。未確認: "+shown);
if(env&&r.redacted)console.error("redacted=true: 応答の一部が伏せられている。伏せられた値は未確認");
console.log(JSON.stringify(v,null,2))' "$code"
```

### NAS（期限と理由を指定）

```sh
code=0; result=$(node -e 'process.stdout.write(JSON.stringify({until:process.argv[1],reason:process.argv[2]}))' '2026-11-13T09:00+09:00' 'Codexの利用上限のため' |
  sh ~/.issue-desk/desk-pc operation --method POST --path /api/provider-disables/codex --body-file -) || code=$?
printf '%s' "$result" | node -e '
const code=process.argv[1],stop=m=>{console.error(m);process.exit(1)},raw=require("fs").readFileSync(0,"utf8");
let r;try{r=JSON.parse(raw)}catch{stop("窓口の終了コード "+code+"。出力が無いかJSONとして解析できない。未確認: "+raw.trim().slice(0,2000))}
const env=r!==null&&typeof r==="object"&&r.bodyType!==undefined,shown=JSON.stringify(env?{status:r.status,body:r.body,redacted:r.redacted}:r);
if(code!=="0")stop("窓口の終了コード "+code+"。受付の拒否または窓口の失敗: "+shown);
const v=env?(r.bodyType==="json"?r.body:null):r;
if(!v||!Array.isArray(v.disables)||!v.set||typeof v.set.provider!=="string")stop("応答の形を確かめられない。未確認: "+shown);
if(env&&r.redacted)console.error("redacted=true: 応答の一部が伏せられている。伏せられた値は未確認");
console.log(JSON.stringify(v,null,2))' "$code"
```

### Ubuntu（期限と理由を省略）

```sh
code=0; result=$(printf '%s' '{}' | ~/.issue-desk/desk operation --method POST --path /api/provider-disables/claude --body-file -) || code=$?
printf '%s' "$result" | node -e '
const code=process.argv[1],stop=m=>{console.error(m);process.exit(1)},raw=require("fs").readFileSync(0,"utf8");
let r;try{r=JSON.parse(raw)}catch{stop("窓口の終了コード "+code+"。出力が無いかJSONとして解析できない。未確認: "+raw.trim().slice(0,2000))}
const env=r!==null&&typeof r==="object"&&r.bodyType!==undefined,shown=JSON.stringify(env?{status:r.status,body:r.body,redacted:r.redacted}:r);
if(code!=="0")stop("窓口の終了コード "+code+"。受付の拒否または窓口の失敗: "+shown);
const v=env?(r.bodyType==="json"?r.body:null):r;
if(!v||!Array.isArray(v.disables)||!v.set||typeof v.set.provider!=="string")stop("応答の形を確かめられない。未確認: "+shown);
if(env&&r.redacted)console.error("redacted=true: 応答の一部が伏せられている。伏せられた値は未確認");
console.log(JSON.stringify(v,null,2))' "$code"
```

### Ubuntu（期限と理由を指定）

```sh
code=0; result=$(node -e 'process.stdout.write(JSON.stringify({until:process.argv[1],reason:process.argv[2]}))' '2026-11-13T09:00+09:00' 'Claudeの利用上限のため' |
  ~/.issue-desk/desk operation --method POST --path /api/provider-disables/claude --body-file -) || code=$?
printf '%s' "$result" | node -e '
const code=process.argv[1],stop=m=>{console.error(m);process.exit(1)},raw=require("fs").readFileSync(0,"utf8");
let r;try{r=JSON.parse(raw)}catch{stop("窓口の終了コード "+code+"。出力が無いかJSONとして解析できない。未確認: "+raw.trim().slice(0,2000))}
const env=r!==null&&typeof r==="object"&&r.bodyType!==undefined,shown=JSON.stringify(env?{status:r.status,body:r.body,redacted:r.redacted}:r);
if(code!=="0")stop("窓口の終了コード "+code+"。受付の拒否または窓口の失敗: "+shown);
const v=env?(r.bodyType==="json"?r.body:null):r;
if(!v||!Array.isArray(v.disables)||!v.set||typeof v.set.provider!=="string")stop("応答の形を確かめられない。未確認: "+shown);
if(env&&r.redacted)console.error("redacted=true: 応答の一部が伏せられている。伏せられた値は未確認");
console.log(JSON.stringify(v,null,2))' "$code"
```

## 取り消す

`POST /api/provider-disables/{provider}/cancel` に `{}` を送る。例は応答の `disables` が配列で、`cancelled` が真偽値であることを確かめてから表示する。

cancelled=true は有効な無効設定を取り消した、cancelled=false は有効な無効設定がなかった結果で、どちらも要求は届いている。`cancelled` の欠測や真偽値でない値はどちらとも扱わない。未確認として停止し、報告する。結果が不明なときは再送せず、上の例で一覧を読み直して、その実行先が `disables` に残っているかを確かめる。残っていなくても、この取消が届いたのか、元から無かったのか、期限が切れたのかは一覧から区別できないので、`cancelled` の値は未確認と報告する。

### NAS

```sh
code=0; result=$(printf '%s' '{}' | sh ~/.issue-desk/desk-pc operation --method POST --path /api/provider-disables/codex/cancel --body-file -) || code=$?
printf '%s' "$result" | node -e '
const code=process.argv[1],stop=m=>{console.error(m);process.exit(1)},raw=require("fs").readFileSync(0,"utf8");
let r;try{r=JSON.parse(raw)}catch{stop("窓口の終了コード "+code+"。出力が無いかJSONとして解析できない。未確認: "+raw.trim().slice(0,2000))}
const env=r!==null&&typeof r==="object"&&r.bodyType!==undefined,shown=JSON.stringify(env?{status:r.status,body:r.body,redacted:r.redacted}:r);
if(code!=="0")stop("窓口の終了コード "+code+"。受付の拒否または窓口の失敗: "+shown);
const v=env?(r.bodyType==="json"?r.body:null):r;
if(!v||!Array.isArray(v.disables)||typeof v.cancelled!=="boolean")stop("応答の形を確かめられない。未確認: "+shown);
if(env&&r.redacted)console.error("redacted=true: 応答の一部が伏せられている。伏せられた値は未確認");
console.log(JSON.stringify(v,null,2))' "$code"
```

### Ubuntu

```sh
code=0; result=$(printf '%s' '{}' | ~/.issue-desk/desk operation --method POST --path /api/provider-disables/claude/cancel --body-file -) || code=$?
printf '%s' "$result" | node -e '
const code=process.argv[1],stop=m=>{console.error(m);process.exit(1)},raw=require("fs").readFileSync(0,"utf8");
let r;try{r=JSON.parse(raw)}catch{stop("窓口の終了コード "+code+"。出力が無いかJSONとして解析できない。未確認: "+raw.trim().slice(0,2000))}
const env=r!==null&&typeof r==="object"&&r.bodyType!==undefined,shown=JSON.stringify(env?{status:r.status,body:r.body,redacted:r.redacted}:r);
if(code!=="0")stop("窓口の終了コード "+code+"。受付の拒否または窓口の失敗: "+shown);
const v=env?(r.bodyType==="json"?r.body:null):r;
if(!v||!Array.isArray(v.disables)||typeof v.cancelled!=="boolean")stop("応答の形を確かめられない。未確認: "+shown);
if(env&&r.redacted)console.error("redacted=true: 応答の一部が伏せられている。伏せられた値は未確認");
console.log(JSON.stringify(v,null,2))' "$code"
```

## 応答を確かめる

一覧・設定・取消の応答は同じ形の一覧を含む。設定・取消の後は、応答（結果が不明なら一覧の読み直し）で次を確かめて報告する。

- `disables`: 有効な無効設定の配列。`codex`・`claude` それぞれについて、行があれば無効で、`provider`・`until`（期限）・`setAt`（設定時刻）・`by`（設定者）・`reason`（理由。無ければ欠ける）を読む。行が無い実行先は無効の設定が無い。
- `defaultUntil`: 今の時点で期限を省略した場合の期限。`maxDays`・`maxUntil`: 期限の上限（7日）。
- `providers`: 両実行先の準備状態。`ready` が false なら `reason` に理由がある。
- 設定の応答の `set` は今回保存された内容（`provider`・`until`・`setAt`・`by`・`reason`）。取消の応答の `cancelled` は上の節のとおり読む。
- 窓口の出力の `redacted` が true のときは、応答の一部が伏せられている。例は標準エラーへ `redacted=true` と出す。伏せられた値（`[REDACTED]`）は未確認とする。
- 例が0以外で終わった、項目が欠けている、型が違うときは、無効の設定が無いとも設定できたとも報告しない。

## 受付が断る要求

受付は次を400で断る。断られたら理由をそのまま利用者に伝え、値を変えて通そうとしない。

- 両方の実行先を同時に無効にする要求。他方が無効の間は設定できない。
- 移す先の実行先の準備ができていないとき。一覧の `providers` で `ready` と `reason` を読んで伝える。
- 期限が過去・7日超・オフセットの無い日時・実在しない日時、理由が200 UTF-16コード単位超・制御文字入り、実行先が `codex`・`claude` 以外。

移す方向を変える（例: Codex を無効にしていたのを、Claude を無効にする側へ変える）のは、利用者がその指示をしたときだけ。先に他方の取消を送り、応答の `cancelled` が真偽値で、`disables` から他方が消えたことを確かめてから設定を送る。取消の結果が未確認なら設定を送らない。

401・403・409・503と通信失敗も成功と扱わない。403では運用の代行の許可を、409・503では表示された理由を伝え、状態を知る必要があれば一覧を読む。

## 旧Scriptの引数との対応

旧 `provider-disable.mjs` は削除した。旧引数を窓口へそのまま渡すことはできない。窓口が受け取るのは `--method`・`--path`・`--body-file` だけ。

| 旧引数 | 今の指定 |
| --- | --- |
| `--list` | 「一覧を読む」の `get --path /api/provider-disables`。ほかの入力は付けない |
| `--provider codex\|claude` | 経路の `{provider}`。設定・取消で必須 |
| `--until <日時>` | 設定の本文の `until`。任意、省略時24時間、上限7日。日本時間は自分で `+09:00` 付きへ直す（旧Scriptの自動変換は無い） |
| `--reason <理由>` | 設定の本文の `reason`。任意 |
| `--cancel --provider <実行先>` | 「取り消す」の `/cancel` 経路に `{}`。`until`・`reason` は付けない |
| `--url`・`--token-file`、環境変数 `ISSUE_DESK_URL`・`ISSUE_DESK_TOKEN_FILE` | 指定しない。窓口がK2の接続先とPC別トークンを読む。窓口の `get`・`operation` はこれらの環境変数を使わない |
