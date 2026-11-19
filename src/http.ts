import type { IncomingMessage, ServerResponse } from 'node:http';
import { nextRequestId } from './util.ts';

export const JSON_TYPE = 'application/json; charset=utf-8';

/** JSON reply with the mandatory non-empty X-GitHub-Request-Id. */
export function json(res: ServerResponse, status: number, body: unknown, headers: Record<string, string> = {}): void {
  const text = JSON.stringify(body);
  res.writeHead(status, {
    'content-type': JSON_TYPE,
    'content-length': Buffer.byteLength(text),
    'x-github-request-id': nextRequestId(),
    ...headers,
  });
  res.end(text);
}

export function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on('data', (c) => chunks.push(Buffer.isBuffer(c) ? c : Buffer.from(c)));
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

export interface Ctx {
  method: string;
  pathname: string;
  query: URLSearchParams;
  rawBody: Buffer;
  authed: boolean;
  host: string;
  headers: Record<string, string | string[] | undefined>;
}

/** Parse a JSON body; returns {ok:false} on malformed JSON (caller answers 400). */
export function parseJson(raw: Buffer): { ok: boolean; value: any } {
  if (raw.length === 0) return { ok: true, value: {} };
  try { return { ok: true, value: JSON.parse(raw.toString('utf8')) }; }
  catch { return { ok: false, value: null }; }
}
