// GraphQL v4: a hand-rolled parser + executor for the documents gh / github-mcp-server send.
import type { ServerResponse } from 'node:http';
import { Store, type Repo, type Issue, type Pull, VIEWER } from './store.ts';
import { json, parseJson, type Ctx } from './http.ts';
import { nodeId, decodeNodeId, trialNow } from './util.ts';

// ---------- parser ----------
interface Value { kind: string; name?: string; value?: any; items?: Value[]; fields?: Record<string, Value>; }
interface Field { alias: string | null; name: string; args: Record<string, Value>; selectionSet: SelectionSet | null; }
interface Fragment { typeCondition: string; selectionSet: SelectionSet; }
interface SelectionSet { fields: Field[]; fragments: Fragment[]; }
interface Operation { type: string; name: string | null; selectionSet: SelectionSet; }

function tokenize(src: string): string[] {
  const tokens: string[] = [];
  let i = 0;
  while (i < src.length) {
    const c = src[i];
    if (c === '#') { while (i < src.length && src[i] !== '\n') i++; continue; }
    if (/\s|,/.test(c)) { i++; continue; }
    if (c === '"') {
      let j = i + 1, out = '"';
      while (j < src.length && src[j] !== '"') { if (src[j] === '\\') { out += src[j] + src[j + 1]; j += 2; } else { out += src[j]; j++; } }
      out += '"'; tokens.push(out); i = j + 1; continue;
    }
    if (/[A-Za-z_]/.test(c)) { let j = i; while (j < src.length && /[A-Za-z0-9_]/.test(src[j])) j++; tokens.push(src.slice(i, j)); i = j; continue; }
    if (/[0-9-]/.test(c)) { let j = i; while (j < src.length && /[0-9.eE+-]/.test(src[j])) j++; tokens.push(src.slice(i, j)); i = j; continue; }
    if (src.startsWith('...', i)) { tokens.push('...'); i += 3; continue; }
    tokens.push(c); i++;
  }
  return tokens;
}

class Parser {
  t: string[]; p = 0;
  constructor(t: string[]) { this.t = t; }
  peek(): string { return this.t[this.p] ?? ''; }
  next(): string { return this.t[this.p++] ?? ''; }
  expect(x: string): void { if (this.next() !== x) throw new Error(`expected ${x}`); }
  parseDocument(): Operation[] {
    const ops: Operation[] = [];
    while (this.p < this.t.length) ops.push(this.parseOperation());
    return ops;
  }
  parseOperation(): Operation {
    let type = 'query'; let name: string | null = null;
    const kw = this.peek();
    if (kw === 'query' || kw === 'mutation' || kw === 'subscription') { type = this.next(); if (/[A-Za-z_]/.test(this.peek()[0] || '') && this.peek() !== '(') name = this.next(); }
    if (this.peek() === '(') this.skipVarDefs();
    const selectionSet = this.parseSelectionSet();
    return { type, name, selectionSet };
  }
  skipVarDefs(): void {
    this.expect('(');
    let depth = 1;
    while (depth > 0 && this.p < this.t.length) { const x = this.next(); if (x === '(') depth++; else if (x === ')') depth--; }
  }
  parseSelectionSet(): SelectionSet {
    this.expect('{');
    const fields: Field[] = []; const fragments: Fragment[] = [];
    while (this.peek() !== '}') {
      if (this.peek() === '...') {
        this.next();
        if (this.peek() === 'on') { this.next(); const tc = this.next(); fragments.push({ typeCondition: tc, selectionSet: this.parseSelectionSet() }); }
        else { this.next(); /* fragment spread name, unsupported */ }
        continue;
      }
      fields.push(this.parseField());
    }
    this.expect('}');
    return { fields, fragments };
  }
  parseField(): Field {
    let name = this.next(); let alias: string | null = null;
    if (this.peek() === ':') { this.next(); alias = name; name = this.next(); }
    let args: Record<string, Value> = {};
    if (this.peek() === '(') args = this.parseArgs();
    let selectionSet: SelectionSet | null = null;
    if (this.peek() === '{') selectionSet = this.parseSelectionSet();
    return { alias, name, args, selectionSet };
  }
  parseArgs(): Record<string, Value> {
    this.expect('(');
    const args: Record<string, Value> = {};
    while (this.peek() !== ')') { const name = this.next(); this.expect(':'); args[name] = this.parseValue(); }
    this.expect(')');
    return args;
  }
  parseValue(): Value {
    const tk = this.peek();
    if (tk === '$') { this.next(); return { kind: 'var', name: this.next() }; }
    if (tk === '[') { this.next(); const items: Value[] = []; while (this.peek() !== ']') items.push(this.parseValue()); this.expect(']'); return { kind: 'list', items }; }
    if (tk === '{') { this.next(); const fields: Record<string, Value> = {}; while (this.peek() !== '}') { const n = this.next(); this.expect(':'); fields[n] = this.parseValue(); } this.expect('}'); return { kind: 'object', fields }; }
    if (tk[0] === '"') { this.next(); return { kind: 'string', value: JSON.parse(tk) }; }
    if (/^-?[0-9]/.test(tk)) { this.next(); return { kind: 'int', value: Number(tk) }; }
    if (tk === 'true' || tk === 'false') { this.next(); return { kind: 'bool', value: tk === 'true' }; }
    if (tk === 'null') { this.next(); return { kind: 'null' }; }
    this.next(); return { kind: 'enum', value: tk };
  }
}

