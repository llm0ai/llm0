#!/usr/bin/env bash
# =============================================================================
# manage_limits.sh — interactive CLI for API-key and project-level controls.
#
# Covers:
#   • api_keys.rate_limit_per_minute  (per-API-key req/min throttle)
#   • api_keys.is_active              (enable/disable a key)
#   • projects.monthly_cap_usd        (hard project-level spend ceiling)
#   • projects.cache_* / semantic_*   (cache toggles + TTL + threshold)
#
# Per-customer caps live elsewhere now (v0.3.0+):
#   • scripts/manage_project_defaults.sh — defaults applied to every customer
#   • scripts/manage_tiers.sh            — owner-defined plans
#                                          (X-Customer-Tier: <slug>)
# The legacy per-customer override table (customer_limits) is no longer
# consulted by the request path; see CHANGELOG v0.3.0 "Removed" notes.
#
# Usage:
#   ./scripts/manage_limits.sh                  # interactive menu
#   ./scripts/manage_limits.sh list-keys
#   ./scripts/manage_limits.sh set-key-rate
#   ./scripts/manage_limits.sh toggle-key
#   ./scripts/manage_limits.sh list-projects
#   ./scripts/manage_limits.sh set-project-cap
#   ./scripts/manage_limits.sh set-project-cache
#
# Requires: docker compose is running (postgres container).
# =============================================================================
set -euo pipefail

# ── Helpers ──────────────────────────────────────────────────────────────────
# NOTE: We close stdin (</dev/null) on every psql call so that `docker compose
# exec -T` does NOT swallow the script's read-prompt input.
PSQL_BIN="docker compose exec -T postgres psql -U llm0 -d llm0_gateway"
psql_run() { $PSQL_BIN "$@" </dev/null; }
PSQL="psql_run"

color_title()   { printf "\033[1;36m%s\033[0m\n" "$1"; }
color_success() { printf "\033[1;32m%s\033[0m\n" "$1"; }
color_warn()    { printf "\033[1;33m%s\033[0m\n" "$1"; }
color_error()   { printf "\033[1;31m%s\033[0m\n" "$1"; }
color_dim()     { printf "\033[2m%s\033[0m\n" "$1"; }

divider() {
  echo "════════════════════════════════════════════════════════════════════════════════"
}

require_postgres() {
  if ! docker compose ps postgres 2>/dev/null | grep -q "Up\|running"; then
    color_error "❌ Postgres container is not running."
    echo "   Start it with: docker compose up -d postgres"
    exit 1
  fi
}

# Accept either a null/empty string or a non-negative integer.
read_int_or_null() {
  local prompt="$1"
  local var
  read -rp "$prompt" var
  if [[ -z "$var" ]]; then
    echo "NULL"
  elif [[ "$var" =~ ^[0-9]+$ ]]; then
    echo "$var"
  else
    color_error "Invalid integer: $var"
    exit 1
  fi
}

read_money_or_null() {
  local prompt="$1"
  local var
  read -rp "$prompt" var
  if [[ -z "$var" ]]; then
    echo "NULL"
  elif [[ "$var" =~ ^[0-9]+(\.[0-9]+)?$ ]]; then
    echo "$var"
  else
    color_error "Invalid amount: $var"
    exit 1
  fi
}

# ── API Keys ─────────────────────────────────────────────────────────────────

cmd_list_keys() {
  divider
  color_title "  API Keys"
  divider
  $PSQL -c "SELECT
              k.key_prefix           AS prefix,
              k.name                 AS name,
              p.name                 AS project,
              k.rate_limit_per_minute AS rate_per_min,
              k.is_active            AS active,
              k.last_used_at         AS last_used
            FROM api_keys k
            JOIN projects p ON p.id = k.project_id
            ORDER BY p.name, k.name;"
}

