#!/usr/bin/env bash
# Runs kube-rightsize-diff for the composite GitHub Action, writes the
# job summary and creates or updates one PR comment. Exits with the CLI's
# exit code after commenting, so a failing gate still leaves a comment.
set -euo pipefail

marker='<!-- kube-rightsize-diff -->'
md="$RUNNER_TEMP/kube-rightsize-diff.md"
json="$RUNNER_TEMP/kube-rightsize-diff.json"

args=(--format markdown --output "$md" --json-output "$json"
  --cpu-percentile "$KRD_CPU_PCT" --memory-percentile "$KRD_MEM_PCT"
  --price-vcpu-hour "$KRD_PRICE_CPU" --price-gib-hour "$KRD_PRICE_MEM"
  --fail-on "$KRD_FAIL_ON" --max-cost-increase "$KRD_MAX_INCREASE")
read -r -a heads <<<"$KRD_HEAD"
for p in "${heads[@]}"; do args+=(--head "$p"); done
if [[ -n "${KRD_BASE:-}" ]]; then
  read -r -a bases <<<"$KRD_BASE"
  for p in "${bases[@]}"; do args+=(--base "$p"); done
fi
[[ -n "${KRD_USAGE:-}" ]] && args+=(--usage "$KRD_USAGE")
[[ -n "${KRD_PROM_URL:-}" ]] && args+=(--prometheus-url "$KRD_PROM_URL" --window "$KRD_WINDOW")
if [[ -n "${KRD_EXTRA:-}" ]]; then
  read -r -a extra <<<"$KRD_EXTRA"
  args+=("${extra[@]}")
fi

set +e
"$KRD_BIN" "${args[@]}"
code=$?
set -e
if [[ $code -ne 0 && $code -ne 2 ]]; then
  echo "::error::kube-rightsize-diff failed with exit code $code"
  exit "$code"
fi

cat "$md" >>"$GITHUB_STEP_SUMMARY"
{
  echo "risks=$(jq '.totals.risks' "$json")"
  echo "wastes=$(jq '.totals.wastes' "$json")"
  echo "monthly-delta-usd=$(jq '.totals.monthly_delta_usd' "$json")"
  echo "report-json=$json"
} >>"$GITHUB_OUTPUT"

pr=$(jq -r '.pull_request.number // empty' "$GITHUB_EVENT_PATH")
if [[ "$KRD_COMMENT" == "true" && -n "$pr" ]]; then
  api="repos/$GITHUB_REPOSITORY/issues"
  existing=$(gh api --paginate "$api/$pr/comments" \
    --jq ".[] | select(.body | startswith(\"$marker\")) | .id" | head -n1 || true)
  if [[ -n "$existing" ]]; then
    gh api -X PATCH "$api/comments/$existing" -F "body=@$md" >/dev/null &&
      echo "Updated PR comment $existing" ||
      echo "::warning::could not update the PR comment (fork PRs get a read-only token)"
  else
    gh api -X POST "$api/$pr/comments" -F "body=@$md" >/dev/null &&
      echo "Posted PR comment" ||
      echo "::warning::could not post a PR comment (fork PRs get a read-only token)"
  fi
fi

if [[ $code -eq 2 ]]; then
  echo "::error::kube-rightsize-diff threshold reached (fail-on=$KRD_FAIL_ON, max-cost-increase=$KRD_MAX_INCREASE)"
fi
exit "$code"
