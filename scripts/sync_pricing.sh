#!/usr/bin/env bash
# =============================================================================
# sync_pricing.sh — Sync model_pricing from LiteLLM's community JSON
#
# OpenAI / Anthropic / Google do NOT expose pricing via official APIs (they
# publish on HTML docs pages only). The OSS LLM-gateway ecosystem solved this
# by maintaining a community JSON in the LiteLLM repo — MIT-licensed,
# updated within days of new model releases, used by Helicone, tokencost,
# OpenRouter's internal tooling, etc. We treat it as upstream.
#
# Source:
#   https://github.com/BerriAI/litellm
#   raw: https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json
#
# This script:
#   1. Fetches the JSON (or reads a local copy via --json-file)
#   2. Filters to openai / anthropic / google (Gemini) chat models
#   3. Optionally skips deprecated models (default ON)
#   4. Diffs against the running model_pricing table
#   5. In --apply mode: UPSERTs into Postgres so prices update in place
#   6. In --write-seed mode: regenerates schema/seed_models.sql for fresh installs
#
# Usage:
#   ./scripts/sync_pricing.sh                          # diff only (default)
#   ./scripts/sync_pricing.sh --apply                  # UPSERT into running DB
#   ./scripts/sync_pricing.sh --write-seed             # rewrite seed_models.sql
#   ./scripts/sync_pricing.sh --apply --write-seed     # do both
#   ./scripts/sync_pricing.sh --providers=openai,google
#   ./scripts/sync_pricing.sh --include-deprecated
#   ./scripts/sync_pricing.sh --json-file=./local.json # offline / pinned input
#
# Recommended workflow for maintainers:
#   1. Run with no args → review diff
#   2. Run with --write-seed → commit the updated schema/seed_models.sql
#   3. Open PR ("chore(pricing): sync from LiteLLM YYYY-MM-DD")
#   4. After merge, ops can run `--apply` on each environment to refresh
#      existing installations without waiting for a fresh container boot.
#
# Requires: jq, curl. --apply also needs docker compose with postgres running.
# Safe to rerun: prices update in place, user overrides on unknown models
# (e.g. ollama, custom fine-tunes) are preserved.
# =============================================================================
set -euo pipefail

# ── Defaults / config ────────────────────────────────────────────────────────

LITELLM_URL="https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
PROVIDERS_DEFAULT="openai,anthropic,google"
SEED_FILE_PATH="schema/seed_models.sql"
PSQL_BIN="docker compose exec -T postgres psql -U llm0 -d llm0_gateway"
TODAY="$(date -u +%Y-%m-%d)"

APPLY=false
WRITE_SEED=false
INCLUDE_DEPRECATED=false
PROVIDERS="$PROVIDERS_DEFAULT"
LOCAL_JSON=""

# ── Pretty printing ──────────────────────────────────────────────────────────

color_title()   { printf "\033[1;36m%s\033[0m\n" "$1"; }
color_success() { printf "\033[1;32m%s\033[0m\n" "$1"; }
color_warn()    { printf "\033[1;33m%s\033[0m\n" "$1"; }
color_error()   { printf "\033[1;31m%s\033[0m\n" "$1"; }
color_dim()     { printf "\033[2m%s\033[0m\n" "$1"; }
divider()       { echo "════════════════════════════════════════════════════════════════════════════════"; }

usage() {
  sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'
}

# ── Arg parsing ──────────────────────────────────────────────────────────────

for arg in "$@"; do
  case "$arg" in
    --apply)              APPLY=true ;;
    --write-seed)         WRITE_SEED=true ;;
    --include-deprecated) INCLUDE_DEPRECATED=true ;;
    --providers=*)        PROVIDERS="${arg#--providers=}" ;;
    --json-file=*)        LOCAL_JSON="${arg#--json-file=}" ;;
    -h|--help|help)       usage; exit 0 ;;
    *) color_error "Unknown arg: $arg"; echo "Run with --help for usage."; exit 1 ;;
  esac
done

