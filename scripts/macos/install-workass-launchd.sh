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
# Keep the historical command name/arguments for callers, but never install a
# service. The server is started once and has no restart or login supervisor.
case "$label" in
  *[!A-Za-z0-9._-]*|'') echo "invalid legacy service label: $label" >&2; exit 2 ;;
esac
plist="$HOME/Library/LaunchAgents/$label.plist"
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

mkdir -p "$state_dir" "$log_dir"

# Remove a legacy registration before starting an ordinary process. Never
# re-enable the job, including labels the user explicitly disabled.
if launchctl print "gui/$(id -u)/$label" >/dev/null 2>&1; then
  launchctl bootout "gui/$(id -u)/$label"
fi
rm -f "$plist"

node - "$script_dir/../../desktop/shell/runtime-bootstrap.js" "$exe" "$state_dir" "$port" "$bind" "$working_dir" "$log_dir" "$runtime_path" "$runtime_home" <<'JS'
const [bootstrapPath, executable, stateDir, port, bind, workingDir, logRoot, runtimePath, runtimeHome] = process.argv.slice(2);
const path = require('node:path');
const { spawnPortableDaemon } = require(bootstrapPath);
const runtime = {
  profile: process.env.WORKASS_PROFILE || 'dev', stateDir, daemonPort: Number(port), daemonBind: bind, logRoot,
  dataRoot: process.env.WORKASS_DATA_ROOT || path.dirname(stateDir),
  browserControlFile: process.env.WORKASS_BROWSER_CONTROL_FILE || path.join(path.dirname(stateDir), 'run', 'browser-control.json'),
};
const { child } = spawnPortableDaemon({
  runtime, executable, platform: 'darwin', workingDir,
  env: { PATH: runtimePath, HOME: runtimeHome },
});
child.once('error', () => { console.error('Workass server could not be started'); process.exitCode = 1; });
child.once('spawn', () => { console.log(`started Workass server once: pid=${child.pid}`); });
JS
