#!/usr/bin/env node
// Transport only: the installed OMP owns the SDK, auth, tools and dependencies.
import net from 'node:net';
import { spawn } from 'node:child_process';
import { randomBytes, timingSafeEqual } from 'node:crypto';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

export function installedOMPCommand(executable, extension, env, platform = process.platform) {
  const args = ['--mode', 'rpc', '--no-session', '--no-tools', '--no-extensions', '--extension', extension];
  if (platform !== 'win32' || !/\.(?:cmd|bat)$/i.test(executable)) return { command: executable, args, env, windowsVerbatimArguments: false };
  if (/["\r\n]/.test(executable + extension)) throw new Error('Invalid installed OMP command path');
  // Expansion values stay inside quotes. Delayed expansion is off; neither a
  // percent sign nor shell metacharacters in an installation path become code.
  return {
    command: env.ComSpec || env.COMSPEC || 'cmd.exe',
    args: ['/d', '/v:off', '/s', '/c', '\"\"%WORKASS_OMP_EXECUTABLE%\" --mode rpc --no-session --no-tools --no-extensions --extension \"%WORKASS_OMP_EXTENSION%\"\"'],
    env: { ...env, WORKASS_OMP_EXECUTABLE: executable, WORKASS_OMP_EXTENSION: extension },
    windowsVerbatimArguments: true,
  };
}

export function runInstalledOMP() {
const executable = process.env.WORKASS_OMP_EXECUTABLE;
if (!executable) throw new Error('WORKASS_OMP_EXECUTABLE must name an installed OMP');
const nonce = randomBytes(32).toString('hex');
let child, peer, stopping = false, inputEnded = false;
const stop = (code = 0) => {
  if (stopping) return;
  stopping = true;
  clearTimeout(startup);
  server.close();
  peer?.destroy();
  if (child && child.exitCode === null) {
    child.kill();
    const kill = setTimeout(() => child.kill('SIGKILL'), 3000);
    kill.unref();
  }
  process.exitCode = code;
};
const server = net.createServer(socket => {
  if (peer) { socket.destroy(); return; }
  let header = Buffer.alloc(0);
  const timeout = setTimeout(() => socket.destroy(), 3000);
  socket.on('error', () => {});
  const authenticate = chunk => {
    header = Buffer.concat([header, chunk]);
    const newline = header.indexOf(10);
    if (newline < 0) { if (header.length > 64) socket.destroy(); return; }
    const provided = header.subarray(0, newline);
    const expected = Buffer.from(nonce);
    if (provided.length !== expected.length || !timingSafeEqual(provided, expected) || peer) { socket.destroy(); return; }
    clearTimeout(timeout);
    clearTimeout(startup);
    peer = socket;
    socket.removeListener('data', authenticate);
    server.close();
    if (header.length > newline + 1) process.stdout.write(header.subarray(newline + 1));
    process.stdin.pipe(socket);
    socket.pipe(process.stdout);
    socket.once('close', () => stop(inputEnded ? 0 : 1));
  };
  socket.on('data', authenticate);
  socket.once('close', () => clearTimeout(timeout));
});
server.on('error', () => { process.stderr.write('OMP SDK bridge could not listen\n'); stop(1); });
const startup = setTimeout(() => {
  process.stderr.write('Installed OMP did not expose its SDK extension bridge\n');
  stop(1);
}, 20000);
server.listen(0, '127.0.0.1', () => {
  const extension = path.join(path.dirname(fileURLToPath(import.meta.url)), 'omp-sdk-extension.mjs');
  const env = { ...process.env, WORKASS_OMP_BRIDGE_PORT: String(server.address().port), WORKASS_OMP_BRIDGE_NONCE: nonce };
  // BUN_BE_BUN is for running standalone Bun scripts, not loading the OMP SDK.
  delete env.BUN_BE_BUN;
  const launch = installedOMPCommand(executable, extension, env);
  child = spawn(launch.command, launch.args, { env: launch.env, stdio: ['pipe', 'pipe', 'pipe'], windowsHide: true, windowsVerbatimArguments: launch.windowsVerbatimArguments });
  // The outer CLI is an idle SDK loader. No prompts are sent to this stream.
  child.stdout.resume();
  child.stderr.resume(); // Vendor diagnostics stay in its own logs; never forward credentials.
  child.once('error', () => { process.stderr.write('Could not start installed OMP\n'); stop(1); });
  child.once('exit', code => stop(code || (peer ? 0 : 1)));
});
process.stdin.once('end', () => { inputEnded = true; if (peer) peer.end(); else stop(); });
process.stdin.once('error', () => stop(1));
process.stdout.once('error', () => stop(1));
for (const signal of ['SIGTERM', 'SIGINT']) process.on(signal, () => stop());

}
if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) runInstalledOMP();
