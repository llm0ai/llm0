#!/usr/bin/env bash
# =============================================================================
# manage_tiers.sh — define owner-named "plans" (tiers) and the limits each
# tier carries. Customers attach to a tier via the X-Customer-Tier request
# header.
#
# LLM0 has NO built-in tier names. You name them whatever fits your product:
# 'free' / 'starter' / 'pro' / 'enterprise' / '1' / '2' — any string works.
# Operates on the customer_tiers table added in Slice A.
#
# See plans/customer-limits-tiers.md for the resolution rule (tier → project
# default → unlimited) and the trust model (X-Customer-Tier is
# server-to-server, never browser-supplied).
#
# Usage:
#   ./scripts/manage_tiers.sh                       # interactive menu
#   ./scripts/manage_tiers.sh list <project-id>
#   ./scripts/manage_tiers.sh create
#   ./scripts/manage_tiers.sh update
#   ./scripts/manage_tiers.sh delete
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

# Accept empty (NULL) or non-negative number.
read_money_or_null() {
  local prompt="$1"; local var
  read -rp "$prompt" var
  if [[ -z "$var" ]];                        then echo "NULL"; return; fi
  if [[ "$var" =~ ^[0-9]+(\.[0-9]+)?$ ]];    then echo "$var"; return; fi
  color_error "Invalid amount: $var"; exit 1
}
read_int_or_null() {
  local prompt="$1"; local var
  read -rp "$prompt" var
  if [[ -z "$var" ]];          then echo "NULL"; return; fi
  if [[ "$var" =~ ^[0-9]+$ ]]; then echo "$var"; return; fi
  color_error "Invalid integer: $var"; exit 1
}

list_projects() {
  $PSQL -c "SELECT id, name FROM projects ORDER BY name;"
}

# ── commands ────────────────────────────────────────────────────────────────

cmd_list() {
  divider; color_title "  Customer Tiers"; divider
  local project_id="${1:-}"
  if [[ -n "$project_id" ]]; then
    $PSQL -c "SELECT
                t.slug,
                p.name                AS project,
                t.daily_spend_limit_usd   AS day_usd,
                t.monthly_spend_limit_usd AS month_usd,
                t.per_request_max_usd     AS per_req_usd,
                t.requests_per_minute  AS rpm,
                t.requests_per_hour    AS rph,
                t.requests_per_day     AS rpd,
                t.on_limit_behavior    AS on_limit,
                t.downgrade_model      AS downgrade_to
              FROM customer_tiers t
              JOIN projects p ON p.id = t.project_id
              WHERE t.project_id = '${project_id}'
              ORDER BY t.slug;"
  else
    $PSQL -c "SELECT
                p.name                  AS project,
                t.slug                  AS tier,
                t.daily_spend_limit_usd   AS day_usd,
                t.monthly_spend_limit_usd AS month_usd,
                t.requests_per_day      AS rpd,
                t.on_limit_behavior     AS on_limit,
                t.downgrade_model       AS downgrade_to
              FROM customer_tiers t
              JOIN projects p ON p.id = t.project_id
              ORDER BY p.name, t.slug;"
  fi
}

