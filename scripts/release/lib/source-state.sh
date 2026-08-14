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
