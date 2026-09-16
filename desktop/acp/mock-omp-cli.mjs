#!/usr/bin/env node
// Deterministic installed-CLI fixture: only the documented extension surface.
import { pathToFileURL } from 'node:url';
import * as pi from './mock-omp-sdk.mjs';
const args = process.argv.slice(2);
if (args.includes('acp') || args.includes('--auto-approve')) throw new Error('Unexpected harness arguments');
if (!args.includes('--no-session') || args[args.indexOf('--mode') + 1] !== 'rpc') throw new Error('Expected idle native SDK loader');
const extension = await import(pathToFileURL(args[args.indexOf('--extension') + 1]).href);
const handlers = [];
extension.default({ pi, on(event, handler) { if (event === 'session_start') handlers.push(handler); } });
for (const handler of handlers) await handler();
process.stdout.write('{"type":"ready"}\n');
process.stdin.resume();
