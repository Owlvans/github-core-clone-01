import * as http from 'node:http';
import {
  repositories,
  VIEWER,
  createRepositoryInternal,
  checkConflict,
  globalNodeId,
  validateAuth,
  computeTreeDiff
} from './app.ts';
import {
  parseCommit,
  parseTree,
  hashObject,
  serializeCommit
} from './git.ts';

function encodeGlobalId(type: string, parts: (string | number)[]): string {
  return Buffer.from([type, ...parts].join(':')).toString('base64');
}

function decodeGlobalId(idStr: string): { type: string; parts: string[] } | null {
  try {
    const decoded = Buffer.from(idStr, 'base64').toString('utf8');
    const parts = decoded.split(':');
    if (parts.length === 0) return null;
    return { type: parts[0], parts: parts.slice(1) };
  } catch {
    return null;
  }
}

// Simple GraphQL Lexer & Parser
interface FieldNode {
  alias?: string;
  name: string;
  arguments?: Record<string, any>;
  selectionSet?: SelectionNode[];
}

interface FragmentNode {
  typeCondition?: string;
  selectionSet: SelectionNode[];
}

type SelectionNode = FieldNode | FragmentNode;

interface OperationNode {
  operation: 'query' | 'mutation';
  name?: string;
  variableDefinitions?: Record<string, any>;
  selectionSet: SelectionNode[];
}

function tokenize(query: string): string[] {
  const regex = /(\$[a-zA-Z_]\w*|"(?:[^"\\]|\\.)*"|#[^\n]*|[a-zA-Z_]\w*|\d+(?:\.\d+)?|\.\.\.|[\{\}\(\)\:\,\!\=\[\]\$])/g;
  const tokens: string[] = [];
  let match;
  while ((match = regex.exec(query)) !== null) {
    if (match[0].startsWith('#')) continue;
    tokens.push(match[0]);
  }
  return tokens;
}

class Parser {
  private tokens: string[];
  private pos = 0;

  constructor(tokens: string[]) {
    this.tokens = tokens;
  }

  private peek(): string | undefined {
    return this.tokens[this.pos];
  }

  private consume(expected?: string): string {
    const token = this.tokens[this.pos++];
    if (expected && token !== expected) {
      throw new Error(`Expected '${expected}', got '${token}'`);
    }
    return token;
  }

  public parseDocument(): OperationNode[] {
    const operations: OperationNode[] = [];
    while (this.pos < this.tokens.length) {
      operations.push(this.parseOperation());
    }
    return operations;
  }

  private parseOperation(): OperationNode {
    let operation: 'query' | 'mutation' = 'query';
    let name: string | undefined;

    const peekToken = this.peek();
    if (peekToken === 'query' || peekToken === 'mutation') {
      operation = this.consume() as any;
      if (this.peek() && /^[a-zA-Z_]/.test(this.peek()!)) {
        name = this.consume();
      }
    }

    if (this.peek() === '(') {
      this.consume('(');
      let depth = 1;
      while (depth > 0 && this.pos < this.tokens.length) {
        const t = this.consume();
        if (t === '(') depth++;
        if (t === ')') depth--;
      }
    }

    const selectionSet = this.parseSelectionSet();
    return { operation, name, selectionSet };
  }

  private parseSelectionSet(): SelectionNode[] {
    this.consume('{');
    const selections: SelectionNode[] = [];
    while (this.pos < this.tokens.length && this.peek() !== '}') {
      selections.push(this.parseSelection());
    }
    this.consume('}');
    return selections;
  }

