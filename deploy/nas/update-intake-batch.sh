# update.sh と deploy-server.sh が読み込む。drain の再試行間隔とは独立した成功後の間隔。
intake_batch_wait() {
  BATCH_ADVANCE=0
  [ "$INTAKE" = 1 ] || return 1
  batch_file=$DIR/deploy/nas/.update-intake-last-success
  batch_reason=missing
  batch_line=''
  batch_extra=''
  batch_tail=0
  if [ -r "$batch_file" ]; then
    {
      IFS= read -r batch_line || :
      if IFS= read -r batch_extra || [ -n "$batch_extra" ]; then batch_tail=1; fi
    } <"$batch_file"
    batch_reason=invalid
    batch_revision=${batch_line%% *}
    batch_at=${batch_line#* }
    case "$batch_revision" in '' | *[!0-9a-f]*) batch_line='' ;; esac
    case "$batch_at" in '' | 0* | *[!0-9]*) batch_line='' ;; esac
    if [ -n "$batch_line" ] && [ "$batch_tail" = 0 ] &&
      [ "${#batch_revision}" = 40 ] && [ "${#batch_at}" -le 15 ] &&
      [ "$batch_line" = "$batch_revision $batch_at" ]; then
      batch_now=$(now_ms)
      batch_reason=future
      if [ "$batch_at" -le "$batch_now" ]; then
        # 稼働版の検証も含む案1の判定。欠測やGit失敗を待機理由にしない。
        if intake_unchanged && [ "$INTAKE_BASE" = "$batch_revision" ]; then return 1; fi
        batch_reason=revision-mismatch
        if [ "$INTAKE_REASON" = impact ] && [ "$INTAKE_BASE" = "$batch_revision" ]; then
          batch_now=$(now_ms)
          batch_next=$((batch_at + 1800000))
          if [ "${INTAKE_NOW:-0}" = 1 ]; then
            say "[update:intake-batch:bypass] base=$INTAKE_BASE target=$TARGET next=$batch_next reason=--intake-now（追加待機だけを明示省略）"
            return 1
          fi
          if [ "$batch_now" -lt "$batch_next" ]; then
            batch_scope='受付・監視・発行を一緒に待機（取り決めの版が不一致または比較不能）'
            if intake_batch_contract_matches; then
              BATCH_ADVANCE=1
              batch_scope='受付だけ待機（取り決めの版が一致。監視・発行は先行）'
            fi
            batch_date=$(TZ=JST-9 date -d "@$((batch_next / 1000))" '+%Y-%m-%d %H:%M')
            say "[update:skipped:intake-batch] base=$INTAKE_BASE target=$TARGET next=$batch_next reason=last-success 受付の入れ替えを $batch_date まで待つ JST（UTC+09:00）。$batch_scope"
            return 0
          fi
          say "[update:intake-batch:ready] base=$INTAKE_BASE target=$TARGET next=$batch_next reason=interval-elapsed"
          return 1
        fi
      fi
    fi
  fi
  say "[update:intake-batch:ignored] target=$TARGET reason=$batch_reason（成功記録を待機理由に使わず通常更新）"
  return 1
}

# Gitの生成物をデータとして読む。定義が複数・非標準・不正なら先行更新しない。
intake_contract_version() {
  contract_text=$(git show "$1:scripts/lib/desk-contract.generated.ts" 2>/dev/null) || return 1
  printf '%s\n' "$contract_text" | awk '
    { rest=$0; while (match(rest, /deskContractVersion[[:space:]]*=/)) {
        definitions++; rest=substr(rest, RSTART+RLENGTH)
      }
      # テンプレート文字列やコメント内の見かけの定義は採用しない。
      if (index($0, "`")) uncertain=1
      if (index($0, "/*")) block=1
      if (!block && $0 ~ /^export const deskContractVersion = '\''[0-9a-f]+'\'';$/) {
        value=$0; sub(/^export const deskContractVersion = '\''/, "", value); sub(/'\'';$/, "", value)
        if (length(value) == 64) valid++
      }
      if (index($0, "*/")) block=0
    }
    END { if (!uncertain && !block && definitions == 1 && valid == 1) print value; else exit 1 }
  '
}

intake_batch_contract_matches() {
  contract_base=$(intake_contract_version "$INTAKE_BASE") || return 1
  contract_target=$(intake_contract_version "$TARGET") || return 1
  [ "$contract_base" = "$contract_target" ]
}

# 完全な通常交代の末尾だけが呼ぶ。保存失敗で交代の成功を取り消さない。
record_intake_success() {
  batch_file=$DIR/deploy/nas/.update-intake-last-success
  if ! { printf '%s %s\n' "$EXPECT" "$(now_ms)" >"$batch_file.tmp" && mv -f "$batch_file.tmp" "$batch_file"; } 2>/dev/null; then
    rm -f "$batch_file.tmp" 2>/dev/null || :
    say '[server:success-record-failed] 交代は成功しましたが成功revision・時刻を保存できません。次周期は通常更新します'
  fi
  return 0
}
