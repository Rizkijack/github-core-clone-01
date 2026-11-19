// The forge's single in-memory store: users, repos, git objects/refs, issues, labels, pulls.
// Shared by REST, GraphQL, MCP and git smart-HTTP so every surface reads the same objects.
import { gitOid, sha1Hex, nodeId, trialNow, trialEpoch, ZERO_OID, isSha40 } from './util.ts';

export interface GitObject { type: 'blob' | 'tree' | 'commit' | 'tag'; data: Buffer; }
export interface TreeEntry { mode: string; name: string; oid: string; }
export interface FlatEntry { mode: string; oid: string; }

export const VIEWER = {
  id: 1,
  login: 'adaiversen',
  name: 'Ada Iversen',
  email: 'adaiversen@users.noreply.github.com',
  node_id: nodeId('User', 1),
  type: 'User',
};

export interface Label { id: number; name: string; color: string; description: string; }
export interface Comment { id: number; body: string; created_at: string; }
export interface Issue {
  id: number; number: number; title: string; body: string; state: 'open' | 'closed';
  labels: Label[]; comments: Comment[]; created_at: string; updated_at: string;
}
export interface Pull {
  id: number; number: number; title: string; body: string; state: 'open' | 'closed';
  head: { ref: string }; base: { ref: string };
  merged: boolean; mergeable: boolean; merge_commit_sha: string | null; merged_at: string | null;
  created_at: string; updated_at: string;
}
export interface Repo {
  id: number; name: string; owner: typeof VIEWER; private: boolean; description: string;
  default_branch: string; created_at: string; updated_at: string;
  objects: Map<string, GitObject>;
  refs: Map<string, string>;
  head: string; // symbolic target, e.g. refs/heads/main
  issues: Issue[]; issueCounter: number;
  labels: Map<string, Label>;
  pulls: Pull[]; pullCounter: number;
}

export class Store {
  repos = new Map<string, Repo>(); // key: name (all owned by VIEWER)
  private nextId = 1000;
  id(): number { return this.nextId++; }

  createRepo(name: string, opts: { private?: boolean; description?: string } = {}): Repo {
    const repo: Repo = {
      id: this.id(), name, owner: VIEWER, private: opts.private ?? true,
      description: opts.description ?? '', default_branch: 'main',
      created_at: trialNow(), updated_at: trialNow(),
      objects: new Map(), refs: new Map(), head: 'refs/heads/main',
      issues: [], issueCounter: 0, labels: new Map(), pulls: [], pullCounter: 0,
    };
    this.repos.set(name, repo);
    return repo;
  }
  getRepo(owner: string, name: string): Repo | null {
    if (owner !== VIEWER.login) return null;
    return this.repos.get(name) ?? null;
  }
  deleteRepo(name: string): boolean { return this.repos.delete(name); }

  // ---- git objects ----
  storeObject(repo: Repo, type: GitObject['type'], data: Buffer): string {
    const oid = gitOid(type, data);
    if (!repo.objects.has(oid)) repo.objects.set(oid, { type, data });
    return oid;
  }
  getObject(repo: Repo, oid: string): GitObject | null { return repo.objects.get(oid.toLowerCase()) ?? null; }

  parseTree(data: Buffer): TreeEntry[] {
    const entries: TreeEntry[] = [];
    let i = 0;
    while (i < data.length) {
      const sp = data.indexOf(0x20, i);
      const mode = data.subarray(i, sp).toString('ascii');
      const nul = data.indexOf(0x00, sp + 1);
      const name = data.subarray(sp + 1, nul).toString('utf8');
      const oid = data.subarray(nul + 1, nul + 21).toString('hex');
      entries.push({ mode: mode === '40000' ? '40000' : mode, name, oid });
      i = nul + 21;
    }
    return entries;
  }
  parseCommit(data: Buffer): { tree: string; parents: string[] } {
    const text = data.toString('utf8');
    const blank = text.indexOf('\n\n');
    const header = blank >= 0 ? text.slice(0, blank) : text;
    let tree = ''; const parents: string[] = [];
    for (const line of header.split('\n')) {
      if (line.startsWith('tree ')) tree = line.slice(5).trim();
      else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
    }
    return { tree, parents };
  }
  commitTree(repo: Repo, commitOid: string): string {
    const obj = this.getObject(repo, commitOid);
    if (!obj || obj.type !== 'commit') return '';
    return this.parseCommit(obj.data).tree;
  }

  flattenTree(repo: Repo, treeOid: string, prefix = ''): Map<string, FlatEntry> {
    const flat = new Map<string, FlatEntry>();
    if (!treeOid) return flat;
    const obj = this.getObject(repo, treeOid);
    if (!obj || obj.type !== 'tree') return flat;
    for (const e of this.parseTree(obj.data)) {
      if (e.mode === '40000') {
        for (const [k, v] of this.flattenTree(repo, e.oid, prefix + e.name + '/')) flat.set(k, v);
      } else {
        flat.set(prefix + e.name, { mode: e.mode, oid: e.oid });
      }
    }
    return flat;
  }

