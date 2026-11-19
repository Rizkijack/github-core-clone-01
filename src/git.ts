// Git smart-HTTP: upload-pack (v2 + v0 fallback) and receive-pack (v0), pure Node stdlib.
import type { ServerResponse } from 'node:http';
import { deflateSync, inflateSync } from 'node:zlib';
import { createHash } from 'node:crypto';
import { Store, type Repo, type GitObject } from './store.ts';
import { json, type Ctx } from './http.ts';
import { ZERO_OID, gitOid } from './util.ts';

const FLUSH = Buffer.from('0000', 'ascii');
const MAX_CHUNK = 65515; // side-band-64k band-1 payload cap (65520 - 4 header - 1 band)

function pkt(payload: string | Buffer): Buffer {
  const data = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const len = (data.length + 4).toString(16).padStart(4, '0');
  return Buffer.concat([Buffer.from(len, 'ascii'), data]);
}

interface Packet { kind: 'data' | 'flush' | 'delim' | 'end'; payload?: Buffer; }
function decodePkts(buf: Buffer): { packets: Packet[]; rest: Buffer } {
  const packets: Packet[] = [];
  let i = 0;
  while (i + 4 <= buf.length) {
    const hex = buf.subarray(i, i + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(hex)) break;
    const len = parseInt(hex, 16);
    if (len === 0) { packets.push({ kind: 'flush' }); i += 4; continue; }
    if (len === 1) { packets.push({ kind: 'delim' }); i += 4; continue; }
    if (len === 2) { packets.push({ kind: 'end' }); i += 4; continue; }
    if (len < 4 || i + len > buf.length) break;
    packets.push({ kind: 'data', payload: buf.subarray(i + 4, i + len) });
    i += len;
  }
  return { packets, rest: buf.subarray(i) };
}

function gitRes(res: ServerResponse, service: string, kind: 'advertisement' | 'result', body: Buffer): void {
  res.writeHead(200, {
    'content-type': `application/x-${service}-${kind}`,
    'content-length': body.length,
    'cache-control': 'no-cache',
  });
  res.end(body);
}

// ---- pack codec ----
const TYPE_CODE: Record<string, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPE: Record<number, GitObject['type']> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

function packHeader(type: string, size: number): Buffer {
  const code = TYPE_CODE[type];
  const bytes: number[] = [];
  let remaining = size;
  let first = (code << 4) | (remaining & 0x0f);
  remaining = Math.floor(remaining / 16);
  while (remaining > 0) { bytes.push(first | 0x80); first = remaining & 0x7f; remaining = Math.floor(remaining / 128); }
  bytes.push(first);
  return Buffer.from(bytes);
}

function buildPack(objects: { type: string; data: Buffer }[]): Buffer {
  const parts: Buffer[] = [];
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 4, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objects.length, 8);
  parts.push(header);
  for (const o of objects) { parts.push(packHeader(o.type, o.data.length)); parts.push(deflateSync(o.data)); }
  const body = Buffer.concat(parts);
  return Buffer.concat([body, createHash('sha1').update(body).digest()]);
}

function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let off = 0;
  const readSize = (): number => {
    let value = 0, shift = 0;
    for (;;) { const b = delta[off++]; value += (b & 0x7f) * 2 ** shift; shift += 7; if (!(b & 0x80)) break; }
    return value;
  };
  const sourceSize = readSize();
  const targetSize = readSize();
  if (sourceSize !== base.length) throw new Error('delta base size mismatch');
  const out = Buffer.alloc(targetSize);
  let written = 0;
  while (off < delta.length) {
    const op = delta[off++];
    if (op & 0x80) {
      let copyOffset = 0, copySize = 0, shift = 0;
      for (let bit = 0; bit < 4; bit++) { if (op & (1 << bit)) copyOffset += delta[off++] * 2 ** shift; shift += 8; }
      shift = 0;
      for (let bit = 4; bit < 7; bit++) { if (op & (1 << bit)) copySize += delta[off++] * 2 ** shift; shift += 8; }
      if (copySize === 0) copySize = 0x10000;
      base.copy(out, written, copyOffset, copyOffset + copySize);
      written += copySize;
    } else if (op !== 0) {
      delta.copy(out, written, off, off + op);
      off += op; written += op;
    } else throw new Error('delta opcode 0');
  }
  return out;
}

