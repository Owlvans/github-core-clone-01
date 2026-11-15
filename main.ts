import { createApp } from './app.ts';

const rawEnvUrl = process.env.MAKEFUN_ENV_URL || '';
const envUrl = rawEnvUrl.replace(/\/+$/, '');

if (!envUrl) {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

const REQUEST_TIMEOUT_MS = 30000;
const RETRY_INITIAL_MS = 250;
const RETRY_MAX_MS = 2000;

const retryDelay = (attempt: number) =>
  Math.min(RETRY_MAX_MS, RETRY_INITIAL_MS * Math.pow(2, Math.min(attempt, 8)));

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

async function register(port: number) {
  for (let attempt = 0; ; attempt++) {
    try {
      const res = await fetch(`${envUrl}/register`, {
        method: 'POST',
        headers: {
          'content-type': 'application/json',
        },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });

      if (res.status === 200 || res.status === 409) {
        return;
      }

      const bodyText = await res.text();
      if (res.status === 400 || res.status === 405) {
        throw new Error(`${res.status} ${bodyText}`);
      }
    } catch (err: any) {
      if (err instanceof Error && /^(400|405)\b/.test(err.message)) {
        throw err;
      }
    }
    await sleep(retryDelay(attempt));
  }
}

async function waitForDone() {
  for (let attempt = 0; ; attempt++) {
    try {
      const res = await fetch(`${envUrl}/done`, {
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
      if (res.ok) {
        const data = await res.json();
        if (data && data.done === true) {
          return;
        }
      }
    } catch {
      // swallow errors
    }
    await sleep(retryDelay(attempt));
  }
}

async function main() {
  const server = createApp();

  await new Promise<void>((resolve, reject) => {
    server.listen(0, () => resolve());
    server.on('error', reject);
  });

  const address = server.address();
  if (!address || typeof address === 'string') {
    throw new Error('Server address is null or a string');
  }

  const port = address.port;
  console.log(`[entry] listening on port ${port}`);

  await register(port);
  console.log('[entry] registered; waiting for the env to say done');

  await waitForDone();

  server.close();
  console.log('[entry] done');
}

try {
  await main();
  process.exit(0);
} catch (err: any) {
  const msg = err instanceof Error ? err.message : String(err);
  process.stderr.write(`[entry] ${msg}\n`);
  process.exit(1);
}