  private parseSelection(): SelectionNode {
    if (this.peek() === '...' || (this.tokens[this.pos] === '.' && this.tokens[this.pos + 1] === '.' && this.tokens[this.pos + 2] === '.')) {
      if (this.peek() === '...') {
        this.consume('...');
      } else {
        this.consume('.');
        this.consume('.');
        this.consume('.');
      }
      let typeCondition: string | undefined;
      if (this.peek() === 'on') {
        this.consume('on');
        typeCondition = this.consume();
      }
      const selectionSet = this.parseSelectionSet();
      return { typeCondition, selectionSet };
    }

    let name = this.consume();
    let alias: string | undefined;
    if (this.peek() === ':') {
      this.consume(':');
      alias = name;
      name = this.consume();
    }

    let args: Record<string, any> | undefined;
    if (this.peek() === '(') {
      args = this.parseArguments();
    }

    let selectionSet: SelectionNode[] | undefined;
    if (this.peek() === '{') {
      selectionSet = this.parseSelectionSet();
    }

    return { alias, name, arguments: args, selectionSet };
  }

  private parseArguments(): Record<string, any> {
    this.consume('(');
    const args: Record<string, any> = {};
    while (this.pos < this.tokens.length && this.peek() !== ')') {
      const key = this.consume();
      this.consume(':');
      args[key] = this.parseValue();
      if (this.peek() === ',') this.consume(',');
    }
    this.consume(')');
    return args;
  }

  private parseValue(): any {
    const t = this.consume();
    if (t.startsWith('"')) {
      return JSON.parse(t);
    }
    if (t === 'true') return true;
    if (t === 'false') return false;
    if (t === 'null') return null;
    if (!isNaN(Number(t))) return Number(t);
    if (t === '[') {
      const arr: any[] = [];
      while (this.peek() !== ']') {
        arr.push(this.parseValue());
        if (this.peek() === ',') this.consume(',');
      }
      this.consume(']');
      return arr;
    }
    if (t === '{') {
      const obj: Record<string, any> = {};
      while (this.peek() !== '}') {
        const k = this.consume();
        this.consume(':');
        obj[k] = this.parseValue();
        if (this.peek() === ',') this.consume(',');
      }
      this.consume('}');
      return obj;
    }
    if (t.startsWith('$')) {
      return { __variable: t.substring(1) };
    }
    return t;
  }
}

function resolveVariables(val: any, variables: Record<string, any>): any {
  if (val && typeof val === 'object' && '__variable' in val) {
    return variables[val.__variable];
  }
  if (Array.isArray(val)) {
    return val.map(v => resolveVariables(v, variables));
  }
  if (val && typeof val === 'object') {
    const res: Record<string, any> = {};
    for (const [k, v] of Object.entries(val)) {
      res[k] = resolveVariables(v, variables);
    }
    return res;
  }
  return val;
}

export async function handleGraphQL(req: http.IncomingMessage, res: http.ServerResponse): Promise<void> {
  if (!validateAuth(req)) {
    res.writeHead(401, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ message: 'Requires authentication' }));
    return;
  }

  let bodyStr = '';
  for await (const chunk of req) {
    bodyStr += chunk;
  }

  let payload: any;
  try {
    payload = JSON.parse(bodyStr);
  } catch {
    res.writeHead(400, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ message: 'Malformed JSON' }));
    return;
  }

  const isBatch = Array.isArray(payload);
  const operations = isBatch ? payload : [payload];
  const results: any[] = [];

  for (const opItem of operations) {
    const query = opItem.query;
    const variables = opItem.variables || {};
    const errors: Array<{ message: string }> = [];

    if (!query) {
      results.push({ errors: [{ message: 'Missing query' }] });
      continue;
    }

    try {
      const tokens = tokenize(query);
      const parser = new Parser(tokens);
      const parsedOps = parser.parseDocument();
      const op = parsedOps[0];

      const data = executeOperation(op, variables, errors);
      const resObj: any = { data };
      if (errors.length > 0) {
        resObj.errors = errors;
      }
      results.push(resObj);
    } catch (err: any) {
      results.push({
        data: null,
        errors: [{ message: err.message || 'Execution error' }]
      });
    }
  }

  res.writeHead(200, { 'content-type': 'application/json' });
  res.end(JSON.stringify(isBatch ? results : results[0]));
}

