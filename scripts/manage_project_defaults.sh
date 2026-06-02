#!/usr/bin/env bash
# =============================================================================
# manage_project_defaults.sh — set the per-customer DEFAULT limits a project
# applies to EVERY customer that doesn't have a more specific tier or
# override. This is the OSS equivalent of "set a $5/day cap on every user
# of my SaaS, without inserting a row per user."
#
# Targets the projects.default_* columns added in Slice A.
# See plans/customer-limits-tiers.md.
#
# Usage:
#   ./scripts/manage_project_defaults.sh                   # interactive menu
#   ./scripts/manage_project_defaults.sh list
#   ./scripts/manage_project_defaults.sh show
#   ./scripts/manage_project_defaults.sh set
#   ./scripts/manage_project_defaults.sh clear
#
# Requires: docker compose is running (postgres container).
# =============================================================================
set -euo pipefail

PSQL_BIN="docker compose exec -T postgres psql -U llm0 -d llm0_gateway"
psql_run() { $PSQL_BIN "$@" </dev/null; }
PSQL="psql_run"

color_title()   { printf "\033[1;36m%s\033[0m\n" "$1"; }
color_success() { printf "\033[1;32m%s\033[0m\n" "$1"; }
color_warn()    { printf "\033[1;33m%s\033[0m\n" "$1"; }
color_error()   { printf "\033[1;31m%s\033[0m\n" "$1"; }
color_dim()     { printf "\033[2m%s\033[0m\n" "$1"; }
divider()       { echo "════════════════════════════════════════════════════════════════════════════════"; }

require_postgres() {
  if ! docker compose ps postgres 2>/dev/null | grep -q "Up\|running"; then
    color_error "❌ Postgres container is not running."
    echo "   Start it with: docker compose up -d postgres"
    exit 1
  fi
}

# ── helpers ──────────────────────────────────────────────────────────────────

# Accept "" (no change), "null" (clear column), or a non-negative number.
# Echoes the SQL fragment to use: NULL | <number> | __SKIP__.
read_money_or_skip() {
  local prompt="$1"
  local var
  read -rp "$prompt" var
  if [[ -z "$var" ]];     then echo "__SKIP__"; return; fi
  if [[ "$var" == "null" || "$var" == "NULL" ]]; then echo "NULL"; return; fi
  if [[ "$var" =~ ^[0-9]+(\.[0-9]+)?$ ]];        then echo "$var"; return; fi
  color_error "Invalid amount: $var"; exit 1
}

read_int_or_skip() {
  local prompt="$1"
  local var
  read -rp "$prompt" var
  if [[ -z "$var" ]];     then echo "__SKIP__"; return; fi
  if [[ "$var" == "null" || "$var" == "NULL" ]]; then echo "NULL"; return; fi
  if [[ "$var" =~ ^[0-9]+$ ]];                   then echo "$var"; return; fi
  color_error "Invalid integer: $var"; exit 1
}

# ── commands ────────────────────────────────────────────────────────────────

cmd_list() {
  divider; color_title "  Projects (with current defaults)"; divider
  $PSQL -c "SELECT
              id,
              name,
              default_daily_spend_limit_usd   AS d_daily,
              default_monthly_spend_limit_usd AS d_monthly,
              default_per_request_max_usd     AS d_per_req,
              default_requests_per_minute     AS d_rpm,
              default_requests_per_hour       AS d_rph,
              default_requests_per_day        AS d_rpd,
              default_on_limit_behavior       AS d_behavior,
              default_downgrade_model         AS d_downgrade
            FROM projects
            ORDER BY name;"
}

