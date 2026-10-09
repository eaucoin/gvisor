#!/bin/bash

# Copyright 2026 The gVisor Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# series.sh checks the fork's series branches and builds main from them, as
# the SERIES file describes (see its header).
#
# Usage:
#   tools/fork/series.sh check [BRANCH...]  # check every series, or the named ones
#   tools/fork/series.sh build [COUNT]      # print the commit main must point to,
#                                           # made of the first COUNT series only
#   tools/fork/series.sh base               # print the base
#   tools/fork/series.sh list               # print "BRANCH TIP COMMITS" per series
#
# A series is checked alone: it must contain the base, contain the series it
# is declared on, and add only commits that are not merges, not fixups and
# carry no AI attribution; the subject of each commit its committer also
# authored must read "area: summary" (carried commits of other authors, such
# as upstream pull requests, keep theirs).
#
# build applies every series' own commits, in order, onto the base, without
# a working tree (git merge-tree). Each commit keeps its author, committer,
# dates and message, so a rebuild with unchanged inputs gives the same main,
# and the commits of a series applied directly on its own parent keep their
# hashes. A conflict stops the build and names both series: the later one must
# then be declared "on" the earlier one and rebased onto it.
#
# Environment:
#   REMOTE      remote whose branches are read (default: origin)
#   SERIES_REF  ref holding the SERIES file (default: $REMOTE/series/fork-infra)

set -euo pipefail

readonly REMOTE="${REMOTE:-origin}"
readonly SERIES_REF="${SERIES_REF:-${REMOTE}/series/fork-infra}"

die() {
  echo "series.sh: $*" >&2
  exit 1
}

# Parsed SERIES: the base, then parallel arrays of branches and of the
# space-separated branches each one is declared on.
declare BASE=""
declare -a BRANCHES=()
declare -a PREREQS=()

