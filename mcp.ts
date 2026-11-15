import * as http from 'node:http';
import {
  repositories,
  VIEWER,
  createRepositoryInternal,
  checkConflict,
  validateAuth
} from './app.ts';
import {
  parseCommit,
  parseTree,
  hashObject,
  serializeCommit
} from './git.ts';

const TOOLS_LIST = [
  {
    name: 'create_repo',
    description: 'Create a new repository',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string' },
        description: { type: 'string' },
        private: { type: 'boolean' }
      },
      required: ['name']
    }
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and repo name',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string' },
        repo: { type: 'string' },
        full_name: { type: 'string' }
      },
      required: ['owner', 'repo']
    }
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string' },
        repo: { type: 'string' },
        full_name: { type: 'string' },
        title: { type: 'string' },
        body: { type: 'string' }
      },
      required: ['owner', 'repo', 'title']
    }
  },
  {
    name: 'list_issues',
    description: 'List issues in a repository',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string' },
        repo: { type: 'string' },
        full_name: { type: 'string' },
        state: { type: 'string' }
      },
      required: ['owner', 'repo']
    }
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string' },
        repo: { type: 'string' },
        full_name: { type: 'string' },
        title: { type: 'string' },
        head: { type: 'string' },
        base: { type: 'string' },
        body: { type: 'string' }
      },
      required: ['owner', 'repo', 'title', 'head', 'base']
    }
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string' },
        repo: { type: 'string' },
        full_name: { type: 'string' },
        pull_number: { type: 'integer' }
      },
      required: ['owner', 'repo', 'pull_number']
    }
  }
];

export async function handleMcp(req: http.IncomingMessage, res: http.ServerResponse): Promise<void> {
  if (!validateAuth(req)) {
    res.writeHead(401, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ message: 'Requires authentication' }));
    return;
  }

  let bodyStr = '';
  for await (const chunk of req) {
    bodyStr += chunk;
  }

  let rpc: any;
  try {
    rpc = JSON.parse(bodyStr);
  } catch {
    res.writeHead(400, { 'content-type': 'application/json' });
    res.end(JSON.stringify({
      jsonrpc: '2.0',
      id: null,
      error: { code: -32700, message: 'Parse error' }
    }));
    return;
  }

  const { jsonrpc, id, method, params = {} } = rpc;

  // Notifications
  if (id === undefined && typeof method === 'string' && method.startsWith('notifications/')) {
    res.writeHead(202, { 'content-type': 'application/json' });
    res.end();
    return;
  }

  const sendRpcResult = (result: any, headers: Record<string, string> = {}) => {
    res.writeHead(200, { 'content-type': 'application/json', ...headers });
    res.end(JSON.stringify({ jsonrpc: '2.0', id, result }));
  };

  const sendRpcError = (code: number, message: string) => {
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ jsonrpc: '2.0', id, error: { code, message } }));
  };

  if (method === 'server/discover') {
    sendRpcResult({
      serverInfo: { name: 'forge-mcp', version: '1.0.0' },
      capabilities: { tools: {} },
      supportedVersions: ['2026-07-28', '2025-11-25'],
      protocolVersion: '2026-07-28'
    });
    return;
  }

  if (method === 'initialize') {
    const protoVer = params.protocolVersion && typeof params.protocolVersion === 'string' && params.protocolVersion.startsWith('2025-')
      ? params.protocolVersion
      : '2025-11-25';
    sendRpcResult({
      protocolVersion: protoVer,
      capabilities: { tools: {} },
      serverInfo: { name: 'forge-mcp', version: '1.0.0' }
    }, { 'Mcp-Session-Id': 'forge-session-1' });
    return;
  }

  if (method === 'ping') {
    sendRpcResult({});
    return;
  }

  if (method === 'tools/list') {
    sendRpcResult({ tools: TOOLS_LIST });
    return;
  }

  if (method === 'tools/call') {
    const name = params.name;
    const args = params.arguments || {};

    if (args.full_name && typeof args.full_name === 'string') {
      const parts = args.full_name.split('/');
      if (parts.length === 2) {
        if (!args.owner) args.owner = parts[0];
        if (!args.repo) args.repo = parts[1];
      }
    }

    const toolDef = TOOLS_LIST.find(t => t.name === name);
    if (!toolDef) {
      sendRpcError(-32602, `Unknown tool: ${name}`);
      return;
    }

    for (const reqArg of toolDef.inputSchema.required) {
      if (args[reqArg] === undefined || args[reqArg] === null || args[reqArg] === '') {
        sendRpcResult({
          content: [{ type: 'text', text: `Error: missing required argument ${reqArg}` }],
          isError: true
        });
        return;
      }
    }

    try {
      const toolResult = executeToolCall(name, args);
      sendRpcResult({
        content: [{ type: 'text', text: JSON.stringify(toolResult) }],
        structuredContent: toolResult,
        isError: false
      });
    } catch (err: any) {
      sendRpcResult({
        content: [{ type: 'text', text: err.message || 'Tool execution error' }],
        isError: true
      });
    }
    return;
  }

  sendRpcError(-32601, `Method not found: ${method}`);
}