function evalValue(v: Value, vars: Record<string, any>): any {
  switch (v.kind) {
    case 'var': return vars[v.name!] ?? null;
    case 'list': return (v.items ?? []).map(x => evalValue(x, vars));
    case 'object': { const o: Record<string, any> = {}; for (const k in v.fields) o[k] = evalValue(v.fields[k], vars); return o; }
    case 'null': return null;
    default: return v.value;
  }
}
function evalArgs(args: Record<string, Value>, vars: Record<string, any>): Record<string, any> {
  const o: Record<string, any> = {}; for (const k in args) o[k] = evalValue(args[k], vars); return o;
}

// ---------- schema / introspection ----------
const SCHEMA_FIELDS: Record<string, string[]> = {
  Issue: ['id', 'number', 'title', 'state', 'body', 'labels', 'comments', 'createdAt', 'updatedAt'],
  PullRequest: ['id', 'number', 'headRefName', 'baseRefName', 'mergeable', 'state', 'merged', 'files', 'title', 'body'],
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest'],
  User: ['login', 'id', 'name'],
};
const SCHEMA_ENUMS: Record<string, string[]> = {
  SearchType: ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION'],
  IssueState: ['OPEN', 'CLOSED'],
  PullRequestState: ['OPEN', 'CLOSED', 'MERGED'],
  MergeableState: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'],
  OrderDirection: ['ASC', 'DESC'],
  IssueOrderField: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'],
};
function introspect(name: string): any {
  if (SCHEMA_FIELDS[name]) return { __type: '__Type', data: { name, fields: SCHEMA_FIELDS[name], enumValues: null } };
  if (SCHEMA_ENUMS[name]) return { __type: '__Type', data: { name, fields: null, enumValues: SCHEMA_ENUMS[name] } };
  return null;
}

// ---------- value wrappers ----------
const repoValue = (store: Store, r: Repo) => ({ __type: 'Repository', data: r });
const issueValue = (i: Issue) => ({ __type: 'Issue', data: i });
const pullValue = (store: Store, r: Repo, p: Pull) => ({ __type: 'PullRequest', data: { store, r, p } });

