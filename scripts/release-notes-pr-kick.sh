#!/usr/bin/env bash
# Start CI on a Notes PR that the Actions bot opened. Events raised with
# GITHUB_TOKEN never start workflows, so such a PR shows action_required with
# zero jobs until a human-authenticated close/reopen re-emits pull_request.
set -euo pipefail

if [ "$#" -ne 1 ] || [[ ! "$1" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
	echo "usage: scripts/release-notes-pr-kick.sh MAJOR.MINOR.PATCH" >&2
	exit 2
fi

version="$1"
repository="${RELEASE_REPOSITORY:-esengine/DeepSeek-Reasonix}"
branch="release-notes/v$version"
wait_seconds="${RELEASE_NOTES_KICK_WAIT_SECONDS:-120}"

for command in gh jq; do
	command -v "$command" >/dev/null || {
		echo "required command is unavailable: $command" >&2
		exit 2
	}
done

prs="$(gh pr list --repo "$repository" --head "$branch" --state open --json number,headRefOid --limit 2)"
count="$(jq 'length' <<<"$prs")"
if [ "$count" -ne 1 ]; then
	echo "expected exactly one open PR for $branch, found $count" >&2
	exit 1
fi
number="$(jq -r '.[0].number' <<<"$prs")"
head="$(jq -r '.[0].headRefOid' <<<"$prs")"

check_runs() {
	gh api "repos/$repository/commits/$head/check-runs" --jq '.total_count'
}

if [ "$(check_runs)" -gt 0 ]; then
	echo "PR #$number already has check runs on $head; nothing to kick"
	exit 0
fi

# Only the recorded-but-never-started shape is safe to kick. Anything else
# with zero check runs needs a look, not a reopen.
stalled="$(gh api "repos/$repository/actions/runs?event=pull_request&head_sha=$head&per_page=20" \
	--jq "[.workflow_runs[] | select(.head_sha == \"$head\" and .conclusion == \"action_required\")] | length")"
if [ "$stalled" -eq 0 ]; then
	echo "PR #$number has no check runs and no action_required run on $head; inspect before kicking" >&2
	exit 1
fi

echo "PR #$number: $stalled recorded-but-unstarted run(s) on $head; closing and reopening"
gh pr close "$number" --repo "$repository"
gh pr reopen "$number" --repo "$repository"

deadline=$((SECONDS + wait_seconds))
while [ "$(check_runs)" -eq 0 ]; do
	if [ "$SECONDS" -ge "$deadline" ]; then
		echo "no check runs appeared on $head within ${wait_seconds}s" >&2
		exit 1
	fi
	sleep 5
done
echo "CI started on PR #$number ($head)"