function resolveArgsDeep(selections: SelectionNode[], variables: Record<string, any>): void {
  for (const sel of selections) {
    if ('typeCondition' in sel) {
      resolveArgsDeep(sel.selectionSet, variables);
      continue;
    }
    if (sel.arguments) {
      sel.arguments = resolveVariables(sel.arguments, variables);
    }
    if (sel.selectionSet) {
      resolveArgsDeep(sel.selectionSet, variables);
    }
  }
}

function executeOperation(op: OperationNode, variables: Record<string, any>, errors: Array<{ message: string }>): Record<string, any> {
  resolveArgsDeep(op.selectionSet, variables);
  const data: Record<string, any> = {};
  for (const sel of op.selectionSet) {
    if ('typeCondition' in sel) continue;
    const fieldName = sel.name;
    const key = sel.alias || fieldName;
    const args = resolveVariables(sel.arguments || {}, variables);

    if (op.operation === 'mutation') {
      data[key] = executeMutation(fieldName, args, sel.selectionSet || [], errors);
    } else {
      data[key] = executeQuery(fieldName, args, sel.selectionSet || [], errors);
    }
  }
  return data;
}

function executeQuery(fieldName: string, args: Record<string, any>, selections: SelectionNode[], errors: Array<{ message: string }>): any {
  if (fieldName === 'viewer') {
    const userObj = {
      login: VIEWER.login,
      id: encodeGlobalId('User', [1000]),
      name: VIEWER.name,
      email: VIEWER.email
    };
    return resolveSelections(userObj, selections, 'User', errors);
  }

  if (fieldName === 'repository') {
    const owner = args.owner;
    const name = args.name;
    if (owner !== VIEWER.login || !repositories.has(name)) {
      return null;
    }
    const repo = repositories.get(name)!;
    return resolveSelections(repo, selections, 'Repository', errors);
  }

  if (fieldName === 'node') {
    const globalId = args.id;
    const decoded = decodeGlobalId(globalId);
    if (!decoded) return null;
    if (decoded.type === 'Repository') {
      const repoId = Number(decoded.parts[0]);
      for (const r of repositories.values()) {
        if (r.id === repoId) {
          return resolveSelections(r, selections, 'Repository', errors);
        }
      }
    }
    if (decoded.type === 'Issue') {
      const repoId = Number(decoded.parts[0]);
      const issueNum = Number(decoded.parts[1]);
      for (const r of repositories.values()) {
        if (r.id === repoId) {
          const issue = r.issues.get(issueNum);
          if (issue) return resolveSelections({ ...issue, repoId: r.id, commentsList: r.comments.get(issueNum) || [] }, selections, 'Issue', errors);
        }
      }
    }
    if (decoded.type === 'PullRequest') {
      const repoId = Number(decoded.parts[0]);
      const prNum = Number(decoded.parts[1]);
      for (const r of repositories.values()) {
        if (r.id === repoId) {
          const pr = r.pulls.get(prNum);
          if (pr) return resolveSelections({ ...pr, repoId: r.id }, selections, 'PullRequest', errors);
        }
      }
    }
    return { id: globalId };
  }

  if (fieldName === '__type') {
    const typeName = args.name;
    return resolveIntrospectionType(typeName, selections);
  }

  errors.push({ message: `Field "${fieldName}" not found` });
  return null;
}

