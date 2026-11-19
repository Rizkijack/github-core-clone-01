// Shared helpers: hashing, encoding, deterministic ids, JSON response discipline.
import { createHash } from 'node:crypto';

export const ZERO_OID = '0'.repeat(40);

export function sha1Hex(data: Buffer | string): string {
  return createHash('sha1').update(data).digest('hex');
}

/** Git object id: sha1 of "<type> <size>\0" + content. */
export function gitOid(type: string, data: Buffer): string {
  const header = Buffer.from(`${type} ${data.length}\0`, 'ascii');
  return createHash('sha1').update(header).update(data).digest('hex');
}

export function b64encode(data: Buffer | string): string {
  return Buffer.from(data).toString('base64');
}
export function b64decode(text: string): Buffer {
  return Buffer.from(String(text).replace(/[\r\n\t ]/g, ''), 'base64');
}

const B58 = '123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz';
export function base58(buf: Buffer): string {
  let n = BigInt('0x' + (buf.length ? buf.toString('hex') : '0'));
  let out = '';
  while (n > 0n) { const r = Number(n % 58n); n /= 58n; out = B58[r] + out; }
  for (const b of buf) { if (b) break; out = '1' + out; }
  return out || '1';
}

/** GitHub-style global node id: base64 of "010:<Type><databaseId>". Opaque, stable, non-numeric. */
export function nodeId(type: string, databaseId: number | string): string {
  return Buffer.from(`010:${type}${databaseId}`, 'ascii').toString('base64');
}
export function decodeNodeId(id: string): { type: string; databaseId: number } | null {
  try {
    const raw = Buffer.from(String(id), 'base64').toString('ascii');
    const m = /^010:([A-Za-z]+)(\d+)$/.exec(raw);
    if (!m) return null;
    return { type: m[1], databaseId: Number(m[2]) };
  } catch { return null; }
}

export function slug(id: string): string {
  return String(id).toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/^-+|-+$/g, '');
}

/** Deterministic request id (monotonic counter, never random). */
let reqCounter = 0;
export function nextRequestId(): string {
  reqCounter += 1;
  return reqCounter.toString(16).padStart(8, '0');
}

/** The fixed trial clock: every timestamp derives from MAKEFUN_TRIAL_NOW, never Date.now(). */
export function trialNow(): string {
  return process.env.MAKEFUN_TRIAL_NOW || '2026-11-14T06:30:00.000Z';
}
export function trialEpoch(): number {
  const t = Date.parse(trialNow());
  return Number.isFinite(t) ? Math.floor(t / 1000) : 1763101800;
}

export function isSha40(s: unknown): s is string {
  return typeof s === 'string' && /^[0-9a-f]{40}$/i.test(s);
}
