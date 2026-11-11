#!/usr/bin/env bash
# Builds and deploys cloud-control with Terraform.
#
#   scripts/deploy.sh          # plan + apply (asks for confirmation)
#   scripts/deploy.sh plan     # plan only
#
# Needs: Go, Terraform, AWS CLI logged in (aws sso login --profile cloud-control),
# and .env with DISCORD_PUBLIC_KEY, OWNER_DISCORD_USER_ID, DISCORD_GUILD_ID.
set -euo pipefail
cd "$(dirname "$0")/.."

[[ -f .env ]] || { echo "missing .env (copy .env.example)"; exit 1; }

# Read only the three values Terraform needs; never echo them.
get() { grep -E "^$1=" .env | tail -1 | cut -d= -f2- | tr -d '\r'; }
export TF_VAR_discord_public_key="$(get DISCORD_PUBLIC_KEY)"
export TF_VAR_owner_discord_user_id="$(get OWNER_DISCORD_USER_ID)"
export TF_VAR_discord_guild_id="$(get DISCORD_GUILD_ID)"
for v in TF_VAR_discord_public_key TF_VAR_owner_discord_user_id TF_VAR_discord_guild_id; do
  [[ -n "${!v}" ]] || { echo "$v is empty: check .env"; exit 1; }
done

aws sts get-caller-identity --profile "${AWS_PROFILE:-cloud-control}" >/dev/null 2>&1 \
  || { echo "AWS session expired: run  aws sso login --profile cloud-control"; exit 1; }

scripts/build-lambda.sh

tf() { terraform -chdir=infra/terraform "$@"; }
tf init -input=false >/dev/null
if [[ "${1:-apply}" == "plan" ]]; then
  tf plan -input=false
else
  tf apply -input=false
  echo
  echo "Interactions Endpoint URL: $(tf output -raw interactions_endpoint_url)"
fi
