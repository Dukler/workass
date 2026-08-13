#!/bin/sh
set -eu

usage() {
  echo "usage: $0 /absolute/path/to/workass [state-dir] [port] [bind] [label] [working-dir] [runtime-path] [runtime-home]" >&2
  exit 2
}

[ "$#" -ge 1 ] || usage

exe="$1"
state_dir="${2:-$HOME/Library/Application Support/Workass/state}"
port="${3:-8788}"
bind="${4:-localhost}"
label="${5:-com.workass.daemon}"
working_dir="${6:-$(dirname -- "$exe")}"
runtime_path="${7:-${PATH:-/usr/bin:/bin:/usr/sbin:/sbin}}"
runtime_home="${8:-$HOME}"

case "$exe" in
  /*) ;;
  *) echo "workass executable path must be absolute" >&2; exit 2 ;;
esac

if [ ! -x "$exe" ]; then
  echo "workass executable is not executable: $exe" >&2
  exit 1
fi

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
template="$script_dir/workass.launchd.plist"
case "$label" in
  *[!A-Za-z0-9._-]*|'') echo "invalid launchd label: $label" >&2; exit 2 ;;
esac
launch_agents="$HOME/Library/LaunchAgents"
plist="$launch_agents/$label.plist"
log_dir="${WORKASS_LOG_ROOT:-$HOME/Library/Logs/Workass}"
case "$working_dir" in
  /*) ;;
  *) echo "workass working directory must be absolute: $working_dir" >&2; exit 2 ;;
esac

if [ ! -d "$working_dir" ]; then
  echo "workass working directory does not exist: $working_dir" >&2
  exit 1
fi
case "$runtime_home" in
  /*) ;;
  *) echo "workass runtime home must be absolute: $runtime_home" >&2; exit 2 ;;
esac
[ -n "$runtime_path" ] || { echo "workass runtime PATH must not be empty" >&2; exit 2; }

mkdir -p "$launch_agents" "$state_dir" "$log_dir"

escape_sed() {
  printf '%s' "$1" | sed 's/[\/&]/\\&/g'
}

sed \
  -e "s/__LABEL__/$(escape_sed "$label")/g" \
  -e "s/__WORKASS_EXE__/$(escape_sed "$exe")/g" \
  -e "s/__STATE_DIR__/$(escape_sed "$state_dir")/g" \
  -e "s/__PORT__/$(escape_sed "$port")/g" \
  -e "s/__BIND__/$(escape_sed "$bind")/g" \
  -e "s/__WORKING_DIR__/$(escape_sed "$working_dir")/g" \
  -e "s/__LOG_DIR__/$(escape_sed "$log_dir")/g" \
  "$template" > "$plist"

# launchd does not inherit the interactive user's PATH. Official provider CLIs
# live in user/Homebrew locations, so binding only the system PATH
# produces a daemon that answers /health while advertising no usable models.
plutil -insert EnvironmentVariables -xml '<dict/>' "$plist"
plutil -insert EnvironmentVariables.PATH -string "$runtime_path" "$plist"
plutil -insert EnvironmentVariables.HOME -string "$runtime_home" "$plist"
if [ -n "${WORKASS_PROFILE:-}" ]; then
  plutil -insert EnvironmentVariables.WORKASS_PROFILE -string "$WORKASS_PROFILE" "$plist"
fi
if [ -n "${WORKASS_DATA_ROOT:-}" ]; then
  plutil -insert EnvironmentVariables.WORKASS_DATA_ROOT -string "$WORKASS_DATA_ROOT" "$plist"
fi
if [ -n "${WORKASS_BROWSER_CONTROL_FILE:-}" ]; then
  plutil -insert EnvironmentVariables.WORKASS_BROWSER_CONTROL_FILE -string "$WORKASS_BROWSER_CONTROL_FILE" "$plist"
fi

if launchctl print "gui/$(id -u)/$label" >/dev/null 2>&1; then
  launchctl bootout "gui/$(id -u)" "$plist" >/dev/null 2>&1 || true
fi
launchctl bootstrap "gui/$(id -u)" "$plist"
launchctl enable "gui/$(id -u)/$label"
launchctl kickstart -k "gui/$(id -u)/$label"

echo "installed launchd agent: $plist"
echo "executable: $exe"
echo "state dir:  $state_dir"
echo "bind:       $bind:$port"
echo "runtime PATH preserved"
echo "profile:    ${WORKASS_PROFILE:-unspecified}"