function repoByNodeId(store: Store, id: string): Repo | null {
  const d = decodeNodeId(id); if (!d || d.type !== 'Repository') return null;
  return [...store.repos.values()].find(r => r.id === d.databaseId) ?? null;
}
function issueByNodeId(store: Store, id: string): Issue | null {
  const d = decodeNodeId(id); if (!d || d.type !== 'Issue') return null;
  for (const r of store.repos.values()) { const i = r.issues.find(x => x.id === d.databaseId); if (i) return i; }
  return null;
}
function pullByNodeId(store: Store, id: string): { r: Repo; p: Pull } | null {
  const d = decodeNodeId(id); if (!d || d.type !== 'PullRequest') return null;
  for (const r of store.repos.values()) { const p = r.pulls.find(x => x.id === d.databaseId); if (p) return { r, p }; }
  return null;
}

function issuesConnection(r: Repo, args: Record<string, any>): any {
  let list = [...r.issues];
  if (Array.isArray(args.states) && args.states.length) list = list.filter(i => args.states.includes(i.state.toUpperCase()));
  const dir = args.orderBy?.direction === 'ASC' ? 1 : -1;
  list.sort((a, b) => dir * (a.number - b.number));
  const first = Math.max(0, args.first ?? 100);
  const afterIdx = args.after != null ? decodeCursor(args.after) : -1;
  const start = afterIdx + 1;
  const page = list.slice(start, start + first);
  const hasNextPage = start + first < list.length;
  const endCursor = page.length ? encodeCursor(start + page.length - 1) : null;
  return { __type: 'IssueConnection', data: { nodes: page.map(issueValue), pageInfo: { __type: 'PageInfo', data: { hasNextPage, endCursor } } } };
}
function encodeCursor(idx: number): string { return Buffer.from(`cursor:${idx}`, 'ascii').toString('base64'); }
function decodeCursor(c: string): number {
  try { const m = /^cursor:(\d+)$/.exec(Buffer.from(String(c), 'base64').toString('ascii')); return m ? Number(m[1]) : -1; } catch { return -1; }
}

