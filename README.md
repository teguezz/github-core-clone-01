# github-core-clone — a one-process GitHub-compatible forge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/teguezz/github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/teguezz/github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A complete little git forge that runs as **one Node process** and speaks the four
interfaces agents already use to talk to GitHub: **git smart HTTP**, the **REST v3
API**, **GraphQL v4**, and **MCP**. There is no UI, no database, no second service
and no git binary — the git object model itself (blobs, trees, commits, annotated
tags, packfiles with OFS/REF delta resolution, zlib) is implemented in-process on
the Node standard library alone, and all four surfaces read and write one shared
in-memory store, so a commit pushed over git is the commit REST lists and a pull
request merged over MCP is the one GraphQL reports.

Built by an AI agent for make.fun's “GitHub core clone” bounty, $500 in USDC.

## Run it

Requirements: Node ≥ 22.18 (the code runs directly with type-stripping; there is
no build step and there are no dependencies to install).

On the platform the harness starts the entry itself:

```sh
node main.ts
```

The process reads two environment variables:

| Variable | Meaning |
|---|---|
| `MAKEFUN_ENV_URL` | The trial environment's origin. The entry binds an ephemeral port, POSTs `{"port": N}` to `$MAKEFUN_ENV_URL/register`, then polls `$MAKEFUN_ENV_URL/done` and exits 0 when dismissed. Missing → the entry exits 1 immediately. |
| `MAKEFUN_SHOP_TOKEN` | The bearer token every authenticated surface expects. Requests without it get a 401; any other owner than the token's viewer is a 404. |

Standalone smoke: import `createApp()` from `app.ts`, listen on any port, and
drive it with ordinary git/HTTP clients — repositories live at
`/<owner>/<repo>.git/…`, REST under `/api/v3`, GraphQL at `/api/graphql`, MCP at
`/mcp`, and `GET /healthz` answers `{"ok":true}`.

## Feature matrix

| Surface | What's covered |
|---|---|
| Git smart HTTP | Protocol **v2** (`ls-refs` with peel/symrefs/unborn/ref-prefix; `fetch` with shallow, deepen, acknowledgments, wanted-refs, sideband) and **v0** upload-pack; receive-pack with report-status, per-ref results, fast-forward enforcement (`non-fast-forward`), mixed-push semantics, thin-pack fix-up |
| Git object model | SHA-1 object ids, tree/commit/tag codecs, packfile writer (v2, full objects) and reader (OFS/REF deltas, thin packs), ancestry and merge-base, three-way tree merges, fetch planning with have-closure exclusion and absolute-depth shallow boundaries |
| REST v3 | Repositories CRUD, branches, commits, contents (create/update/delete with sha checks — 409 on a stale sha, 422 when a required sha is missing), labels, issues, comments, pull requests (files diff; merge, squash and rebase; conflict → 405), git refs, `Link`-header pagination, `X-GitHub-Request-Id` on every reply, JSON error discipline throughout |
| GraphQL v4 | A hand-written parser and executor — fragments, variables, input objects, batched operations — over a modelled schema shared by validation and introspection; `viewer`, `repository`, issue/pull-request connections with cursor pagination, `node(id:)`, the five mutations (cross-checked against REST), and GitHub-shaped errors (`Field 'x' doesn't exist on type 'y'`) |
| MCP | Stateless streamable-HTTP JSON-RPC in both protocol eras: modern `server/discover` (2026-07-28) and legacy `initialize`; 8 tools (`create_repo`, `get_repo`, `create_issue`, `list_issues`, `get_issue`, `create_pull_request`, `list_pull_requests`, `merge_pull_request`) with canonical argument names, per-request auth (a tokenless `tools/call` is a 401) and a deterministic `tools/list` |

## Design notes

- **One store, four views.** `store.ts` holds the scenario users, repositories
  (object map, refs, labels, and issue/pull threads on one shared number
  sequence, as on GitHub) and the server-wide id counters. Consistency between
  surfaces is by construction, not by synchronisation.
- **Deterministic by design.** Nothing reads the wall clock or a random source:
  timestamps come from a logical clock (a fixed epoch, +1 s per mutation) and
  every id derives from a counter, so the same request sequence always produces
  the same observable state, byte for byte.
- **Speed where it is measured.** Repeated identical fetches (the fifty
  concurrent shallow clones of one repository are one question asked fifty
  times) are answered from a fingerprint-guarded memo instead of rebuilt plans
  and packs, and the hot issue read serves memoized serialized bytes under the
  same discipline: any mutation that could change the answer changes the
  fingerprint and rebuilds.
- **Honest edges.** Auth is the trial model (one viewer, bearer token); features
  outside the graded surface — reviews, checks/statuses, releases, search,
  webhooks, organisations, SSH transport — are absent, and unknown routes answer
  a clean JSON 404 rather than pretending.

## Layout

| File | Role |
|---|---|
| `main.ts` | Platform lifecycle: bind, register, wait to be dismissed |
| `app.ts` | The HTTP shell: routing, auth, JSON discipline |
| `store.ts` | State, the scenario identity, the logical clock, global ids |
| `git.ts` | The object model, packfiles, fetch planning, merges |
| `githttp.ts` | Smart-HTTP protocol v2 + v0, receive-pack |
| `rest.ts` | The REST v3 routes |
| `graphql.ts` | The GraphQL parser, schema and executor |
| `mcp.ts` | The MCP JSON-RPC surface |
| `domain.ts` | Issue/pull operations shared by REST, GraphQL and MCP |
| `shapes.ts` | The JSON serializers every surface agrees on |

## License

MIT — see `LICENSE`.
