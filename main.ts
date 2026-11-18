// The forge's launcher shim.
//
// The trial harness starts every entry the same way — `node main.ts` — and the
// competition's rules expressly allow a statically linked Linux amd64 binary
// committed to the repository to run as vendored source. The forge itself is
// that binary (./forge, built from the Go sources under go/): it implements
// the whole surface — git smart HTTP, REST v3, GraphQL v4, MCP — and the full
// environment lifecycle (bind, /register, /done) on its own.
//
// This file only conducts the hand-off:
//   - The repository mount is read-only in the trial pod, and a committed file
//     can arrive without its execute bit, so the binary is staged into the
//     temp directory and made executable there before it is spawned. On a
//     plain local checkout the staging is a no-op copy of a few megabytes.
//   - The child inherits stdio and the environment (MAKEFUN_ENV_URL,
//     MAKEFUN_SHOP_TOKEN) untouched, signals are forwarded, and this process
//     exits with the child's verdict, leaving no process behind.

import { spawn } from 'node:child_process';
import { chmodSync, copyFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = fileURLToPath(new URL('.', import.meta.url));
const committed = join(here, 'forge');

let binary = committed;
try {
  const staged = join(tmpdir(), 'forge-entry-bin');
  copyFileSync(committed, staged);
  chmodSync(staged, 0o755);
  binary = staged;
} catch {
  // Staging failed (no writable temp dir): run the committed file directly.
  binary = committed;
}

const child = spawn(binary, [], { stdio: 'inherit', env: process.env });

for (const signal of ['SIGINT', 'SIGTERM'] as const) {
  process.on(signal, () => {
    child.kill(signal);
  });
}

child.on('error', (err) => {
  process.stderr.write(`[entry] the forge binary failed to start: ${err.message}\n`);
  process.exit(1);
});

child.on('exit', (code, signal) => {
  process.exit(signal !== null ? 1 : (code ?? 1));
});