/** Parse a packfile into resolved objects (plain + OFS/REF deltas). */
function parsePack(buf: Buffer): Map<string, GitObject> {
  if (buf.length < 32 || buf.subarray(0, 4).toString('ascii') !== 'PACK') throw new Error('bad pack header');
  const version = buf.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error('bad pack version');
  const count = buf.readUInt32BE(8);
  const trailerStart = buf.length - 20;
  const computed = createHash('sha1').update(buf.subarray(0, trailerStart)).digest('hex');
  if (computed !== buf.subarray(trailerStart).toString('hex')) throw new Error('pack checksum mismatch');
  interface Raw { offset: number; typeCode: number; size: number; data: Buffer; baseOffset?: number; baseOid?: string; resolved?: Buffer; rtype?: GitObject['type']; oid?: string; }
  const raw: Raw[] = [];
  const byOffset = new Map<number, Raw>();
  let off = 12;
  for (let n = 0; n < count; n++) {
    const entryOffset = off;
    let byte = buf[off]; const first = byte;
    let size = byte & 0x0f; let shift = 4; off++;
    while (byte & 0x80) { byte = buf[off++]; size += (byte & 0x7f) * 2 ** shift; shift += 7; }
    const typeCode = (first >> 4) & 0x07;
    const entry: Raw = { offset: entryOffset, typeCode, size, data: Buffer.alloc(0) };
    if (typeCode === 6) {
      let b = buf[off]; off++; let value = b & 0x7f;
      while (b & 0x80) { b = buf[off++]; value = ((value + 1) * 128) + (b & 0x7f); }
      entry.baseOffset = entryOffset - value;
    } else if (typeCode === 7) {
      entry.baseOid = buf.subarray(off, off + 20).toString('hex'); off += 20;
    } else if (!CODE_TYPE[typeCode]) throw new Error('bad pack type');
    const view = buf.subarray(off, trailerStart);
    const infl = inflateSync(view, { info: true }) as unknown as { buffer: Buffer; engine: { bytesWritten: number } };
    if (infl.buffer.length !== size) throw new Error('pack size mismatch');
    entry.data = infl.buffer;
    off += infl.engine.bytesWritten;
    raw.push(entry); byOffset.set(entryOffset, entry);
  }
  const objects = new Map<string, GitObject>();
  const byOid = new Map<string, Raw>();
  const resolve = (e: Raw): { type: GitObject['type']; data: Buffer; oid: string } => {
    if (e.resolved && e.rtype && e.oid) return { type: e.rtype, data: e.resolved, oid: e.oid };
    let type: GitObject['type']; let data: Buffer;
    if (e.typeCode === 6) {
      const base = resolve(byOffset.get(e.baseOffset!)!);
      type = base.type; data = applyDelta(base.data, e.data);
    } else if (e.typeCode === 7) {
      const baseEntry = byOid.get(e.baseOid!);
      if (!baseEntry) throw new Error('ref-delta base missing');
      const base = resolve(baseEntry);
      type = base.type; data = applyDelta(base.data, e.data);
    } else { type = CODE_TYPE[e.typeCode]; data = e.data; }
    const oid = gitOid(type, data);
    e.resolved = data; e.rtype = type; e.oid = oid; byOid.set(oid, e);
    return { type, data, oid };
  };
  for (const e of raw) {
    const r = resolve(e);
    if (!objects.has(r.oid)) objects.set(r.oid, { type: r.type, data: r.data });
  }
  return objects;
}

// ---- reachability ----
function parseTagObj(data: Buffer): string {
  const text = data.toString('utf8');
  const m = /^object ([0-9a-f]{40})/m.exec(text);
  return m ? m[1] : '';
}
function peel(store: Store, repo: Repo, oid: string): string {
  let cur = oid;
  for (let i = 0; i < 16; i++) {
    const obj = store.getObject(repo, cur);
    if (!obj || obj.type !== 'tag') return cur;
    cur = parseTagObj(obj.data);
  }
  return cur;
}