function executeMutation(fieldName: string, args: Record<string, any>, selections: SelectionNode[], errors: Array<{ message: string }>): any {
  const input = args.input || {};

  if (fieldName === 'createIssue') {
    const repoId = Number(input.repositoryId ? decodeGlobalId(input.repositoryId)?.parts[0] : 0);
    let targetRepo: any = null;
    for (const r of repositories.values()) {
      if (r.id === repoId) { targetRepo = r; break; }
    }
    if (!targetRepo) {
      errors.push({ message: 'Repository not found' });
      return null;
    }
    const title = input.title;
    const body = input.body || null;
    const labelIds = input.labelIds || [];

    const formattedLabels: any[] = [];
    for (const lName of labelIds) {
      if (targetRepo.labels.has(lName)) {
        formattedLabels.push(targetRepo.labels.get(lName)!);
      }
    }

    const issueNumber = targetRepo.issues.size + targetRepo.pulls.size + 1;
    const issue = {
      id: issueNumber,
      node_id: '',
      number: issueNumber,
      title,
      body,
      state: 'open' as const,
      labels: formattedLabels,
      user: VIEWER,
      comments: 0,
      created_at: new Date().toISOString(),
      updated_at: new Date().toISOString(),
      html_url: `http://localhost/${VIEWER.login}/${targetRepo.name}/issues/${issueNumber}`
    };
    targetRepo.issues.set(issueNumber, issue);

    const issueObj = resolveSelections({ ...issue, repoId: targetRepo.id, commentsList: [] }, selections.find(s => s.name === 'issue')?.selectionSet || [], 'Issue', errors);
    return { issue: issueObj };
  }

  if (fieldName === 'closeIssue') {
    const issueId = input.issueId;
    const decoded = decodeGlobalId(issueId);
    if (!decoded || decoded.type !== 'Issue') {
      errors.push({ message: 'Invalid issue id' });
      return null;
    }
    const repoId = Number(decoded.parts[0]);
    const issueNum = Number(decoded.parts[1]);
    let targetRepo: any = null;
    for (const r of repositories.values()) {
      if (r.id === repoId) { targetRepo = r; break; }
    }
    if (!targetRepo) {
      errors.push({ message: 'Repository not found' });
      return null;
    }
    const issue = targetRepo.issues.get(issueNum);
    if (!issue) {
      errors.push({ message: 'Issue not found' });
      return null;
    }
    issue.state = 'closed';
    issue.updated_at = new Date().toISOString();

    const issueObj = resolveSelections({ ...issue, repoId: targetRepo.id, commentsList: targetRepo.comments.get(issueNum) || [] }, selections.find(s => s.name === 'issue')?.selectionSet || [], 'Issue', errors);
    return { issue: issueObj };
  }

  if (fieldName === 'createPullRequest') {
    const repoId = Number(input.repositoryId ? decodeGlobalId(input.repositoryId)?.parts[0] : 0);
    let targetRepo: any = null;
    for (const r of repositories.values()) {
      if (r.id === repoId) { targetRepo = r; break; }
    }
    if (!targetRepo) {
      errors.push({ message: 'Repository not found' });
      return null;
    }
    const { title, body: prBody = null } = input;
    const headRaw = input.headRefName ?? input.head ?? '';
    const baseRaw = input.baseRefName ?? input.base ?? '';
    const headRefName = headRaw.startsWith('refs/heads/') ? headRaw : 'refs/heads/' + headRaw;
    const baseRefName = baseRaw.startsWith('refs/heads/') ? baseRaw : 'refs/heads/' + baseRaw;

    const headSha = targetRepo.refs.get(headRefName);
    const baseSha = targetRepo.refs.get(baseRefName);

    const hasConflict = headSha && baseSha ? checkConflict(targetRepo, headSha, baseSha) : true;
    const prNumber = targetRepo.issues.size + targetRepo.pulls.size + 1;

    const pr = {
      id: prNumber,
      node_id: '',
      number: prNumber,
      title,
      body: prBody,
      state: 'open' as const,
      head: { ref: headRefName.replace('refs/heads/', ''), sha: headSha || '', label: `${VIEWER.login}:${headRefName.replace('refs/heads/', '')}` },
      base: { ref: baseRefName.replace('refs/heads/', ''), sha: baseSha || '', label: `${VIEWER.login}:${baseRefName.replace('refs/heads/', '')}` },
      merged: false,
      mergeable: !hasConflict,
      user: VIEWER,
      created_at: new Date().toISOString(),
      updated_at: new Date().toISOString(),
      merged_at: null,
      html_url: `http://localhost/${VIEWER.login}/${targetRepo.name}/pull/${prNumber}`,
      merged_by: null,
      merge_commit_sha: null
    };
    targetRepo.pulls.set(prNumber, pr);

    const prObj = resolveSelections({ ...pr, repoId: targetRepo.id }, selections.find(s => s.name === 'pullRequest')?.selectionSet || [], 'PullRequest', errors);
    return { pullRequest: prObj };
  }

  if (fieldName === 'mergePullRequest') {
    const prId = input.pullRequestId;
    const decoded = decodeGlobalId(prId);
    if (!decoded || decoded.type !== 'PullRequest') {
      errors.push({ message: 'Invalid pull request id' });
      return null;
    }
    const repoId = Number(decoded.parts[0]);
    const prNum = Number(decoded.parts[1]);
    let targetRepo: any = null;
    for (const r of repositories.values()) {
      if (r.id === repoId) { targetRepo = r; break; }
    }
    if (!targetRepo) {
      errors.push({ message: 'Repository not found' });
      return null;
    }
    const pr = targetRepo.pulls.get(prNum);
    if (!pr) {
      errors.push({ message: 'Pull request not found' });
      return null;
    }

    const baseRefName = 'refs/heads/' + pr.base.ref;
    const baseTipSha = targetRepo.refs.get(baseRefName);
    const headCommitObj = targetRepo.objects.get(pr.head.sha);
    if (baseTipSha && headCommitObj) {
      const headTreeSha = parseCommit(headCommitObj.raw).tree;
      const commitRaw = serializeCommit({
        tree: headTreeSha,
        parents: [baseTipSha],
        author: `${VIEWER.name} <${VIEWER.email}> ${new Date().toISOString()} +0000`,
        committer: `${VIEWER.name} <${VIEWER.email}> ${new Date().toISOString()} +0000`,
        message: pr.title
      });
      const { sha: newCommitSha } = hashObject('commit', commitRaw);
      targetRepo.objects.set(newCommitSha, { type: 'commit', raw: commitRaw });
      targetRepo.refs.set(baseRefName, newCommitSha);

      pr.merged = true;
      pr.state = 'closed';
      pr.merged_at = new Date().toISOString();
      pr.merged_by = VIEWER;
      pr.merge_commit_sha = newCommitSha;
      pr.mergeable = false;
    }

    const prObj = resolveSelections({ ...pr, repoId: targetRepo.id }, selections.find(s => s.name === 'pullRequest')?.selectionSet || [], 'PullRequest', errors);
    return { pullRequest: prObj };
  }

  if (fieldName === 'addComment') {
    const subjectId = input.subjectId;
    const decoded = decodeGlobalId(subjectId);
    if (!decoded || decoded.type !== 'Issue') {
      errors.push({ message: 'Invalid subject id' });
      return null;
    }
    const repoId = Number(decoded.parts[0]);
    const issueNum = Number(decoded.parts[1]);
    let targetRepo: any = null;
    for (const r of repositories.values()) {
      if (r.id === repoId) { targetRepo = r; break; }
    }
    if (!targetRepo) {
      errors.push({ message: 'Repository not found' });
      return null;
    }
    const issue = targetRepo.issues.get(issueNum);
    if (!issue) {
      errors.push({ message: 'Issue not found' });
      return null;
    }

    const commentId = Date.now();
    const comment = {
      id: commentId,
      body: input.body,
      user: VIEWER,
      created_at: new Date().toISOString()
    };
    let list = targetRepo.comments.get(issueNum);
    if (!list) {
      list = [];
      targetRepo.comments.set(issueNum, list);
    }
    list.push(comment as any);
    issue.comments = list.length;

    const commentEdgeSelections = selections.find(s => s.name === 'commentEdge')?.selectionSet || [];
    const nodeSelections = commentEdgeSelections.find(s => s.name === 'node')?.selectionSet || [];

    const commentNode = {
      id: encodeGlobalId('Comment', [targetRepo.id, issueNum, commentId]),
      body: comment.body,
      author: { login: comment.user.login },
      createdAt: comment.created_at
    };

    return {
      commentEdge: {
        node: resolveSelections(commentNode, nodeSelections, 'Comment', errors)
      }
    };
  }

  errors.push({ message: `Mutation "${fieldName}" not found` });
  return null;
}

