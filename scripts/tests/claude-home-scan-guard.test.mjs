import assert from 'node:assert/strict';
import test from 'node:test';
import { homeScanDenial } from '../claude-native-host.mjs';

// The classifier behind the Claude host's macOS PreToolUse guard: a recursive
// walk rooted at /, /Users, ~, ~/Library or a container folder makes macOS ask
// "Workass would like to access data from other apps", and Allow never sticks.
const home = '/Users/fixture';
const repo = '/Users/fixture/Workspace/repo';

const denied = [
  // Bash: the scan that started it, and its common shapes.
  ['Bash', { command: 'find ~ -maxdepth 5 -name "*.sqlite" 2>/dev/null' }, repo],
  ['Bash', { command: 'find ~ -name .env' }, repo],
  ['Bash', { command: 'find $HOME -type f -name "*.plist"' }, repo],
  ['Bash', { command: 'find "${HOME}" -name x' }, repo],
  ['Bash', { command: 'find /Users/fixture -name x' }, repo],
  ['Bash', { command: 'find /Users/$USER -name x' }, repo],
  ['Bash', { command: 'find / -name libfoo.dylib 2>/dev/null | head' }, repo],
  ['Bash', { command: 'find /Users -name x' }, repo],
  ['Bash', { command: 'find ~/Library -name "*.db"' }, repo],
  ['Bash', { command: 'find ~/library -name "*.db"' }, repo],
  ['Bash', { command: 'find ~/Library/Containers -name x' }, repo],
  ['Bash', { command: 'find "$HOME/Library/Group Containers" -name x' }, repo],
  ['Bash', { command: 'find ~/Library/Group\\ Containers -name x' }, repo],
  ['Bash', { command: 'find ~ -maxdepth 3 -name x' }, repo],
  ['Bash', { command: 'find ~ -name node_modules -prune -o -name "*.json" -print' }, repo],
  ['Bash', { command: 'find . -name "*.sqlite"' }, home],
  ['Bash', { command: 'find ../.. -name x' }, repo],
  ['Bash', { command: 'cd ~ && find . -name x' }, repo],
  ['Bash', { command: 'cd && rg token' }, repo],
  ['Bash', { command: 'cd /tmp; cd ~/Library; ls -R' }, repo],
  ['Bash', { command: '(cd ~ && fd config)' }, repo],
  ['Bash', { command: 'sudo find / -name x' }, repo],
  ['Bash', { command: 'timeout 30 find ~ -name x' }, repo],
  ['Bash', { command: 'FOO=1 nice -n 5 find ~ -name x' }, repo],
  ['Bash', { command: 'if true; then find ~ -name x; fi' }, repo],
  ['Bash', { command: 'bash -lc "find ~ -name x"' }, repo],
  ['Bash', { command: 'for f in $(find ~ -name "*.log"); do echo "$f"; done' }, repo],
  ['Bash', { command: 'rg -n token ~' }, repo],
  ['Bash', { command: 'rg --files ~ | head -50' }, repo],
  ['Bash', { command: 'rg -g "*.json" apiKey /' }, repo],
  ['Bash', { command: 'rg token' }, home],
  ['Bash', { command: 'grep -rn token ~' }, repo],
  ['Bash', { command: 'grep -R --include=*.plist token ~/Library' }, repo],
  ['Bash', { command: 'grep --recursive token ~' }, repo],
  ['Bash', { command: 'grep -r token' }, home],
  ['Bash', { command: 'fd -H config ~' }, repo],
  ['Bash', { command: 'fd sqlite' }, home],
  ['Bash', { command: 'fd --base-directory ~ db' }, repo],
  ['Bash', { command: 'du -sh ~' }, repo],
  ['Bash', { command: 'du -d 1 -h ~ | sort -h' }, repo],
  ['Bash', { command: 'du -sh ~/*' }, repo],
  ['Bash', { command: 'ls -R ~' }, repo],
  ['Bash', { command: 'ls -laR ~/Library' }, repo],
  ['Bash', { command: 'tree ~' }, repo],
  ['Bash', { command: 'tree -L 4 ~' }, repo],
  ['Bash', { command: 'ag token ~' }, repo],
  ['Bash', { command: 'ls ~/**/*.json' }, repo],
  ['Bash', { command: 'echo build && find ~ -name x' }, repo],
  // Glob / Grep: the path, or the session cwd when there is none.
  ['Glob', { pattern: '**/*.sqlite', path: home }, repo],
  ['Glob', { pattern: '**/*.sqlite', path: '~' }, repo],
  ['Glob', { pattern: '**/*.plist', path: `${home}/Library` }, repo],
  ['Glob', { pattern: '**/*.go' }, home],
  ['Glob', { pattern: '/Users/fixture/**/*.plist' }, repo],
  ['Glob', { pattern: '~/Library/**' }, repo],
  ['Glob', { pattern: '**/*' , path: '/' }, repo],
  ['Grep', { pattern: 'token', path: '~' }, repo],
  ['Grep', { pattern: 'token', path: home }, repo],
  ['Grep', { pattern: 'token', path: `${home}/Library/Containers` }, repo],
  ['Grep', { pattern: 'token' }, home],
  ['Grep', { pattern: 'token' }, '/'],
];

