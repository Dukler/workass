#!/usr/bin/env node
import { run } from 'node:test';
import { spec } from 'node:test/reporters';
import path from 'node:path';

const testFile = process.argv[2];
if (!testFile) {
  console.error('Usage: node scripts/test-native-host-suite.mjs <test-file>');
  process.exitCode = 2;
} else {
  let failed = false;
  const tests = run({
    files: [path.resolve(testFile)],
    concurrency: 4,
    isolation: 'none',
  });
  tests.on('test:fail', () => { failed = true; });
  tests.on('error', error => {
    failed = true;
    console.error(error);
  });
  try {
    await new Promise((resolve, reject) => {
      const reporter = spec();
      reporter.on('error', reject);
      reporter.on('finish', resolve);
      tests.pipe(reporter).pipe(process.stdout);
    });
  } catch (error) {
    failed = true;
    console.error(error);
  }
  if (failed) process.exitCode = 1;
}
