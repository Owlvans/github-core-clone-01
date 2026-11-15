import * as http from 'node:http';
import * as crypto from 'node:crypto';
import * as zlib from 'node:zlib';
import { handleGraphQL } from './graphql.ts';
import { handleMcp } from './mcp.ts';
import {
  encodePkt,
  encodeFlush,
  encodeDelim,
  encodeResponseEnd,
  parsePktLines,
  hashObject,
  parseTree,
  serializeTree,
  parseCommit,
  serializeCommit,
  parsePackfile,
  buildPackfile
} from './git.ts';
import type {
  GitObjectType,
  ParsedObject
} from './git.ts';

// Logical clock & ID counters
let logicalClock = 0;
function nextTimestamp(): string {
  logicalClock += 1;
  const epoch = 1735689600 + logicalClock;
  return new Date(epoch * 1000).toISOString();
}

let repoIdCounter = 5000;
let requestIdCounter = 0;
let globalNodeIdCounter = 1;

function nextRequestId(): string {
  requestIdCounter += 1;
  return `req_${requestIdCounter}`;
}

export function globalNodeId(typeName: string, dbId: number): string {
  return Buffer.from(`0X:${typeName}${dbId}`).toString('base64');
}

export function validateAuth(req: http.IncomingMessage): boolean {
  const authHeader = req.headers['authorization'];
  const expectedToken = process.env.MAKEFUN_SHOP_TOKEN;
  if (!authHeader || !authHeader.startsWith('Bearer ')) {
    return false;
  }
  const token = authHeader.substring(7).trim();
  if (expectedToken) {
    if (token !== expectedToken) {
      return false;
    }
  } else {
    if (!token) {
      return false;
    }
  }
  return true;
}

// Data models
interface User {
  login: string;
  id: number;
  name: string;
  email: string;
  type: string;
}

export const VIEWER: User = {
  login: 'adaiversen',
  id: 1000,
  name: 'ada iversen',
  email: 'adaiversen@example.invalid',
  type: 'User'
};

interface Repository {
  id: number;
  node_id: string;
  name: string;
  full_name: string;
  owner: User;
  private: boolean;
  description: string | null;
  default_branch: string;
  html_url: string;
  clone_url: string;
  created_at: string;
  updated_at: string;
  // Git state
  objects: Map<string, { type: GitObjectType; raw: Buffer }>;
  refs: Map<string, string>; // refname -> sha
  // Forge metadata
  labels: Map<string, { name: string; color: string }>;
  issues: Map<number, Issue>;
  pulls: Map<number, PullRequest>;
  comments: Map<number, Comment[]>; // issue/PR number -> comments
}

interface Issue {
  id: number;
  node_id: string;
  number: number;
  title: string;
  body: string | null;
  state: 'open' | 'closed';
  labels: { name: string; color?: string }[];
  user: User;
  comments: number;
  created_at: string;
  updated_at: string;
  html_url: string;
}

interface PullRequest {
  id: number;
  node_id: string;
  number: number;
  title: string;
  body: string | null;
  state: 'open' | 'closed';
  head: { ref: string; sha: string; label: string };
  base: { ref: string; sha: string; label: string };
  merged: boolean;
  mergeable: boolean | null;
  user: User;
  created_at: string;
  updated_at: string;
  merged_at: string | null;
  html_url: string;
  merged_by: User | null;
  merge_commit_sha: string | null;
}

interface Comment {
  id: number;
  node_id: string;
  body: string;
  user: User;
  created_at: string;
  updated_at: string;
  html_url: string;
}

export const repositories = new Map<string, Repository>(); // repoName -> Repository (owner is always adaiversen)

// Helper: resolve tree recursively to get file blob sha or directory entries
function getTreeEntryByPath(repo: Repository, treeSha: string, pathParts: string[]): { sha: string; type: 'blob' | 'tree'; mode?: string } | null {
  const obj = repo.objects.get(treeSha);
  if (!obj || obj.type !== 'tree') return null;
  const entries = parseTree(obj.raw);
  const currentName = pathParts[0];
  const entry = entries.find(e => e.name === currentName);
  if (!entry) return null;

  if (pathParts.length === 1) {
    return { sha: entry.sha, type: entry.mode === '40000' ? 'tree' : 'blob', mode: entry.mode };
  } else {
    if (entry.mode !== '40000') return null;
    return getTreeEntryByPath(repo, entry.sha, pathParts.slice(1));
  }
}

function getDirectoryEntries(repo: Repository, treeSha: string): Array<{ name: string; path: string; sha: string; type: 'file' | 'dir'; size: number; mode: string }> {
  const obj = repo.objects.get(treeSha);
  if (!obj || obj.type !== 'tree') return [];
  const entries = parseTree(obj.raw);
  const result: Array<{ name: string; path: string; sha: string; type: 'file' | 'dir'; size: number; mode: string }> = [];
  for (const e of entries) {
    const isDir = e.mode === '40000';
    let size = 0;
    if (!isDir) {
      const blobObj = repo.objects.get(e.sha);
      if (blobObj) size = blobObj.raw.length;
    }
    result.push({
      name: e.name,
      path: e.name, // relative path at this level
      sha: e.sha,
      type: isDir ? 'dir' : 'file',
      size,
      mode: e.mode
    });
  }
  return result;
}

// Tree builder / modifier for contents API
function updateTreeAtPath(repo: Repository, currentTreeSha: string | undefined, pathParts: string[], blobSha: string | null): string {
  const entries = currentTreeSha && repo.objects.has(currentTreeSha)
    ? parseTree(repo.objects.get(currentTreeSha)!.raw)
    : [];

  const currentName = pathParts[0];
  const isLeaf = pathParts.length === 1;

  if (isLeaf) {
    const filtered = entries.filter(e => e.name !== currentName);
    if (blobSha !== null) {
      filtered.push({ mode: '100644', name: currentName, sha: blobSha });
    }
    const newRaw = serializeTree(filtered);
    const { sha: newTreeSha } = hashObject('tree', newRaw);
    repo.objects.set(newTreeSha, { type: 'tree', raw: newRaw });
    return newTreeSha;
  } else {
    const existing = entries.find(e => e.name === currentName && e.mode === '40000');
    const subTreeSha = existing ? existing.sha : undefined;
    const newSubTreeSha = updateTreeAtPath(repo, subTreeSha, pathParts.slice(1), blobSha);

    const filtered = entries.filter(e => e.name !== currentName);
    filtered.push({ mode: '40000', name: currentName, sha: newSubTreeSha });
    const newRaw = serializeTree(filtered);
    const { sha: newTreeSha } = hashObject('tree', newRaw);
    repo.objects.set(newTreeSha, { type: 'tree', raw: newRaw });
    return newTreeSha;
  }
}

// Tree diff helper for PR files and merge-base / conflict detection
function getTreeFiles(repo: Repository, treeSha: string, prefix = ''): Map<string, string> {
  const map = new Map<string, string>(); // path -> blobSha
  const obj = repo.objects.get(treeSha);
  if (!obj || obj.type !== 'tree') return map;
  const entries = parseTree(obj.raw);
  for (const e of entries) {
    const fullPath = prefix ? `${prefix}/${e.name}` : e.name;
    if (e.mode === '40000') {
      const sub = getTreeFiles(repo, e.sha, fullPath);
      for (const [k, v] of sub.entries()) {
        map.set(k, v);
      }
    } else {
      map.set(fullPath, e.sha);
    }
  }
  return map;
}

