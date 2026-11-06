#!/usr/bin/env bash
# Usage:
#   scripts/branches.sh                   census: TSV of every origin branch
#   scripts/branches.sh --prune           list merged lane/* branches; delete them only when PRUNE_LANE_BRANCHES=true
#   scripts/branches.sh --dry-run-prune   same list, never deletes
set -euo pipefail

mode=census
case "${1:-}" in
  "") ;;
  --prune) mode=prune ;;
  --dry-run-prune) mode=dry ;;
  *) echo "usage: scripts/branches.sh [--prune|--dry-run-prune]" >&2; exit 2 ;;
esac

KEEP=(main archive/rust-seed lane/traces lane/traces-route)
SUPERSEDED=(
  "lane/release-install|superseded by lane/release-day once created"
  "lane/release-public|superseded by lane/release-day once created"
  "lane/release-fixes|superseded by lane/release-day once created"
  "backup/main-pre-rewrite-20261026|pre-rewrite backup, superseded by main"
  "deploy/ingest-stamp|superseded"
)

git fetch --quiet --prune origin

kept() {
  local b
  for b in "${KEEP[@]}"; do [[ $1 == "$b" ]] && return 0; done
  return 1
}

merged_state() {
  local rc=0 out
  git merge-base --is-ancestor "origin/$1" origin/main || rc=$?
  case $rc in
    0) echo yes; return ;;
    1) ;;
    *) echo "branches.sh: merge-base failed (exit $rc) for $1, treating as not merged" >&2; echo no; return ;;
  esac
  if ! out=$(git rev-list --merges origin/main.."origin/$1"); then
    echo "branches.sh: git rev-list failed for $1, treating as not merged" >&2
    echo no
    return
  elif [[ -n $out ]]; then
    echo no
    return
  fi
  if ! out=$(git cherry origin/main "origin/$1"); then
    echo "branches.sh: git cherry failed for $1, treating as not merged" >&2
    echo no
  elif grep -q '^+' <<<"$out"; then
    echo no
  else
    echo patch-equivalent
  fi
}

branches() {
  git for-each-ref --format='%(refname:strip=3) %(objectname)' refs/remotes/origin |
    grep -v '^HEAD '
}

census() {
  local b sha merged verdict reason entry
  while read -r b sha; do
    merged=$(merged_state "$b")
    verdict=ask
    reason="$(git rev-list --count origin/main.."origin/$b") commits not on main, tip $(git log -1 --format='%cs %s' "origin/$b")"
    if kept "$b"; then
      verdict=keep reason="protected"
    elif [[ $merged == yes ]]; then
      verdict=delete reason="head is an ancestor of main"
    elif [[ $merged == patch-equivalent ]]; then
      verdict=delete reason="every commit has a patch-equivalent on main"
    elif [[ $b == lane/r2-* ]]; then
      verdict=delete reason="round-2 lane, lands on main today"
    else
      for entry in "${SUPERSEDED[@]}"; do
        [[ $b == "${entry%%|*}" ]] && { verdict=delete reason="${entry#*|}"; break; }
      done
    fi
    printf '%s\t%s\t%s\t%s\t%s\n' "$b" "$sha" "$merged" "$verdict" "$reason"
  done < <(branches)
}

prune() {
  local b sha merged n=0
  while read -r b sha; do
    [[ $b == lane/* ]] || continue
    kept "$b" && continue
    merged=$(merged_state "$b")
    [[ $merged == no ]] && continue
    if [[ $mode == prune && ${PRUNE_LANE_BRANCHES:-} == true ]]; then
      echo "deleting $b $sha ($merged)"
      git push origin "--force-with-lease=refs/heads/$b:$sha" ":refs/heads/$b"
    else
      echo "would delete $b $sha ($merged)"
    fi
    n=$((n + 1))
  done < <(branches)
  echo "$n merged lane branches"
}

if [[ $mode == census ]]; then census; else prune; fi