function resolveSelections(obj: any, selections: SelectionNode[], typeName: string, errors: Array<{ message: string }>): Record<string, any> {
  if (!obj) return null;
  const result: Record<string, any> = {};

  for (const sel of selections) {
    if ('typeCondition' in sel) {
      if (sel.typeCondition && sel.typeCondition !== typeName) continue;
      const sub = resolveSelections(obj, sel.selectionSet, typeName, errors);
      Object.assign(result, sub);
      continue;
    }

    const fieldName = sel.name;
    const key = sel.alias || fieldName;
    const args = sel.arguments || {};

    const val = getFieldValue(obj, fieldName, args, typeName, errors, sel.selectionSet || []);
    if (val !== undefined) {
      result[key] = val;
    } else {
      result[key] = null;
      errors.push({ message: `Field "${fieldName}" not found on type "${typeName}"` });
    }
  }

  return result;
}

function getFieldValue(obj: any, fieldName: string, args: Record<string, any>, typeName: string, errors: Array<{ message: string }>, subSel: SelectionNode[] = []): any {
  if (typeName === 'User') {
    if (fieldName === 'login') return obj.login;
    if (fieldName === 'id') return obj.id;
    if (fieldName === 'name') return obj.name;
    if (fieldName === 'email') return obj.email;
  }

  if (typeName === 'Repository') {
    if (fieldName === 'id') return encodeGlobalId('Repository', [obj.id]);
    if (fieldName === 'databaseId') return obj.id;
    if (fieldName === 'name') return obj.name;
    if (fieldName === 'nameWithOwner') return `${VIEWER.login}/${obj.name}`;
    if (fieldName === 'owner') return { login: VIEWER.login, id: encodeGlobalId('User', [1000]) };
    if (fieldName === 'description') return obj.description;
    if (fieldName === 'isPrivate') return obj.private;
    if (fieldName === 'viewerPermission') return 'ADMIN';
    if (fieldName === 'hasIssuesEnabled') return true;
    if (fieldName === 'createdAt') return obj.created_at;
    if (fieldName === 'updatedAt') return obj.updated_at;
    if (fieldName === 'defaultBranchRef') {
      const defaultBranch = obj.default_branch || 'main';
      const refName = 'refs/heads/' + defaultBranch;
      const oid = obj.refs.get(refName) || obj.refs.get('HEAD');
      if (!oid) return null;
      return { name: defaultBranch, target: { oid } };
    }
    if (fieldName === 'issue') {
      const num = args.number;
      const issue = obj.issues.get(num);
      return issue ? resolveSelections({ ...issue, repoId: obj.id, commentsList: obj.comments.get(num) || [] }, subSel, 'Issue', errors) : null;
    }
    if (fieldName === 'issues') {
      return resolveIssuesConnection(obj, args, errors, subSel);
    }
    if (fieldName === 'pullRequest') {
      const num = args.number;
      const pr = obj.pulls.get(num);
      return pr ? resolveSelections({ ...pr, repoId: obj.id }, subSel, 'PullRequest', errors) : null;
    }
    if (fieldName === 'pullRequests') {
      return resolvePullRequestsConnection(obj, args, errors, subSel);
    }
  }

  if (typeName === 'Issue') {
    if (fieldName === 'id') return encodeGlobalId('Issue', [obj.repoId || 5001, obj.number]);
    if (fieldName === 'databaseId') return (obj.repoId || 5001) * 100000 + obj.number;
    if (fieldName === 'number') return obj.number;
    if (fieldName === 'title') return obj.title;
    if (fieldName === 'body') return obj.body;
    if (fieldName === 'state') return obj.state ? obj.state.toUpperCase() : 'OPEN';
    if (fieldName === 'url') return obj.html_url;
    if (fieldName === 'createdAt') return obj.created_at;
    if (fieldName === 'updatedAt') return obj.updated_at;
    if (fieldName === 'author') return { login: obj.user ? obj.user.login : VIEWER.login };
    if (fieldName === 'labels') {
      const nodeSel = subSel.find(s => 'name' in s && (s as any).name === 'nodes')?.selectionSet || [];
      const nodes = (obj.labels || []).map((l: any) => resolveSelections({ name: typeof l === 'string' ? l : l.name }, nodeSel, 'Label', errors));
      return { nodes };
    }
    if (fieldName === 'comments') {
      const nodeSel = subSel.find(s => 'name' in s && (s as any).name === 'nodes')?.selectionSet || [];
      const commentList = obj.commentsList || [];
      const nodes = commentList.map((c: any) => resolveSelections({
        id: encodeGlobalId('Comment', [obj.repoId || 5001, obj.number, c.id]),
        body: c.body,
        author: { login: c.user ? c.user.login : VIEWER.login },
        createdAt: c.created_at
      }, nodeSel, 'Comment', errors));
      return { nodes };
    }
  }

  if (typeName === 'PullRequest') {
    if (fieldName === 'id') return encodeGlobalId('PullRequest', [obj.repoId || 5001, obj.number]);
    if (fieldName === 'databaseId') return (obj.repoId || 5001) * 100000 + obj.number;
    if (fieldName === 'number') return obj.number;
    if (fieldName === 'title') return obj.title;
    if (fieldName === 'body') return obj.body;
    if (fieldName === 'state') return obj.merged ? 'MERGED' : (obj.state ? obj.state.toUpperCase() : 'OPEN');
    if (fieldName === 'merged') return Boolean(obj.merged);
    if (fieldName === 'mergeable') return obj.mergeable === false ? 'CONFLICTING' : 'MERGEABLE';
    if (fieldName === 'headRefName') return obj.head ? obj.head.ref : '';
    if (fieldName === 'baseRefName') return obj.base ? obj.base.ref : '';
    if (fieldName === 'headRefOid') return obj.head ? obj.head.sha : '';
    if (fieldName === 'baseRefOid') return obj.base ? obj.base.sha : '';
    if (fieldName === 'createdAt') return obj.created_at;
    if (fieldName === 'updatedAt') return obj.updated_at;
    if (fieldName === 'author') return { login: obj.user ? obj.user.login : VIEWER.login };
    if (fieldName === 'mergedAt') return obj.merged_at || null;
    if (fieldName === 'files') {
      const nodeSel = subSel.find(s => 'name' in s && (s as any).name === 'nodes')?.selectionSet || [];
      let repo: any = null;
      for (const r of repositories.values()) {
        if (r.id === (obj.repoId || 5001)) { repo = r; break; }
      }
      if (!repo || !obj.head || !obj.base) return { nodes: [] };
      const headObj = repo.objects.get(obj.head.sha);
      const baseObj = repo.objects.get(obj.base.sha);
      if (!headObj || !baseObj) return { nodes: [] };
      const headTree = parseCommit(headObj.raw).tree;
      const baseTree = parseCommit(baseObj.raw).tree;
      const diffs = computeTreeDiff(repo, baseTree, headTree);
      const nodes = diffs.map((d: any) => resolveSelections({ path: d.filename }, nodeSel, 'PullRequestChangedFile', errors));
      return { nodes };
    }
  }

  if (typeName === 'Comment') {
    if (fieldName === 'id') return obj.id;
    if (fieldName === 'body') return obj.body;
    if (fieldName === 'createdAt') return obj.createdAt || obj.created_at;
    if (fieldName === 'author') return obj.author;
  }

  if (fieldName in obj) {
    return obj[fieldName];
  }

  return undefined;
}