cmd_create() {
  divider; color_title "  Create / Update Tier"; divider
  list_projects
  echo ""
  local project_id slug
  read -rp "Project ID: " project_id
  read -rp "Tier slug (your name — e.g. 'free', 'pro', 'enterprise', '1'): " slug
  [[ -z "$project_id" || -z "$slug" ]] && { color_error "Both required."; exit 1; }

  echo ""
  color_dim "Leave any cap blank to store NULL (no limit on that axis)."
  echo ""
  local daily monthly per_req rpm rph rpd
  daily=$(read_money_or_null   "daily_spend_limit_usd      : ")
  monthly=$(read_money_or_null "monthly_spend_limit_usd    : ")
  per_req=$(read_money_or_null "per_request_max_usd        : ")
  rpm=$(read_int_or_null       "requests_per_minute        : ")
  rph=$(read_int_or_null       "requests_per_hour          : ")
  rpd=$(read_int_or_null       "requests_per_day           : ")

  echo ""
  echo "on_limit_behavior options: block | downgrade | warn"
  read -rp "on_limit_behavior (default: block): " behavior
  behavior="${behavior:-block}"
  if [[ "$behavior" != "block" && "$behavior" != "downgrade" && "$behavior" != "warn" ]]; then
    color_error "Invalid behavior."
    exit 1
  fi

  local downgrade="NULL"
  if [[ "$behavior" == "downgrade" ]]; then
    local d
    read -rp "downgrade_model (e.g. gpt-4o-mini): " d
    [[ -z "$d" ]] && { color_error "downgrade_model required when behavior=downgrade."; exit 1; }
    downgrade="'${d}'"
  fi

  $PSQL -c "INSERT INTO customer_tiers
              (project_id, slug,
               daily_spend_limit_usd, monthly_spend_limit_usd, per_request_max_usd,
               requests_per_minute, requests_per_hour, requests_per_day,
               on_limit_behavior, downgrade_model)
            VALUES
              ('${project_id}', '${slug}',
               ${daily}, ${monthly}, ${per_req},
               ${rpm}, ${rph}, ${rpd},
               '${behavior}', ${downgrade})
            ON CONFLICT (project_id, slug) DO UPDATE SET
              daily_spend_limit_usd    = EXCLUDED.daily_spend_limit_usd,
              monthly_spend_limit_usd  = EXCLUDED.monthly_spend_limit_usd,
              per_request_max_usd      = EXCLUDED.per_request_max_usd,
              requests_per_minute      = EXCLUDED.requests_per_minute,
              requests_per_hour        = EXCLUDED.requests_per_hour,
              requests_per_day         = EXCLUDED.requests_per_day,
              on_limit_behavior        = EXCLUDED.on_limit_behavior,
              downgrade_model          = EXCLUDED.downgrade_model,
              updated_at               = NOW();"

  color_success "✅ Tier '${slug}' upserted on project ${project_id}."
  color_dim "   Customers carrying X-Customer-Tier: ${slug} now resolve to these limits."
  color_dim "   The in-process tier cache refreshes within ~60s; or restart the gateway."
}

cmd_delete() {
  divider; color_title "  Delete Tier"; divider
  cmd_list
  echo ""
  local project_id slug confirm
  read -rp "Project ID: " project_id
  read -rp "Tier slug: " slug
  read -rp "Type DELETE to confirm: " confirm
  [[ "$confirm" == "DELETE" ]] || { color_warn "Aborted."; exit 0; }

  local res
  res=$($PSQL -t -A -c "DELETE FROM customer_tiers
                        WHERE project_id = '${project_id}' AND slug = '${slug}'
                        RETURNING id;" | tr -d '[:space:]')
  if [[ -z "$res" ]]; then
    color_error "❌ Tier '${slug}' not found on project ${project_id}."
    exit 1
  fi
  color_success "✅ Tier '${slug}' deleted."
  color_dim "   Customers that were carrying this slug fall through to the project default."
}

show_menu() {
  divider; color_title "  LLM0 Gateway — manage_tiers.sh"; divider
  echo ""
  echo "  1) List all tiers (across projects)"
  echo "  2) List tiers for one project"
  echo "  3) Create or update a tier"
  echo "  4) Delete a tier"
  echo "  q) Quit"
  echo ""
  read -rp "Choose: " choice
  case "$choice" in
    1) cmd_list ;;
    2) list_projects; echo ""; read -rp "Project ID: " p; cmd_list "$p" ;;
    3) cmd_create ;;
    4) cmd_delete ;;
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
  list)   shift; cmd_list "${1:-}" ;;
  create) cmd_create ;;
  update) cmd_create ;;   # same upsert path
  delete) cmd_delete ;;
  *)
    color_error "Unknown command: $1"
    echo "Run './scripts/manage_tiers.sh' with no args for the interactive menu."
    exit 1
    ;;
esac