export function computeTreeDiff(repo: Repository, baseTreeSha: string, headTreeSha: string) {
  const baseFiles = getTreeFiles(repo, baseTreeSha);
  const headFiles = getTreeFiles(repo, headTreeSha);
  const allPaths = new Set([...baseFiles.keys(), ...headFiles.keys()]);
  const diffs: Array<{ filename: string; status: 'added' | 'modified' | 'removed'; additions: number; deletions: number; changes: number }> = [];

  for (const path of allPaths) {
    const baseSha = baseFiles.get(path);
    const headSha = headFiles.get(path);
    if (!baseSha && headSha) {
      const size = repo.objects.get(headSha)?.raw.length || 0;
      diffs.push({ filename: path, status: 'added', additions: size, deletions: 0, changes: size });
    } else if (baseSha && !headSha) {
      const size = repo.objects.get(baseSha)?.raw.length || 0;
      diffs.push({ filename: path, status: 'removed', additions: 0, deletions: size, changes: size });
    } else if (baseSha && headSha && baseSha !== headSha) {
      diffs.push({ filename: path, status: 'modified', additions: 1, deletions: 1, changes: 2 });
    }
  }
  return diffs;
}

// Find merge base between head tip and base tip
function findMergeBase(repo: Repository, headTip: string, baseTip: string): string | null {
  const baseAncestors = new Set<string>();
  const queueBase = [baseTip];
  while (queueBase.length > 0) {
    const sha = queueBase.shift()!;
    if (baseAncestors.has(sha)) continue;
    baseAncestors.add(sha);
    const commitObj = repo.objects.get(sha);
    if (commitObj && commitObj.type === 'commit') {
      const parsed = parseCommit(commitObj.raw);
      for (const p of parsed.parents) {
        queueBase.push(p);
      }
    }
  }

  const queueHead = [headTip];
  const visitedHead = new Set<string>();
  while (queueHead.length > 0) {
    const sha = queueHead.shift()!;
    if (baseAncestors.has(sha)) return sha;
    if (visitedHead.has(sha)) continue;
    visitedHead.add(sha);
    const commitObj = repo.objects.get(sha);
    if (commitObj && commitObj.type === 'commit') {
      const parsed = parseCommit(commitObj.raw);
      for (const p of parsed.parents) {
        queueHead.push(p);
      }
    }
  }
  return null;
}

export function checkConflict(repo: Repository, headTip: string, baseTip: string): boolean {
  if (headTip === baseTip) return false;
  const B = findMergeBase(repo, headTip, baseTip);
  const headCommitObj = repo.objects.get(headTip);
  const baseCommitObj = repo.objects.get(baseTip);
  const headTreeSha = headCommitObj && headCommitObj.type === 'commit' ? parseCommit(headCommitObj.raw).tree : '';
  const baseTreeNowSha = baseCommitObj && baseCommitObj.type === 'commit' ? parseCommit(baseCommitObj.raw).tree : '';
  const headFiles = getTreeFiles(repo, headTreeSha);
  const baseFilesNow = getTreeFiles(repo, baseTreeNowSha);

  if (!B) {
    const allPaths = new Set([...headFiles.keys(), ...baseFilesNow.keys()]);
    for (const p of allPaths) {
      const h = headFiles.get(p);
      const n = baseFilesNow.get(p);
      if (h !== undefined && n !== undefined && h !== n) {
        return true;
      }
    }
    return false;
  }

  const bCommitObj = repo.objects.get(B);
  const bTreeSha = bCommitObj && bCommitObj.type === 'commit' ? parseCommit(bCommitObj.raw).tree : '';
  const bFiles = getTreeFiles(repo, bTreeSha);

  const allPaths = new Set([...bFiles.keys(), ...headFiles.keys(), ...baseFilesNow.keys()]);
  for (const p of allPaths) {
    const b = bFiles.get(p);
    const h = headFiles.get(p);
    const n = baseFilesNow.get(p);
    const differsHead = h !== b;
    const differsBase = n !== b;
    const differEachOther = h !== n;
    if (differsHead && differsBase && differEachOther) {
      return true;
    }
  }
  return false;
}

// Check if commit B is a descendant of commit A
function isDescendant(repo: Repository, commitA: string, commitB: string): boolean {
  if (commitA === commitB) return true;
  const visited = new Set<string>();
  const queue = [commitB];
  while (queue.length > 0) {
    const sha = queue.shift()!;
    if (sha === commitA) return true;
    if (visited.has(sha)) continue;
    visited.add(sha);
    const obj = repo.objects.get(sha);
    if (obj && obj.type === 'commit') {
      const parsed = parseCommit(obj.raw);
      for (const p of parsed.parents) {
        queue.push(p);
      }
    }
  }
  return false;
}

// HTTP Server creation
interface FetchSelection {
  objectShas: Set<string>;
  shallow: string[];
  unshallow: string[];
}

// Object selection for upload-pack fetches (v2 and v0 share it).
// held = everything reachable from haves + client-declared shallow tips (never re-sent).
// Depth counts from the wants through held commits too (deepen semantics);
// the shallow boundary is recomputed over what the client holds after the fetch.
function selectFetchObjects(
  repo: Repository,
  wants: string[],
  haves: string[],
  depth: number | null,
  clientShallow: string[],
): FetchSelection {
  // Full object closure of the starts. Commits in stopParents are included
  // (with their trees) but their parents are NOT followed: a shallow
  // client's knowledge ends exactly at its declared shallow boundary, even
  // when it also lists the boundary commit as a have.
  const shallowSet = new Set(clientShallow);
  const objectClosure = (starts: string[]): Set<string> => {
    const seen = new Set<string>();
    const queue = [...starts];
    while (queue.length) {
      const sha = queue.shift()!;
      if (seen.has(sha)) continue;
      const obj = repo.objects.get(sha);
      if (!obj) continue;
      seen.add(sha);
      if (obj.type === 'commit') {
        const parsed = parseCommit(obj.raw);
        queue.push(parsed.tree);
        if (!shallowSet.has(sha)) queue.push(...parsed.parents);
      } else if (obj.type === 'tree') {
        for (const e of parseTree(obj.raw)) queue.push(e.sha);
      } else if (obj.type === 'tag') {
        const m = /^object ([0-9a-f]{40})/m.exec(obj.raw.toString('utf-8'));
        if (m) queue.push(m[1]);
      }
    }
    return seen;
  };

  const held = objectClosure([...haves, ...clientShallow]);
  const objectShas = new Set<string>();

  // Resolve wants to start commits; tag objects themselves are sendable.
  const startCommits: string[] = [];
  for (const w of wants) {
    let cur = w;
    for (let hops = 0; hops < 8; hops++) {
      const obj = repo.objects.get(cur);
      if (!obj) break;
      if (obj.type === 'tag') {
        if (!held.has(cur)) objectShas.add(cur);
        const m = /^object ([0-9a-f]{40})/m.exec(obj.raw.toString('utf-8'));
        if (!m) break;
        cur = m[1];
        continue;
      }
      if (obj.type === 'commit') startCommits.push(cur);
      break;
    }
  }

  // BFS over commits with depth (tip = depth 1), traversing through held commits.
  const depthOf = new Map<string, number>();
  let frontier: string[] = [];
  for (const c of startCommits) {
    if (!depthOf.has(c)) { depthOf.set(c, 1); frontier.push(c); }
  }
  let d = 1;
  const visited = new Set<string>();
  while (frontier.length) {
    const next: string[] = [];
    for (const c of frontier) {
      const obj = repo.objects.get(c);
      if (!obj || obj.type !== 'commit') continue;
      visited.add(c);
      if (depth !== null && d >= depth) continue;
      for (const p of parseCommit(obj.raw).parents) {
        if (!depthOf.has(p) && repo.objects.has(p)) { depthOf.set(p, d + 1); next.push(p); }
      }
    }
    frontier = next;
    d += 1;
  }

  // Sendable commits: visited minus held; plus each one's tree/blob closure
  // (NOT its parents — depth limits and have-negotiation bound the history).
  for (const c of visited) {
    if (held.has(c)) continue;
    objectShas.add(c);
    const commitObj = repo.objects.get(c);
    if (!commitObj || commitObj.type !== 'commit') continue;
    const treeQueue = [parseCommit(commitObj.raw).tree];
    const treeSeen = new Set<string>();
    while (treeQueue.length) {
      const t = treeQueue.shift()!;
      if (treeSeen.has(t) || held.has(t)) continue;
      treeSeen.add(t);
      const tobj = repo.objects.get(t);
      if (!tobj) continue;
      objectShas.add(t);
      if (tobj.type === 'tree') {
        for (const e of parseTree(tobj.raw)) treeQueue.push(e.sha);
      }
    }
  }

  // Boundary: a visited commit with a parent that exists in the store but is
  // not in the client's possession after this fetch.
  const possession = new Set<string>(visited);
  for (const sha of held) {
    const obj = repo.objects.get(sha);
    if (obj && obj.type === 'commit') possession.add(sha);
  }
  const boundary: string[] = [];
  for (const c of visited) {
    const obj = repo.objects.get(c);
    if (!obj || obj.type !== 'commit') continue;
    const hasOutsideParent = parseCommit(obj.raw).parents.some(
      (p) => repo.objects.has(p) && !possession.has(p),
    );
    if (hasOutsideParent) boundary.push(c);
  }
  const boundarySet = new Set(boundary);
  const unshallow = clientShallow.filter((s) => visited.has(s) && !boundarySet.has(s));
  return { objectShas, shallow: boundary, unshallow };
}

