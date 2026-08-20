#!/bin/sh
set -eu

MAX_LOGICAL_BYTES=8388608

usage() {
  printf 'usage: %s check FILE\n' "$0" >&2
  printf '       %s search-count PATTERN FILE\n' "$0" >&2
  exit 64
}

fail() {
  printf 'unsafe profile file: %s\n' "$1" >&2
  exit 65
}

case "${1:-}" in
  check)
    [ "$#" -eq 2 ] || usage
    mode=check
    candidate_file=$2
    ;;
  search-count)
    [ "$#" -eq 3 ] || usage
    mode=search-count
    search_pattern=$2
    candidate_file=$3
    ;;
  *) usage ;;
esac

[ ! -L "$candidate_file" ] || fail 'symbolic links are not accepted'
[ -f "$candidate_file" ] || fail 'expected one explicit regular file'

candidate_dir=$(CDPATH= cd -- "$(dirname -- "$candidate_file")" && pwd -P)
candidate_name=$(basename -- "$candidate_file")
canonical_file="$candidate_dir/$candidate_name"

case "$canonical_file" in
  */apple-container|*/apple-container/*|\
  */update-feed|*/update-feed/*|\
  */cache|*/cache/*|*/Cache|*/Cache/*|\
  */attachments|*/attachments/*|*/blobs|*/blobs/*|\
  */browser-data|*/browser-data/*)
    fail 'path belongs to a non-text Workass data tree'
    ;;
esac

if logical_bytes=$(stat -f '%z' "$canonical_file" 2>/dev/null); then
  allocated_blocks=$(stat -f '%b' "$canonical_file")
else
  logical_bytes=$(stat -c '%s' "$canonical_file")
  allocated_blocks=$(stat -c '%b' "$canonical_file")
fi
allocated_bytes=$((allocated_blocks * 512))

[ "$logical_bytes" -le "$MAX_LOGICAL_BYTES" ] || fail 'logical size exceeds 8 MiB'
[ "$allocated_bytes" -ge "$logical_bytes" ] || fail 'sparse or compressed files are not accepted'

mime_type=$(file --brief --mime-type -- "$canonical_file")
case "$mime_type" in
  text/*|application/json|application/x-ndjson|application/xml|application/javascript|application/x-shellscript|inode/x-empty) ;;
  *) fail "non-text MIME type $mime_type" ;;
esac

printf 'safe_file=%s logical_bytes=%s allocated_bytes=%s mime=%s\n' \
  "$canonical_file" "$logical_bytes" "$allocated_bytes" "$mime_type"

[ "$mode" = search-count ] || exit 0

set +e
match_count=$(rg --count-matches --no-filename -- "$search_pattern" "$canonical_file" 2>/dev/null)
search_status=$?
set -e
case "$search_status" in
  0) printf 'match_count=%s\n' "$match_count" ;;
  1) printf 'match_count=0\n' ;;
  *) fail 'bounded text search failed' ;;
esac