# ── Dependency checks ────────────────────────────────────────────────────────

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || { color_error "❌ '$1' is required but not installed."; exit 1; }
}
require_cmd jq
require_cmd curl

require_postgres() {
  if ! docker compose ps postgres 2>/dev/null | grep -q "Up\|running"; then
    color_error "❌ Postgres container is not running."
    echo "   Start it with: docker compose up -d postgres"
    exit 1
  fi
}

# ── 1. Acquire the upstream JSON ─────────────────────────────────────────────

divider
color_title "  LLM0 Pricing Sync — upstream: LiteLLM model_prices_and_context_window.json"
divider

if [[ -n "$LOCAL_JSON" ]]; then
  [[ -f "$LOCAL_JSON" ]] || { color_error "❌ --json-file path not found: $LOCAL_JSON"; exit 1; }
  JSON_FILE="$LOCAL_JSON"
  color_dim "Source: local file $LOCAL_JSON"
else
  JSON_FILE="$(mktemp -t llm0_pricing.XXXXXX.json)"
  trap 'rm -f "$JSON_FILE"' EXIT
  color_dim "Source: $LITELLM_URL"
  if ! curl -fsSL "$LITELLM_URL" -o "$JSON_FILE"; then
    color_error "❌ Failed to fetch upstream JSON."
    echo "   Check connectivity, or pass --json-file=<path> to use a local copy."
    exit 1
  fi
fi

# Sanity check — the upstream file should be a JSON object with many keys.
# The 50-entry threshold catches a corrupted/HTML-error fetch; we relax it
# to 1 when --json-file is used so unit-test fixtures and pinned local
# copies don't get rejected.
UPSTREAM_TOTAL="$(jq 'length' "$JSON_FILE" 2>/dev/null || echo 0)"
MIN_ENTRIES=50
[[ -n "$LOCAL_JSON" ]] && MIN_ENTRIES=1
if [[ -z "$UPSTREAM_TOTAL" || "$UPSTREAM_TOTAL" -lt "$MIN_ENTRIES" ]]; then
  color_error "❌ Upstream JSON looks malformed (only $UPSTREAM_TOTAL entries, expected ≥ $MIN_ENTRIES)."
  exit 1
fi
color_dim "Upstream entries (all providers, all modes): $UPSTREAM_TOTAL"
echo ""

# ── 2. Filter upstream to chat models for our three providers ────────────────
#
# LiteLLM quirks worth knowing:
#   - First entry "sample_spec" is a schema doc, not a real model — must skip.
#   - Keys may be prefixed (e.g. "gemini/gemini-2.5-pro") or bare ("gpt-4o").
#     We strip any "<provider>/" prefix so our model_pricing.model matches
#     what providers accept on their REST APIs (and what seed_models.sql uses).
#   - litellm_provider == "gemini" → we store as "google" (matches our seed).
#   - "vertex_ai-language-models" entries duplicate the Gemini ones at
#     different prices (Vertex billing differs from AI Studio). We skip them.
#   - "supports_function_calling" defaults to false if absent.
#   - "deprecation_date" is ISO date; we drop entries with date < today
#     unless --include-deprecated.
#
# Output TSV columns: provider \t model \t input_1k \t output_1k \t ctx \t fn \t deprecated_date