  buildTree(repo: Repo, flat: Map<string, FlatEntry>): string {
    // nested trie
    const root: any = {};
    for (const [path, entry] of flat) {
      const parts = path.split('/');
      let node = root;
      for (let i = 0; i < parts.length - 1; i++) {
        node[parts[i]] = node[parts[i]] || { __dir: true };
        node = node[parts[i]];
      }
      node[parts[parts.length - 1]] = { __file: true, ...entry };
    }
    const serialize = (node: any): string => {
      const entries: TreeEntry[] = [];
      for (const name of Object.keys(node)) {
        if (name === '__dir' || name === '__file') continue;
        const child = node[name];
        if (child.__dir) entries.push({ mode: '40000', name, oid: serialize(child) });
        else entries.push({ mode: child.mode || '100644', name, oid: child.oid });
      }
      entries.sort(treeCompare);
      const chunks: Buffer[] = [];
      for (const e of entries) {
        chunks.push(Buffer.from(`${e.mode === '40000' ? '40000' : e.mode} ${e.name}\0`, 'utf8'));
        chunks.push(Buffer.from(e.oid, 'hex'));
      }
      const data = Buffer.concat(chunks);
      return this.storeObject(repo, 'tree', data);
    };
    return serialize(root);
  }

  makeCommit(repo: Repo, opts: { tree: string; parents: string[]; message: string }): string {
    const epoch = trialEpoch();
    const ident = `${VIEWER.name} <${VIEWER.email}> ${epoch} +0000`;
    let msg = opts.message;
    if (!msg.endsWith('\n')) msg += '\n';
    const text = `tree ${opts.tree}\n` + opts.parents.map(p => `parent ${p}\n`).join('') +
      `author ${ident}\ncommitter ${ident}\n\n${msg}`;
    return this.storeObject(repo, 'commit', Buffer.from(text, 'utf8'));
  }

  resolveRef(repo: Repo, ref: string): string | null {
    if (!ref) return null;
    if (ref === 'HEAD') return this.resolveRef(repo, repo.head);
    if (ref.startsWith('refs/')) return repo.refs.get(ref) ?? null;
    if (repo.refs.has('refs/heads/' + ref)) return repo.refs.get('refs/heads/' + ref)!;
    if (repo.refs.has('refs/tags/' + ref)) return repo.refs.get('refs/tags/' + ref)!;
    if (isSha40(ref) && repo.objects.has(ref.toLowerCase())) return ref.toLowerCase();
    return null;
  }
  branchTip(repo: Repo, branch: string): string | null { return repo.refs.get('refs/heads/' + branch) ?? null; }

  ancestors(repo: Repo, oid: string): Set<string> {
    const seen = new Set<string>();
    const queue = [oid];
    while (queue.length) {
      const cur = queue.shift()!;
      if (!cur || seen.has(cur)) continue;
      seen.add(cur);
      const obj = this.getObject(repo, cur);
      if (obj && obj.type === 'commit') queue.push(...this.parseCommit(obj.data).parents);
    }
    return seen;
  }
  mergeBase(repo: Repo, a: string, b: string): string | null {
    if (a === b) return a;
    const ancB = this.ancestors(repo, b);
    const queue = [a]; const seen = new Set<string>();
    while (queue.length) {
      const cur = queue.shift()!;
      if (!cur || seen.has(cur)) continue;
      seen.add(cur);
      if (ancB.has(cur)) return cur;
      const obj = this.getObject(repo, cur);
      if (obj && obj.type === 'commit') queue.push(...this.parseCommit(obj.data).parents);
    }
    return null;
  }

  diffTrees(repo: Repo, baseTree: string, headTree: string): { path: string; status: string; baseOid?: string; headOid?: string }[] {
    const b = this.flattenTree(repo, baseTree);
    const h = this.flattenTree(repo, headTree);
    const out: { path: string; status: string; baseOid?: string; headOid?: string }[] = [];
    for (const [path, he] of h) {
      const be = b.get(path);
      if (!be) out.push({ path, status: 'added', headOid: he.oid });
      else if (be.oid !== he.oid) out.push({ path, status: 'modified', baseOid: be.oid, headOid: he.oid });
    }
    for (const [path, be] of b) if (!h.has(path)) out.push({ path, status: 'removed', baseOid: be.oid });
    out.sort((x, y) => x.path < y.path ? -1 : x.path > y.path ? 1 : 0);
    return out;
  }

