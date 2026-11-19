// REST v3 surface: repos, contents, branches, commits, refs, labels, issues, comments, pulls.
import type { ServerResponse } from 'node:http';
import { Store, VIEWER, repoJson, issueJson, labelJson, commentJson, pullJson, type Repo, type FlatEntry } from './store.ts';
import { json, parseJson, type Ctx } from './http.ts';
import { b64encode, b64decode, isSha40, nodeId, trialNow, ZERO_OID } from './util.ts';

function notFound(res: ServerResponse, msg = 'Not Found'): void { json(res, 404, { message: msg }); }
function unauth(res: ServerResponse): void { json(res, 401, { message: 'Requires authentication' }); }
function badReq(res: ServerResponse, msg = 'Invalid request body'): void { json(res, 400, { message: msg }); }

function commitMessage(data: Buffer): string {
  const text = data.toString('utf8');
  const i = text.indexOf('\n\n');
  const msg = i >= 0 ? text.slice(i + 2) : '';
  return msg.replace(/\n$/, '');
}

/** Resolve a contents path within a commit's tree. */
function lookupPath(store: Store, repo: Repo, commitOid: string | null, path: string):
  { type: 'file'; oid: string } | { type: 'dir'; treeOid: string } | null {
  if (!commitOid) return null;
  let treeOid = store.commitTree(repo, commitOid);
  const clean = path.replace(/^\/+|\/+$/g, '');
  if (clean === '') return treeOid ? { type: 'dir', treeOid } : null;
  const parts = clean.split('/');
  for (let i = 0; i < parts.length; i++) {
    const obj = store.getObject(repo, treeOid);
    if (!obj || obj.type !== 'tree') return null;
    const entry = store.parseTree(obj.data).find(e => e.name === parts[i]);
    if (!entry) return null;
    if (i === parts.length - 1) {
      return entry.mode === '40000' ? { type: 'dir', treeOid: entry.oid } : { type: 'file', oid: entry.oid };
    }
    if (entry.mode !== '40000') return null;
    treeOid = entry.oid;
  }
  return null;
}

function listRepos(store: Store, ctx: Ctx, res: ServerResponse): void {
  const perPage = Math.min(Math.max(parseInt(ctx.query.get('per_page') || '30', 10) || 30, 1), 100);
  const page = Math.max(parseInt(ctx.query.get('page') || '1', 10) || 1, 1);
  const all = [...store.repos.values()];
  const start = (page - 1) * perPage;
  const items = all.slice(start, start + perPage);
  const headers: Record<string, string> = {};
  if (start + perPage < all.length) {
    const next = `http://${ctx.host}/api/v3/user/repos?per_page=${perPage}&page=${page + 1}`;
    headers['link'] = `<${next}>; rel="next"`;
  }
  json(res, 200, items.map(repoJson), headers);
}

export function handleRest(store: Store, ctx: Ctx, res: ServerResponse): void {
  if (!ctx.authed) { unauth(res); return; }
  // path relative to /api/v3
  let p = ctx.pathname.replace(/^\/api\/v3/, '');
  p = p.replace(/^\/+|\/+$/g, '');
  const seg = p === '' ? [] : p.split('/');
  const m = ctx.method;

  // root + meta + user
  if (p === '') {
    if (m === 'GET') { json(res, 200, { current_user_url: `http://${ctx.host}/api/v3/user`, rate_limit_url: `http://${ctx.host}/api/v3/rate_limit` }, { 'x-oauth-scopes': 'repo, read:org' }); return; }
  }
  if (p === 'meta' && m === 'GET') { json(res, 200, { installed_version: '1.0.0', verifiable_password_authentication: false }); return; }
  if (p === 'user' && m === 'GET') { json(res, 200, { login: VIEWER.login, id: VIEWER.id, node_id: VIEWER.node_id, type: 'User' }); return; }
  if (p === 'user/repos') {
    if (m === 'GET') { listRepos(store, ctx, res); return; }
    if (m === 'POST') {
      const body = parseJson(ctx.rawBody);
      if (!body.ok) { badReq(res); return; }
      const name = body.value?.name;
      if (!name || typeof name !== 'string') { json(res, 422, { message: 'Repository name is required' }); return; }
      if (store.repos.has(name)) { json(res, 422, { message: 'Repository creation failed: name already exists on this account' }); return; }
      const repo = store.createRepo(name, { private: body.value.private ?? true, description: body.value.description ?? '' });
      if (body.value.auto_init) {
        store.commitContents(repo, { branch: 'main', path: 'README.md', content: Buffer.from(`# ${name}\n`, 'utf8'), message: 'Initial commit' });
      }
      json(res, 201, repoJson(repo)); return;
    }
  }

  // repos/{owner}/{repo}/...
  if (seg[0] === 'repos' && seg.length >= 3) {
    const owner = decodeURIComponent(seg[1]);
    const name = decodeURIComponent(seg[2]);
    const repo = store.getRepo(owner, name);
    if (!repo) { notFound(res); return; }
    const rest = seg.slice(3);
    handleRepo(store, ctx, res, repo, rest);
    return;
  }

  notFound(res);
}