function addClosure(store: Store, repo: Repo, oid: string, out: Set<string>): void {
  if (out.has(oid)) return;
  const obj = store.getObject(repo, oid);
  if (!obj) return;
  out.add(oid);
  if (obj.type === 'tree') for (const e of store.parseTree(obj.data)) addClosure(store, repo, e.oid, out);
  else if (obj.type === 'tag') addClosure(store, repo, parseTagObj(obj.data), out);
}

/** Reachable objects from roots; commits limited to maxDepth generations, cut at `cut` commits. */
function reach(store: Store, repo: Repo, roots: string[], opts: { maxDepth?: number; cut?: Set<string>; boundary?: Set<string> } = {}): Set<string> {
  const maxDepth = opts.maxDepth ?? Infinity;
  const cut = opts.cut ?? new Set<string>();
  const out = new Set<string>();
  const commitDepth = new Map<string, number>();
  const stack: [string, number][] = roots.map(o => [o, 1]);
  while (stack.length) {
    const [oid, depth] = stack.pop()!;
    const obj = store.getObject(repo, oid);
    if (!obj) continue;
    if (obj.type === 'commit') {
      const prev = commitDepth.get(oid);
      if (prev !== undefined && prev <= depth) continue;
      commitDepth.set(oid, depth);
      out.add(oid);
      const { tree, parents } = store.parseCommit(obj.data);
      addClosure(store, repo, tree, out);
      const atBoundary = depth >= maxDepth;
      if (atBoundary && opts.boundary) opts.boundary.add(oid);
      if (!cut.has(oid) && !atBoundary) for (const p of parents) stack.push([p, depth + 1]);
    } else {
      addClosure(store, repo, oid, out);
    }
  }
  return out;
}

// ---- advertisement ----
function advertise(store: Store, repo: Repo, service: string, v2: boolean): Buffer {
  if (v2 && service === 'git-upload-pack') {
    return Buffer.concat([
      pkt('version 2\n'), pkt('ls-refs=unborn\n'), pkt('fetch=shallow\n'),
      pkt('object-format=sha1\n'), pkt('agent=forge/1.0\n'), FLUSH,
    ]);
  }
  const caps = service === 'git-receive-pack'
    ? 'report-status side-band-64k delete-refs ofs-delta symref=HEAD:refs/heads/main agent=forge/1.0'
    : 'multi_ack_detailed side-band-64k thin-pack ofs-delta shallow no-progress include-tag symref=HEAD:refs/heads/main agent=forge/1.0';
  const parts: Buffer[] = [pkt(`# service=${service}\n`), FLUSH];
  const headTip = store.resolveRef(repo, 'HEAD');
  const lines: string[] = [];
  if (headTip) lines.push(`${headTip} HEAD`);
  for (const [ref, oid] of repo.refs) lines.push(`${oid} ${ref}`);
  if (lines.length === 0) {
    parts.push(pkt(`${ZERO_OID} capabilities^{}\0${caps}\n`));
  } else {
    lines.forEach((line, i) => parts.push(pkt(i === 0 ? `${line}\0${caps}\n` : `${line}\n`)));
    for (const [ref, oid] of repo.refs) {
      if (ref.startsWith('refs/tags/')) {
        const obj = store.getObject(repo, oid);
        if (obj && obj.type === 'tag') parts.push(pkt(`${peel(store, repo, oid)} ${ref}^{}\n`));
      }
    }
  }
  parts.push(FLUSH);
  return Buffer.concat(parts);
}

