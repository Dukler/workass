#!/bin/zsh
# The updater: rig.zsh BIN TAG [RENDERER_DIR]
source ${0:a:h}/lib.zsh "$@"
echo "=== $TAG  $($BIN --version 2>/dev/null | head -1)  real npm=$(curl -s https://registry.npmjs.org/@anthropic-ai/claude-code/latest | python3 -I -c 'import sys,json;print(json.load(sys.stdin)["version"])') real native=$(curl -s https://downloads.claude.ai/claude-code-releases/latest)"

NATIVE=$(curl -s https://downloads.claude.ai/claude-code-releases/latest)
echo "--- 1. installed == native channel, npm ahead: the operator's exact state"
up $NATIVE ok; wait_claude; echo "card: $(card)"
if [[ "$(card)" != '(no card)' ]]; then
  click_update; echo "after click: $(wait_terminal)"; offer; shot $TAG-1-failed
  rail "document.querySelector('.uretry')?.click()" >/dev/null; sleep 2; echo "after Reintentar: $(wait_terminal)"; offer
else
  echo "direct providers:update: $(rail "window.api.providersUpdate('claude').then(r=>JSON.stringify(r),e=>String(e)).catch(String)")"; shot $TAG-1-no-offer
fi
grep -a 'provider update finished' $P/logs/daemon.log | tail -2 | cut -c21-220

OLD=2.1.300
echo "--- 2. a real pending update, the updater installs it"
up $OLD ok; wait_claude; echo "card: $(card)"; click_update; sleep 2; echo "after click: $(wait_terminal)"; sleep 1; offer; ls -l $P/home/.local/bin/claude | sed 's/.* -> //;s#.*/##'

echo "--- 3. the updater really fails, then Reintentar with it working"
up $OLD fail; wait_claude; click_update; echo "after click: $(wait_terminal)"; offer; shot $TAG-3-fail
print -r -- ok > $P/home/stub-mode; rail "document.querySelector('.uretry')?.click()" >/dev/null; sleep 3; echo "after Reintentar: $(wait_terminal)"; offer

echo "--- 4. the updater is killed"
up $OLD kill; wait_claude; click_update; echo "after click: $(wait_terminal)"; offer; shot $TAG-4-killed

echo "--- 5. exits 0 without installing"
up $OLD noinstall; wait_claude; click_update; sleep 1; echo "after click: $(wait_terminal)"; offer; shot $TAG-5-noinstall

echo "--- 6. subagents across an update that really swaps the CLI, then one stopped from the rail"
up $OLD slow; wait_claude; mockchat; own
A=$(spawn across-update); B=$(spawn stopped-by-rail); sleep 2; echo "ids $A $B"; states
pgrep -f "$P" >/dev/null; click_update; echo "update running: $(card)"; sleep 1; echo "after click: $(wait_terminal)"; sleep 1; offer
ls -l $P/home/.local/bin/claude | sed 's/.* -> //;s#.*/##'; echo "children after the update:"; states
echo "rail stop: $(rail "window.api.spawnedWorkStop('$TAB','$CHAT','$B').then(r=>JSON.stringify(r))")"; sleep 3; states
touch $P/release-hold; own; tool workass_message_subagent "{\"tab_id\":\"$TAB\",\"chat_id\":\"$CHAT\",\"subagent_id\":\"$A\",\"operation_id\":\"m-$RANDOM\",\"message\":\"finish\"}" >/dev/null 2>&1; sleep 6; echo "after release:"; states
own; for r in $P/state/subagent-receipts/*.jsonl; do python3 -I -c "
import sys,json
for l in open(sys.argv[1]):
    d=json.loads(l); print('  receipt',d.get('label'),'|',d.get('status'),'|',(d.get('result') or '')[:40])" $r; done
down; echo "=== end $TAG"