function projectConnection(conn: any, subSel: SelectionNode[], nodeType: string, errors: Array<{ message: string }>): any {
  const out: Record<string, any> = {};
  for (const sel of subSel) {
    if ('typeCondition' in sel) continue;
    const key = (sel as any).alias || (sel as any).name;
    if ((sel as any).name === 'nodes') {
      out[key] = conn.nodes.map((n: any) => resolveSelections(n, (sel as any).selectionSet || [], nodeType, errors));
    } else if ((sel as any).name === 'pageInfo') {
      out[key] = resolveSelections(conn.pageInfo, (sel as any).selectionSet || [], 'PageInfo', errors);
    } else if ((sel as any).name === 'totalCount') {
      out[key] = conn.totalCount;
    }
  }
  if (Object.keys(out).length === 0) {
    // No explicit sub-selections (defensive): return the raw connection.
    return conn;
  }
  return out;
}

function resolveIssuesConnection(repo: any, args: Record<string, any>, errors: Array<{ message: string }>, subSel: SelectionNode[] = []): any {
  let issues = Array.from(repo.issues.values()).map((i: any) => ({ ...i, repoId: repo.id }));
  const states = args.states;
  if (states && Array.isArray(states)) {
    issues = issues.filter(i => states.includes(i.state.toUpperCase()));
  }

  const orderBy = args.orderBy;
  const direction = orderBy && orderBy.direction === 'ASC' ? 1 : -1;
  issues.sort((a, b) => (a.number - b.number) * direction);

  const after = args.after;
  if (after) {
    const decoded = decodeGlobalId(after);
    if (decoded && decoded.type === 'cursor') {
      const afterNum = Number(decoded.parts[0]);
      issues = issues.filter(i => direction === -1 ? i.number < afterNum : i.number > afterNum);
    }
  }

  const first = args.first || issues.length;
  const sliced = issues.slice(0, first);
  const hasNextPage = issues.length > first;
  const lastNum = sliced.length > 0 ? sliced[sliced.length - 1].number : null;
  const endCursor = lastNum !== null ? encodeGlobalId('cursor', [lastNum]) : null;

  const conn = {
    nodes: sliced.map(i => ({
      ...i,
      repoId: repo.id,
      commentsList: repo.comments.get(i.number) || []
    })),
    pageInfo: {
      hasNextPage,
      endCursor
    },
    totalCount: repo.issues.size
  };
  return projectConnection(conn, subSel, 'Issue', errors);
}

