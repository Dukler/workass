#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
WORKASS_REPO_ROOT="$repo_root"
export WORKASS_REPO_ROOT
# shellcheck disable=SC1091
. "$repo_root/scripts/lib/workass-profile.sh"

workass_load_profile prod
[ "$WORKASS_DAEMON_PORT" = 8788 ]
[ "$WORKASS_VIEW_PORT" = 8798 ]
prod_root="$WORKASS_DATA_ROOT"
prod_browser="$WORKASS_BROWSER_CONTROL_FILE"

workass_load_profile dev
[ "$WORKASS_DAEMON_PORT" = 18788 ]
[ "$WORKASS_VIEW_PORT" = 8799 ]
[ "$WORKASS_DATA_ROOT" != "$prod_root" ]
[ "$WORKASS_BROWSER_CONTROL_FILE" != "$prod_browser" ]

test_root=$(mktemp -d "${TMPDIR:-/tmp}/workass-profile-test.XXXXXX")
trap 'rm -rf "$test_root"' EXIT HUP INT TERM
WORKASS_TEST_ROOT="$test_root"
export WORKASS_TEST_ROOT
workass_load_profile test
[ "$WORKASS_DATA_ROOT" = "$test_root" ]
[ "$WORKASS_DAEMON_PORT" = 0 ]
[ "$WORKASS_VIEW_PORT" = 0 ]

echo "WORKASS_PROFILE_ISOLATION_PASS prod=8788/8798 dev=18788/8799 test=ephemeral"
