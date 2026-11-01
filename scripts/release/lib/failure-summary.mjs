#!/usr/bin/env node
// Prints what a release log needs a person to know, and nothing that passed.
//   failure-summary.mjs LOG           failing tests with one line of cause
//   failure-summary.mjs --flakes LOG  WORKASS_TEST_FLAKE lines only
// A release log nests: ship's log names phase logs, and those name inner phase
// logs, so every named log is followed.
import fs from 'node:fs';
import { fileURLToPath } from 'node:url';

const MAX_LINES = 40;
const FALLBACK_LINES = 8;
const NOISE = /status=passed|^WORKASS_RELEASE_(?:PHASE_START|PHASE_END|PHASE_LOG|TOTAL)|^WORKASS_GATE_PHASE .*status=passed|^WORKASS_TEST_SUITE |^WORKASS_TEST_SUITE_TOTAL|^\s*✔|^\s*﹣|^ok\b|^# (?:pass|tests|suites|skipped|todo|duration_ms)|^ℹ|^(?:receipt|commit|candidate|timings|phase_logs)=|^\s*$/;

function readLines(file) {
  try { return fs.readFileSync(file, 'utf8').split(/\r?\n/); } catch { return []; }
}

// `all` also follows the logs of phases that passed: a flake lives in one.
function logTree(root, { all = false } = {}) {
  const seen = new Set();
  const order = [];
  const visit = file => {
    if (seen.has(file)) return;
    seen.add(file);
    const lines = readLines(file);
    order.push({ file, lines });
    for (const line of lines) {
      const failedPhase = line.match(/\(full output: (.+)\)\s*$/)?.[1];
      const namedPhase = all ? line.match(/^WORKASS_RELEASE_PHASE_LOG .*\blog=(.+)$/)?.[1] : undefined;
      if (failedPhase) visit(failedPhase);
      if (namedPhase) visit(namedPhase);
    }
  };
  visit(root);
  return order;
}

function unique(lines) {
  return [...new Set(lines)];
}

export function flakeLines(root) {
  return unique(logTree(root, { all: true }).flatMap(({ lines }) => lines.filter(line => line.startsWith('WORKASS_TEST_FLAKE '))));
}

export function failureLines(root) {
  const tree = logTree(root);
  const all = tree.flatMap(({ lines }) => lines);
  const failedTests = unique(all.filter(line => line.startsWith('WORKASS_TEST_FAILED ')));
  const namedSuites = new Set(failedTests.map(line => line.match(/\bsuite=(\S+)/)?.[1]));
  const suites = unique(all.filter(line => line.startsWith('WORKASS_TEST_SUITE_FAILURE ') && !namedSuites.has(line.match(/\bname=(\S+)/)?.[1])));
  const skipped = unique(all.filter(line => line.startsWith('WORKASS_TEST_RERUN_SKIPPED ')));
  const flakes = unique(all.filter(line => line.startsWith('WORKASS_TEST_FLAKE ')));
  const phases = unique(all.filter(line => line.startsWith('release phase failed: ')));
  const tests = [...failedTests, ...skipped, ...suites];
  // Nothing test-shaped failed (a build, a signature, an upload): show the end
  // of the deepest log, which is the one that actually broke.
  const fallback = tests.length ? [] : unique(tree.at(-1).lines.filter(line => !NOISE.test(line) && !phases.includes(line))).slice(-FALLBACK_LINES);
  const selected = [...phases, ...tests, ...fallback, ...flakes];
  if (selected.length <= MAX_LINES) return selected;
  return [...selected.slice(0, MAX_LINES), `... and ${selected.length - MAX_LINES} more in the log`];
}

if (process.argv[1] && fs.realpathSync(process.argv[1]) === fs.realpathSync(fileURLToPath(import.meta.url))) {
  const args = process.argv.slice(2);
  const flakes = args[0] === '--flakes';
  const log = flakes ? args[1] : args[0];
  if (!log) {
    process.stderr.write('usage: failure-summary.mjs [--flakes] LOG\n');
    process.exit(2);
  }
  for (const line of flakes ? flakeLines(log) : failureLines(log)) process.stdout.write(`${line}\n`);
}
