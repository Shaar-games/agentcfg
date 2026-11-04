#!/usr/bin/env bash
set -euo pipefail

commit="${1:?Expected the immutable release commit}"
[[ "$commit" =~ ^[0-9a-f]{40}$ ]] || { echo 'Invalid release commit' >&2; exit 1; }
repository="${GITHUB_REPOSITORY:?Expected GITHUB_REPOSITORY}"

# Only this repository's trusted CI workflow can attest to a release. A green
# run for a different commit, a pull request, or an incomplete matrix cannot.
runs="$(gh api --method GET "repos/$repository/actions/workflows/check.yml/runs" \
  -f head_sha="$commit" -f status=success -f per_page=100)"
run_id="$(jq -r --arg commit "$commit" --arg repository "$repository" '
  [.workflow_runs[] | select(
    .head_sha == $commit and .head_repository.full_name == $repository and
    .path == ".github/workflows/check.yml" and .status == "completed" and
    .conclusion == "success" and (.event == "push" or .event == "workflow_dispatch")
  )][0].id // empty
' <<<"$runs")"
[[ "$run_id" =~ ^[0-9]+$ ]] || {
  echo "No successful trusted CI run for $commit; finish CI before publishing." >&2
  exit 1
}
jobs="$(gh api "repos/$repository/actions/runs/$run_id/jobs?filter=latest&per_page=100")"
jq -e '
  def step_passed($name): any(.steps[]?;
    .name == $name and .status == "completed" and .conclusion == "success");
  def verified:
    .status == "completed" and .conclusion == "success" and
    step_passed("Static checks") and step_passed("Unit and integration tests") and
    step_passed("CLI build") and step_passed("No-argument CLI help");
  any(.jobs[]; .name == "Go tests (ubuntu-latest)" and verified and
    step_passed("Go formatting") and step_passed("Race detector")) and
  any(.jobs[]; .name == "Go tests (windows-latest)" and verified) and
  any(.jobs[]; (.name | test("^Go tests \\(macos-[^)]+\\)$")) and verified)
' <<<"$jobs" >/dev/null || {
  echo "CI run $run_id lacks a successful complete Linux/Windows/macOS matrix; refusing release." >&2
  exit 1
}
echo "Verified complete CI for $commit: https://github.com/$repository/actions/runs/$run_id"
