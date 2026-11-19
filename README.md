# github-core-clone-01

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/Rizkijack/github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/Rizkijack/github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone, GitHub-compatible git forge that runs as one Node process and serves everything from
its own in-memory store, with no UI, no outside service, no proxy and no `git` binary. Built on the
Node standard library alone (`node:http`, `node:zlib`, `node:crypto`).

It speaks four surfaces on one base URL:

- **git smart HTTP** at `/{owner}/{repo}.git`: `upload-pack` protocol v2 (with a v0 fallback) for
  clone and fetch, including shallow/deepen and incremental have-exclusion, and `receive-pack` v0 for
  push with `report-status`, `side-band-64k` and `delete-refs`. Packfiles are parsed and built by hand
  (plain objects plus OFS/REF delta resolution); every update is fast-forward checked per ref.
- **REST v3** under `/api/v3`: repositories, branches, commits, contents (create/update/delete with
  sha semantics), labels, issues, comments, pull requests, their files and squash merges, with
  `Link` pagination and GitHub-style JSON errors.
- **GraphQL v4** at `/api/graphql`: a hand-rolled parser and executor for the documents `gh` and
  `github-mcp-server` send, including introspection, aliases, inline fragments, variables, batched
  operations and cursor pagination.
- **MCP** at `/mcp`: JSON-RPC 2.0 over Streamable HTTP, revision `2026-07-28` (plus the legacy
  `initialize` handshake), `server/discover`, `tools/list` and `tools/call` acting on the same store.

All four surfaces share one store, so a repository or issue created over MCP or GraphQL reads back
over REST as the same object with the same id.

## Run it

```sh
node --experimental-strip-types main.ts
```

The platform runs `node /bot/main.ts` on Node 22.18 or later, which strips TypeScript types on its
own. There is nothing to install: the program imports only Node built-ins and its own sources, and
the trial has no package manager and no internet.

`main.ts` is the entry <-> env lifecycle: bind an ephemeral port (never 8080), `POST {"port": N}` to
`$MAKEFUN_ENV_URL/register`, answer `GET /healthz`, then poll `$MAKEFUN_ENV_URL/done` until it says
`{"done": true}` and exit 0. `app.ts` builds the server; `src/` holds the store and the four surfaces.

## Layout

- `main.ts` — the lifecycle the harness drives.
- `app.ts` — the HTTP server, routing and bearer-token auth.
- `src/store.ts` — the shared store: repos, git objects/refs, issues, labels, pulls, trees and merges.
- `src/git.ts` — git smart HTTP (pkt-line, pack codec, upload-pack v2/v0, receive-pack v0).
- `src/rest.ts` — REST v3 resources.
- `src/graphql.ts` — GraphQL v4 parser + executor.
- `src/mcp.ts` — MCP JSON-RPC endpoint and tools.
- `src/http.ts`, `src/util.ts` — response helpers, hashing and deterministic ids.

## Rules

The rules, scoring weights, deadline and prizes are on the contest page:
**https://make.fun/bounty/github-core-clone-01**. Node 22 only, no run-time dependencies, no internet
and no `git` binary in the trial; the repository is mounted read-only at `/bot`.

---

<sub>Entered on [make.fun](https://make.fun/?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