  /** Squash-merge pull.head onto pull.base. Returns conflict flag or the new commit sha. */
  squashMerge(repo: Repo, pull: Pull): { conflict: boolean; sha?: string } {
    const headTip = this.resolveRef(repo, pull.head.ref);
    const baseTip = this.resolveRef(repo, pull.base.ref);
    if (!headTip || !baseTip) return { conflict: true };
    const mb = this.mergeBase(repo, headTip, baseTip) ?? baseTip;
    const mbTree = this.commitTree(repo, mb);
    const baseTree = this.commitTree(repo, baseTip);
    const headTree = this.commitTree(repo, headTip);
    const changes = this.diffTrees(repo, mbTree, headTree);
    const baseFlat = this.flattenTree(repo, baseTree);
    const mbFlat = this.flattenTree(repo, mbTree);
    const headFlat = this.flattenTree(repo, headTree);
    for (const c of changes) {
      if ((baseFlat.get(c.path)?.oid ?? null) !== (mbFlat.get(c.path)?.oid ?? null)) return { conflict: true };
    }
    const result = new Map(baseFlat);
    for (const c of changes) {
      const he = headFlat.get(c.path);
      if (he) result.set(c.path, he); else result.delete(c.path);
    }
    const tree = this.buildTree(repo, result);
    const sha = this.makeCommit(repo, { tree, parents: [baseTip], message: `${pull.title} (#${pull.number})` });
    repo.refs.set('refs/heads/' + pull.base.ref, sha);
    pull.merged = true; pull.state = 'closed'; pull.mergeable = false;
    pull.merge_commit_sha = sha; pull.merged_at = trialNow(); pull.updated_at = trialNow();
    return { conflict: false, sha };
  }

  // ---- contents API commit ----
  commitContents(repo: Repo, opts: { branch: string; path: string; content: Buffer | null; message: string }): { commitSha: string; blobSha: string | null } {
    const tip = this.branchTip(repo, opts.branch);
    const flat = tip ? this.flattenTree(repo, this.commitTree(repo, tip)) : new Map<string, FlatEntry>();
    let blobSha: string | null = null;
    if (opts.content === null) flat.delete(opts.path);
    else { blobSha = this.storeObject(repo, 'blob', opts.content); flat.set(opts.path, { mode: '100644', oid: blobSha }); }
    const tree = this.buildTree(repo, flat);
    const commitSha = this.makeCommit(repo, { tree, parents: tip ? [tip] : [], message: opts.message });
    repo.refs.set('refs/heads/' + opts.branch, commitSha);
    repo.updated_at = trialNow();
    return { commitSha, blobSha };
  }
}

/** Git tree entry ordering: names compared as bytes, directories as if suffixed '/'. */
export function treeCompare(a: TreeEntry, b: TreeEntry): number {
  const an = a.mode === '40000' ? a.name + '/' : a.name;
  const bn = b.mode === '40000' ? b.name + '/' : b.name;
  return an < bn ? -1 : an > bn ? 1 : 0;
}

// ---- REST serializers ----
export function repoJson(r: Repo) {
  return {
    id: r.id, node_id: nodeId('Repository', r.id), name: r.name, full_name: `${r.owner.login}/${r.name}`,
    owner: { login: r.owner.login, id: r.owner.id, node_id: r.owner.node_id, type: 'User' },
    private: r.private, description: r.description, fork: false,
    default_branch: r.default_branch, created_at: r.created_at, updated_at: r.updated_at,
    has_issues: true, has_issues_enabled: true, visibility: r.private ? 'private' : 'public',
  };
}
export function labelJson(l: Label) {
  return { id: l.id, node_id: nodeId('Label', l.id), name: l.name, color: l.color, description: l.description, default: false };
}
export function issueJson(i: Issue) {
  return {
    id: i.id, node_id: nodeId('Issue', i.id), number: i.number, title: i.title, body: i.body,
    state: i.state, labels: i.labels.map(labelJson), user: { login: VIEWER.login, id: VIEWER.id, node_id: VIEWER.node_id },
    comments: i.comments.length, created_at: i.created_at, updated_at: i.updated_at,
  };
}
export function commentJson(c: Comment) {
  return { id: c.id, node_id: nodeId('IssueComment', c.id), body: c.body, user: { login: VIEWER.login }, created_at: c.created_at, updated_at: c.created_at };
}
export function pullJson(store: Store, r: Repo, p: Pull) {
  const headSha = store.resolveRef(r, p.head.ref) ?? ZERO_OID;
  const baseSha = store.resolveRef(r, p.base.ref) ?? ZERO_OID;
  return {
    id: p.id, node_id: nodeId('PullRequest', p.id), number: p.number, state: p.state, title: p.title, body: p.body,
    head: { ref: p.head.ref, sha: headSha, label: `${r.owner.login}:${p.head.ref}` },
    base: { ref: p.base.ref, sha: baseSha, label: `${r.owner.login}:${p.base.ref}` },
    merged: p.merged, mergeable: p.merged ? false : p.mergeable, merge_commit_sha: p.merge_commit_sha,
    merged_at: p.merged_at, created_at: p.created_at, updated_at: p.updated_at,
    user: { login: VIEWER.login },
  };
}
