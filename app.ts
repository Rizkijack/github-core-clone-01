// The forge HTTP server. main.ts owns the entry<->env lifecycle; this file is the surface.
import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { Store } from './src/store.ts';
import { json, readBody, type Ctx } from './src/http.ts';
import { handleRest } from './src/rest.ts';
import { handleGraphQL } from './src/graphql.ts';
import { handleMcp } from './src/mcp.ts';
import { handleGit } from './src/git.ts';

function bearer(req: IncomingMessage): string | null {
  const h = req.headers['authorization'];
  if (typeof h === 'string' && h.startsWith('Bearer ')) return h.slice(7).trim();
  return null;
}

export function createApp(): Server {
  const store = new Store();
  const expected = process.env.MAKEFUN_SHOP_TOKEN || '';
  return createServer(async (req: IncomingMessage, res: ServerResponse) => {
    req.socket.setNoDelay(true);
    try {
      const url = new URL(req.url ?? '/', 'http://entry');
      const pathname = url.pathname;
      const method = req.method ?? 'GET';

      // Boot probe: no auth.
      if (method === 'GET' && pathname === '/healthz') { json(res, 200, { ok: true }); return; }

      const token = bearer(req);
      const authed = token !== null && (expected ? token === expected : true);
      const rawBody = (method === 'POST' || method === 'PUT' || method === 'PATCH' || method === 'DELETE')
        ? await readBody(req) : Buffer.alloc(0);
      const ctx: Ctx = { method, pathname, query: url.searchParams, rawBody, authed, host: req.headers.host || 'entry', headers: req.headers };

      if (pathname === '/api/graphql' || pathname === '/api/graphql/') { handleGraphQL(store, ctx, res); return; }
      if (pathname === '/mcp' || pathname === '/mcp/') { handleMcp(store, ctx, res); return; }

      const gitMatch = /^\/([^/]+)\/([^/]+)\.git(\/.*)?$/.exec(pathname);
      if (gitMatch) {
        const owner = decodeURIComponent(gitMatch[1]);
        const repoName = decodeURIComponent(gitMatch[2]);
        const repo = store.getRepo(owner, repoName);
        if (!repo) { json(res, 404, { message: 'Not Found' }); return; }
        handleGit(store, ctx, res, repo, gitMatch[3] || '');
        return;
      }

      if (pathname === '/api/v3' || pathname.startsWith('/api/v3/')) { handleRest(store, ctx, res); return; }

      json(res, 404, { message: 'Not Found' });
    } catch (err) {
      process.stderr.write(`[forge] ${req.method} ${req.url}: ${err instanceof Error ? err.stack : String(err)}\n`);
      if (!res.headersSent) json(res, 500, { message: 'Internal server error' });
      else res.end();
    }
  });
}