parse_series() {
  local text line lineno=0 name rest
  text="$(git show "${SERIES_REF}:SERIES")" || die "cannot read SERIES from ${SERIES_REF}"
  while IFS= read -r line; do
    lineno=$((lineno + 1))
    line="${line%%#*}"
    read -r name rest <<<"${line}" || true
    [[ -z "${name}" ]] && continue
    if [[ "${name}" == "base" ]]; then
      [[ -z "${BASE}" && -n "${rest}" && "${rest}" != *" "* ]] ||
        die "SERIES:${lineno}: expected one 'base REF' line"
      BASE="${rest}"
      continue
    fi
    [[ -n "${BASE}" ]] || die "SERIES:${lineno}: the base line must come first"
    [[ "${name}" == series/* ]] || die "SERIES:${lineno}: ${name} is not a series/ branch"
    local prereqs=""
    if [[ -n "${rest}" ]]; then
      local word
      read -r word prereqs <<<"${rest}"
      [[ "${word}" == "on" && -n "${prereqs}" ]] ||
        die "SERIES:${lineno}: expected 'BRANCH [on BRANCH...]'"
      local p
      for p in ${prereqs}; do
        index_of "${p}" >/dev/null ||
          die "SERIES:${lineno}: ${name} is on ${p}, which is not listed before it"
      done
    fi
    ! index_of "${name}" >/dev/null || die "SERIES:${lineno}: ${name} is listed twice"
    BRANCHES+=("${name}")
    PREREQS+=("${prereqs}")
  done <<<"${text}"
  [[ -n "${BASE}" ]] || die "SERIES has no base line"
}

# index_of prints the index of a listed branch, or fails.
index_of() {
  local i
  for i in "${!BRANCHES[@]}"; do
    if [[ "${BRANCHES[$i]}" == "$1" ]]; then
      echo "${i}"
      return 0
    fi
  done
  return 1
}

base_commit() {
  git rev-parse --verify --quiet "${BASE}^{commit}" || die "base ${BASE} not found; are tags fetched?"
}

tip_of() {
  git rev-parse --verify --quiet "refs/remotes/${REMOTE}/$1^{commit}" ||
    die "$1 not found at ${REMOTE}"
}

# own_commits prints, oldest first, the commits a listed series adds to the
# base and to the series it is declared on.
own_commits() {
  local i="$1" p
  local -a excludes=("^$(base_commit)")
  for p in ${PREREQS[$i]}; do
    excludes+=("^$(tip_of "${p}")")
  done
  git rev-list --reverse --topo-order "$(tip_of "${BRANCHES[$i]}")" "${excludes[@]}"
}

check_one() {
  local i="$1" branch="${BRANCHES[$1]}" tip base p c subject body problems=0
  tip="$(tip_of "${branch}")"
  base="$(base_commit)"
  if ! git merge-base --is-ancestor "${base}" "${tip}"; then
    echo "${branch}: does not contain the base ${BASE}; rebase it" >&2
    return 1
  fi
  for p in ${PREREQS[$i]}; do
    if ! git merge-base --is-ancestor "$(tip_of "${p}")" "${tip}"; then
      echo "${branch}: does not contain the tip of ${p}, which it is on; rebase it onto ${p}" >&2
      problems=$((problems + 1))
    fi
  done
  for c in $(own_commits "${i}"); do
    subject="$(git show -s --format=%s "${c}")"
    body="$(git show -s --format=%b "${c}")"
    if [[ "$(git show -s --format=%p "${c}" | wc -w)" -ne 1 ]]; then
      echo "${branch}: ${c:0:12} is a merge commit" >&2
      problems=$((problems + 1))
    fi
    if [[ "${subject}" =~ ^(fixup|squash|amend)! ]]; then
      echo "${branch}: ${c:0:12} is a fixup; squash it into its commit" >&2
      problems=$((problems + 1))
    fi
    if grep -qiE '^(co-authored-by|assisted-by|generated-by):|generated with ' <<<"${body}"; then
      echo "${branch}: ${c:0:12} carries an AI attribution or co-author trailer" >&2
      problems=$((problems + 1))
    fi
    if [[ "$(git show -s --format=%ae "${c}")" == "$(git show -s --format=%ce "${c}")" &&
      ! "${subject}" =~ ^[^:[:space:]][^:]{0,60}:\ [^[:space:]] ]]; then
      echo "${branch}: ${c:0:12} subject \"${subject}\" does not read \"area: summary\"" >&2
      problems=$((problems + 1))
    fi
  done
  if [[ -z "$(own_commits "${i}")" ]]; then
    echo "${branch}: adds no commits" >&2
    problems=$((problems + 1))
  fi
  return $((problems > 0))
}

cmd_check() {
  local failed=0 name i n
  local -a names=("$@")
  if [[ ${#names[@]} -eq 0 ]]; then
    names=("${BRANCHES[@]}")
  fi
  for name in "${names[@]}"; do
    if ! i="$(index_of "${name}")"; then
      echo "${name}: not listed in SERIES; checked as a series on the base" >&2
      BRANCHES+=("${name}")
      PREREQS+=("")
      i=$((${#BRANCHES[@]} - 1))
    fi
    if check_one "${i}"; then
      n="$(own_commits "${i}" | wc -l)"
      echo "${name}: ok (${n} commit$( ((n == 1)) || echo s))"
    else
      failed=1
    fi
  done
  return "${failed}"
}

# commit_like writes a commit with the given tree and parent and everything
# else taken from commit $1, and prints it.
commit_like() {
  local c="$1" tree="$2" parent="$3"
  local an ae ad cn ce cd
  {
    IFS= read -r -d '' an
    IFS= read -r -d '' ae
    IFS= read -r -d '' ad
    IFS= read -r -d '' cn
    IFS= read -r -d '' ce
    IFS= read -r -d '' cd
  } < <(git show -s --date=raw --format='%an%x00%ae%x00%ad%x00%cn%x00%ce%x00%cd%x00' "${c}")
  git cat-file commit "${c}" | sed '1,/^$/d' |
    GIT_AUTHOR_NAME="${an}" GIT_AUTHOR_EMAIL="${ae}" GIT_AUTHOR_DATE="${ad}" \
    GIT_COMMITTER_NAME="${cn}" GIT_COMMITTER_EMAIL="${ce}" GIT_COMMITTER_DATE="${cd}" \
    git commit-tree --no-gpg-sign "${tree}" -p "${parent}"
}

# conflicting prints which of the series applied before series $2 (indices
# $3...) changed a file that the git merge-tree output $1 reports in conflict,
# leaving out those series $2 contains, or "the base".
conflicting() {
  local out="$1" i="$2" j
  shift 2
  local -a files=() found=()
  # The output is the tree, the conflicted files, an empty line, messages.
  mapfile -t files < <(sed -n '2,/^$/p' <<<"${out}" | sed '/^$/d' | sort -u)
  for j in "$@"; do
    if git merge-base --is-ancestor "$(tip_of "${BRANCHES[$j]}")" "$(tip_of "${BRANCHES[$i]}")"; then
      continue
    fi
    if own_commits "${j}" | xargs -r git show --format= --name-only | sort -u |
      comm -12 - <(printf '%s\n' "${files[@]}") | grep -q .; then
      found+=("${BRANCHES[$j]}")
    fi
  done
  echo "${found[*]:-the base}"
}

cmd_build() {
  local count="${1:-${#BRANCHES[@]}}" head i c parent tree out
  # The indices of the series applied so far.
  local -a applied=()
  [[ "${count}" =~ ^[0-9]+$ && "${count}" -le ${#BRANCHES[@]} ]] || die "build: bad count ${count}"
  head="$(base_commit)"
  for ((i = 0; i < count; i++)); do
    check_one "${i}" || die "${BRANCHES[$i]} fails its checks; main is not built"
    for c in $(own_commits "${i}"); do
      parent="$(git rev-parse "${c}^")"
      if [[ "${parent}" == "${head}" ]]; then
        head="${c}"
        continue
      fi
      if ! out="$(git merge-tree --write-tree --name-only --messages --merge-base="${parent}" "${head}" "${c}")"; then
        echo "${out}" >&2
        die "${BRANCHES[$i]}: ${c:0:12} conflicts with $(conflicting "${out}" "${i}" "${applied[@]}"); rebase it onto the series it conflicts with and declare it on that series"
      fi
      tree="$(head -n 1 <<<"${out}")"
      if [[ "${tree}" == "$(git rev-parse "${head}^{tree}")" ]]; then
        die "${BRANCHES[$i]}: ${c:0:12} changes nothing on top of what is applied before it; it duplicates an earlier series' change"
      fi
      head="$(commit_like "${c}" "${tree}" "${head}")"
    done
    applied+=("${i}")
  done
  echo "${head}"
}

cmd_list() {
  local i
  for i in "${!BRANCHES[@]}"; do
    echo "${BRANCHES[$i]} $(tip_of "${BRANCHES[$i]}") $(own_commits "${i}" | wc -l)"
  done
}

[[ $# -ge 1 ]] || die "usage: series.sh check [BRANCH...] | build [COUNT] | base | list"
parse_series
case "$1" in
  check) shift && cmd_check "$@" ;;
  build) shift && cmd_build "$@" ;;
  base) echo "${BASE}" ;;
  list) cmd_list ;;
  *) die "unknown command $1" ;;
esac
