'use strict';

// Resolve packaged CommonJS dependencies without executing Electron startup.
// This runs against the staged app, not the source tree, on both platforms.
const fs = require('node:fs');
const path = require('node:path');
const { createRequire } = require('node:module');

function checkShellDependencies(directory) {
  const root = fs.realpathSync(directory);
  const files = fs.readdirSync(root).filter((name) => name.endsWith('.js'));
  if (!files.includes('main.js') || !files.includes('preload.js')) throw new Error('packaged shell entrypoints missing');
  const visited = new Set();
  const check = (file) => {
    if (visited.has(file)) return;
    visited.add(file);
    const source = fs.readFileSync(file, 'utf8');
    const resolve = createRequire(file).resolve;
    for (const match of source.matchAll(/\brequire\s*\(\s*(['"])(\.{1,2}\/[^'"]+)\1\s*\)/gu)) {
      let dependency;
      try { dependency = resolve(match[2]); }
      catch { throw new Error(`packaged shell dependency missing: ${path.basename(file)} requires ${match[2]}`); }
      const relative = path.relative(root, fs.realpathSync(dependency));
      if (relative.startsWith(`..${path.sep}`) || relative === '..' || path.isAbsolute(relative)) {
        throw new Error(`packaged shell dependency escapes app: ${match[2]}`);
      }
      if (dependency.endsWith('.js') || dependency.endsWith('.cjs')) check(dependency);
    }
  };
  for (const name of files) check(path.join(root, name));
  return visited.size;
}

if (require.main === module) {
  try {
    if (process.argv.length !== 3) throw new Error('usage: check-shell-dependencies.cjs STAGED_APP_DIRECTORY');
    console.log(`PACKAGED_SHELL_DEPENDENCIES_OK files=${checkShellDependencies(process.argv[2])}`);
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
module.exports = { checkShellDependencies };