// ---------- resolvers ----------
type Resolver = (store: Store, data: any, args: Record<string, any>) => any;
const RESOLVERS: Record<string, Record<string, Resolver>> = {
  Query: {
    viewer: () => ({ __type: 'User', data: { login: VIEWER.login } }),
    repository: (s, _d, a) => { const r = s.getRepo(a.owner, a.name); return r ? repoValue(s, r) : null; },
    node: (s, _d, a) => {
      const d = decodeNodeId(a.id); if (!d) return null;
      if (d.type === 'Repository') { const r = [...s.repos.values()].find(x => x.id === d.databaseId); return r ? repoValue(s, r) : null; }
      if (d.type === 'Issue') { const i = issueByNodeId(s, a.id); return i ? issueValue(i) : null; }
      if (d.type === 'PullRequest') { const f = pullByNodeId(s, a.id); return f ? pullValue(s, f.r, f.p) : null; }
      return null;
    },
    __type: (_s, _d, a) => introspect(a.name),
  },
  Mutation: {
    createIssue: (s, _d, a) => {
      const r = repoByNodeId(s, a.input?.repositoryId); if (!r) return null;
      r.issueCounter += 1;
      const i: Issue = { id: s.id(), number: r.issueCounter, title: a.input.title ?? '', body: a.input.body ?? '', state: 'open', labels: [], comments: [], created_at: trialNow(), updated_at: trialNow() };
      r.issues.push(i);
      return { __type: 'CreateIssuePayload', data: { issue: issueValue(i) } };
    },
    closeIssue: (s, _d, a) => {
      const i = issueByNodeId(s, a.input?.issueId); if (!i) return null;
      i.state = 'closed'; i.updated_at = trialNow();
      return { __type: 'CloseIssuePayload', data: { issue: issueValue(i) } };
    },
    createPullRequest: (s, _d, a) => {
      const r = repoByNodeId(s, a.input?.repositoryId); if (!r) return null;
      const head = a.input.headRefName, base = a.input.baseRefName;
      if (!s.resolveRef(r, head) || !s.resolveRef(r, base)) return null;
      r.pullCounter += 1;
      const p: Pull = { id: s.id(), number: r.pullCounter, title: a.input.title ?? '', body: a.input.body ?? '', state: 'open', head: { ref: head }, base: { ref: base }, merged: false, mergeable: true, merge_commit_sha: null, merged_at: null, created_at: trialNow(), updated_at: trialNow() };
      r.pulls.push(p);
      return { __type: 'CreatePullRequestPayload', data: { pullRequest: pullValue(s, r, p) } };
    },
    mergePullRequest: (s, _d, a) => {
      const f = pullByNodeId(s, a.input?.pullRequestId); if (!f) return null;
      s.squashMerge(f.r, f.p);
      return { __type: 'MergePullRequestPayload', data: { pullRequest: pullValue(s, f.r, f.p) } };
    },
    addComment: (s, _d, a) => {
      const i = issueByNodeId(s, a.input?.subjectId); if (!i) return null;
      const c = { id: s.id(), body: a.input.body ?? '', created_at: trialNow() };
      i.comments.push(c);
      return { __type: 'AddCommentPayload', data: { commentEdge: { __type: 'IssueCommentEdge', data: { node: { __type: 'IssueComment', data: c } } } } };
    },
  },
  Repository: {
    id: (_s, r) => nodeId('Repository', r.id),
    databaseId: (_s, r) => r.id,
    name: (_s, r) => r.name,
    nameWithOwner: (_s, r) => `${r.owner.login}/${r.name}`,
    owner: (_s, r) => ({ __type: 'User', data: { login: r.owner.login } }),
    defaultBranchRef: (s, r) => s.branchTip(r, r.default_branch) ? { __type: 'Ref', data: { name: r.default_branch } } : null,
    viewerPermission: () => 'ADMIN',
    hasIssuesEnabled: () => true,
    issue: (_s, r, a) => { const i = r.issues.find(x => x.number === a.number); return i ? issueValue(i) : null; },
    issues: (_s, r, a) => issuesConnection(r, a),
    pullRequests: (s, r, a) => ({ __type: 'PullRequestConnection', data: { nodes: [...r.pulls].sort((x, y) => y.number - x.number).slice(0, a.first ?? 100).map(p => pullValue(s, r, p)) } }),
    pullRequest: (s, r, a) => { const p = r.pulls.find(x => x.number === a.number); return p ? pullValue(s, r, p) : null; },
  },
  Ref: { name: (_s, d) => d.name },
  User: { login: (_s, d) => d.login, id: () => VIEWER.id, name: () => VIEWER.name },
  Issue: {
    id: (_s, i) => nodeId('Issue', i.id),
    number: (_s, i) => i.number,
    title: (_s, i) => i.title,
    state: (_s, i) => i.state.toUpperCase(),
    body: (_s, i) => i.body,
  },
  PullRequest: {
    id: (_s, { p }) => nodeId('PullRequest', p.id),
    number: (_s, { p }) => p.number,
    state: (_s, { p }) => p.merged ? 'MERGED' : p.state.toUpperCase(),
    merged: (_s, { p }) => p.merged,
    headRefName: (_s, { p }) => p.head.ref,
    baseRefName: (_s, { p }) => p.base.ref,
    mergeable: (_s, { p }) => p.merged ? 'UNKNOWN' : (p.mergeable ? 'MERGEABLE' : 'CONFLICTING'),
    files: (s, { r, p }, a) => {
      const headTip = s.resolveRef(r, p.head.ref), baseTip = s.resolveRef(r, p.base.ref);
      const changes = headTip && baseTip ? s.diffTrees(r, s.commitTree(r, baseTip), s.commitTree(r, headTip)) : [];
      const nodes = changes.slice(0, a.first ?? 100).map(c => ({ __type: 'PullRequestChangedFile', data: { path: c.path } }));
      return { __type: 'PullRequestChangedFileConnection', data: { nodes } };
    },
  },
  IssueConnection: { nodes: (_s, d) => d.nodes, pageInfo: (_s, d) => d.pageInfo },
  PullRequestConnection: { nodes: (_s, d) => d.nodes },
  PullRequestChangedFileConnection: { nodes: (_s, d) => d.nodes },
  PullRequestChangedFile: { path: (_s, d) => d.path },
  PageInfo: { hasNextPage: (_s, d) => d.hasNextPage, endCursor: (_s, d) => d.endCursor },
  CreateIssuePayload: { issue: (_s, d) => d.issue },
  CloseIssuePayload: { issue: (_s, d) => d.issue },
  CreatePullRequestPayload: { pullRequest: (_s, d) => d.pullRequest },
  MergePullRequestPayload: { pullRequest: (_s, d) => d.pullRequest },
  AddCommentPayload: { commentEdge: (_s, d) => d.commentEdge },
  IssueCommentEdge: { node: (_s, d) => d.node },
  IssueComment: { id: (_s, c) => nodeId('IssueComment', c.id), body: (_s, c) => c.body },
  __Type: {
    name: (_s, d) => d.name,
    fields: (_s, d) => d.fields ? d.fields.map((n: string) => ({ __type: '__Field', data: { name: n } })) : null,
    enumValues: (_s, d) => d.enumValues ? d.enumValues.map((n: string) => ({ __type: '__EnumValue', data: { name: n } })) : null,
  },
  __Field: { name: (_s, d) => d.name },
  __EnumValue: { name: (_s, d) => d.name },
};