// ---- ls-refs (v2) ----
function lsRefs(store: Store, repo: Repo, packets: Packet[]): Buffer {
  let peelFlag = false, symrefs = false, unborn = false;
  const prefixes: string[] = [];
  let afterDelim = false;
  for (const p of packets) {
    if (p.kind === 'delim') { afterDelim = true; continue; }
    if (p.kind !== 'data' || !afterDelim) continue;
    const line = p.payload!.toString('utf8').replace(/\n$/, '');
    if (line === 'peel') peelFlag = true;
    else if (line === 'symrefs') symrefs = true;
    else if (line === 'unborn') unborn = true;
    else if (line.startsWith('ref-prefix ')) prefixes.push(line.slice(11));
  }
  const match = (name: string) => prefixes.length === 0 || prefixes.some(p => name.startsWith(p));
  const parts: Buffer[] = [];
  const headTip = store.resolveRef(repo, 'HEAD');
  if (symrefs && match('HEAD')) {
    if (headTip) parts.push(pkt(`${headTip} HEAD symref-target:${repo.head}\n`));
    else if (unborn && repo.refs.size === 0) parts.push(pkt(`unborn HEAD symref-target:${repo.head}\n`));
  }
  for (const [ref, oid] of repo.refs) {
    if (!match(ref)) continue;
    const obj = store.getObject(repo, oid);
    if (peelFlag && obj && obj.type === 'tag') parts.push(pkt(`${oid} ${ref} peeled:${peel(store, repo, oid)}\n`));
    else parts.push(pkt(`${oid} ${ref}\n`));
  }
  parts.push(FLUSH);
  return Buffer.concat(parts);
}

// ---- fetch ----
function sidebandPack(pack: Buffer): Buffer[] {
  const parts: Buffer[] = [];
  if (pack.length === 0) { parts.push(pkt(Buffer.concat([Buffer.from([1]), pack]))); return parts; }
  for (let i = 0; i < pack.length; i += MAX_CHUNK) {
    parts.push(pkt(Buffer.concat([Buffer.from([1]), pack.subarray(i, i + MAX_CHUNK)])));
  }
  return parts;
}

function doFetch(store: Store, repo: Repo, packets: Packet[], v2: boolean): Buffer {
  const wants: string[] = []; const haves: string[] = []; const clientShallow: string[] = [];
  let deepen: number | null = null;
  for (const p of packets) {
    if (p.kind !== 'data') continue;
    const line = p.payload!.toString('utf8').replace(/\n$/, '');
    if (line.startsWith('want ')) wants.push(line.slice(5).split(' ')[0]);
    else if (line.startsWith('have ')) haves.push(line.slice(5).trim());
    else if (line.startsWith('shallow ')) clientShallow.push(line.slice(8).trim());
    else if (line.startsWith('deepen ')) deepen = parseInt(line.slice(7), 10);
  }
  const cut = new Set(clientShallow);
  const clientHas = reach(store, repo, [...haves, ...clientShallow], { cut });
  const boundary = new Set<string>();
  const wanted = reach(store, repo, wants, { maxDepth: deepen ?? Infinity, boundary });
  const sendOids = [...wanted].filter(o => !clientHas.has(o));
  const sendObjects = sendOids.map(o => store.getObject(repo, o)!).filter(Boolean);
  const pack = buildPack(sendObjects);

  const parts: Buffer[] = [];
  if (v2) {
    parts.push(pkt('acknowledgments\n'), pkt('NAK\n'), FLUSH);
    if (deepen !== null) {
      parts.push(pkt('shallow-info\n'));
      for (const b of boundary) parts.push(pkt(`shallow ${b}\n`));
      for (const c of clientShallow) if (!boundary.has(c) && wanted.has(c)) parts.push(pkt(`unshallow ${c}\n`));
      parts.push(FLUSH);
    }
    parts.push(pkt('packfile\n'), ...sidebandPack(pack), FLUSH);
  } else {
    if (deepen !== null) {
      for (const b of boundary) parts.push(pkt(`shallow ${b}\n`));
      for (const c of clientShallow) if (!boundary.has(c) && wanted.has(c)) parts.push(pkt(`unshallow ${c}\n`));
    }
    parts.push(pkt('NAK\n'), ...sidebandPack(pack));
  }
  return Buffer.concat(parts);
}

