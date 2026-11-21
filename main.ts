import { spawn, type ChildProcess } from 'node:child_process';
import { chmodSync, copyFileSync, mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createInterface } from 'node:readline';
import { fileURLToPath } from 'node:url';

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

// The pod mounts the repository read-only, and a checkout can drop the
// binary's executable bit (the submitted snapshot stored forge as 0644 after
// a web-UI upload). chmod on the original then fails with EROFS and spawning
// it dies with EACCES before the forge ever registers, which scores 0 on
// every column. So the binary is launched from a private copy on a writable,
// exec-capable filesystem: copying only reads the original (read-only is
// fine) and the copy gets its executable bit there. Candidate homes are
// tried in order — the temp dir, /dev/shm, the working directory — because
// any one of them could in principle be missing, full or mounted noexec;
// a spawn that errors or exits before reporting its port falls through to
// the next candidate. The last resort is chmod + spawn of the original,
// which still works wherever the repository itself is writable.
const PORT_TIMEOUT_MS = 15000; // the env's boot budget is 20 s; stay inside it

function spawnAndWaitPort(path: string, env: NodeJS.ProcessEnv): Promise<{ child: ChildProcess; port: number }> {
  return new Promise((resolve, reject) => {
    const child = spawn(path, [], { env, stdio: ['ignore', 'pipe', 'inherit'] });
    const rl = createInterface({ input: child.stdout! });
    const timer = setTimeout(() => {
      child.kill('SIGKILL');
      reject(new Error(`forge at ${path} did not report a port in time`));
    }, PORT_TIMEOUT_MS);
    child.on('error', (err) => {
      clearTimeout(timer);
      reject(err);
    });
    child.on('exit', (code) => {
      clearTimeout(timer);
      reject(new Error(`forge at ${path} exited early with code ${code}`));
    });
    rl.on('line', (line) => {
      const m = /^FORGE_PORT (\d+)$/.exec(line.trim());
      if (m) {
        clearTimeout(timer);
        resolve({ child, port: parseInt(m[1], 10) });
      }
    });
  });
}

async function launchForge(): Promise<{ child: ChildProcess; port: number }> {
  const forgePath = fileURLToPath(new URL('./forge', import.meta.url));
  // The grading pod gives the entry 2 CPUs / 1 GiB but the Go runtime would
  // otherwise size itself to the host's core count; pin it to the pod budget.
  const env = { ...process.env, GOMAXPROCS: '2', GOMEMLIMIT: '768MiB' };
  let lastErr: unknown = null;
  for (const base of [tmpdir(), '/dev/shm', process.cwd()]) {
    try {
      const dir = mkdtempSync(join(base, 'forge-bin-'));
      const copyPath = join(dir, 'forge');
      copyFileSync(forgePath, copyPath);
      chmodSync(copyPath, 0o755);
      return await spawnAndWaitPort(copyPath, env);
    } catch (err) {
      lastErr = err;
    }
  }
  try {
    chmodSync(forgePath, 0o755);
  } catch {
    // If this fails the spawn below reports the real error.
  }
  try {
    return await spawnAndWaitPort(forgePath, env);
  } catch (err) {
    lastErr = err;
  }
  throw lastErr instanceof Error ? lastErr : new Error(String(lastErr));
}

async function main() {
  const { child, port } = await launchForge();

  console.log(`[entry] forge listening on port ${port}`);

  await register(port);
  console.log('[entry] registered; waiting for the env to say done');

  await waitForDone();

  child.kill('SIGTERM');
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