function projectValue(store: Store, value: any, sel: SelectionSet | null, vars: Record<string, any>, errors: any[]): any {
  if (value === null || value === undefined) return null;
  if (Array.isArray(value)) return value.map(v => projectValue(store, v, sel, vars, errors));
  if (typeof value !== 'object' || !value.__type) return value;
  if (!sel) return value.data ?? value;
  const typeName = value.__type; const data = value.data;
  const result: Record<string, any> = {};
  const apply = (s: SelectionSet) => {
    for (const field of s.fields) {
      const key = field.alias ?? field.name;
      const resolver = RESOLVERS[typeName]?.[field.name];
      if (!resolver) { errors.push({ message: `Cannot query field "${field.name}" on type "${typeName}".` }); result[key] = null; continue; }
      const args = evalArgs(field.args, vars);
      const v = resolver(store, data, args);
      result[key] = field.selectionSet ? projectValue(store, v, field.selectionSet, vars, errors) : (v && typeof v === 'object' && v.__type ? (v.data ?? null) : v);
    }
    for (const frag of s.fragments) {
      if (frag.typeCondition === typeName || frag.typeCondition === 'Node') apply(frag.selectionSet);
    }
  };
  apply(sel);
  return result;
}

function executeOperation(store: Store, op: Operation, vars: Record<string, any>): any {
  const errors: any[] = [];
  const rootType = op.type === 'mutation' ? 'Mutation' : 'Query';
  const data = projectValue(store, { __type: rootType, data: null }, op.selectionSet, vars, errors);
  return errors.length ? { data, errors } : { data };
}

export function handleGraphQL(store: Store, ctx: Ctx, res: ServerResponse): void {
  if (!ctx.authed) { json(res, 401, { message: 'Requires authentication' }); return; }
  const body = parseJson(ctx.rawBody);
  if (!body.ok) { json(res, 400, { message: 'Invalid JSON body' }); return; }
  try {
    const run = (entry: any) => {
      const ops = new Parser(tokenize(String(entry.query ?? ''))).parseDocument();
      const op = (entry.operationName ? ops.find(o => o.name === entry.operationName) : null) ?? ops[0];
      if (!op) return { data: null, errors: [{ message: 'No operation' }] };
      return executeOperation(store, op, entry.variables ?? {});
    };
    if (Array.isArray(body.value)) { json(res, 200, body.value.map(run)); return; }
    json(res, 200, run(body.value));
  } catch (err) {
    json(res, 200, { data: null, errors: [{ message: err instanceof Error ? err.message : String(err) }] });
  }
}
