#!/usr/bin/env bash
set -euo pipefail

verifier="$(cd "$(dirname "$0")" && pwd)/verify-release-ci.sh"
test_root="$(mktemp -d)"
trap 'rm -rf "$test_root"' EXIT
export RELEASE_CI_TEST_ROOT="$test_root" GITHUB_REPOSITORY=fixture/agentcfg
commit=0123456789abcdef0123456789abcdef01234567

jq -n --arg commit "$commit" --arg repository "$GITHUB_REPOSITORY" '{workflow_runs:[{
  id:123, head_sha:$commit, head_repository:{full_name:$repository},
  path:".github/workflows/check.yml", status:"completed", conclusion:"success", event:"push"
}]}' >"$test_root/runs-original.json"
jq -n '{jobs:(["ubuntu-latest","windows-latest","macos-15-intel"] | map(. as $os | {
  name:("Go tests (" + $os + ")"), status:"completed", conclusion:"success",
  steps:(["Static checks","Unit and integration tests","CLI build","No-argument CLI help"] +
    (if $os == "ubuntu-latest" then ["Go formatting","Race detector"] else [] end) |
    map({name:.,status:"completed",conclusion:"success"}))
}))}' >"$test_root/jobs-original.json"

function gh() {
  if [[ "${RELEASE_CI_TEST_API_FAILURE:-}" == 1 ]]; then return 1; fi
  case "$*" in
    *workflows/check.yml/runs*) cat "$RELEASE_CI_TEST_ROOT/runs.json" ;;
    *runs/123/jobs*) cat "$RELEASE_CI_TEST_ROOT/jobs.json" ;;
    *) echo 'Unexpected CI API endpoint' >&2; return 1 ;;
  esac
}
export -f gh
cp "$test_root/runs-original.json" "$test_root/runs.json"
cp "$test_root/jobs-original.json" "$test_root/jobs.json"
bash "$verifier" "$commit"

function reject() {
  if bash "$verifier" "$commit" >"$test_root/output" 2>&1; then
    echo "Release gate accepted $1" >&2
    exit 1
  fi
  echo "Rejected $1"
}
for mutation in \
  '.workflow_runs[0].head_sha = "different-commit"' \
  '.workflow_runs[0].head_repository.full_name = "other/repository"' \
  '.workflow_runs[0].event = "pull_request"' \
  '.workflow_runs[0].path = ".github/workflows/other.yml"' \
  '.workflow_runs[0].conclusion = "failure"' \
  '.workflow_runs[0].status = "in_progress"' \
  '.workflow_runs = []'; do
  jq "$mutation" "$test_root/runs-original.json" >"$test_root/runs.json"
  reject "$mutation"
done
cp "$test_root/runs-original.json" "$test_root/runs.json"
for mutation in \
  'del(.jobs[2])' \
  '.jobs[1].status = "queued"' \
  '.jobs[2].conclusion = "failure"' \
  '(.jobs[0].steps[] | select(.name == "Race detector").conclusion) = "skipped"' \
  '.jobs[1].steps = []'; do
  jq "$mutation" "$test_root/jobs-original.json" >"$test_root/jobs.json"
  reject "$mutation"
done
export RELEASE_CI_TEST_API_FAILURE=1
reject 'API failure'