function resolvePullRequestsConnection(repo: any, args: Record<string, any>, errors: Array<{ message: string }>, subSel: SelectionNode[] = []): any {
  let pulls = Array.from(repo.pulls.values()).map((p: any) => ({ ...p, repoId: repo.id }));
  const states = args.states;
  if (states && Array.isArray(states)) {
    pulls = pulls.filter(p => states.includes(p.merged ? 'MERGED' : p.state.toUpperCase()));
  }
  const first = args.first || pulls.length;
  const sliced = pulls.slice(0, first);
  const conn = {
    nodes: sliced.map(p => ({ ...p, repoId: repo.id })),
    pageInfo: {
      hasNextPage: pulls.length > first,
      endCursor: null
    },
    totalCount: repo.pulls.size
  };
  return projectConnection(conn, subSel, 'PullRequest', errors);
}

function resolveIntrospectionType(typeName: string, selections: SelectionNode[]): any {
  if (typeName === 'SearchType') {
    return {
      name: 'SearchType',
      kind: 'ENUM',
      enumValues: [{ name: 'ISSUE' }, { name: 'REPOSITORY' }, { name: 'USER' }]
    };
  }

  let fields: any[] = [];
  if (typeName === 'Issue') {
    fields = ['id', 'databaseId', 'number', 'title', 'body', 'state', 'url', 'createdAt', 'updatedAt', 'author', 'labels', 'comments'].map(name => ({ name }));
  } else if (typeName === 'PullRequest') {
    fields = ['id', 'databaseId', 'number', 'title', 'body', 'state', 'merged', 'mergeable', 'headRefName', 'baseRefName', 'files', 'createdAt', 'updatedAt', 'author'].map(name => ({ name }));
  } else if (typeName === 'Repository') {
    fields = ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'description', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequest', 'pullRequests'].map(name => ({ name }));
  }

  return {
    name: typeName,
    kind: 'OBJECT',
    fields
  };
}