function executeToolCall(name: string, args: Record<string, any>): any {
  if (name === 'create_repo') {
    const { name: repoName, description = null, private: privateRepo = false } = args;
    if (repositories.has(repoName)) {
      throw new Error('Repository already exists');
    }
    const repo = createRepositoryInternal(repoName, description, privateRepo);
    return formatRepo(repo);
  }

  if (name === 'get_repo') {
    const { owner, repo: repoName } = args;
    if (owner !== VIEWER.login || !repositories.has(repoName)) {
      throw new Error('Repository not found');
    }
    return formatRepo(repositories.get(repoName)!);
  }

  if (name === 'create_issue') {
    const { owner, repo: repoName, title, body = null } = args;
    if (owner !== VIEWER.login || !repositories.has(repoName)) {
      throw new Error('Repository not found');
    }
    const repo = repositories.get(repoName)!;
    const issueNumber = repo.issues.size + repo.pulls.size + 1;
    const issue = {
      id: issueNumber,
      node_id: '',
      number: issueNumber,
      title,
      body,
      state: 'open',
      labels: [],
      user: VIEWER,
      comments: 0,
      created_at: new Date().toISOString(),
      updated_at: new Date().toISOString(),
      html_url: `http://localhost/${owner}/${repoName}/issues/${issueNumber}`
    };
    repo.issues.set(issueNumber, issue);
    return formatIssue(repo, issue);
  }

  if (name === 'list_issues') {
    const { owner, repo: repoName, state = 'open' } = args;
    if (owner !== VIEWER.login || !repositories.has(repoName)) {
      throw new Error('Repository not found');
    }
    const repo = repositories.get(repoName)!;
    let list = Array.from(repo.issues.values());
    if (state !== 'all') {
      list = list.filter(i => i.state === state);
    }
    return { issues: list.map(i => formatIssue(repo, i)) };
  }

  if (name === 'create_pull_request') {
    const { owner, repo: repoName, title, head, base, body: prBody = null } = args;
    if (owner !== VIEWER.login || !repositories.has(repoName)) {
      throw new Error('Repository not found');
    }
    const repo = repositories.get(repoName)!;
    const headRefName = head.startsWith('refs/heads/') ? head : 'refs/heads/' + head;
    const baseRefName = base.startsWith('refs/heads/') ? base : 'refs/heads/' + base;

    const headSha = repo.refs.get(headRefName);
    const baseSha = repo.refs.get(baseRefName);

    if (!headSha || !baseSha) {
      throw new Error('Head or base ref not found');
    }

    const hasConflict = checkConflict(repo, headSha, baseSha);
    const prNumber = repo.issues.size + repo.pulls.size + 1;

    const pr = {
      id: prNumber,
      node_id: '',
      number: prNumber,
      title,
      body: prBody,
      state: 'open',
      head: { ref: headRefName.replace('refs/heads/', ''), sha: headSha, label: `${owner}:${headRefName.replace('refs/heads/', '')}` },
      base: { ref: baseRefName.replace('refs/heads/', ''), sha: baseSha, label: `${owner}:${baseRefName.replace('refs/heads/', '')}` },
      merged: false,
      mergeable: !hasConflict,
      user: VIEWER,
      created_at: new Date().toISOString(),
      updated_at: new Date().toISOString(),
      merged_at: null,
      html_url: `http://localhost/${owner}/${repoName}/pull/${prNumber}`,
      merged_by: null,
      merge_commit_sha: null
    };
    repo.pulls.set(prNumber, pr);
    return formatPr(repo, pr);
  }

  if (name === 'merge_pull_request') {
    const { owner, repo: repoName, pull_number } = args;
    if (owner !== VIEWER.login || !repositories.has(repoName)) {
      throw new Error('Repository not found');
    }
    const repo = repositories.get(repoName)!;
    const pr = repo.pulls.get(Number(pull_number));
    if (!pr) {
      throw new Error('Pull request not found');
    }
    if (pr.merged) {
      throw new Error('Pull request already merged');
    }

    const baseRefName = 'refs/heads/' + pr.base.ref;
    const baseTipSha = repo.refs.get(baseRefName);
    const headCommitObj = repo.objects.get(pr.head.sha);
    if (!baseTipSha || !headCommitObj) {
      throw new Error('Cannot merge');
    }

    if (checkConflict(repo, pr.head.sha, baseTipSha)) {
      throw new Error('Pull request has merge conflicts');
    }

    const headTreeSha = parseCommit(headCommitObj.raw).tree;
    const commitRaw = serializeCommit({
      tree: headTreeSha,
      parents: [baseTipSha],
      author: `${VIEWER.name} <${VIEWER.email}> ${new Date().toISOString()} +0000`,
      committer: `${VIEWER.name} <${VIEWER.email}> ${new Date().toISOString()} +0000`,
      message: pr.title
    });
    const { sha: newCommitSha } = hashObject('commit', commitRaw);
    repo.objects.set(newCommitSha, { type: 'commit', raw: commitRaw });
    repo.refs.set(baseRefName, newCommitSha);

    pr.merged = true;
    pr.state = 'closed';
    pr.merged_at = new Date().toISOString();
    pr.merged_by = VIEWER;
    pr.merge_commit_sha = newCommitSha;
    pr.mergeable = false;

    return formatPr(repo, pr);
  }

  throw new Error(`Unknown tool: ${name}`);
}

function formatRepo(repo: any): any {
  return {
    id: repo.id,
    name: repo.name,
    full_name: `${VIEWER.login}/${repo.name}`,
    owner: { login: VIEWER.login, id: VIEWER.id },
    private: repo.private,
    default_branch: repo.default_branch || 'main',
    description: repo.description
  };
}

function formatIssue(repo: any, issue: any): any {
  return {
    id: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels || [],
    html_url: issue.html_url
  };
}

function formatPr(repo: any, pr: any): any {
  return {
    id: pr.id,
    number: pr.number,
    title: pr.title,
    body: pr.body,
    state: pr.state,
    head: pr.head,
    base: pr.base,
    merged: pr.merged,
    html_url: pr.html_url
  };
}