cmd_set_key_rate() {
  divider
  color_title "  Update API Key Rate Limit"
  divider
  cmd_list_keys
  echo ""
  local prefix new_rate
  read -rp "API key prefix to update (exact match, e.g. 'llm0_live_a7ea9...'): " prefix
  if [[ -z "$prefix" ]]; then
    color_error "Prefix required."
    exit 1
  fi
  read -rp "New rate_limit_per_minute (integer): " new_rate
  if ! [[ "$new_rate" =~ ^[0-9]+$ ]]; then
    color_error "Rate must be a non-negative integer."
    exit 1
  fi

  local updated
  updated=$($PSQL -t -A -c "UPDATE api_keys
                            SET rate_limit_per_minute = ${new_rate},
                                updated_at = NOW()
                            WHERE key_prefix = '${prefix}'
                            RETURNING id;" | tr -d '[:space:]')

  if [[ -z "$updated" ]]; then
    color_error "❌ No API key found with prefix '${prefix}'."
    exit 1
  fi
  color_success "✅ Rate limit for ${prefix} set to ${new_rate} req/min."
  color_dim "   Takes effect immediately — no restart required."
}

cmd_toggle_key() {
  divider
  color_title "  Enable / Disable API Key"
  divider
  cmd_list_keys
  echo ""
  local prefix active_str
  read -rp "API key prefix: " prefix
  read -rp "Set active? (y/n): " active_str
  local active="true"
  [[ "$active_str" =~ ^[Nn]$ ]] && active="false"

  local updated
  updated=$($PSQL -t -A -c "UPDATE api_keys
                            SET is_active = ${active}, updated_at = NOW()
                            WHERE key_prefix = '${prefix}'
                            RETURNING id;" | tr -d '[:space:]')
  if [[ -z "$updated" ]]; then
    color_error "❌ No API key found with prefix '${prefix}'."
    exit 1
  fi
  color_success "✅ ${prefix} is_active = ${active}"
}

# ── Projects ─────────────────────────────────────────────────────────────────

cmd_list_projects() {
  divider
  color_title "  Projects"
  divider
  $PSQL -c "SELECT
              id,
              name,
              monthly_cap_usd         AS cap_usd,
              current_month_spend_usd AS spent_usd,
              cache_enabled           AS cache,
              semantic_cache_enabled  AS sem_cache,
              semantic_threshold      AS sem_thresh,
              cache_ttl_seconds       AS cache_ttl,
              is_active               AS active
            FROM projects
            ORDER BY name;"
}

cmd_set_project_cap() {
  divider
  color_title "  Update Project Monthly Spend Cap"
  divider
  cmd_list_projects
  echo ""
  local project_id new_cap
  read -rp "Project ID: " project_id
  read -rp "New monthly_cap_usd (e.g. 100.00): " new_cap
  if ! [[ "$new_cap" =~ ^[0-9]+(\.[0-9]+)?$ ]]; then
    color_error "Cap must be a decimal (e.g. 100 or 100.00)."
    exit 1
  fi
  local updated
  updated=$($PSQL -t -A -c "UPDATE projects
                            SET monthly_cap_usd = ${new_cap}, updated_at = NOW()
                            WHERE id = '${project_id}'
                            RETURNING id;" | tr -d '[:space:]')
  if [[ -z "$updated" ]]; then
    color_error "❌ No project with id '${project_id}'."
    exit 1
  fi
  color_success "✅ Project ${project_id} monthly cap set to \$${new_cap}."
}

cmd_set_project_cache() {
  divider
  color_title "  Update Project Cache Settings"
  divider
  cmd_list_projects
  echo ""
  local project_id cache_enabled sem_cache_enabled threshold ttl

  read -rp "Project ID: " project_id
  read -rp "cache_enabled (y/n, blank = leave unchanged): " cache_enabled
  read -rp "semantic_cache_enabled (y/n, blank = leave unchanged): " sem_cache_enabled
  read -rp "semantic_threshold (0.0–1.0, blank = leave unchanged): " threshold
  read -rp "cache_ttl_seconds (integer, blank = leave unchanged): " ttl

  local sets=()
  [[ "$cache_enabled" =~ ^[Yy]$ ]]       && sets+=("cache_enabled = true")
  [[ "$cache_enabled" =~ ^[Nn]$ ]]       && sets+=("cache_enabled = false")
  [[ "$sem_cache_enabled" =~ ^[Yy]$ ]]   && sets+=("semantic_cache_enabled = true")
  [[ "$sem_cache_enabled" =~ ^[Nn]$ ]]   && sets+=("semantic_cache_enabled = false")
  [[ -n "$threshold" ]]                  && sets+=("semantic_threshold = ${threshold}")
  [[ -n "$ttl" ]]                        && sets+=("cache_ttl_seconds = ${ttl}")

  if [[ ${#sets[@]} -eq 0 ]]; then
    color_warn "No changes requested — skipping."
    return
  fi

  local set_clause
  set_clause=$(IFS=', '; echo "${sets[*]}")

  local updated
  updated=$($PSQL -t -A -c "UPDATE projects
                            SET ${set_clause}, updated_at = NOW()
                            WHERE id = '${project_id}'
                            RETURNING id;" | tr -d '[:space:]')
  if [[ -z "$updated" ]]; then
    color_error "❌ No project with id '${project_id}'."
    exit 1
  fi
  color_success "✅ Updated: ${set_clause}"
}

# ── Menu ─────────────────────────────────────────────────────────────────────

show_menu() {
  divider
  color_title "  LLM0 Gateway — manage_limits.sh"
  divider
  echo ""
  echo "API Keys"
  echo "  1) List API keys"
  echo "  2) Update an API key's rate_limit_per_minute"
  echo "  3) Enable / disable an API key"
  echo ""
  echo "Projects"
  echo "  4) List projects"
  echo "  5) Update project monthly_cap_usd"
  echo "  6) Update project cache settings (exact + semantic)"
  echo ""
  color_dim "Per-customer caps moved out of this script:"
  color_dim "  scripts/manage_project_defaults.sh   defaults applied to every customer"
  color_dim "  scripts/manage_tiers.sh              owner-defined plans (X-Customer-Tier)"
  echo ""
  echo "  q) Quit"
  echo ""
  read -rp "Choose: " choice

  case "$choice" in
    1) cmd_list_keys ;;
    2) cmd_set_key_rate ;;
    3) cmd_toggle_key ;;
    4) cmd_list_projects ;;
    5) cmd_set_project_cap ;;
    6) cmd_set_project_cache ;;
    q|Q) exit 0 ;;
    *) color_error "Unknown choice: $choice"; exit 1 ;;
  esac
}

# ── Main ─────────────────────────────────────────────────────────────────────

require_postgres

if [[ $# -eq 0 ]]; then
  show_menu
  exit 0
fi

case "$1" in
  list-keys)               cmd_list_keys ;;
  set-key-rate)            cmd_set_key_rate ;;
  toggle-key)              cmd_toggle_key ;;
  list-projects)           cmd_list_projects ;;
  set-project-cap)         cmd_set_project_cap ;;
  set-project-cache)       cmd_set_project_cache ;;
  list-customers|set-customer-limit|delete-customer-limit)
    color_error "'$1' was removed in v0.3.0."
    echo "  Per-customer caps are now managed via:"
    echo "    scripts/manage_project_defaults.sh   (defaults applied to every customer)"
    echo "    scripts/manage_tiers.sh              (owner-defined plans via X-Customer-Tier)"
    echo "  See CHANGELOG v0.3.0 \"Removed\" notes for the migration."
    exit 1
    ;;
  *)
    color_error "Unknown command: $1"
    echo "Run './scripts/manage_limits.sh' with no args for the interactive menu."
    exit 1
    ;;
esac
