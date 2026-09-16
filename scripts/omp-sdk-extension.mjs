// Loaded by the user's installed OMP. Use its public extension SDK exports;
// leave the CLI's stdin/stdout and extension-loader guards alone.
import net from 'node:net';
import { serveOMP } from './omp-native-host.mjs';

export default function workassSDK(api) {
  const port = Number(process.env.WORKASS_OMP_BRIDGE_PORT);
  const nonce = process.env.WORKASS_OMP_BRIDGE_NONCE;
  if (!Number.isInteger(port) || port < 1 || port > 65535 || !nonce) throw new Error('Missing Workass SDK bridge');
  // Do not pass the bootstrap credentials to native tools/subagents.
  delete process.env.WORKASS_OMP_EXTENSION;
  delete process.env.WORKASS_OMP_BRIDGE_PORT;
  delete process.env.WORKASS_OMP_BRIDGE_NONCE;
  api.on('session_start', () => {
    const socket = net.createConnection({ host: '127.0.0.1', port });
    socket.once('connect', () => {
      socket.write(`${nonce}\n`);
      void serveOMP({ sdkModule: api.pi, input: socket, output: socket }).catch(() => socket.destroy());
    });
    socket.on('error', () => socket.destroy());
  });
}
