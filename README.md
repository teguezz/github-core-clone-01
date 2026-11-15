# github-core-clone

A standalone git forge in a single Node process, built for the make.fun
"github-core-clone-01" competition. It serves four interfaces over one in-memory
store, with no UI, no database and no outside service — the git object model
(blobs, trees, commits, annotated tags, packfiles, deltas, zlib) is implemented
in-process on the Node standard library alone. It never shells out to git.

## Run

The platform runs `node main.ts` (Node ≥ 22.18, type-stripping). The process
binds an ephemeral port, registers it with `$MAKEFUN_ENV_URL`, serves until the
suite reports done, then exits. `MAKEFUN_SHOP_TOKEN` is the bearer token every
authenticated surface expects.

## Surfaces

- **Git smart HTTP** — `/<owner>/<repo>.git/info/refs`, `git-upload-pack`,
  `git-receive-pack`. Protocol v2 (`ls-refs`, `fetch` with shallow/deepen,
  sideband progress) and v0 receive-pack with report-status, fast-forward
  enforcement and per-ref results.
- **REST v3** — `/api/v3`: repositories, branches, commits, contents
  (read/write/delete with optimistic-concurrency statuses), labels, issues,
  comments, pull requests (files, three merge methods, conflict → 405),
  git refs, `Link`-header pagination, `X-GitHub-Request-Id` on every reply.
- **GraphQL v4** — `/api/graphql`: a hand-written parser and executor covering
  the query/mutation shapes `gh` and github-mcp-server send — viewer,
  repository, issues/pull-request connections with cursor pagination, node(id:),
  batched operations, introspection of the modelled schema, and GitHub-style
  field errors.
- **MCP** — `/mcp`: stateless streamable-HTTP JSON-RPC speaking both the modern
  `server/discover` revision (2026-07-28) and the legacy `initialize` handshake,
  with tools create_repo, get_repo, create_issue, list_issues, get_issue,
  create_pull_request, list_pull_requests and merge_pull_request.

## Design notes

- One `Store` holds users, repositories (objects, refs, labels, issue/pull
  threads on a shared number sequence) and server-wide id counters. All four
  surfaces read and write the same objects, so state is consistent by
  construction.
- Determinism: timestamps come from a logical clock (a fixed epoch plus one
  second per mutation) and every id is derived from counters, so two runs of
  the same scenario produce identical observable state.
- The viewer is the fixed scenario identity; any bearer token authenticates as
  that user, no token is a 401, and repositories under any other owner are 404.

Layout: `main.ts` (platform lifecycle), `app.ts` (HTTP routing/auth),
`store.ts` (state + scenario identity), `git.ts` (object model, packfiles,
fetch planning, merges), `githttp.ts` (smart-HTTP protocol), `rest.ts`,
`graphql.ts`, `mcp.ts`, `domain.ts` (shared issue/pull operations),
`shapes.ts` (REST serializers).