cmd_show() {
  cmd_list
  echo ""
  local project_id
  read -rp "Project ID to inspect: " project_id
  [[ -z "$project_id" ]] && { color_error "Project ID required."; exit 1; }
  divider; color_title "  Default limits for ${project_id}"; divider

  local count
  count=$($PSQL -t -A -c "SELECT COUNT(DISTINCT customer_id)
                          FROM customer_spend
                          WHERE project_id = '${project_id}'
                            AND date >= CURRENT_DATE - INTERVAL '30 days';" \
          | tr -d '[:space:]')
  echo ""
  color_dim "Customers seen in the last 30 days: ${count}"
  color_dim "Any default you set will apply to ALL ${count} of them."
}

cmd_set() {
  cmd_list
  echo ""
  local project_id
  read -rp "Project ID: " project_id
  [[ -z "$project_id" ]] && { color_error "Project ID required."; exit 1; }

  local count
  count=$($PSQL -t -A -c "SELECT COUNT(DISTINCT customer_id)
                          FROM customer_spend
                          WHERE project_id = '${project_id}'
                            AND date >= CURRENT_DATE - INTERVAL '30 days';" \
          | tr -d '[:space:]')
  echo ""
  if [[ "${count:-0}" != "0" ]]; then
    color_warn "⚠  This will affect ${count} customer(s) seen in the last 30 days."
  else
    color_dim "No customers tracked yet; defaults will apply once requests arrive."
  fi
  echo ""
  color_dim "For each field: <number> = set, 'null' = clear, blank = leave unchanged."
  echo ""

  local daily monthly per_req rpm rph rpd behavior downgrade
  daily=$(read_money_or_skip   "default_daily_spend_limit_usd   : ")
  monthly=$(read_money_or_skip "default_monthly_spend_limit_usd : ")
  per_req=$(read_money_or_skip "default_per_request_max_usd     : ")
  rpm=$(read_int_or_skip       "default_requests_per_minute     : ")
  rph=$(read_int_or_skip       "default_requests_per_hour       : ")
  rpd=$(read_int_or_skip       "default_requests_per_day        : ")

  echo ""
  echo "default_on_limit_behavior options: block | downgrade | warn"
  read -rp "default_on_limit_behavior (blank = unchanged): " behavior
  if [[ -n "$behavior" && "$behavior" != "block" && "$behavior" != "downgrade" && "$behavior" != "warn" ]]; then
    color_error "Invalid behavior. Must be: block | downgrade | warn."
    exit 1
  fi

  read -rp "default_downgrade_model (blank = unchanged, 'null' to clear): " downgrade

  local sets=()
  [[ "$daily"    != "__SKIP__" ]] && sets+=("default_daily_spend_limit_usd   = ${daily}")
  [[ "$monthly"  != "__SKIP__" ]] && sets+=("default_monthly_spend_limit_usd = ${monthly}")
  [[ "$per_req"  != "__SKIP__" ]] && sets+=("default_per_request_max_usd     = ${per_req}")
  [[ "$rpm"      != "__SKIP__" ]] && sets+=("default_requests_per_minute     = ${rpm}")
  [[ "$rph"      != "__SKIP__" ]] && sets+=("default_requests_per_hour       = ${rph}")
  [[ "$rpd"      != "__SKIP__" ]] && sets+=("default_requests_per_day        = ${rpd}")
  [[ -n "$behavior" ]]            && sets+=("default_on_limit_behavior       = '${behavior}'")
  if [[ "$downgrade" == "null" || "$downgrade" == "NULL" ]]; then
    sets+=("default_downgrade_model = NULL")
  elif [[ -n "$downgrade" ]]; then
    sets+=("default_downgrade_model = '${downgrade}'")
  fi

  if [[ ${#sets[@]} -eq 0 ]]; then
    color_warn "No changes requested — skipping."
    exit 0
  fi

  local set_clause
  set_clause=$(IFS=', '; echo "${sets[*]}")

  local updated
  updated=$($PSQL -t -A -c "UPDATE projects
                            SET ${set_clause}, updated_at = NOW()
                            WHERE id = '${project_id}'
                            RETURNING id;" | tr -d '[:space:]')

  if [[ -z "$updated" ]]; then
    color_error "❌ No project found with id '${project_id}'."
    exit 1
  fi
  color_success "✅ Project defaults updated."
  color_dim "   Changes apply to new requests after the API key cache expires"
  color_dim "   (~${CACHE_TTL_HINT:-60}s). To force-refresh, restart the gateway"
  color_dim "   or invalidate the cached key in Redis."
}

cmd_clear() {
  cmd_list
  echo ""
  local project_id confirm
  read -rp "Project ID to CLEAR all defaults: " project_id
  [[ -z "$project_id" ]] && { color_error "Project ID required."; exit 1; }
  read -rp "Type CLEAR to confirm wiping every default_* column: " confirm
  [[ "$confirm" == "CLEAR" ]] || { color_warn "Aborted."; exit 0; }

  $PSQL -c "UPDATE projects SET
              default_daily_spend_limit_usd   = NULL,
              default_monthly_spend_limit_usd = NULL,
              default_per_request_max_usd     = NULL,
              default_requests_per_minute     = NULL,
              default_requests_per_hour       = NULL,
              default_requests_per_day        = NULL,
              default_on_limit_behavior       = 'block',
              default_downgrade_model         = NULL,
              updated_at = NOW()
            WHERE id = '${project_id}';"
  color_success "✅ All project defaults cleared for ${project_id}."
}

# ── menu / dispatch ─────────────────────────────────────────────────────────

show_menu() {
  divider; color_title "  LLM0 Gateway — manage_project_defaults.sh"; divider
  echo ""
  echo "  1) List projects with their current defaults"
  echo "  2) Show defaults + customer count for one project"
  echo "  3) Set / update default limits for a project"
  echo "  4) Clear all defaults on a project"
  echo "  q) Quit"
  echo ""
  read -rp "Choose: " choice
  case "$choice" in
    1) cmd_list ;;
    2) cmd_show ;;
    3) cmd_set ;;
    4) cmd_clear ;;
    q|Q) exit 0 ;;
    *) color_error "Unknown choice: $choice"; exit 1 ;;
  esac
}

require_postgres

if [[ $# -eq 0 ]]; then
  show_menu
  exit 0
fi

case "$1" in
  list)  cmd_list ;;
  show)  cmd_show ;;
  set)   cmd_set ;;
  clear) cmd_clear ;;
  *)
    color_error "Unknown command: $1"
    echo "Run './scripts/manage_project_defaults.sh' with no args for the interactive menu."
    exit 1
    ;;
esac