const allowed = [
  // Specific folders, including ones inside ~/Library.
  ['Bash', { command: 'find ~/Workspace/repo -name "*.go"' }, repo],
  ['Bash', { command: 'find . -name "*.go"' }, repo],
  ['Bash', { command: 'find ~/Library/Logs -name "*.log"' }, repo],
  ['Bash', { command: 'find "$HOME/Library/Application Support/Workass" -name "*.json"' }, repo],
  ['Bash', { command: 'find /usr/local /opt/homebrew -name x' }, repo],
  ['Bash', { command: 'rg token ~/Workspace/repo' }, repo],
  ['Bash', { command: 'rg token' }, repo],
  ['Bash', { command: 'grep -rn token src' }, repo],
  ['Bash', { command: 'fd config ~/.config' }, repo],
  ['Bash', { command: 'du -sh ~/Workspace/*' }, repo],
  ['Bash', { command: 'du -sh ~/.*' }, repo],
  ['Bash', { command: 'du -sh ~/Library/Caches' }, repo],
  ['Bash', { command: 'tree -L 2 ~/Workspace' }, repo],
  ['Bash', { command: 'ls ~/Workspace/**/*.go' }, repo],
  ['Bash', { command: 'cd ~/Workspace/repo && find . -name x' }, home],
  // Not recursive at all.
  ['Bash', { command: 'ls ~' }, repo],
  ['Bash', { command: 'ls -la ~/Library' }, repo],
  ['Bash', { command: 'cat ~/x' }, repo],
  ['Bash', { command: 'grep token ~/.zshrc' }, repo],
  ['Bash', { command: 'grep -rn token ~/.zshrc' }, repo],
  ['Bash', { command: 'ls' }, home],
  ['Bash', { command: 'command -v find' }, home],
  // A depth limit that never reaches the container folders.
  ['Bash', { command: 'find ~ -maxdepth 1 -name "*.txt"' }, repo],
  ['Bash', { command: 'find ~ -maxdepth 2 -type d -name .git' }, repo],
  ['Bash', { command: 'find ~/Library -maxdepth 1' }, repo],
  ['Bash', { command: 'tree -L 2 ~' }, repo],
  ['Bash', { command: 'fd -d 1 . ~' }, repo],
  ['Bash', { command: 'rg --max-depth 1 token ~' }, repo],
  // A walk that prunes the folders in question.
  ['Bash', { command: 'find ~ -path ~/Library -prune -o -name x -print' }, repo],
  // Text that only mentions a scan, and pipes a searcher reads instead.
  ['Bash', { command: 'echo "find ~ -name x"' }, repo],
  ['Bash', { command: 'git commit -m "stop running find ~ -name x"' }, repo],
  ['Bash', { command: '# find ~ -name x\nls' }, repo],
  ['Bash', { command: 'cat > scan.sh <<\'EOF\'\nfind ~ -name x\nEOF\nchmod +x scan.sh' }, repo],
  ['Bash', { command: 'ps aux | grep -r node' }, home],
  ['Bash', { command: 'history | rg find' }, home],
  // Ambiguous: unknown variables, quoted tildes, unterminated quotes.
  ['Bash', { command: 'find "$TARGET" -name x' }, repo],
  ['Bash', { command: "find '~' -name x" }, repo],
  ['Bash', { command: 'find ~ -name "x' }, repo],
  ['Bash', { command: 'cd "$TMPDIR" && find . -name x' }, repo],
  // Glob / Grep on specific folders.
  ['Glob', { pattern: '**/*.go' }, repo],
  ['Glob', { pattern: '**/*.log', path: '~/Library/Logs' }, repo],
  ['Glob', { pattern: 'Workspace/repo/**/*.go', path: '~' }, repo],
  ['Glob', { pattern: '/Users/fixture/Workspace/**/*.go' }, repo],
  ['Grep', { pattern: 'token', path: '~/Workspace' }, repo],
  ['Grep', { pattern: 'token', path: '~/.zshrc' }, repo],
  ['Grep', { pattern: 'token' }, repo],
  // Tools the guard does not read.
  ['Read', { file_path: '~' }, home],
  ['Bash', {}, home],
];

test('home-scan guard refuses recursive walks rooted at the home folder, ~/Library or /', () => {
  for (const [tool, input, cwd] of denied) {
    const reason = homeScanDenial(tool, input, { cwd, home });
    assert.ok(reason, `expected a denial: ${tool} ${JSON.stringify(input)} from ${cwd}`);
    assert.match(reason, /^Search a specific folder instead/);
    assert.match(reason, /privacy prompt/);
  }
});

test('home-scan guard allows specific folders, shallow looks and non-recursive commands', () => {
  for (const [tool, input, cwd] of allowed) {
    assert.equal(homeScanDenial(tool, input, { cwd, home }), '', `expected no denial: ${tool} ${JSON.stringify(input)} from ${cwd}`);
  }
});

test('home-scan guard names the root it refused, in ~ form', () => {
  assert.match(homeScanDenial('Bash', { command: 'find ~/Library -name x' }, { cwd: repo, home }), /scanning ~\/Library recursively/);
  assert.match(homeScanDenial('Bash', { command: 'find / -name x' }, { cwd: repo, home }), /scanning \/ recursively/);
  assert.match(homeScanDenial('Grep', { pattern: 'x' }, { cwd: home, home }), /scanning ~ recursively/);
});

test('home-scan guard is off without an absolute home folder', () => {
  assert.equal(homeScanDenial('Bash', { command: 'find / -name x' }, { cwd: repo, home: '' }), '');
});