export function createApp(): http.Server {
  const server = http.createServer(async (req, res) => {
    res.setHeader('x-github-request-id', nextRequestId());
    res.setHeader('access-control-allow-origin', '*');
    try {
      const urlObj = new URL(req.url || '/', `http://${req.headers.host || 'localhost'}`);
      const pathname = urlObj.pathname;

      // /healthz (no auth required)
      if (pathname === '/healthz' || pathname === '/') {
        if (pathname === '/' && !req.url?.startsWith('/api/v3')) {
          // let general root handle or healthz
        }
      }
      if (pathname === '/healthz') {
        res.writeHead(200, { 'content-type': 'application/json' });
        res.end(JSON.stringify({ ok: true }));
        return;
      }

      // Auth check
      const authHeader = req.headers['authorization'];
      const expectedToken = process.env.MAKEFUN_SHOP_TOKEN;
      if (!authHeader || !authHeader.startsWith('Bearer ')) {
        res.writeHead(401, { 'content-type': 'application/json' });
        res.end(JSON.stringify({ message: 'Requires authentication' }));
        return;
      }
      const token = authHeader.substring(7).trim();
      if (expectedToken) {
        if (token !== expectedToken) {
          res.writeHead(401, { 'content-type': 'application/json' });
          res.end(JSON.stringify({ message: 'Bad credentials' }));
          return;
        }
      } else {
        if (!token) {
          res.writeHead(401, { 'content-type': 'application/json' });
          res.end(JSON.stringify({ message: 'Requires authentication' }));
          return;
        }
      }

      // Route handling: Git Smart HTTP (/{owner}/{repo}.git/... or /{repo}.git/...)
      let gitMatch = pathname.match(/^\/([^\/]+)\/([^\/]+)\.git(\/.*)?$/);
      let repoName: string;
      let gitPath: string;
      if (gitMatch) {
        const owner = gitMatch[1];
        if (owner !== VIEWER.login) {
          res.writeHead(404, { 'content-type': 'application/json' });
          res.end(JSON.stringify({ message: 'Not Found' }));
          return;
        }
        repoName = gitMatch[2];
        gitPath = gitMatch[3] || '';
      } else {
        const shortMatch = pathname.match(/^\/([^\/]+)\.git(\/.*)?$/);
        if (shortMatch) {
          repoName = shortMatch[1];
          gitPath = shortMatch[2] || '';
          gitMatch = shortMatch as any;
        }
      }

      if (gitMatch) {
        let repo = repositories.get(repoName);
        if (!repo) {
          // auto-create empty repo for git pushes/clones if needed, or return 404 if not found
          // If repo doesn't exist on upload-pack -> 404. On receive-pack -> create if missing or 404.
          if (gitPath.includes('receive-pack')) {
            // create repo on the fly if user is owner
            repo = createRepositoryInternal(repoName, null, false);
          } else {
            res.writeHead(404, { 'content-type': 'application/json' });
            res.end(JSON.stringify({ message: 'Repository not found' }));
            return;
          }
        }

        await handleGitSmartHttp(req, res, repo, gitPath, urlObj);
        return;
      }

      // REST v3 API (/api/v3/...)
      if (pathname.startsWith('/api/v3')) {
        await handleRestApi(req, res, pathname.substring(7) || '/', urlObj);
        return;
      }

      if (pathname === '/api/graphql') {
        await handleGraphQL(req, res);
        return;
      }

      if (pathname === '/mcp') {
        await handleMcp(req, res);
        return;
      }

      // Unknown route
      res.writeHead(404, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ message: 'Not Found' }));
    } catch (err) {
      if (!res.headersSent) {
        res.writeHead(500, { 'content-type': 'application/json' });
        res.end(JSON.stringify({ message: err instanceof Error ? err.message : String(err) }));
      }
    }
  });

  return server;
}

export function createRepositoryInternal(name: string, description: string | null, privateRepo: boolean): Repository {
  repoIdCounter += 1;
  const repo: Repository = {
    id: repoIdCounter,
    node_id: globalNodeId('Repository', repoIdCounter),
    name,
    full_name: `${VIEWER.login}/${name}`,
    owner: VIEWER,
    private: privateRepo,
    description,
    default_branch: 'main',
    html_url: `http://localhost/`,
    clone_url: `http://localhost/${VIEWER.login}/${name}.git`,
    created_at: nextTimestamp(),
    updated_at: nextTimestamp(),
    objects: new Map(),
    refs: new Map(),
    labels: new Map(),
    issues: new Map(),
    pulls: new Map(),
    comments: new Map()
  };
  repositories.set(name, repo);
  return repo;
}

