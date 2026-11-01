#!/usr/bin/env node
import fs from 'node:fs';
import { run } from 'node:test';
import { spec } from 'node:test/reporters';
import path from 'node:path';
import { testEventLine } from './test-events-reporter.mjs';

const testFile = process.argv[2];
if (!testFile) {
  console.error('Usage: node scripts/test-native-host-suite.mjs <test-file>');
  process.exitCode = 2;
} else {
  let failed = false;
  const eventsFile = process.env.WORKASS_TEST_EVENTS_FILE;
  const tests = run({
    files: [path.resolve(testFile)],
    concurrency: Number(process.env.WORKASS_TEST_CONCURRENCY) || 4,
    isolation: 'none',
  });
  tests.on('test:fail', () => { failed = true; });
  if (eventsFile) {
    const record = event => fs.appendFileSync(eventsFile, testEventLine(event));
    tests.on('test:pass', data => record({ type: 'test:pass', data }));
    tests.on('test:fail', data => record({ type: 'test:fail', data }));
  }
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