function handleRepo(store: Store, ctx: Ctx, res: ServerResponse, repo: Repo, rest: string[]): void {
  const m = ctx.method;
  const r0 = rest[0];

  if (rest.length === 0) {
    if (m === 'GET') { json(res, 200, repoJson(repo)); return; }
    if (m === 'PATCH') {
      const body = parseJson(ctx.rawBody); if (!body.ok) { badReq(res); return; }
      if (typeof body.value?.description === 'string') repo.description = body.value.description;
      if (typeof body.value?.private === 'boolean') repo.private = body.value.private;
      if (typeof body.value?.default_branch === 'string') repo.default_branch = body.value.default_branch;
      repo.updated_at = trialNow();
      json(res, 200, repoJson(repo)); return;
    }
    if (m === 'DELETE') { store.deleteRepo(repo.name); res.writeHead(204, { 'x-github-request-id': 'del' }); res.end(); return; }
  }

  // contents
  if (r0 === 'contents') {
    const path = rest.slice(1).map(decodeURIComponent).join('/');
    const ref = ctx.query.get('ref') || ctx.query.get('branch') || repo.default_branch;
    const tip = store.resolveRef(repo, ref);
    if (m === 'GET') {
      const found = lookupPath(store, repo, tip, path);
      if (!found) { notFound(res); return; }
      if (found.type === 'file') {
        const obj = store.getObject(repo, found.oid)!;
        json(res, 200, { type: 'file', encoding: 'base64', content: b64encode(obj.data), sha: found.oid, size: obj.data.length, name: path.split('/').pop(), path });
        return;
      }
      const obj = store.getObject(repo, found.treeOid)!;
      const entries = store.parseTree(obj.data).map(e => ({
        type: e.mode === '40000' ? 'dir' : 'file', name: e.name,
        path: path ? `${path}/${e.name}` : e.name, sha: e.oid,
      }));
      json(res, 200, entries); return;
    }
    if (m === 'PUT') {
      const body = parseJson(ctx.rawBody); if (!body.ok) { badReq(res); return; }
      const branch = body.value?.branch || repo.default_branch;
      const content = b64decode(body.value?.content ?? '');
      const cur = lookupPath(store, repo, store.branchTip(repo, branch), path);
      const existingOid = cur && cur.type === 'file' ? cur.oid : null;
      if (existingOid) {
        if (!body.value?.sha || String(body.value.sha).toLowerCase() !== existingOid) {
          json(res, 409, { message: 'The file has been modified since the provided sha (stale sha)' }); return;
        }
      }
      const { commitSha, blobSha } = store.commitContents(repo, { branch, path, content, message: body.value?.message || `Update ${path}` });
      json(res, existingOid ? 200 : 201, {
        content: { sha: blobSha, encoding: 'base64', name: path.split('/').pop(), path },
        commit: { sha: commitSha },
      }); return;
    }
    if (m === 'DELETE') {
      const body = parseJson(ctx.rawBody); if (!body.ok) { badReq(res); return; }
      const branch = body.value?.branch || repo.default_branch;
      const cur = lookupPath(store, repo, store.branchTip(repo, branch), path);
      if (!cur || cur.type !== 'file') { notFound(res); return; }
      if (body.value?.sha && String(body.value.sha).toLowerCase() !== cur.oid) { json(res, 409, { message: 'stale sha' }); return; }
      const { commitSha } = store.commitContents(repo, { branch, path, content: null, message: body.value?.message || `Delete ${path}` });
      json(res, 200, { commit: { sha: commitSha } }); return;
    }
  }

  // branches
  if (r0 === 'branches') {
    if (rest.length === 1 && m === 'GET') {
      const items = [...repo.refs.entries()].filter(([k]) => k.startsWith('refs/heads/'))
        .map(([k, sha]) => ({ name: k.slice('refs/heads/'.length), commit: { sha } }));
      json(res, 200, items); return;
    }
    if (rest.length === 2 && m === 'GET') {
      const sha = repo.refs.get('refs/heads/' + rest[1]);
      if (!sha) { notFound(res); return; }
      json(res, 200, { name: rest[1], commit: { sha } }); return;
    }
  }

  // commits
  if (r0 === 'commits' && m === 'GET') {
    if (rest.length === 1) {
      const ref = ctx.query.get('sha') || repo.default_branch;
      const tip = store.resolveRef(repo, ref);
      if (!tip) { json(res, 200, []); return; }
      const order: string[] = [];
      const seen = new Set<string>();
      const queue = [tip];
      while (queue.length) {
        const cur = queue.shift()!;
        if (seen.has(cur)) continue; seen.add(cur);
        const obj = store.getObject(repo, cur);
        if (obj && obj.type === 'commit') { order.push(cur); queue.push(...store.parseCommit(obj.data).parents); }
      }
      const perPage = Math.min(parseInt(ctx.query.get('per_page') || '30', 10) || 30, 100);
      json(res, 200, order.slice(0, perPage).map(sha => {
        const obj = store.getObject(repo, sha)!;
        return { sha, commit: { message: commitMessage(obj.data) }, node_id: nodeId('Commit', sha) };
      })); return;
    }
    if (rest.length === 2) {
      const obj = store.getObject(repo, rest[1]);
      if (!obj || obj.type !== 'commit') { notFound(res); return; }
      json(res, 200, { sha: rest[1], commit: { message: commitMessage(obj.data) } }); return;
    }
  }

  // git/refs
  if (r0 === 'git' && rest[1] === 'refs') {
    if (rest.length === 2 && m === 'POST') {
      const body = parseJson(ctx.rawBody); if (!body.ok) { badReq(res); return; }
      const ref = body.value?.ref; const sha = body.value?.sha;
      if (!ref || !isSha40(sha)) { json(res, 422, { message: 'ref and sha are required' }); return; }
      if (!store.getObject(repo, String(sha).toLowerCase())) { json(res, 422, { message: 'Object does not exist' }); return; }
      if (repo.refs.has(ref)) { json(res, 422, { message: 'Reference already exists' }); return; }
      repo.refs.set(ref, String(sha).toLowerCase());
      json(res, 201, { ref, object: { sha: String(sha).toLowerCase(), type: 'commit' } }); return;
    }
    if (rest.length >= 3 && m === 'GET') {
      const ref = 'refs/' + rest.slice(2).join('/');
      const sha = repo.refs.get(ref);
      if (!sha) { notFound(res); return; }
      json(res, 200, { ref, object: { sha, type: 'commit' } }); return;
    }
    if (rest.length === 2 && m === 'GET') {
      json(res, 200, [...repo.refs.entries()].map(([ref, sha]) => ({ ref, object: { sha } }))); return;
    }
  }

  // labels
  if (r0 === 'labels') {
    if (rest.length === 1 && m === 'POST') {
      const body = parseJson(ctx.rawBody); if (!body.ok) { badReq(res); return; }
      const name = body.value?.name;
      if (!name) { json(res, 422, { message: 'Label name is required' }); return; }
      if (repo.labels.has(name)) { json(res, 422, { message: 'Label already exists' }); return; }
      const label = { id: store.id(), name, color: body.value.color || 'ededed', description: body.value.description || '' };
      repo.labels.set(name, label);
      json(res, 201, labelJson(label)); return;
    }
    if (rest.length === 1 && m === 'GET') { json(res, 200, [...repo.labels.values()].map(labelJson)); return; }
  }

  // issues
  if (r0 === 'issues') {
    if (rest.length === 1) {
      if (m === 'POST') {
        const body = parseJson(ctx.rawBody); if (!body.ok) { badReq(res); return; }
        const title = body.value?.title;
        if (!title) { json(res, 422, { message: 'Issue title is required' }); return; }
        const labelNames: string[] = Array.isArray(body.value.labels) ? body.value.labels : [];
        const labels = [];
        for (const ln of labelNames) {
          const l = repo.labels.get(ln);
          if (!l) { json(res, 422, { message: `Label "${ln}" does not exist` }); return; }
          labels.push(l);
        }
        repo.issueCounter += 1;
        const issue = { id: store.id(), number: repo.issueCounter, title, body: body.value.body ?? '', state: 'open' as const, labels, comments: [], created_at: trialNow(), updated_at: trialNow() };
        repo.issues.push(issue);
        json(res, 201, issueJson(issue)); return;
      }
      if (m === 'GET') {
        const state = ctx.query.get('state') || 'open';
        const direction = ctx.query.get('direction') || 'desc';
        let items = repo.issues.filter(i => state === 'all' ? true : i.state === state);
        items = [...items].sort((a, b) => direction === 'asc' ? a.number - b.number : b.number - a.number);
        const perPage = Math.min(parseInt(ctx.query.get('per_page') || '30', 10) || 30, 100);
        json(res, 200, items.slice(0, perPage).map(issueJson)); return;
      }
    }
    if (rest.length === 2) {
      const n = parseInt(rest[1], 10);
      const issue = repo.issues.find(i => i.number === n);
      if (!issue) { notFound(res); return; }
      if (m === 'GET') { json(res, 200, issueJson(issue)); return; }
      if (m === 'PATCH') {
        const body = parseJson(ctx.rawBody); if (!body.ok) { badReq(res); return; }
        if (body.value?.state === 'open' || body.value?.state === 'closed') issue.state = body.value.state;
        if (typeof body.value?.title === 'string') issue.title = body.value.title;
        if (typeof body.value?.body === 'string') issue.body = body.value.body;
        issue.updated_at = trialNow();
        json(res, 200, issueJson(issue)); return;
      }
    }
    if (rest.length === 3 && rest[2] === 'comments') {
      const n = parseInt(rest[1], 10);
      const issue = repo.issues.find(i => i.number === n);
      if (!issue) { notFound(res); return; }
      if (m === 'POST') {
        const body = parseJson(ctx.rawBody); if (!body.ok) { badReq(res); return; }
        const comment = { id: store.id(), body: body.value?.body ?? '', created_at: trialNow() };
        issue.comments.push(comment);
        json(res, 201, commentJson(comment)); return;
      }
      if (m === 'GET') { json(res, 200, issue.comments.map(commentJson)); return; }
    }
  }

  // pulls
  if (r0 === 'pulls') {
    if (rest.length === 1) {
      if (m === 'POST') {
        const body = parseJson(ctx.rawBody); if (!body.ok) { badReq(res); return; }
        const head = body.value?.head; const base = body.value?.base;
        if (!head || !base) { json(res, 422, { message: 'head and base are required' }); return; }
        const headRef = String(head).includes(':') ? String(head).split(':').pop()! : String(head);
        if (!store.resolveRef(repo, headRef)) { json(res, 422, { message: `head ref "${headRef}" does not exist` }); return; }
        if (!store.resolveRef(repo, base)) { json(res, 422, { message: `base ref "${base}" does not exist` }); return; }
        repo.pullCounter += 1;
        const pull = { id: store.id(), number: repo.pullCounter, title: body.value.title ?? '', body: body.value.body ?? '', state: 'open' as const, head: { ref: headRef }, base: { ref: base }, merged: false, mergeable: true, merge_commit_sha: null, merged_at: null, created_at: trialNow(), updated_at: trialNow() };
        repo.pulls.push(pull);
        json(res, 201, pullJson(store, repo, pull)); return;
      }
      if (m === 'GET') {
        const state = ctx.query.get('state') || 'open';
        let items = repo.pulls.filter(p => state === 'all' ? true : p.state === state);
        items = [...items].sort((a, b) => b.number - a.number);
        const perPage = Math.min(parseInt(ctx.query.get('per_page') || '30', 10) || 30, 100);
        json(res, 200, items.slice(0, perPage).map(p => pullJson(store, repo, p))); return;
      }
    }
    if (rest.length >= 2) {
      const n = parseInt(rest[1], 10);
      const pull = repo.pulls.find(p => p.number === n);
      if (!pull) { notFound(res); return; }
      if (rest.length === 2 && m === 'GET') { json(res, 200, pullJson(store, repo, pull)); return; }
      if (rest.length === 3 && rest[2] === 'files' && m === 'GET') {
        const headTip = store.resolveRef(repo, pull.head.ref);
        const baseTip = store.resolveRef(repo, pull.base.ref);
        const changes = headTip && baseTip ? store.diffTrees(repo, store.commitTree(repo, baseTip), store.commitTree(repo, headTip)) : [];
        const files = changes.map(c => {
          let additions = 0, deletions = 0;
          const headObj = c.headOid ? store.getObject(repo, c.headOid) : null;
          const baseObj = c.baseOid ? store.getObject(repo, c.baseOid) : null;
          const hl = headObj ? headObj.data.toString('utf8').split('\n').length : 0;
          const bl = baseObj ? baseObj.data.toString('utf8').split('\n').length : 0;
          if (c.status === 'added') additions = Math.max(hl, 1);
          else if (c.status === 'removed') deletions = Math.max(bl, 1);
          else { additions = Math.max(hl - bl, 1); deletions = Math.max(bl - hl, 0); }
          return { filename: c.path, status: c.status === 'modified' ? 'modified' : c.status, additions, deletions, changes: additions + deletions, sha: c.headOid || c.baseOid };
        });
        json(res, 200, files); return;
      }
      if (rest.length === 3 && rest[2] === 'merge' && m === 'PUT') {
        const body = parseJson(ctx.rawBody); if (!body.ok) { badReq(res); return; }
        if (pull.merged) { json(res, 405, { message: 'Pull request is already merged' }); return; }
        const result = store.squashMerge(repo, pull);
        if (result.conflict) { json(res, 405, { message: 'Pull request is not mergeable (conflict)' }); return; }
        json(res, 200, { sha: result.sha, merged: true, message: `Merge pull request #${pull.number} (${body.value?.merge_method || 'squash'})` }); return;
      }
    }
  }

  notFound(res);
}