// Git Smart HTTP Handlers
async function handleGitSmartHttp(req: http.IncomingMessage, res: http.ServerResponse, repo: Repository, gitPath: string, urlObj: URL) {
  const serviceQuery = urlObj.searchParams.get('service');
  const protocolHeader = req.headers['git-protocol'] as string | undefined;

  // GET /info/refs?service=...
  if (gitPath === '/info/refs') {
    if (serviceQuery === 'git-upload-pack') {
      res.setHeader('content-type', 'application/x-git-upload-pack-advertisement');
      res.writeHead(200);
      const bufs: Buffer[] = [];
      if (protocolHeader?.includes('version=2')) {
        bufs.push(encodePkt('version 2\n'));
        bufs.push(encodePkt('agent=forge/1.0\n'));
        bufs.push(encodePkt('ls-refs=unborn\n'));
        bufs.push(encodePkt('fetch=shallow wait-for-done\n'));
        bufs.push(encodePkt('object-format=sha1\n'));
        bufs.push(encodeFlush());
      } else {
        bufs.push(encodePkt('# service=git-upload-pack\n'));
        bufs.push(encodeFlush());

        const headSha = repo.refs.get('HEAD') || repo.refs.get('refs/heads/' + repo.default_branch) || '0000000000000000000000000000000000000000';
        const caps = 'side-band-64k ofs-delta shallow multi_ack_detailed agent=forge/1.0';
        
        const sortedRefs = Array.from(repo.refs.entries()).sort((a, b) => a[0].localeCompare(b[0]));
        // ensure HEAD is first if present
        let firstLine = `${headSha} HEAD\0${caps}\n`;
        if (repo.refs.size === 0) {
          firstLine = `0000000000000000000000000000000000000000 HEAD\0${caps}\n`;
        }
        bufs.push(encodePkt(firstLine));

        for (const [refname, sha] of sortedRefs) {
          if (refname === 'HEAD') continue;
          bufs.push(encodePkt(`${sha} ${refname}\n`));
        }
        bufs.push(encodeFlush());
      }
      res.end(Buffer.concat(bufs));
      return;
    } else if (serviceQuery === 'git-receive-pack') {
      res.setHeader('content-type', 'application/x-git-receive-pack-advertisement');
      res.writeHead(200);
      const bufs: Buffer[] = [];
      bufs.push(encodePkt('# service=git-receive-pack\n'));
      bufs.push(encodeFlush());

      const caps = 'report-status side-band-64k delete-refs ofs-delta';
      const sortedRefs = Array.from(repo.refs.entries()).sort((a, b) => a[0].localeCompare(b[0]));
      let first = true;
      if (sortedRefs.length === 0) {
        bufs.push(encodePkt(`0000000000000000000000000000000000000000 refs/heads/main\0${caps}\n`));
      } else {
        for (const [refname, sha] of sortedRefs) {
          if (first) {
            bufs.push(encodePkt(`${sha} ${refname}\0${caps}\n`));
            first = false;
          } else {
            bufs.push(encodePkt(`${sha} ${refname}\n`));
          }
        }
      }
      bufs.push(encodeFlush());
      res.end(Buffer.concat(bufs));
      return;
    }
  }

  // Read full body for POST requests
  const chunks: Buffer[] = [];
  for await (const chunk of req) {
    chunks.push(chunk as Buffer);
  }
  const bodyBuf = Buffer.concat(chunks);

  // POST /git-upload-pack
  if (gitPath === '/git-upload-pack') {
    res.setHeader('content-type', 'application/x-git-upload-pack-result');
    res.writeHead(200);

    if (protocolHeader?.includes('version=2')) {
      // v2 protocol: parse command and args
      const { lines } = parsePktLines(bodyBuf);
      let command = '';
      const args: string[] = [];
      let mode: 'cmd' | 'args' = 'cmd';

      for (const line of lines) {
        const text = line.toString('utf-8').trim();
        if (text === '0001' || line.equals(encodeDelim())) {
          mode = 'args';
          continue;
        }
        if (text === '0000' || line.equals(encodeFlush())) {
          continue;
        }
        if (mode === 'cmd' && text.startsWith('command=')) {
          command = text.substring(8);
        } else if (mode === 'args') {
          args.push(text);
        }
      }

      if (command === 'ls-refs') {
        const wantPeel = args.includes('peel');
        const wantSymrefs = args.includes('symrefs');
        const wantUnborn = args.includes('unborn');
        const prefixes = args.filter((a) => a.startsWith('ref-prefix ')).map((a) => a.substring('ref-prefix '.length));
        const matchesPrefix = (name: string): boolean =>
          prefixes.length === 0 || prefixes.some((p) => name.startsWith(p));
        const bufs: Buffer[] = [];
        const sortedRefs = Array.from(repo.refs.entries()).sort((a, b) => a[0].localeCompare(b[0]));
        const headSha = repo.refs.get('refs/heads/' + repo.default_branch);
        if (matchesPrefix('HEAD')) {
          const symref = wantSymrefs ? ` symref-target:refs/heads/${repo.default_branch}` : '';
          if (headSha) {
            bufs.push(encodePkt(`${headSha} HEAD${symref}\n`));
          } else if (wantUnborn) {
            bufs.push(encodePkt(`unborn HEAD${symref}\n`));
          } else {
            bufs.push(encodePkt(`0000000000000000000000000000000000000000 HEAD${symref}\n`));
          }
        }
        for (const [refname, sha] of sortedRefs) {
          if (refname === 'HEAD') continue;
          if (!matchesPrefix(refname)) continue;
          let line = `${sha} ${refname}`;
          if (wantPeel) {
            // Peel annotated tags only: follow the tag chain to the first
            // non-tag object. Lightweight tags get no peeled attribute.
            const direct = repo.objects.get(sha);
            if (direct && direct.type === 'tag') {
              let cur = sha;
              for (let hops = 0; hops < 8; hops++) {
                const obj = repo.objects.get(cur);
                if (!obj || obj.type !== 'tag') break;
                const m = /^object ([0-9a-f]{40})/m.exec(obj.raw.toString('utf-8'));
                if (!m) { cur = ''; break; }
                cur = m[1];
              }
              const target = repo.objects.get(cur);
              if (target && target.type !== 'tag') line += ` peeled:${cur}`;
            }
          }
          bufs.push(encodePkt(line + '\n'));
        }
        bufs.push(encodeFlush());
        res.end(Buffer.concat(bufs));
        return;
      } else if (command === 'fetch') {
        const wants: string[] = [];
        const haves: string[] = [];
        const clientShallow: string[] = [];
        let depth: number | null = null;
        let requestDone = false;
        for (const a of args) {
          if (a.startsWith('want ')) wants.push(a.substring(5).trim());
          else if (a.startsWith('have ')) haves.push(a.substring(5).trim());
          else if (a.startsWith('shallow ')) clientShallow.push(a.substring(8).trim());
          else if (a.startsWith('deepen ')) depth = parseInt(a.substring(7).trim(), 10);
          else if (a === 'done') requestDone = true;
        }

        const selection = selectFetchObjects(repo, wants, haves, depth, clientShallow);
        const objectsToPack: Array<{ type: GitObjectType; raw: Buffer }> = [];
        for (const sha of selection.objectShas) {
          const obj = repo.objects.get(sha);
          if (obj) objectsToPack.push({ type: obj.type, raw: obj.raw });
        }
        const packBuf = buildPackfile(objectsToPack);

        const bufs: Buffer[] = [];
        // acknowledgments section
        bufs.push(encodePkt('acknowledgments\n'));
        const common = haves.filter((h) => repo.objects.has(h));
        if (common.length === 0) {
          bufs.push(encodePkt('NAK\n'));
        } else {
          for (const h of common) bufs.push(encodePkt(`ACK ${h}\n`));
          if (requestDone) bufs.push(encodePkt(`ACK ${common[common.length - 1]} ready\n`));
        }
        bufs.push(encodeDelim());
        // shallow-info section (only when it carries information)
        if (selection.shallow.length || selection.unshallow.length) {
          bufs.push(encodePkt('shallow-info\n'));
          for (const s of selection.shallow) bufs.push(encodePkt(`shallow ${s}\n`));
          for (const u of selection.unshallow) bufs.push(encodePkt(`unshallow ${u}\n`));
          bufs.push(encodeDelim());
        }
        // packfile section: sideband-1 chunks, then a final flush. Protocol v2
        // fetch responses must NOT end with a response-end packet here.
        bufs.push(encodePkt('packfile\n'));
        let offset = 0;
        while (offset < packBuf.length) {
          const chunk = packBuf.subarray(offset, offset + 65515);
          const pktData = Buffer.concat([Buffer.from([1]), chunk]);
          bufs.push(encodePkt(pktData));
          offset += 65515;
        }
        bufs.push(encodeFlush());

        res.end(Buffer.concat(bufs));
        return;
      }
    } else {
      // v0 upload-pack
      const { lines } = parsePktLines(bodyBuf);
      const wants: string[] = [];
      const haves: string[] = [];
      const clientShallow: string[] = [];
      let depth: number | null = null;

      for (const line of lines) {
        const text = line.toString('utf-8').trim();
        if (text.startsWith('want ')) {
          wants.push(text.substring(5).split(' ')[0]);
        } else if (text.startsWith('have ')) {
          haves.push(text.substring(5).trim());
        } else if (text.startsWith('shallow ')) {
          clientShallow.push(text.substring(8).trim());
        } else if (text.startsWith('deepen ')) {
          depth = parseInt(text.substring(7).trim(), 10);
        }
      }

      const selection = selectFetchObjects(repo, wants, haves, depth, clientShallow);
      const objectsToPack: Array<{ type: GitObjectType; raw: Buffer }> = [];
      for (const sha of selection.objectShas) {
        const obj = repo.objects.get(sha);
        if (obj) {
          objectsToPack.push({ type: obj.type, raw: obj.raw });
        }
      }

      const packBuf = buildPackfile(objectsToPack);
      const bufs: Buffer[] = [];
      for (const s of selection.shallow) bufs.push(encodePkt(`shallow ${s}\n`));
      for (const u of selection.unshallow) bufs.push(encodePkt(`unshallow ${u}\n`));
      const common = haves.filter((h) => repo.objects.has(h));
      if (common.length === 0) {
        bufs.push(encodePkt('NAK\n'));
      } else {
        for (const h of common) bufs.push(encodePkt(`ACK ${h}\n`));
      }

      let offset = 0;
      while (offset < packBuf.length) {
        const chunk = packBuf.subarray(offset, offset + 65515);
        const pktData = Buffer.concat([Buffer.from([1]), chunk]);
        bufs.push(encodePkt(pktData));
        offset += 65515;
      }
      bufs.push(encodeFlush());
      res.end(Buffer.concat(bufs));
      return;
    }
  }

  // POST /git-receive-pack
  if (gitPath === '/git-receive-pack') {
    res.setHeader('content-type', 'application/x-git-receive-pack-result');
    res.writeHead(200);

    const { lines, remainder } = parsePktLines(bodyBuf);
    const commands: Array<{ oldOid: string; newOid: string; refname: string }> = [];
    let hasSideband = false;

    if (lines.length > 0 && !lines[0].equals(encodeFlush())) {
      const firstLineText = lines[0].toString('utf-8');
      if (firstLineText.includes('side-band-64k')) {
        hasSideband = true;
      }
    }

    for (let i = 0; i < lines.length; i++) {
      const line = lines[i];
      if (line.equals(encodeFlush())) break;
      const text = line.toString('utf-8');
      const trimmed = text.trim();
      const parts = trimmed.split(/\s+/);
      if (parts.length >= 3) {
        const oldOid = parts[0];
        const newOid = parts[1];
        let refnameToken = parts[2];
        let refname = refnameToken;
        if (refnameToken.includes('\0')) {
          const subParts = refnameToken.split('\0');
          refname = subParts[0];
        }
        commands.push({ oldOid, newOid, refname });
      }
    }

    // Parse packfile from remainder or rest of bodyBuf
    // Find where packfile starts ('PACK' magic)
    let packOffset = bodyBuf.indexOf('PACK');
    if (packOffset !== -1) {
      const packBuf = bodyBuf.subarray(packOffset);
      const parsedObjects = parsePackfile(packBuf);
      for (const obj of parsedObjects) {
        repo.objects.set(obj.sha, { type: obj.type, raw: obj.raw });
      }
    }

    // Process ref updates with fast-forward validation
    const results: string[] = [];
    results.push('unpack ok\n');

    for (const cmd of commands) {
      const zeroOid = '0000000000000000000000000000000000000000';
      if (cmd.newOid === zeroOid) {
        // Delete ref
        repo.refs.delete(cmd.refname);
        results.push(`ok ${cmd.refname}\n`);
      } else if (cmd.oldOid === zeroOid) {
        // Create ref
        repo.refs.set(cmd.refname, cmd.newOid);
        results.push(`ok ${cmd.refname}\n`);
      } else {
        // Update ref: check fast-forward
        const currentSha = repo.refs.get(cmd.refname);
        if (currentSha && currentSha !== cmd.oldOid) {
          results.push(`ng ${cmd.refname} non-fast-forward\n`);
        } else if (isDescendant(repo, cmd.oldOid, cmd.newOid)) {
          repo.refs.set(cmd.refname, cmd.newOid);
          results.push(`ok ${cmd.refname}\n`);
        } else {
          results.push(`ng ${cmd.refname} non-fast-forward\n`);
        }
      }
    }

    if (hasSideband) {
      const reportLineBufs = results.map(r => encodePkt(r));
      reportLineBufs.push(encodeFlush());
      const reportStream = Buffer.concat(reportLineBufs);

      const bufs: Buffer[] = [];
      let offset = 0;
      while (offset < reportStream.length) {
        const chunk = reportStream.subarray(offset, offset + 65515);
        bufs.push(encodePkt(Buffer.concat([Buffer.from([1]), chunk])));
        offset += 65515;
      }
      bufs.push(encodeFlush());
      res.end(Buffer.concat(bufs));
    } else {
      const bufs: Buffer[] = [];
      for (const r of results) {
        bufs.push(encodePkt(r));
      }
      bufs.push(encodeFlush());
      res.end(Buffer.concat(bufs));
    }
    return;
  }

  res.writeHead(404, { 'content-type': 'application/json' });
  res.end(JSON.stringify({ message: 'Not Found' }));
}

