// git-forge — Go entry point launcher.
//
// The forge itself is a statically linked Linux amd64 Go binary committed
// beside this file (./forge). This script only spawns it with the same
// environment, forwards stdio/signals, and exits with the child's code.
// Build the binary with:
//   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o forge .
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const dir = path.dirname(fileURLToPath(import.meta.url));
const binary = path.join(dir, 'forge');
const child = spawn(binary, [], { stdio: 'inherit', env: process.env });

const forward = (signal: NodeJS.Signals): void => {
  if (!child.kill(signal)) process.kill(process.pid, signal);
};
process.on('SIGINT', () => forward('SIGINT'));
process.on('SIGTERM', () => forward('SIGTERM'));
child.on('error', (err) => {
  process.stderr.write(`[git-forge] failed to start ./forge: ${err.message}\n`);
  process.exit(1);
});
child.on('exit', (code, signal) => {
  if (signal !== null) process.kill(process.pid, signal);
  process.exit(code ?? 1);
});
