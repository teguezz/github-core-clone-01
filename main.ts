// Boot-and-wait shell for the forge.
//
// This file owns no forge logic at all — app.ts builds the server. What lives here is the
// conversation with the trial environment that hosts the process, and it runs in four beats:
//
//   1. Find the environment. MAKEFUN_ENV_URL carries its origin. If the variable is missing
//      or blank there is nobody to report to, so the process complains on stderr and exits 1
//      before doing anything else.
//   2. Take whatever port the kernel offers. The server listens on port 0 (an ephemeral
//      pick) — never a fixed number, because the environment owns the well-known ones.
//   3. Introduce ourselves: POST {"port": N} to the environment's /register endpoint. It
//      notes the caller's address together with N and reaches the server at that base URL
//      from then on. Registration is first-come-first-served: a 409 means another process
//      registered earlier and won, which this process simply accepts. A 400 or 405 is a
//      contract violation and is fatal. Anything else — a refused connection, a timeout, a
//      5xx while the environment is still coming up — is retried with a growing pause.
//   4. Wait to be dismissed. GET /done answers immediately with {"done": false} while the
//      trial runs, so dismissal is polled, with the same retry rhythm, until the body says
//      {"done": true}. Only then is the server closed and the process exits 0 — it never
//      leaves early on its own judgement.

import { createApp } from './app.ts';

/** The environment's origin, with any trailing slashes trimmed so paths join cleanly. */
const envBase = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');

if (envBase === '') {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

/** Ceiling for a single register or poll request before it counts as failed. */
const REQUEST_DEADLINE_MS = 30_000;
/** Pause after the first failure; each further failure doubles it, up to the cap. */
const BACKOFF_START_MS = 250;
const BACKOFF_CAP_MS = 2_000;

/** Promise-flavoured setTimeout. */
const pause = (ms: number): Promise<void> =>
  new Promise((resolve) => {
    setTimeout(resolve, ms);
  });

/**
 * How long to rest after `failures` consecutive failures: BACKOFF_START_MS doubled once per
 * failure (the exponent is clamped so the shift stays sane) and never above BACKOFF_CAP_MS.
 */
function backoffMs(failures: number): number {
  const doubled = BACKOFF_START_MS * 2 ** Math.min(failures, 8);
  return Math.min(BACKOFF_CAP_MS, doubled);
}

/**
 * Tell the environment which port the server bound, retrying until it acknowledges us.
 * 200 (recorded) and 409 (someone registered first) both end the loop normally; 400 and
 * 405 reject the registration outright and are thrown to the caller as fatal.
 */
async function registerWithEnv(port: number): Promise<void> {
  let failures = 0;
  for (;;) {
    let refusal: Error | null = null;
    try {
      const reply = await fetch(`${envBase}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_DEADLINE_MS),
      });
      if (reply.status === 200 || reply.status === 409) return;
      if (reply.status === 400 || reply.status === 405) {
        refusal = new Error(`register answered HTTP ${reply.status}: ${await reply.text()}`);
      }
      // Any other status: the environment is not ready to hear us yet — fall through and retry.
    } catch {
      // Transport trouble (unreachable, aborted, timed out): also just a reason to retry.
    }
    if (refusal !== null) throw refusal;
    await pause(backoffMs(failures));
    failures += 1;
  }
}

/**
 * Ask the environment, again and again, whether the trial is finished. Returns only once a
 * poll answers 200 with {"done": true}; every other outcome — not done yet, an error status,
 * an unreachable environment — waits out the backoff and asks again.
 */
async function pollUntilDone(): Promise<void> {
  let failures = 0;
  for (;;) {
    let finished = false;
    try {
      const reply = await fetch(`${envBase}/done`, {
        signal: AbortSignal.timeout(REQUEST_DEADLINE_MS),
      });
      if (reply.ok) {
        const payload = (await reply.json()) as { done?: unknown };
        finished = payload.done === true;
      }
    } catch {
      finished = false;
    }
    if (finished) return;
    await pause(backoffMs(failures));
    failures += 1;
  }
}

/** The process's whole life: bind, register, serve by waiting, shut down when dismissed. */
async function start(): Promise<void> {
  const server = createApp();

  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, () => {
      resolve();
    });
  });

  const bound = server.address();
  if (bound === null || typeof bound === 'string') {
    throw new Error('the server did not bind an ephemeral TCP port');
  }
  process.stdout.write(`[entry] listening on port ${bound.port}\n`);

  await registerWithEnv(bound.port);
  process.stdout.write('[entry] registered; waiting for the env to say done\n');

  await pollUntilDone();
  server.close();
  process.stdout.write('[entry] done\n');
}

start().then(
  () => {
    process.exit(0);
  },
  (err: unknown) => {
    process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
    process.exit(1);
  },
);