// REST v3 Handlers
async function handleRestApi(req: http.IncomingMessage, res: http.ServerResponse, apiPath: string, urlObj: URL) {
  res.setHeader('x-github-request-id', nextRequestId());
  res.setHeader('access-control-allow-origin', '*');

  const method = req.method || 'GET';

  // Helper to send JSON response
  const sendJson = (status: number, data: any, headers?: Record<string, string>) => {
    const h: Record<string, string> = { 'content-type': 'application/json', ...(headers || {}) };
    res.writeHead(status, h);
    res.end(JSON.stringify(data));
  };

  // Helper to parse JSON body
  async function parseBody(): Promise<any> {
    const chunks: Buffer[] = [];
    for await (const chunk of req) {
      chunks.push(chunk as Buffer);
    }
    const buf = Buffer.concat(chunks);
    if (buf.length === 0) return {};
    try {
      return JSON.parse(buf.toString('utf-8'));
    } catch {
      throw new Error('Malformed JSON');
    }
  }

  // GET /api/v3/
  if (apiPath === '/' || apiPath === '') {
    sendJson(200, { current_user_url: 'http://localhost/api/v3/user', current_user_authorization_html_url: 'http://localhost/settings/connections/applications{/client_id}', current_authorizations_html_url: 'http://localhost/settings/tokens{/client_id}', authorisations_url: 'http://localhost/api/v3/authorizations', user_repositories_url: 'http://localhost/api/v3/users/{user}/repos{?type,page,per_page,sort}' }, { 'x-oauth-scopes': 'repo, read:org' });
    return;
  }

  // GET /api/v3/meta
  if (apiPath === '/meta') {
    sendJson(200, { installed_version: '1.0.0' });
    return;
  }

  // GET /api/v3/user
  if (apiPath === '/user') {
    sendJson(200, VIEWER);
    return;
  }

  // GET /api/v3/user/repos or POST /api/v3/user/repos
  if (apiPath === '/user/repos') {
    if (method === 'GET') {
      const perPage = parseInt(urlObj.searchParams.get('per_page') || '30', 10);
      const page = parseInt(urlObj.searchParams.get('page') || '1', 10);
      const allRepos = Array.from(repositories.values());
      const start = (page - 1) * perPage;
      const slice = allRepos.slice(start, start + perPage);

      const host = req.headers.host || 'localhost';
      const nextLink = start + perPage < allRepos.length ? `<http://${host}/api/v3/user/repos?page=${page + 1}&per_page=${perPage}>; rel="next"` : '';
      const headers: Record<string, string> = {};
      if (nextLink) headers['link'] = nextLink;

      sendJson(200, slice.map(repoToJson), headers);
      return;
    } else if (method === 'POST') {
      try {
        const body = await parseBody();
        const { name, description = null, private: isPrivate = false } = body;
        if (!name || typeof name !== 'string') {
          sendJson(422, { message: 'Repository name is required' });
          return;
        }
        if (repositories.has(name)) {
          sendJson(422, { message: 'Repository already exists' });
          return;
        }
        const repo = createRepositoryInternal(name, description, isPrivate);
        sendJson(201, repoToJson(repo));
        return;
      } catch {
        sendJson(400, { message: 'Malformed JSON' });
        return;
      }
    }
  }

  // Repo specific routes: /repos/{owner}/{repo}/...
  const repoMatch = apiPath.match(/^\/repos\/([^\/]+)\/([^\/]+)(\/.*)?$/);
  if (repoMatch) {
    const owner = repoMatch[1];
    const repoName = repoMatch[2];
    const subPath = repoMatch[3] || '/';

    if (owner !== VIEWER.login) {
      sendJson(404, { message: 'Not Found' });
      return;
    }

    const repo = repositories.get(repoName);
    if (!repo) {
      sendJson(404, { message: 'Repository not found' });
      return;
    }

    // GET /repos/{o}/{r}, PATCH, DELETE
    if (subPath === '/') {
      if (method === 'GET') {
        sendJson(200, repoToJson(repo));
        return;
      } else if (method === 'PATCH') {
        try {
          const body = await parseBody();
          if (body.description !== undefined) repo.description = body.description;
          if (body.private !== undefined) repo.private = body.private;
          repo.updated_at = nextTimestamp();
          sendJson(200, repoToJson(repo));
          return;
        } catch {
          sendJson(400, { message: 'Malformed JSON' });
          return;
        }
      } else if (method === 'DELETE') {
        repositories.delete(repoName);
        res.writeHead(204);
        res.end();
        return;
      }
    }

    // GET /repos/{o}/{r}/branches
    if (subPath === '/branches') {
      if (method === 'GET') {
        const branches: any[] = [];
        for (const [refname, sha] of repo.refs.entries()) {
          if (refname.startsWith('refs/heads/')) {
            const name = refname.substring(11);
            branches.push({ name, commit: { sha, url: `http://localhost/api/v3/repos/${owner}/${repoName}/commits/${sha}` } });
          }
        }
        sendJson(200, branches);
        return;
      }
    }

    // POST /repos/{o}/{r}/git/refs or GET /repos/{o}/{r}/git/refs/...
    if (subPath.startsWith('/git/refs')) {
      if (method === 'POST') {
        try {
          const body = await parseBody();
          const { ref, sha } = body;
          if (!ref || !sha) {
            sendJson(422, { message: 'ref and sha required' });
            return;
          }
          repo.refs.set(ref, sha);
          sendJson(201, { ref, object: { sha, type: 'commit', url: `http://localhost/api/v3/repos/${owner}/${repoName}/git/commits/${sha}` } });
          return;
        } catch {
          sendJson(400, { message: 'Malformed JSON' });
          return;
        }
      }
    }

    // GET /repos/{o}/{r}/commits
    if (subPath === '/commits') {
      if (method === 'GET') {
        const value = urlObj.searchParams.get('sha');
        let shaParam = '';
        if (value && repo.refs.has('refs/heads/' + value)) {
          shaParam = repo.refs.get('refs/heads/' + value)!;
        } else if (value && repo.refs.has('refs/tags/' + value)) {
          shaParam = repo.refs.get('refs/tags/' + value)!;
        } else if (value && repo.objects.has(value)) {
          shaParam = value;
        } else {
          shaParam = repo.refs.get('refs/heads/' + repo.default_branch) || repo.refs.get('HEAD') || '';
        }
        const commits: any[] = [];
        let curr = shaParam;
        const visited = new Set<string>();

        while (curr && repo.objects.has(curr) && !visited.has(curr)) {
          visited.add(curr);
          const obj = repo.objects.get(curr)!;
          if (obj.type !== 'commit') break;
          const parsed = parseCommit(obj.raw);
          commits.push({
            sha: curr,
            commit: {
              author: { name: 'ada iversen', email: 'adaiversen@example.invalid', date: nextTimestamp() },
              committer: { name: 'ada iversen', email: 'adaiversen@example.invalid', date: nextTimestamp() },
              message: parsed.message
            },
            author: VIEWER,
            committer: VIEWER,
            parents: parsed.parents.map(p => ({ sha: p, url: `http://localhost/api/v3/repos/${owner}/${repoName}/commits/${p}` }))
          });
          curr = parsed.parents[0]; // first parent
        }
        sendJson(200, commits);
        return;
      }
    }

    // Contents API: /repos/{o}/{r}/contents/{path}
    const contentsMatch = subPath.match(/^\/contents(\/.+)?$/);
    if (contentsMatch) {
      const rawPath = contentsMatch[1] ? contentsMatch[1].substring(1) : '';
      const pathParts = rawPath ? rawPath.split('/') : [];
      const refParam = urlObj.searchParams.get('ref') || 'refs/heads/' + repo.default_branch;
      const commitSha = repo.refs.get(refParam) || repo.refs.get('refs/heads/' + refParam) || repo.refs.get('HEAD') || refParam;

      const commitObj = repo.objects.get(commitSha);
      let treeSha = '';
      if (commitObj && commitObj.type === 'commit') {
        treeSha = parseCommit(commitObj.raw).tree;
      } else if (commitObj && commitObj.type === 'tree') {
        treeSha = commitSha;
      }

      if (method === 'GET') {
        if (!treeSha) {
          sendJson(404, { message: 'Not Found' });
          return;
        }

        if (rawPath === '') {
          // Root directory listing
          const entries = getDirectoryEntries(repo, treeSha);
          sendJson(200, entries);
          return;
        }

        const entry = getTreeEntryByPath(repo, treeSha, pathParts);
        if (!entry) {
          sendJson(404, { message: 'Not Found' });
          return;
        }

        if (entry.type === 'tree') {
          const entries = getDirectoryEntries(repo, entry.sha);
          sendJson(200, entries);
          return;
        } else {
          const blobObj = repo.objects.get(entry.sha);
          if (!blobObj) {
            sendJson(404, { message: 'Not Found' });
            return;
          }
          sendJson(200, {
            name: pathParts[pathParts.length - 1],
            path: rawPath,
            sha: entry.sha,
            size: blobObj.raw.length,
            type: 'file',
            encoding: 'base64',
            content: blobObj.raw.toString('base64'),
            html_url: `http://localhost/${owner}/${repoName}/blob/${refParam}/${rawPath}`
          });
          return;
        }
      } else if (method === 'PUT') {
        try {
          const body = await parseBody();
          const { message, content, sha: expectedSha, branch = repo.default_branch } = body;
          if (!message || content === undefined) {
            sendJson(422, { message: 'message and content required' });
            return;
          }

          const decodedContent = Buffer.from(content, 'base64');
          const { sha: newBlobSha } = hashObject('blob', decodedContent);
          repo.objects.set(newBlobSha, { type: 'blob', raw: decodedContent });

          const branchRef = 'refs/heads/' + branch;
          const currentCommitSha = repo.refs.get(branchRef);
          let currentTreeSha = '';
          const parents: string[] = [];
          if (currentCommitSha) {
            parents.push(currentCommitSha);
            const parentCommitObj = repo.objects.get(currentCommitSha);
            if (parentCommitObj && parentCommitObj.type === 'commit') {
              currentTreeSha = parseCommit(parentCommitObj.raw).tree;
            }
          }

          // Check expectedSha if provided
          if (expectedSha && rawPath) {
            const existingEntry = getTreeEntryByPath(repo, currentTreeSha, pathParts);
            if (existingEntry && existingEntry.sha !== expectedSha) {
              sendJson(409, { message: 'Conflict: sha does not match current file sha' });
              return;
            }
          }

          const newTreeSha = updateTreeAtPath(repo, currentTreeSha || undefined, pathParts, newBlobSha);

          const commitRaw = serializeCommit({
            tree: newTreeSha,
            parents,
            author: `${VIEWER.name} <${VIEWER.email}> ${nextTimestamp()} +0000`,
            committer: `${VIEWER.name} <${VIEWER.email}> ${nextTimestamp()} +0000`,
            message
          });
          const { sha: newCommitSha } = hashObject('commit', commitRaw);
          repo.objects.set(newCommitSha, { type: 'commit', raw: commitRaw });

          repo.refs.set(branchRef, newCommitSha);

          const isNew = !expectedSha && !getTreeEntryByPath(repo, currentTreeSha, pathParts);
          sendJson(isNew ? 201 : 200, {
            content: {
              name: pathParts[pathParts.length - 1],
              path: rawPath,
              sha: newBlobSha,
              size: decodedContent.length,
              type: 'file',
              encoding: 'base64'
            },
            commit: {
              sha: newCommitSha,
              message,
              tree: newTreeSha,
              parents: parents.map(p => ({ sha: p }))
            }
          });
          return;
        } catch {
          sendJson(400, { message: 'Malformed JSON or invalid request' });
          return;
        }
      } else if (method === 'DELETE') {
        try {
          const body = await parseBody();
          const { message, sha: expectedSha, branch = repo.default_branch } = body;
          if (!message) {
            sendJson(422, { message: 'message required' });
            return;
          }

          const branchRef = 'refs/heads/' + branch;
          const currentCommitSha = repo.refs.get(branchRef);
          let currentTreeSha = '';
          const parents: string[] = [];
          if (currentCommitSha) {
            parents.push(currentCommitSha);
            const parentCommitObj = repo.objects.get(currentCommitSha);
            if (parentCommitObj && parentCommitObj.type === 'commit') {
              currentTreeSha = parseCommit(parentCommitObj.raw).tree;
            }
          }

          const newTreeSha = updateTreeAtPath(repo, currentTreeSha || undefined, pathParts, null);

          const commitRaw = serializeCommit({
            tree: newTreeSha,
            parents,
            author: `${VIEWER.name} <${VIEWER.email}> ${nextTimestamp()} +0000`,
            committer: `${VIEWER.name} <${VIEWER.email}> ${nextTimestamp()} +0000`,
            message
          });
          const { sha: newCommitSha } = hashObject('commit', commitRaw);
          repo.objects.set(newCommitSha, { type: 'commit', raw: commitRaw });

          repo.refs.set(branchRef, newCommitSha);

          sendJson(200, {
            content: null,
            commit: { sha: newCommitSha, message }
          });
          return;
        } catch {
          sendJson(400, { message: 'Malformed JSON' });
          return;
        }
      }
    }

    // Labels: /repos/{o}/{r}/labels
    if (subPath === '/labels') {
      if (method === 'GET') {
        sendJson(200, Array.from(repo.labels.values()));
        return;
      } else if (method === 'POST') {
        try {
          const body = await parseBody();
          const { name, color = 'ededed' } = body;
          if (!name) {
            sendJson(422, { message: 'Label name required' });
            return;
          }
          const label = { name, color };
          repo.labels.set(name, label);
          sendJson(201, label);
          return;
        } catch {
          sendJson(400, { message: 'Malformed JSON' });
          return;
        }
      }
    }

    // Issues: /repos/{o}/{r}/issues
    if (subPath === '/issues' || subPath.startsWith('/issues/')) {
      const issueParts = subPath.substring(1).split('/');
      if (issueParts.length === 1 && method === 'GET') {
        // List issues
        const state = urlObj.searchParams.get('state') || 'open';
        const sort = urlObj.searchParams.get('sort') || 'created';
        const direction = urlObj.searchParams.get('direction') || 'desc';

        let list = Array.from(repo.issues.values());
        if (state !== 'all') {
          list = list.filter(i => i.state === state);
        }

        list.sort((a, b) => {
          const cmp = a.number - b.number;
          return direction === 'asc' ? cmp : -cmp;
        });

        sendJson(200, list);
        return;
      } else if (issueParts.length === 1 && method === 'POST') {
        // Create issue (shares number sequence with PRs)
        try {
          const body = await parseBody();
          const { title, body: issueBody = null, labels = [] } = body;
          if (!title) {
            sendJson(422, { message: 'Issue title is required' });
            return;
          }

          // Validate labels
          const formattedLabels: { name: string; color?: string }[] = [];
          for (const l of labels) {
            const lName = typeof l === 'string' ? l : l.name;
            if (!repo.labels.has(lName)) {
              sendJson(422, { message: `Label "${lName}" does not exist` });
              return;
            }
            formattedLabels.push(repo.labels.get(lName)!);
          }

          const issueNumber = repo.issues.size + repo.pulls.size + 1;
          globalNodeIdCounter += 1;
          const issue: Issue = {
            id: globalNodeIdCounter,
            node_id: globalNodeId('Issue', globalNodeIdCounter),
            number: issueNumber,
            title,
            body: issueBody,
            state: 'open',
            labels: formattedLabels,
            user: VIEWER,
            comments: 0,
            created_at: nextTimestamp(),
            updated_at: nextTimestamp(),
            html_url: `http://localhost/${owner}/${repoName}/issues/${issueNumber}`
          };
          repo.issues.set(issueNumber, issue);
          sendJson(201, issue);
          return;
        } catch {
          sendJson(400, { message: 'Malformed JSON' });
          return;
        }
      } else if (issueParts.length === 2) {
        const issueNum = parseInt(issueParts[1], 10);
        const issue = repo.issues.get(issueNum);
        if (!issue) {
          sendJson(404, { message: 'Issue not found' });
          return;
        }

        if (method === 'GET') {
          sendJson(200, issue);
          return;
        } else if (method === 'PATCH') {
          try {
            const body = await parseBody();
            if (body.state) issue.state = body.state;
            if (body.title !== undefined) issue.title = body.title;
            if (body.body !== undefined) issue.body = body.body;
            issue.updated_at = nextTimestamp();
            sendJson(200, issue);
            return;
          } catch {
            sendJson(400, { message: 'Malformed JSON' });
            return;
          }
        }
      } else if (issueParts.length === 3 && issueParts[2] === 'comments') {
        const issueNum = parseInt(issueParts[1], 10);
        const issue = repo.issues.get(issueNum);
        if (!issue) {
          sendJson(404, { message: 'Issue not found' });
          return;
        }

        if (method === 'GET') {
          const comments = repo.comments.get(issueNum) || [];
          sendJson(200, comments);
          return;
        } else if (method === 'POST') {
          try {
            const body = await parseBody();
            const { body: commentBody } = body;
            if (!commentBody) {
              sendJson(422, { message: 'Comment body required' });
              return;
            }
            globalNodeIdCounter += 1;
            const comment: Comment = {
              id: globalNodeIdCounter,
              node_id: globalNodeId('IssueComment', globalNodeIdCounter),
              body: commentBody,
              user: VIEWER,
              created_at: nextTimestamp(),
              updated_at: nextTimestamp(),
              html_url: `http://localhost/${owner}/${repoName}/issues/${issueNum}#issuecomment-${globalNodeIdCounter}`
            };
            let list = repo.comments.get(issueNum);
            if (!list) {
              list = [];
              repo.comments.set(issueNum, list);
            }
            list.push(comment);
            issue.comments = list.length;
            sendJson(201, comment);
            return;
          } catch {
            sendJson(400, { message: 'Malformed JSON' });
            return;
          }
        }
      }
    }

    // Pull Requests: /repos/{o}/{r}/pulls
    if (subPath === '/pulls' || subPath.startsWith('/pulls/')) {
      const prParts = subPath.substring(1).split('/');
      if (prParts.length === 1 && method === 'GET') {
        const state = urlObj.searchParams.get('state') || 'open';
        let list = Array.from(repo.pulls.values());
        for (const p of list) {
          p.mergeable = !checkConflict(repo, p.head.sha, repo.refs.get('refs/heads/' + p.base.ref) || p.base.sha);
        }
        if (state !== 'all') {
          list = list.filter(p => p.state === state);
        }
        sendJson(200, list);
        return;
      } else if (prParts.length === 1 && method === 'POST') {
        try {
          const body = await parseBody();
          const { title, body: prBody = null, head, base } = body;
          if (!title || !head || !base) {
            sendJson(422, { message: 'title, head, and base required' });
            return;
          }

          const headRefName = head.startsWith('refs/heads/') ? head : 'refs/heads/' + head;
          const baseRefName = base.startsWith('refs/heads/') ? base : 'refs/heads/' + base;

          const headSha = repo.refs.get(headRefName);
          const baseSha = repo.refs.get(baseRefName);

          if (!headSha || !baseSha) {
            sendJson(422, { message: 'Head or base ref not found' });
            return;
          }

          const hasConflict = checkConflict(repo, headSha, baseSha);

          const prNumber = repo.issues.size + repo.pulls.size + 1;
          globalNodeIdCounter += 1;
          const pr: PullRequest = {
            id: globalNodeIdCounter,
            node_id: globalNodeId('PullRequest', globalNodeIdCounter),
            number: prNumber,
            title,
            body: prBody,
            state: 'open',
            head: { ref: headRefName.replace('refs/heads/', ''), sha: headSha, label: `${owner}:${headRefName.replace('refs/heads/', '')}` },
            base: { ref: baseRefName.replace('refs/heads/', ''), sha: baseSha, label: `${owner}:${baseRefName.replace('refs/heads/', '')}` },
            merged: false,
            mergeable: !hasConflict,
            user: VIEWER,
            created_at: nextTimestamp(),
            updated_at: nextTimestamp(),
            merged_at: null,
            html_url: `http://localhost/${owner}/${repoName}/pull/${prNumber}`,
            merged_by: null,
            merge_commit_sha: null
          };
          repo.pulls.set(prNumber, pr);
          sendJson(201, pr);
          return;
        } catch {
          sendJson(400, { message: 'Malformed JSON' });
          return;
        }
      } else if (prParts.length >= 2) {
        const prNum = parseInt(prParts[1], 10);
        const pr = repo.pulls.get(prNum);
        if (!pr) {
          sendJson(404, { message: 'Pull request not found' });
          return;
        }
        pr.mergeable = !checkConflict(repo, pr.head.sha, repo.refs.get('refs/heads/' + pr.base.ref) || pr.base.sha);

        if (prParts.length === 2 && method === 'GET') {
          sendJson(200, pr);
          return;
        } else if (prParts.length === 3 && prParts[2] === 'files' && method === 'GET') {
          // Get PR files diff
          const baseCommitObj = repo.objects.get(pr.base.sha);
          const headCommitObj = repo.objects.get(pr.head.sha);
          if (!baseCommitObj || !headCommitObj) {
            sendJson(404, { message: 'Commit objects not found' });
            return;
          }
          const baseTreeSha = parseCommit(baseCommitObj.raw).tree;
          const headTreeSha = parseCommit(headCommitObj.raw).tree;
          const diffs = computeTreeDiff(repo, baseTreeSha, headTreeSha);
          sendJson(200, diffs);
          return;
        } else if (prParts.length === 3 && prParts[2] === 'merge' && method === 'PUT') {
          if (pr.merged) {
            sendJson(405, { message: 'Pull request already merged' });
            return;
          }
          try {
            const body = await parseBody();
            const commitTitle = body.commit_title || pr.title;

            // Perform squash merge onto base ref
            const baseRefName = 'refs/heads/' + pr.base.ref;
            const baseTipSha = repo.refs.get(baseRefName);
            const headCommitObj = repo.objects.get(pr.head.sha);
            if (!baseTipSha || !headCommitObj) {
              sendJson(405, { message: 'Cannot merge' });
              return;
            }

            if (checkConflict(repo, pr.head.sha, baseTipSha)) {
              pr.mergeable = false;
              sendJson(405, { message: 'Pull request has merge conflicts' });
              return;
            }

            const headTreeSha = parseCommit(headCommitObj.raw).tree;

            const commitRaw = serializeCommit({
              tree: headTreeSha,
              parents: [baseTipSha],
              author: `${VIEWER.name} <${VIEWER.email}> ${nextTimestamp()} +0000`,
              committer: `${VIEWER.name} <${VIEWER.email}> ${nextTimestamp()} +0000`,
              message: commitTitle
            });
            const { sha: newCommitSha } = hashObject('commit', commitRaw);
            repo.objects.set(newCommitSha, { type: 'commit', raw: commitRaw });

            repo.refs.set(baseRefName, newCommitSha);
            pr.merged = true;
            pr.state = 'closed';
            pr.merged_at = nextTimestamp();
            pr.merged_by = VIEWER;
            pr.merge_commit_sha = newCommitSha;
            pr.mergeable = false;

            sendJson(200, {
              sha: newCommitSha,
              merged: true,
              message: 'Pull Request successfully merged',
              merge_commit_sha: newCommitSha
            });
            return;
          } catch {
            sendJson(400, { message: 'Malformed JSON' });
            return;
          }
        }
      }
    }
  }

  sendJson(404, { message: 'Not Found' });
}

function repoToJson(repo: Repository) {
  return {
    id: repo.id,
    node_id: repo.node_id,
    name: repo.name,
    full_name: repo.full_name,
    owner: repo.owner,
    private: repo.private,
    html_url: repo.html_url,
    description: repo.description,
    fork: false,
    url: `http://localhost/api/v3/repos/${repo.owner.login}/${repo.name}`,
    clone_url: repo.clone_url,
    default_branch: repo.default_branch,
    created_at: repo.created_at,
    updated_at: repo.updated_at
  };
}