// ---- receive-pack ----
function doReceive(store: Store, repo: Repo, body: Buffer): Buffer {
  const { packets, rest } = decodePkts(body);
  const commands: { old: string; new: string; ref: string }[] = [];
  let caps = '';
  for (const p of packets) {
    if (p.kind !== 'data') continue;
    let line = p.payload!.toString('utf8').replace(/\n$/, '');
    const nul = line.indexOf('\0');
    if (nul >= 0) { caps = line.slice(nul + 1); line = line.slice(0, nul); }
    const m = /^([0-9a-f]{40}) ([0-9a-f]{40}) (.+)$/.exec(line);
    if (m) commands.push({ old: m[1], new: m[2], ref: m[3] });
  }
  const sideband = caps.includes('side-band-64k');
  let unpackOk = true;
  if (rest.length > 0) {
    try {
      const objs = parsePack(rest);
      for (const [oid, o] of objs) if (!repo.objects.has(oid)) repo.objects.set(oid, o);
    } catch { unpackOk = false; }
  }
  const report: string[] = [`unpack ${unpackOk ? 'ok' : 'error'}`];
  if (unpackOk) {
    for (const c of commands) {
      const current = repo.refs.get(c.ref) ?? ZERO_OID;
      let ok = false;
      if (c.new === ZERO_OID) {
        ok = current === c.old && repo.refs.has(c.ref);
        if (ok) repo.refs.delete(c.ref);
      } else if (c.old === ZERO_OID) {
        ok = !repo.refs.has(c.ref) && repo.objects.has(c.new);
        if (ok) repo.refs.set(c.ref, c.new);
      } else {
        const ff = current === c.old && repo.objects.has(c.new) && store.ancestors(repo, c.new).has(c.old);
        ok = ff;
        if (ok) repo.refs.set(c.ref, c.new);
      }
      report.push(ok ? `ok ${c.ref}` : `ng ${c.ref} non-fast-forward`);
    }
  }
  const inner = Buffer.concat([...report.map(l => pkt(l + '\n')), FLUSH]);
  if (sideband) {
    const parts: Buffer[] = [];
    for (let i = 0; i < inner.length; i += MAX_CHUNK) parts.push(pkt(Buffer.concat([Buffer.from([1]), inner.subarray(i, i + MAX_CHUNK)])));
    return Buffer.concat(parts);
  }
  return inner;
}

export function handleGit(store: Store, ctx: Ctx, res: ServerResponse, repo: Repo, gitPath: string): void {
  if (!ctx.authed) { json(res, 401, { message: 'Requires authentication' }); return; }
  const service = ctx.query.get('service') || '';
  const v2 = ctx.headers['git-protocol'] === 'version=2';
  try {
    if (gitPath === '/info/refs' && ctx.method === 'GET') {
      if (service !== 'git-upload-pack' && service !== 'git-receive-pack') { json(res, 400, { message: 'bad service' }); return; }
      const useV2 = v2 && service === 'git-upload-pack';
      gitRes(res, service, 'advertisement', advertise(store, repo, service, useV2));
      return;
    }
    if (gitPath === '/git-upload-pack' && ctx.method === 'POST') {
      const { packets } = decodePkts(ctx.rawBody);
      const cmd = packets.find(p => p.kind === 'data' && p.payload!.toString('utf8').startsWith('command='));
      const command = cmd ? cmd.payload!.toString('utf8').replace(/\n$/, '').slice(8) : '';
      if (v2 && command === 'ls-refs') { gitRes(res, 'git-upload-pack', 'result', lsRefs(store, repo, packets)); return; }
      if (v2 && command === 'fetch') { gitRes(res, 'git-upload-pack', 'result', doFetch(store, repo, packets, true)); return; }
      if (!v2) { gitRes(res, 'git-upload-pack', 'result', doFetch(store, repo, packets, false)); return; }
      json(res, 400, { message: 'bad upload-pack request' }); return;
    }
    if (gitPath === '/git-receive-pack' && ctx.method === 'POST') {
      gitRes(res, 'git-receive-pack', 'result', doReceive(store, repo, ctx.rawBody));
      return;
    }
    json(res, 404, { message: 'Not Found' });
  } catch (err) {
    process.stderr.write(`[git] ${gitPath}: ${err instanceof Error ? err.message : String(err)}\n`);
    if (!res.headersSent) json(res, 400, { message: 'git protocol error' });
    else res.end();
  }
}
