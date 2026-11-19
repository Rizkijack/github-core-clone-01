// MCP: JSON-RPC 2.0 over Streamable HTTP, revision 2026-07-28 (+ legacy initialize handshake).
import type { ServerResponse } from 'node:http';
import { Store, VIEWER, repoJson, issueJson, pullJson } from './store.ts';
import { json, type Ctx } from './http.ts';
import { trialNow } from './util.ts';

const SERVER_INFO = { name: 'forge-mcp', version: '1.0.0' };
const SUPPORTED = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];

interface ToolDef { name: string; description: string; inputSchema: { type: 'object'; properties: Record<string, any>; required: string[] }; }
const TOOLS: ToolDef[] = [
  { name: 'create_repo', description: 'Create a new repository owned by the authenticated user.', inputSchema: { type: 'object', properties: { name: { type: 'string' }, description: { type: 'string' }, private: { type: 'boolean' }, owner: { type: 'string' } }, required: ['name'] } },
  { name: 'get_repo', description: 'Get a repository by owner and name.', inputSchema: { type: 'object', properties: { owner: { type: 'string' }, repo: { type: 'string' } }, required: ['owner', 'repo'] } },
  { name: 'create_issue', description: 'Create an issue in a repository.', inputSchema: { type: 'object', properties: { owner: { type: 'string' }, repo: { type: 'string' }, title: { type: 'string' }, body: { type: 'string' } }, required: ['owner', 'repo', 'title'] } },
  { name: 'list_issues', description: 'List issues in a repository, optionally filtered by state.', inputSchema: { type: 'object', properties: { owner: { type: 'string' }, repo: { type: 'string' }, state: { type: 'string' } }, required: ['owner', 'repo'] } },
  { name: 'create_pull_request', description: 'Open a pull request from a head branch into a base branch.', inputSchema: { type: 'object', properties: { owner: { type: 'string' }, repo: { type: 'string' }, title: { type: 'string' }, body: { type: 'string' }, head: { type: 'string' }, base: { type: 'string' } }, required: ['owner', 'repo', 'title', 'head', 'base'] } },
  { name: 'merge_pull_request', description: 'Squash-merge a pull request by number.', inputSchema: { type: 'object', properties: { owner: { type: 'string' }, repo: { type: 'string' }, pull_number: { type: 'integer' } }, required: ['owner', 'repo', 'pull_number'] } },
];

const norm = (s: string) => String(s).toLowerCase().replace(/[^a-z0-9]/g, '');
function resolveTool(name: string): ToolDef | null {
  const n = norm(name);
  return TOOLS.find(t => norm(t.name) === n) ?? TOOLS.find(t => norm(t.name).length < n.length && n.endsWith(norm(t.name))) ?? null;
}

function rpcResult(res: ServerResponse, id: any, result: any): void {
  json(res, 200, { jsonrpc: '2.0', id, result });
}
function rpcError(res: ServerResponse, id: any, code: number, message: string, status = 200): void {
  json(res, status, { jsonrpc: '2.0', id, error: { code, message } });
}
function toolText(res: ServerResponse, id: any, obj: any, isError = false): void {
  const text = JSON.stringify(obj);
  rpcResult(res, id, { content: [{ type: 'text', text }], structuredContent: obj, isError });
}

