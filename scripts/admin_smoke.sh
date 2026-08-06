#!/usr/bin/env bash
# =============================================================================
# admin_smoke.sh — walks the admin REST API end to end:
#   create project → create API key → list projects → list API keys →
#   create tier → list tiers → delete tier →
#   set project defaults → get project defaults → clear project defaults
#
# This is the canonical way to manage projects/keys going forward — the
# bash + psql scripts (create_api_key.sh, manage_limits.sh, ...) still work
# and remain as convenience wrappers, but this script proves the same flow
# works over HTTP against the admin control plane described in
# plans/managed/06-milestones-and-roadmap.md (M0).
#
# Usage:
#   ADMIN_TOKEN=dev-admin-token ./scripts/admin_smoke.sh
#   ./scripts/admin_smoke.sh                     # uses ADMIN_TOKEN from .env
#
# Requires: the gateway is running with its admin listener reachable —
#   docker compose up -d   (admin API on http://localhost:8081)
# =============================================================================
set -euo pipefail

ADMIN_URL="${ADMIN_URL:-http://localhost:8081}"
ADMIN_TOKEN="${ADMIN_TOKEN:-$(grep -E '^ADMIN_TOKEN=' .env 2>/dev/null | cut -d= -f2-)}"

if [[ -z "$ADMIN_TOKEN" ]]; then
  echo "❌ ADMIN_TOKEN is not set (env var, or ADMIN_TOKEN= in .env). Aborting." >&2
  exit 1
fi

color_title()   { printf "\033[1;36m%s\033[0m\n" "$1"; }
color_success() { printf "\033[1;32m%s\033[0m\n" "$1"; }
color_error()   { printf "\033[1;31m%s\033[0m\n" "$1"; }
divider()       { echo "════════════════════════════════════════════════════════════════════════════"; }

# admin_curl <method> <path> [json body]
admin_curl() {
  local method="$1" path="$2" body="${3:-}"
  if [[ -n "$body" ]]; then
    curl -sS -w '\n%{http_code}' -X "$method" "$ADMIN_URL$path" \
      -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
      -d "$body"
  else
    curl -sS -w '\n%{http_code}' -X "$method" "$ADMIN_URL$path" \
      -H "Authorization: Bearer $ADMIN_TOKEN"
  fi
}

# split_status <curl_output_var_name> — pulls the trailing %{http_code}
# line off admin_curl's output and echoes the JSON body; sets $STATUS.
split_status() {
  local output="$1"
  STATUS="${output##*$'\n'}"
  BODY="${output%$'\n'*}"
}

require_2xx() {
  local step="$1"
  if [[ ! "$STATUS" =~ ^2 ]]; then
    color_error "❌ $step failed (HTTP $STATUS):"
    echo "$BODY"
    exit 1
  fi
}

divider
color_title "  LLM0 Gateway — Admin API Smoke Test"
color_title "  $ADMIN_URL"
divider
echo ""

echo "▶ 1/10  Create project"
out="$(admin_curl POST /v1/admin/projects '{"user_id":"'"$(uuidgen | tr '[:upper:]' '[:lower:]')"'","name":"admin_smoke test project","monthly_cap_usd":25}')"
split_status "$out"; require_2xx "create project"
PROJECT_ID="$(echo "$BODY" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)"
color_success "  ✓ project $PROJECT_ID"
echo ""

echo "▶ 2/10  Create API key"
out="$(admin_curl POST "/v1/admin/projects/$PROJECT_ID/api-keys" '{"name":"admin_smoke test key","rate_limit_per_minute":60}')"
split_status "$out"; require_2xx "create API key"
RAW_KEY="$(echo "$BODY" | grep -o '"api_key":"[^"]*"' | cut -d'"' -f4)"
color_success "  ✓ key ${RAW_KEY:0:15}... (full value shown once above; not re-displayed)"
echo ""

echo "▶ 3/10  List projects (expect the one just created)"
out="$(admin_curl GET "/v1/admin/projects")"
split_status "$out"; require_2xx "list projects"
if [[ "$BODY" != *"$PROJECT_ID"* ]]; then
  color_error "❌ created project not found in list response"; exit 1
fi
color_success "  ✓ found in project list"
echo ""

echo "▶ 4/10  List API keys for the project (expect the one just created)"
out="$(admin_curl GET "/v1/admin/projects/$PROJECT_ID/api-keys")"
split_status "$out"; require_2xx "list API keys"
color_success "  ✓ $(echo "$BODY" | grep -o '"id"' | wc -l | tr -d ' ') key(s) on the project"
echo ""

echo "▶ 5/10  Create tier 'pro' (\$5/day cap)"
out="$(admin_curl POST "/v1/admin/projects/$PROJECT_ID/tiers" '{"slug":"pro","daily_spend_limit_usd":5,"requests_per_day":1000}')"
split_status "$out"; require_2xx "create tier"
color_success "  ✓ tier 'pro' created on project $PROJECT_ID"
echo ""

echo "▶ 6/10  List tiers for the project (expect 'pro')"
out="$(admin_curl GET "/v1/admin/projects/$PROJECT_ID/tiers")"
split_status "$out"; require_2xx "list tiers"
if [[ "$BODY" != *'"slug":"pro"'* ]]; then
  color_error "❌ created tier not found in list response"; exit 1
fi
color_success "  ✓ found in tier list"
echo ""

echo "▶ 7/10  Delete tier 'pro'"
out="$(admin_curl DELETE "/v1/admin/projects/$PROJECT_ID/tiers/pro")"
split_status "$out"; require_2xx "delete tier"
color_success "  ✓ tier 'pro' deleted"
echo ""

echo "▶ 8/10  Set project defaults (\$2/day cap on every customer without a tier)"
out="$(admin_curl PATCH "/v1/admin/projects/$PROJECT_ID/defaults" '{"default_daily_spend_limit_usd":2,"default_requests_per_day":500}')"
split_status "$out"; require_2xx "set project defaults"
if [[ "$BODY" != *'"default_daily_spend_limit_usd":2'* ]]; then
  color_error "❌ PATCH response doesn't reflect the new default"; exit 1
fi
color_success "  ✓ defaults set on project $PROJECT_ID"
echo ""

echo "▶ 9/10  Get project defaults (expect the \$2/day cap just set)"
out="$(admin_curl GET "/v1/admin/projects/$PROJECT_ID/defaults")"
split_status "$out"; require_2xx "get project defaults"
if [[ "$BODY" != *'"default_daily_spend_limit_usd":2'* ]]; then
  color_error "❌ GET doesn't reflect the default set in the previous step"; exit 1
fi
color_success "  ✓ defaults read back correctly"
echo ""

echo "▶ 10/10  Clear project defaults"
out="$(admin_curl DELETE "/v1/admin/projects/$PROJECT_ID/defaults")"
split_status "$out"; require_2xx "clear project defaults"
color_success "  ✓ defaults cleared"
echo ""

divider
color_success "  ✅ All admin API smoke checks passed."
divider
echo ""
echo "Test the created key against the public data-plane API (default :8080):"
echo ""
echo "  curl http://localhost:8080/v1/chat/completions \\"
echo "    -H \"Authorization: Bearer $RAW_KEY\" \\"
echo "    -H \"Content-Type: application/json\" \\"
echo "    -d '{\"model\":\"gpt-4o-mini\",\"messages\":[{\"role\":\"user\",\"content\":\"Say hello!\"}]}'"
echo ""
