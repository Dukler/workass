#!/usr/bin/env node

import process from 'node:process';

const strict = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/;
const parsed = process.argv.slice(2).map((value) => {
  if (!strict.test(value)) {
    process.stderr.write(`invalid release version: ${value}\n`);
    process.exit(1);
  }
  return value.split('.').map(Number);
});
if (parsed.length === 0) parsed.push([0, 0, 0]);
parsed.sort((left, right) => {
  for (let index = 0; index < 3; index += 1) {
    if (left[index] !== right[index]) return right[index] - left[index];
  }
  return 0;
});
const [major, minor, patch] = parsed[0];
process.stdout.write(`${major}.${minor}.${patch + 1}\n`);
