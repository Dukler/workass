#!/bin/sh

# Release artifacts copy shell and configuration files directly from the source
# tree. A verified binary input is therefore valid only beside the exact clean
# commit that produced it; keep that gate identical in every entrypoint.

workass_release_require_source() {
  source_repo_root=$1
  source_branch=$(git -C "$source_repo_root" branch --show-current)
  [ "$source_branch" = main ] || {
    echo "release requires the primary main worktree" >&2
    return 1
  }
  [ -z "$(git -C "$source_repo_root" status --porcelain=v1 --untracked-files=all)" ] || {
    echo "release requires a clean primary worktree" >&2
    return 1
  }
  WORKASS_RELEASE_COMMIT=$(git -C "$source_repo_root" rev-parse HEAD)
  source_upstream=$(git -C "$source_repo_root" rev-parse '@{upstream}' 2>/dev/null) || {
    echo "release requires a configured upstream" >&2
    return 1
  }
  [ "$WORKASS_RELEASE_COMMIT" = "$source_upstream" ] || {
    echo "release requires main and its upstream to be identical" >&2
    return 1
  }
  export WORKASS_RELEASE_COMMIT
}

# CFBundleVersion must be numeric, while interrupted release retries must use
# the exact same build identity without another manually copied argument. The
# UTC commit time is stable for the immutable release commit and retains the
# established sortable YYYYMMDDhhmmss shape.
workass_release_build_number() {
  build_repo_root=$1
  build_commit=$2
  build_number=$(TZ=UTC git -C "$build_repo_root" show -s \
    --format=%cd --date=format-local:%Y%m%d%H%M%S "$build_commit") || return 1
  case "$build_number" in
    ''|*[!0-9]*|0) echo "cannot derive release build number from $build_commit" >&2; return 1 ;;
  esac
  printf '%s\n' "$build_number"
}
