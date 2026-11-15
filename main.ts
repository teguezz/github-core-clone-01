// The forge's lifecycle shell: how this process reports for duty and takes its leave.
//
// app.ts builds the server; this file only conducts the conversation with the trial
// environment around it, and that conversation has a strict shape:
//
//   - The environment's origin arrives as MAKEFUN_ENV_URL. Without it there is nobody
//     to register with and nothing to wait for, so the process says so on stderr and
//     exits 1 before it has bound a socket or built anything.
//   - The server binds port 0. The environment, not the entry, owns the fixed ports;
//     an ephemeral pick is the only polite choice, and the chosen number is meaningless
//     until it has been announced.
//   - Announcing is a POST of {"port": N} to /register. A 200 records us; a 409 means
//     another process registered first and keeps the slot — also a fine outcome, the
//     slot is what matters, not who holds it. A 400 or 405 says the announcement
//     itself broke the contract, which no retry can fix, so it is fatal. Everything
//     else (a 5xx from an environment still warming up, a refused connection, a
//     timeout) is retried on a doubling pause.
//   - Leaving is the environment's decision. GET /done answers at once, so the
//     process polls it on the same retry rhythm until the body says {"done": true},
//     then closes the server and exits 0. It never walks away early on its own say-so.

import { createApp } from './app.ts';

/** Where the environment lives, trailing slashes stripped so path joins stay clean. */
const envOrigin = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');

if (envOrigin === '') {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

/** One environment call gets this long before it counts as a failure. */
const ENV_CALL_TIMEOUT_MS = 30_000;
/** The pause after a first failure; it doubles per consecutive failure, capped. */
const FIRST_RETRY_MS = 250;
const MAX_RETRY_MS = 2_000;

/** setTimeout as a promise, for the retry pauses. */
const sleep = (ms: number): Promise<void> =>
  new Promise((resolve) => {
    setTimeout(resolve, ms);
  });

/**
 * The pause after `failures` failures in a row: FIRST_RETRY_MS doubled once per
 * failure — the exponent clamped at 8 so the doubling stays sane — and never more
 * than MAX_RETRY_MS.
 */
function retryDelay(failures: number): number {
  return Math.min(MAX_RETRY_MS, FIRST_RETRY_MS * 2 ** Math.min(failures, 8));
}

/** One attempt's verdict: finished (with a value), or not yet — pause and try again. */
type Attempt<T> = { finished: true; value: T } | { finished: false };

/**
 * Drive an attempt until it reports finished, pausing on the retry rhythm between
 * tries. An attempt that throws (a fatal refusal) escapes immediately: the rhythm
 * is for an environment that is not ready, not for answers that reject us outright.
 */
async function drive<T>(attempt: () => Promise<Attempt<T>>): Promise<T> {
  let failures = 0;
  for (;;) {
    const outcome = await attempt();
    if (outcome.finished) return outcome.value;
    await sleep(retryDelay(failures));
    failures += 1;
  }
}

/**
 * Announce the bound port until the environment acknowledges it. 200 (recorded) and
 * 409 (an earlier process holds the slot) both finish; 400 and 405 refuse the
 * announcement itself and are thrown as fatal; any other outcome is not finished.
 */
async function announcePort(port: number): Promise<void> {
  await drive(async (): Promise<Attempt<void>> => {
    let refusal: Error | null = null;
    try {
      const reply = await fetch(`${envOrigin}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(ENV_CALL_TIMEOUT_MS),
      });
      if (reply.status === 200 || reply.status === 409) return { finished: true, value: undefined };
      if (reply.status === 400 || reply.status === 405) {
        refusal = new Error(`register answered HTTP ${reply.status}: ${await reply.text()}`);
      }
      // Any other status: the environment is not ready to hear us yet.
    } catch {
      // Transport trouble (unreachable, aborted, timed out): also just not finished.
    }
    if (refusal !== null) throw refusal;
    return { finished: false };
  });
}

/**
 * Poll /done until the environment dismisses us. Only a 200 whose body says
 * {"done": true} finishes; every other outcome — not done yet, an error status, an
 * unreachable environment — is simply not finished.
 */
async function awaitDismissal(): Promise<void> {
  await drive(async (): Promise<Attempt<void>> => {
    try {
      const reply = await fetch(`${envOrigin}/done`, {
        signal: AbortSignal.timeout(ENV_CALL_TIMEOUT_MS),
      });
      if (reply.ok) {
        const payload = (await reply.json()) as { done?: unknown };
        if (payload.done === true) return { finished: true, value: undefined };
      }
    } catch {
      // A failed poll says nothing about dismissal; keep waiting.
    }
    return { finished: false };
  });
}

/** The whole life of the process: bind, announce, serve by waiting, leave when told. */
async function main(): Promise<void> {
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

  await announcePort(bound.port);
  process.stdout.write('[entry] registered; waiting for the env to say done\n');

  await awaitDismissal();
  server.close();
  process.stdout.write('[entry] done\n');
}

main().then(
  () => {
    process.exit(0);
  },
  (err: unknown) => {
    process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
    process.exit(1);
  },
);
