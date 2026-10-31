#!/bin/sh
# A stand-in for the native Claude Code launcher, installed only under the rig's
# own HOME. It repeats what the real `claude update` printed in an isolated HOME
# and reads the same public release pointer, so no real CLI is ever replaced.
V=__VERSION__
case "$1" in
  --version) echo "$V (Claude Code)"; exit 0;;
  update) ;;
  *) exit 1;;
esac
echo "Current version: $V"
echo "Checking for updates to latest version..."
mode=$(cat "$HOME/stub-mode" 2>/dev/null)
case "$mode" in
  kill) kill -9 $$;;
  fail) echo "Error: could not download the release" >&2; exit 3;;
  slow) sleep 6;;
esac
T=$(curl -s --max-time 10 https://downloads.claude.ai/claude-code-releases/latest)
if [ "$T" = "$V" ] || [ "$mode" = noinstall ]; then echo "Claude Code is up to date ($V)"; exit 0; fi
D="$HOME/.local/share/claude/versions"
sed "s/^V=.*/V=$T/" "$D/$V" > "$D/$T"; chmod +x "$D/$T"
ln -sfn "$D/$T" "$HOME/.local/bin/claude"
echo "Successfully updated from $V to version $T"