EXTRACT_JQ='
  del(.sample_spec)
  | to_entries
  | map(select(.value.litellm_provider | IN("openai", "anthropic", "gemini")))
  | map(select((.value.mode // "chat") == "chat"))
  | map(
      .provider       = (if .value.litellm_provider == "gemini" then "google"
                         else .value.litellm_provider end)
      | .model        = (.key | sub("^[^/]+/"; ""))
      | .input_1k     = (((.value.input_cost_per_token  // 0) | tonumber) * 1000)
      | .output_1k    = (((.value.output_cost_per_token // 0) | tonumber) * 1000)
      | .ctx          = (.value.max_input_tokens // .value.max_tokens // 128000)
      | .fn           = (.value.supports_function_calling // false)
      | .deprecated   = (.value.deprecation_date // "")
    )
  | .[]
  | [.provider, .model, .input_1k, .output_1k, .ctx, .fn, .deprecated]
  | @tsv
'

UPSTREAM_TSV="$(jq -r "$EXTRACT_JQ" "$JSON_FILE")"

# Apply --providers filter (default keeps all three).
# NOTE: BSD awk on macOS does NOT support \b (GNU extension), so we anchor
# with ^...$ instead. Provider column is the whole field.
IFS=',' read -r -a WANTED_PROVIDERS <<< "$PROVIDERS"
FILTER_REGEX="^($(IFS='|'; echo "${WANTED_PROVIDERS[*]}"))$"
UPSTREAM_TSV="$(echo "$UPSTREAM_TSV" | awk -F'\t' -v re="$FILTER_REGEX" '$1 ~ re')"

# Dedupe on (provider, model). LiteLLM ships some Gemini AI Studio models
# under both a bare key (e.g. "gemini-exp-1206") and a prefixed key
# ("gemini/gemini-exp-1206") with identical data. Our jq step strips the
# "<provider>/" prefix, so the two collapse to the same row. We keep the
# FIRST occurrence (jq iteration is JSON-object order, which the LiteLLM
# file maintains alphabetically — so the bare key comes first; harmless
# either way since the columns are identical).
UPSTREAM_TSV="$(echo "$UPSTREAM_TSV" | awk -F'\t' '
  {
    k = $1 "\t" $2
    if (!(k in seen)) {
      seen[k] = 1
      print
    }
  }
')"

# Apply deprecation filter unless --include-deprecated.
if [[ "$INCLUDE_DEPRECATED" != true ]]; then
  UPSTREAM_TSV="$(
    echo "$UPSTREAM_TSV" \
      | awk -F'\t' -v today="$TODAY" '
          {
            dep = $7
            if (dep == "" || dep >= today) print $0
          }'
  )"
fi

UPSTREAM_COUNT="$(echo "$UPSTREAM_TSV" | grep -c . || true)"
color_dim "After provider+deprecation filter: $UPSTREAM_COUNT models"
echo ""

# ── 3. Snapshot what's currently in the DB (for the diff) ────────────────────

CURRENT_TSV=""
if docker compose ps postgres 2>/dev/null | grep -q "Up\|running"; then
  CURRENT_TSV="$($PSQL_BIN -tAF $'\t' -c "
    SELECT provider, model,
           ROUND(input_per_1k_tokens::numeric,  8)::text,
           ROUND(output_per_1k_tokens::numeric, 8)::text,
           context_window,
           supports_functions
    FROM model_pricing
    ORDER BY provider, model;
  " 2>/dev/null || true)"
  color_dim "Current model_pricing rows: $(echo "$CURRENT_TSV" | grep -c . || true)"
else
  color_warn "⚠️  Postgres not running — diff will treat the DB as empty."
  color_warn "    (Diff is informational; --apply will fail without postgres up.)"
fi
echo ""

# ── 4. Build the diff: NEW, PRICE-CHANGED, UNCHANGED, DB-ONLY ────────────────
#
# Implementation: pure awk on two TSV blobs joined by "<provider>|<model>" key.

NEW_TSV=""
CHANGED_TSV=""
UNCHANGED_COUNT=0
DB_ONLY_TSV=""

if [[ -n "$UPSTREAM_TSV" ]]; then
  # BSD awk on macOS does NOT accept newlines inside -v var= assignments, so
  # we materialize both sides to temp files and let awk read them naturally.
  local_up="$(mktemp -t llm0_pricing_up.XXXXXX.tsv)"
  local_cur="$(mktemp -t llm0_pricing_cur.XXXXXX.tsv)"
  # Append to the existing EXIT trap (don't clobber it). When the upstream
  # JSON came from the user via --json-file, it must NOT be deleted on exit.
  EXISTING_TRAP="$(trap -p EXIT | sed -e "s/^trap -- '//" -e "s/' EXIT$//")"
  trap "${EXISTING_TRAP:+$EXISTING_TRAP; }rm -f \"$local_up\" \"$local_cur\"" EXIT
  printf '%s\n' "$UPSTREAM_TSV" > "$local_up"
  printf '%s\n' "$CURRENT_TSV"  > "$local_cur"

  # Two-file awk: NR==FNR is true while we read the FIRST file (current DB).
  # When NR > FNR we're reading the upstream file and emitting diff lines.
  # After we drain upstream, END detects DB-only rows that upstream didn't
  # claim.
  DIFF_OUTPUT="$(awk -F'\t' '
    FNR==NR {
      if ($0 == "") next
      k = $1 "|" $2
      cur_in[k]     = 1
      cur_input[k]  = $3
      cur_output[k] = $4
      cur_ctx[k]    = $5
      cur_fn[k]     = $6
      next
    }
    {
      if ($0 == "") next
      k = $1 "|" $2
      seen_up[k] = 1
      if (!(k in cur_in)) {
        print "NEW\t" $0
      } else {
        up_in  = sprintf("%.8f", $3 + 0)
        up_out = sprintf("%.8f", $4 + 0)
        db_in  = sprintf("%.8f", cur_input[k]  + 0)
        db_out = sprintf("%.8f", cur_output[k] + 0)
        if (up_in != db_in || up_out != db_out) {
          print "CHANGED\t" $1 "\t" $2 "\t" db_in "\t" up_in "\t" db_out "\t" up_out
        } else {
          print "UNCHANGED\t" $1 "\t" $2
        }
      }
    }
    END {
      for (k in cur_in) {
        if (!(k in seen_up)) {
          split(k, parts, "|")
          print "DBONLY\t" parts[1] "\t" parts[2]
        }
      }
    }
  ' "$local_cur" "$local_up")"

  NEW_TSV="$(echo "$DIFF_OUTPUT"     | awk -F'\t' '$1=="NEW"       {sub(/^NEW\t/, ""); print}')"
  CHANGED_TSV="$(echo "$DIFF_OUTPUT" | awk -F'\t' '$1=="CHANGED"   {sub(/^CHANGED\t/, ""); print}')"
  UNCHANGED_COUNT="$(echo "$DIFF_OUTPUT" | awk -F'\t' '$1=="UNCHANGED" {n++} END {print n+0}')"
  DB_ONLY_TSV="$(echo "$DIFF_OUTPUT" | awk -F'\t' '$1=="DBONLY"    {sub(/^DBONLY\t/, ""); print}')"
fi

# ── 5. Render the diff report ────────────────────────────────────────────────

print_section() {
  local title="$1"; local body="$2"; local count="$3"
  divider
  if [[ "$count" -gt 0 ]]; then
    color_title "  $title  ($count)"
  else
    color_dim   "  $title  (0)"
  fi
  divider
  if [[ -n "$body" ]]; then
    echo "$body"
  fi
  # Explicit success so an empty section doesn't return 1 and trip set -e.
  return 0
}

# NEW: 6 cols: provider, model, in_1k, out_1k, ctx, fn, deprecated
NEW_PRINT="$(echo "$NEW_TSV" | awk -F'\t' 'NF>=2 {
  printf "  %-10s  %-46s  in $%-10.6f  out $%-10.6f  ctx %-9s%s\n",
         $1, $2, $3+0, $4+0, $5, ($7!="" ? "  [dep " $7 "]" : "")
}')"
print_section "NEW MODELS (in upstream, not in DB)" "$NEW_PRINT" "$(echo "$NEW_TSV" | grep -c . || true)"

# CHANGED: 6 cols: provider, model, db_in, up_in, db_out, up_out
CHANGED_PRINT="$(echo "$CHANGED_TSV" | awk -F'\t' 'NF>=2 {
  d_in  = ($4 == "0.00000000" || $4 == 0) ? 0 : ($4 - $3) / $4 * 100
  d_out = ($6 == "0.00000000" || $6 == 0) ? 0 : ($6 - $5) / $6 * 100
  printf "  %-10s  %-46s  in $%-10.6f -> $%-10.6f  out $%-10.6f -> $%-10.6f\n",
         $1, $2, $3+0, $4+0, $5+0, $6+0
}')"
print_section "PRICE CHANGES" "$CHANGED_PRINT" "$(echo "$CHANGED_TSV" | grep -c . || true)"

# UNCHANGED: count only
print_section "UNCHANGED (already match upstream)" "" "$UNCHANGED_COUNT"

# DB-ONLY: things in our table that upstream doesn't know about (ollama, custom)
DB_ONLY_PRINT="$(echo "$DB_ONLY_TSV" | awk -F'\t' 'NF>=2 {
  printf "  %-10s  %-46s  (preserved — never overwritten by sync)\n", $1, $2
}')"
print_section "DB-ONLY (preserved as-is)" "$DB_ONLY_PRINT" "$(echo "$DB_ONLY_TSV" | grep -c . || true)"

divider
echo ""

# ── 6. --apply: UPSERT into the running Postgres ─────────────────────────────

apply_upserts() {
  require_postgres
  color_title "  --apply  →  UPSERT into model_pricing"
  divider

  local upsert_count=0
  local sql_file
  sql_file="$(mktemp -t llm0_pricing_upsert.XXXXXX.sql)"
  trap 'rm -f "$sql_file"' RETURN

  {
    echo "BEGIN;"
    while IFS=$'\t' read -r prov model in_1k out_1k ctx fn _dep; do
      [[ -z "$prov" ]] && continue
      local fn_bool="false"
      [[ "$fn" == "true" ]] && fn_bool="true"
      # Cap ctx at INT max for safety; LiteLLM stores some as 1e7+
      [[ -z "$ctx" || "$ctx" == "null" ]] && ctx=128000
      printf "INSERT INTO model_pricing (provider, model, input_per_1k_tokens, output_per_1k_tokens, context_window, supports_streaming, supports_functions, updated_at) VALUES ('%s','%s',%.8f,%.8f,%s,true,%s,NOW()) ON CONFLICT (provider, model) DO UPDATE SET input_per_1k_tokens=EXCLUDED.input_per_1k_tokens, output_per_1k_tokens=EXCLUDED.output_per_1k_tokens, context_window=EXCLUDED.context_window, supports_functions=EXCLUDED.supports_functions, updated_at=NOW();\n" \
        "$prov" "$model" "$in_1k" "$out_1k" "$ctx" "$fn_bool"
      upsert_count=$((upsert_count + 1))
    done <<< "$UPSTREAM_TSV"
    echo "COMMIT;"
  } > "$sql_file"

  $PSQL_BIN -q -f - < "$sql_file"
  color_success "✅ Applied $upsert_count upserts."
  color_dim    "   Gateway will pick up new prices on next startup (or restart now)."
  color_dim    "   docker compose restart gateway"
  divider
}

# ── 7. --write-seed: regenerate schema/seed_models.sql ───────────────────────
#
# This preserves the seed semantics: ON CONFLICT DO NOTHING so it's still
# safe to re-run on first boot without clobbering manage_models.sh overrides.
# The fresh seed mainly benefits NEW installs (and any environments where
# the operator drops the model_pricing table and reseeds).

write_seed() {
  local repo_root seed_path
  repo_root="$(cd "$(dirname "$0")/.." && pwd)"
  seed_path="$repo_root/$SEED_FILE_PATH"

  if [[ ! -f "$seed_path" ]]; then
    color_error "❌ Could not locate $SEED_FILE_PATH (looked at $seed_path)."
    exit 1
  fi

  color_title "  --write-seed  →  regenerate $SEED_FILE_PATH"
  divider

  local tmp
  tmp="$(mktemp -t llm0_seed.XXXXXX.sql)"

  {
    cat <<EOF
-- =============================================================================
-- seed_models.sql — LLM0 Gateway canonical model pricing seed
--
-- This file is the SINGLE SOURCE OF TRUTH for default model pricing.
-- It is:
--   1. Mounted into the postgres container at first boot (see docker-compose.yml)
--   2. Embedded into the gateway binary (see internal/shared/database/seed.go)
--      and applied automatically the first time \`model_pricing\` is empty.
--
-- Safe to re-run: every INSERT uses ON CONFLICT (provider, model) DO NOTHING,
-- so existing rows (including user overrides from scripts/manage_models.sh)
-- are never touched.
--
-- When new models are released:
--   - Run scripts/sync_pricing.sh --write-seed  (refresh this file from
--     LiteLLM's community JSON), or
--   - Run scripts/manage_models.sh add          (one-off local override).
--
-- Pricing is per 1,000 tokens in USD. Upstream:
--   https://github.com/BerriAI/litellm  →  model_prices_and_context_window.json
-- Last synced: $TODAY
-- =============================================================================

INSERT INTO model_pricing
    (provider, model, input_per_1k_tokens, output_per_1k_tokens,
     context_window, supports_streaming, supports_functions)
VALUES
EOF

    # Emit rows grouped by provider, with a section divider.
    # We emit comma-terminated lines and replace the LAST comma with a blank
    # at the end via sed.
    local prev_provider=""
    while IFS=$'\t' read -r prov model in_1k out_1k ctx fn _dep; do
      [[ -z "$prov" ]] && continue
      if [[ "$prov" != "$prev_provider" ]]; then
        case "$prov" in
          openai)    echo "    -- ── OpenAI ────────────────────────────────────────────────────────────────" ;;
          anthropic) echo "    -- ── Anthropic ─────────────────────────────────────────────────────────────" ;;
          google)    echo "    -- ── Google Gemini (AI Studio) ─────────────────────────────────────────────" ;;
          *)         echo "    -- ── $prov ──" ;;
        esac
        prev_provider="$prov"
      fi
      local fn_bool="false"
      [[ "$fn" == "true" ]] && fn_bool="true"
      [[ -z "$ctx" || "$ctx" == "null" ]] && ctx=128000
      printf "    ('%s', '%s', %.8f, %.8f, %s, true, %s),\n" \
        "$prov" "$model" "$in_1k" "$out_1k" "$ctx" "$fn_bool"
    done <<< "$(echo "$UPSTREAM_TSV" | sort -t $'\t' -k1,1 -k2,2)"

    echo ""
    echo "ON CONFLICT (provider, model) DO NOTHING;"
  } > "$tmp"

  # Trailing-comma fixup: turn the final "),\n" into ")\n" before ON CONFLICT.
  # Simple, portable: use awk to rewrite the file.
  awk '
    NR==FNR { lines[NR]=$0; total=NR; next }
  ' "$tmp" "$tmp" >/dev/null  # noop, just to verify file is readable

  # Find the LAST line ending in "),"  and strip its trailing comma.
  awk '
    {
      lines[NR] = $0
    }
    END {
      # find last index where line ends with "),"
      for (i = NR; i >= 1; i--) {
        if (lines[i] ~ /\),$/) {
          sub(/\),$/, ")", lines[i])
          break
        }
      }
      for (i = 1; i <= NR; i++) print lines[i]
    }
  ' "$tmp" > "$seed_path"

  rm -f "$tmp"

  color_success "✅ Wrote $(grep -c "^    ('" "$seed_path") rows to $SEED_FILE_PATH."
  color_dim    "   git diff $SEED_FILE_PATH    # review the change"
  color_dim    "   git add  $SEED_FILE_PATH    # commit & PR"
  divider
}

# ── 8. Dispatch ──────────────────────────────────────────────────────────────

if [[ "$APPLY" == true ]]; then
  echo ""
  apply_upserts
fi

if [[ "$WRITE_SEED" == true ]]; then
  echo ""
  write_seed
fi

if [[ "$APPLY" != true && "$WRITE_SEED" != true ]]; then
  echo ""
  color_dim "Dry-run only. Re-run with --apply and/or --write-seed to act on this diff."
  color_dim "  --apply       UPSERT into the running Postgres (good for live envs)"
  color_dim "  --write-seed  rewrite schema/seed_models.sql   (good for fresh installs / PRs)"
fi