function callTool(store: Store, tool: ToolDef, args: Record<string, any>): { obj?: any; isError?: boolean; message?: string } {
  for (const req of tool.inputSchema.required) {
    if (args[req] === undefined || args[req] === null || args[req] === '') return { isError: true, message: `missing required argument: ${req}` };
  }
  const owner = args.owner ?? VIEWER.login;
  const repoName = args.repo ?? args.name;
  const repo = store.getRepo(owner, repoName);
  switch (tool.name) {
    case 'create_repo': {
      if (store.repos.has(args.name)) return { obj: repoJson(store.repos.get(args.name)!) };
      const r = store.createRepo(args.name, { private: args.private ?? true, description: args.description ?? '' });
      return { obj: repoJson(r) };
    }
    case 'get_repo': {
      if (!repo) return { isError: true, message: 'repository not found' };
      return { obj: repoJson(repo) };
    }
    case 'create_issue': {
      if (!repo) return { isError: true, message: 'repository not found' };
      repo.issueCounter += 1;
      const i = { id: store.id(), number: repo.issueCounter, title: args.title, body: args.body ?? '', state: 'open' as const, labels: [], comments: [], created_at: trialNow(), updated_at: trialNow() };
      repo.issues.push(i);
      return { obj: issueJson(i) };
    }
    case 'list_issues': {
      if (!repo) return { isError: true, message: 'repository not found' };
      const state = args.state ?? 'open';
      const items = repo.issues.filter(i => state === 'all' ? true : i.state === state);
      return { obj: items.map(issueJson) };
    }
    case 'create_pull_request': {
      if (!repo) return { isError: true, message: 'repository not found' };
      const head = String(args.head), base = String(args.base);
      if (!store.resolveRef(repo, head) || !store.resolveRef(repo, base)) return { isError: true, message: 'head or base ref does not exist' };
      repo.pullCounter += 1;
      const p = { id: store.id(), number: repo.pullCounter, title: args.title, body: args.body ?? '', state: 'open' as const, head: { ref: head }, base: { ref: base }, merged: false, mergeable: true, merge_commit_sha: null, merged_at: null, created_at: trialNow(), updated_at: trialNow() };
      repo.pulls.push(p);
      return { obj: pullJson(store, repo, p) };
    }
    case 'merge_pull_request': {
      if (!repo) return { isError: true, message: 'repository not found' };
      const p = repo.pulls.find(x => x.number === Number(args.pull_number));
      if (!p) return { isError: true, message: 'pull request not found' };
      const result = store.squashMerge(repo, p);
      if (result.conflict) return { isError: true, message: 'pull request is not mergeable' };
      return { obj: pullJson(store, repo, p) };
    }
    default: return { isError: true, message: 'unknown tool' };
  }
}

export function handleMcp(store: Store, ctx: Ctx, res: ServerResponse): void {
  if (ctx.method === 'DELETE') { res.writeHead(200, { 'x-github-request-id': 'mcp' }); res.end(); return; }
  if (!ctx.authed) { json(res, 401, { message: 'Requires authentication' }); return; }
  let msg: any;
  try { msg = JSON.parse(ctx.rawBody.toString('utf8')); }
  catch { rpcError(res, null, -32700, 'Parse error', 400); return; }
  const id = msg?.id;
  const method = msg?.method;
  const params = msg?.params ?? {};
  if (method === 'notifications/initialized' || (id === undefined && typeof method === 'string' && method.startsWith('notifications/'))) {
    res.writeHead(202, { 'x-github-request-id': 'mcp' }); res.end(); return;
  }
  if (method === 'server/discover') {
    rpcResult(res, id, { supportedVersions: SUPPORTED, capabilities: { tools: {} }, serverInfo: SERVER_INFO });
    return;
  }
  if (method === 'initialize') {
    const pv = typeof params.protocolVersion === 'string' ? params.protocolVersion : SUPPORTED[0];
    rpcResult(res, id, { protocolVersion: pv, capabilities: { tools: {} }, serverInfo: SERVER_INFO });
    return;
  }
  if (method === 'tools/list') {
    rpcResult(res, id, { tools: TOOLS });
    return;
  }
  if (method === 'tools/call') {
    const tool = resolveTool(params.name ?? '');
    if (!tool) { rpcError(res, id, -32601, `Unknown tool: ${params.name}`); return; }
    const out = callTool(store, tool, params.arguments ?? {});
    if (out.isError) { toolText(res, id, { message: out.message ?? 'tool error' }, true); return; }
    toolText(res, id, out.obj, false);
    return;
  }
  rpcError(res, id, -32601, `Method not found: ${method}`);
}
