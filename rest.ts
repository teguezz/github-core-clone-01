// The REST v3 surface: repositories, branches, commits, contents, labels, issues, comments and
// pull requests, in GitHub's shapes and status codes (201 creates, 204 delete, 404 JSON for
// anything unknown or not the token owner's, 409 for a stale contents sha, 422 for validation,
// 405 for an unmergeable pull request). Every reply is JSON with an X-GitHub-Request-Id header
// (added by the app shell).

import { DomainError, addComment, createIssue, createPull, mergePull, pullFiles, pullMergeable, type MergeMethod } from './domain.ts';
import {
  buildCommit,
  flattenTree,
  historyOf,
  parseCommit,
  parseTree,
  peelToCommit,
  putObject,
  treeOfCommit,
  treeWithPath,
  type TreeEntry,
} from './git.ts';
import { branchJson, commentJson, commitJson, issueJson, labelJson, pullJson, repoJson, userJson } from './shapes.ts';
import { ZERO_OID, type IssueRec, type Repo, type Store, type ThreadRec } from './store.ts';

export interface RestCtx {
  store: Store;
  method: string;
  /** Decoded path segments after /api/v3. */
  segments: string[];
  query: URLSearchParams;
  body: unknown;
  bodyMalformed: boolean;
  /** The request's own origin (http://host), for absolute Link headers. */
  origin: string;
  /** The full request path, for Link headers. */
  path: string;
}

export interface RestResult {
  status: number;
  body?: unknown;
  /** Pre-serialized JSON body, byte-identical to what the shell would stringify; wins over body. */
  text?: string;
  headers?: Record<string, string>;
}

class RestError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

function notFound(): RestError {
  return new RestError(404, 'Not Found');
}

function bodyObject(ctx: RestCtx): Record<string, unknown> {
  if (ctx.bodyMalformed) throw new RestError(400, 'Problems parsing JSON');
  if (typeof ctx.body !== 'object' || ctx.body === null || Array.isArray(ctx.body)) {
    throw new RestError(400, 'Body should be a JSON object');
  }
  return ctx.body as Record<string, unknown>;
}

function optionalBody(ctx: RestCtx): Record<string, unknown> {
  if (ctx.bodyMalformed) throw new RestError(400, 'Problems parsing JSON');
  if (typeof ctx.body !== 'object' || ctx.body === null || Array.isArray(ctx.body)) return {};
  return ctx.body as Record<string, unknown>;
}

function repoOr404(ctx: RestCtx, owner: string, name: string): Repo {
  const repo = ctx.store.repoOf(owner, name);
  if (repo === null) throw notFound();
  return repo;
}

function threadOr404(repo: Repo, number: number): ThreadRec {
  const thread = repo.threads.get(number);
  if (thread === undefined) throw notFound();
  return thread;
}

/** GitHub-style pagination: page/per_page (default 30, cap 100) plus a Link header. */
function paginate<T>(ctx: RestCtx, items: readonly T[]): { page: T[]; headers: Record<string, string> } {
  const perPageRaw = Number(ctx.query.get('per_page') ?? '30');
  const perPage = Number.isFinite(perPageRaw) && perPageRaw > 0 ? Math.min(100, Math.floor(perPageRaw)) : 30;
  const pageRaw = Number(ctx.query.get('page') ?? '1');
  const page = Number.isFinite(pageRaw) && pageRaw > 0 ? Math.floor(pageRaw) : 1;
  const totalPages = Math.max(1, Math.ceil(items.length / perPage));
  const start = (page - 1) * perPage;
  const slice = items.slice(start, start + perPage);
  const urlFor = (target: number): string => {
    const params = new URLSearchParams(ctx.query);
    params.set('page', String(target));
    params.set('per_page', String(perPage));
    return `${ctx.origin}${ctx.path}?${params.toString()}`;
  };
  const links: string[] = [];
  if (page < totalPages) {
    links.push(`<${urlFor(page + 1)}>; rel="next"`);
    links.push(`<${urlFor(totalPages)}>; rel="last"`);
  }
  if (page > 1) {
    links.push(`<${urlFor(page - 1)}>; rel="prev"`);
    links.push(`<${urlFor(1)}>; rel="first"`);
  }
  return { page: slice, headers: links.length > 0 ? { link: links.join(', ') } : {} };
}

// ---------------------------------------------------------------------------------------------
// The serialized-issue memo

/**
 * GET /issues/{n} is the read hot path, and an issue's JSON is a pure function of the issue
 * record plus the definitions of the labels it carries. The memo keeps the exact serialized
 * bytes for that function, guarded by a fingerprint of its inputs: any mutation that could
 * change the bytes — a comment (the count), a PATCH (title/body/state/labels, and the
 * updatedSec bump every PATCH performs), a label edit (its colour or description) — changes
 * the fingerprint and rebuilds, so stale bytes are never served.
 */
const issueTextMemo = new WeakMap<Store, Map<string, { fingerprint: string; text: string }>>();

function issueFingerprint(repo: Repo, issue: IssueRec): string {
  let fingerprint = [
    issue.id,
    issue.number,
    issue.state,
    issue.atSec,
    issue.updatedSec,
    issue.comments.length,
    issue.authorLogin,
    issue.title,
    issue.body,
  ].join('|');
  for (const name of issue.labels) {
    const label = repo.labels.get(name);
    fingerprint += `|${name}=${label?.color ?? ''}:${label?.description ?? ''}`;
  }
  return fingerprint;
}

function issueGetText(store: Store, repo: Repo, issue: IssueRec): string {
  let memo = issueTextMemo.get(store);
  if (memo === undefined) {
    memo = new Map();
    issueTextMemo.set(store, memo);
  }
  const key = `${repo.databaseId}:${issue.number}`;
  const fingerprint = issueFingerprint(repo, issue);
  const hit = memo.get(key);
  if (hit !== undefined && hit.fingerprint === fingerprint) return hit.text;
  const text = `${JSON.stringify(issueJson(store, repo, issue))}\n`;
  if (memo.size >= 256) memo.clear();
  memo.set(key, { fingerprint, text });
  return text;
}

/**
 * The app shell's accelerator for the single-issue read: the same memoized bytes the
 * generic route serves for GET /repos/{owner}/{repo}/issues/{n}, or null when the
 * generic route would answer anything else (unknown repo, unknown number, a pull
 * request) — the caller then falls through to the generic route untouched, so the
 * fast path can only ever serve a response the generic route would have produced.
 */
export function fastIssueGetText(store: Store, owner: string, name: string, number: number): string | null {
  const repo = store.repoOf(owner, name);
  if (repo === null) return null;
  const thread = repo.threads.get(number);
  if (thread === undefined || thread.kind !== 'issue') return null;
  return issueGetText(store, repo, thread);
}

// ---------------------------------------------------------------------------------------------
// Contents helpers

/** The blob at a repository-relative path in a tree, or null. */
function lookupPath(repo: Repo, treeOid: string | null, path: string): { oid: string; mode: string } | null {
  if (treeOid === null) return null;
  const flat = flattenTree(repo, treeOid);
  return flat.get(path) ?? null;
}

/** Resolve a ref parameter (branch, tag, or sha) to a commit oid. */
function resolveRef(repo: Repo, ref: string | undefined): string | null {
  if (ref === undefined || ref === '') {
    return repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? null;
  }
  const branch = repo.refs.get(`refs/heads/${ref}`);
  if (branch !== undefined) return peelToCommit(repo, branch);
  const tag = repo.refs.get(`refs/tags/${ref}`);
  if (tag !== undefined) return peelToCommit(repo, tag);
  if (/^[0-9a-f]{40}$/.test(ref) && repo.objects.has(ref)) return peelToCommit(repo, ref);
  return null;
}

interface ContentsCommit {
  commitSha: string;
  blobSha: string | null;
  created: boolean;
}

/** Create or delete one file on a branch with a commit, GitHub contents-API semantics. */
function commitFile(
  ctx: RestCtx,
  repo: Repo,
  options: { path: string; content: Buffer | null; message: string; branch?: string; sha?: string },
): ContentsCommit {
  const store = ctx.store;
  const branch = options.branch ?? repo.defaultBranch;
  const ref = `refs/heads/${branch}`;
  const tip = repo.refs.get(ref) ?? null;
  const baseTree = tip === null ? null : treeOfCommit(repo, tip);
  const current = lookupPath(repo, baseTree, options.path);
  let blobSha: string | null = null;
  let created = false;
  if (options.content !== null) {
    if (current !== null) {
      if (options.sha === undefined) throw new RestError(422, `Validation Failed: "sha" is required when updating ${options.path}`);
      if (options.sha !== current.oid) throw new RestError(409, `${options.path} does not match the given sha`);
    } else if (options.sha !== undefined) {
      throw new RestError(409, `${options.path} does not exist, so no sha can match`);
    }
    created = current === null;
    blobSha = putObject(repo, 'blob', options.content);
  } else {
    if (current === null) throw notFound();
    if (options.sha !== undefined && options.sha !== current.oid) {
      throw new RestError(409, `${options.path} does not match the given sha`);
    }
  }
  const segments = options.path.split('/');
  const newTree = treeWithPath(repo, baseTree, segments, options.content === null ? null : { oid: blobSha as string, mode: current?.mode ?? '100644' });
  const at = store.tick();
  const identity = `${store.identity()} ${at} +0000`;
  const commitOid = putObject(
    repo,
    'commit',
    buildCommit({ tree: newTree, parents: tip === null ? [] : [tip], author: identity, committer: identity, message: options.message }),
  );
  repo.refs.set(ref, commitOid);
  return { commitSha: commitOid, blobSha, created };
}

// ---------------------------------------------------------------------------------------------
// The router

export function handleRest(ctx: RestCtx): RestResult {
  try {
    return route(ctx);
  } catch (err) {
    if (err instanceof RestError) return { status: err.status, body: { message: err.message } };
    if (err instanceof DomainError) return { status: err.status, body: { message: err.message } };
    throw err;
  }
}

function route(ctx: RestCtx): RestResult {
  const { store, method, segments } = ctx;

  if (segments.length === 0) {
    if (method !== 'GET') throw notFound();
    return {
      status: 200,
      body: {
        current_user_url: '/api/v3/user',
        repository_url: '/api/v3/repos/{owner}/{repo}',
        issue_url: '/api/v3/repos/{owner}/{repo}/issues',
        rate_limit_url: '/api/v3/rate_limit',
      },
      headers: { 'x-oauth-scopes': 'repo, read:org' },
    };
  }

  if (segments[0] === 'meta' && segments.length === 1) {
    if (method !== 'GET') throw notFound();
    return {
      status: 200,
      body: {
        installed_version: '1.0.0',
        services: { git: true, rest: true, graphql: true, mcp: true },
      },
    };
  }

  if (segments[0] === 'user' && segments.length === 1) {
    if (method !== 'GET') throw notFound();
    const viewer = store.viewer;
    return {
      status: 200,
      body: {
        ...userJson(store, viewer.login),
        name: viewer.name,
        email: viewer.email,
        created_at: store.isoOf(store.nowSec()),
        updated_at: store.isoOf(store.nowSec()),
      },
    };
  }

  if (segments[0] === 'user' && segments[1] === 'repos' && segments.length === 2) {
    if (method === 'GET') {
      const all = [...store.repos.values()]
        .filter((repo) => repo.owner === store.viewer.login)
        .sort((a, b) => a.databaseId - b.databaseId);
      const { page, headers } = paginate(ctx, all);
      return { status: 200, body: page.map((repo) => repoJson(store, repo)), headers };
    }
    if (method === 'POST') {
      const body = bodyObject(ctx);
      const name = typeof body.name === 'string' ? body.name : '';
      if (!/^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$/.test(name) || name === '.' || name === '..') {
        throw new RestError(422, 'Repository creation failed: the name is invalid');
      }
      if (store.repos.has(`${store.viewer.login}/${name}`)) {
        return {
          status: 422,
          body: {
            message: 'Repository creation failed.',
            errors: [{ resource: 'Repository', code: 'custom', field: 'name', message: 'name already exists on this account' }],
          },
        };
      }
      const repo = store.createRepo(name, {
        description: typeof body.description === 'string' ? body.description : '',
        isPrivate: body.private === true,
      });
      if (body.auto_init === true) {
        commitFile(ctx, repo, {
          path: 'README.md',
          content: Buffer.from(`# ${name}\n`, 'utf8'),
          message: 'Initial commit',
        });
      }
      return { status: 201, body: repoJson(store, repo) };
    }
    throw notFound();
  }

  if (segments[0] === 'repos' && segments.length >= 3) {
    const owner = segments[1] as string;
    const name = segments[2] as string;
    const rest = segments.slice(3);
    if (rest.length === 0) return repoRoot(ctx, owner, name);
    const repo = repoOr404(ctx, owner, name);
    return repoRoute(ctx, repo, rest);
  }

  throw notFound();
}

function repoRoot(ctx: RestCtx, owner: string, name: string): RestResult {
  const { store, method } = ctx;
  if (method === 'GET') {
    const repo = repoOr404(ctx, owner, name);
    return { status: 200, body: repoJson(store, repo) };
  }
  if (method === 'PATCH') {
    const repo = repoOr404(ctx, owner, name);
    const body = bodyObject(ctx);
    if (typeof body.description === 'string') repo.description = body.description;
    if (typeof body.private === 'boolean') repo.isPrivate = body.private;
    if (typeof body.name === 'string' && body.name !== repo.name && /^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$/.test(body.name)) {
      store.repos.delete(`${repo.owner}/${repo.name}`);
      repo.name = body.name;
      store.repos.set(`${repo.owner}/${repo.name}`, repo);
    }
    if (typeof body.default_branch === 'string' && body.default_branch !== '') repo.defaultBranch = body.default_branch;
    return { status: 200, body: repoJson(store, repo) };
  }
  if (method === 'DELETE') {
    const repo = repoOr404(ctx, owner, name);
    store.deleteRepo(repo);
    return { status: 204 };
  }
  throw notFound();
}

function repoRoute(ctx: RestCtx, repo: Repo, rest: string[]): RestResult {
  const { store, method } = ctx;
  const head = rest[0] as string;

  if (head === 'branches') {
    if (rest.length === 1) {
      if (method !== 'GET') throw notFound();
      const names = [...repo.refs.keys()]
        .filter((ref) => ref.startsWith('refs/heads/'))
        .map((ref) => ref.slice('refs/heads/'.length))
        .sort();
      const { page, headers } = paginate(ctx, names);
      return { status: 200, body: page.map((branch) => branchJson(repo, branch)), headers };
    }
    if (method !== 'GET') throw notFound();
    const branch = rest.slice(1).join('/');
    if (!repo.refs.has(`refs/heads/${branch}`)) throw notFound();
    return { status: 200, body: branchJson(repo, branch) };
  }

  if (head === 'commits') {
    if (rest.length === 1) {
      if (method !== 'GET') throw notFound();
      const tip = resolveRef(repo, ctx.query.get('sha') ?? undefined);
      if (tip === null) return { status: 200, body: [] };
      const history = historyOf(repo, tip, 10_000);
      const { page, headers } = paginate(ctx, history);
      return { status: 200, body: page.map((oid) => commitJson(store, repo, oid)), headers };
    }
    if (rest.length === 2 && method === 'GET') {
      const json = commitJson(store, repo, rest[1] as string);
      if (json === null) throw notFound();
      return { status: 200, body: json };
    }
    throw notFound();
  }

  if (head === 'contents') return contentsRoute(ctx, repo, rest.slice(1).join('/'));
  if (head === 'labels') return labelsRoute(ctx, repo, rest);
  if (head === 'issues') return issuesRoute(ctx, repo, rest);
  if (head === 'pulls') return pullsRoute(ctx, repo, rest);
  if (head === 'git' && rest[1] === 'refs') return gitRefsRoute(ctx, repo, rest.slice(2));

  throw notFound();
}

function contentsRoute(ctx: RestCtx, repo: Repo, path: string): RestResult {
  const { store, method } = ctx;
  if (method === 'GET') {
    const commit = resolveRef(repo, ctx.query.get('ref') ?? undefined);
    if (commit === null) throw notFound();
    const tree = treeOfCommit(repo, commit);
    if (tree === null) throw notFound();
    if (path === '') return { status: 200, body: dirListing(repo, tree, '') };
    // Walk to the target: a file answers its object, a directory its listing.
    const segments = path.split('/');
    let treeOid: string | null = tree;
    for (let i = 0; i < segments.length; i += 1) {
      if (treeOid === null) throw notFound();
      const entries = parseTree((repo.objects.get(treeOid) as { data: Buffer }).data);
      const entry: TreeEntry | undefined = entries.find((candidate) => candidate.name === segments[i]);
      if (entry === undefined) throw notFound();
      if (i === segments.length - 1) {
        if (entry.mode === '40000') return { status: 200, body: dirListing(repo, entry.oid, path) };
        const blob = repo.objects.get(entry.oid);
        if (blob === undefined) throw notFound();
        return {
          status: 200,
          body: {
            type: 'file',
            encoding: 'base64',
            content: blob.data.toString('base64'),
            sha: entry.oid,
            size: blob.data.length,
            name: entry.name,
            path,
          },
        };
      }
      if (entry.mode !== '40000') throw notFound();
      treeOid = entry.oid;
    }
    throw notFound();
  }
  if (method === 'PUT') {
    if (path === '') throw new RestError(422, 'Validation Failed: a file path is required');
    const body = bodyObject(ctx);
    const message = typeof body.message === 'string' ? body.message : '';
    const content = typeof body.content === 'string' ? Buffer.from(body.content.replace(/\s+/g, ''), 'base64') : Buffer.alloc(0);
    const branch = typeof body.branch === 'string' ? body.branch : undefined;
    const sha = typeof body.sha === 'string' ? body.sha : undefined;
    const result = commitFile(ctx, repo, { path, content, message, branch, sha });
    const blob = result.blobSha === null ? null : repo.objects.get(result.blobSha);
    return {
      status: result.created ? 201 : 200,
      body: {
        content: {
          type: 'file',
          encoding: 'base64',
          sha: result.blobSha,
          size: blob?.data.length ?? 0,
          name: path.split('/').pop(),
          path,
        },
        commit: { sha: result.commitSha },
      },
    };
  }
  if (method === 'DELETE') {
    if (path === '') throw notFound();
    const body = bodyObject(ctx);
    const message = typeof body.message === 'string' ? body.message : '';
    const branch = typeof body.branch === 'string' ? body.branch : undefined;
    const sha = typeof body.sha === 'string' ? body.sha : undefined;
    const result = commitFile(ctx, repo, { path, content: null, message, branch, sha });
    return { status: 200, body: { content: null, commit: { sha: result.commitSha } } };
  }
  throw notFound();
}

function dirListing(repo: Repo, treeOid: string, prefix: string): unknown[] {
  const entries = parseTree((repo.objects.get(treeOid) as { data: Buffer }).data);
  return entries.map((entry) => {
    const isDir = entry.mode === '40000';
    const object = repo.objects.get(entry.oid);
    return {
      type: isDir ? 'dir' : 'file',
      sha: entry.oid,
      size: isDir ? 0 : (object?.data.length ?? 0),
      name: entry.name,
      path: prefix === '' ? entry.name : `${prefix}/${entry.name}`,
    };
  });
}

function labelsRoute(ctx: RestCtx, repo: Repo, rest: string[]): RestResult {
  const { method } = ctx;
  if (rest.length !== 1) throw notFound();
  if (method === 'GET') {
    const { page, headers } = paginate(ctx, [...repo.labels.keys()].sort());
    return { status: 200, body: page.map((name) => labelJson(repo, name)), headers };
  }
  if (method === 'POST') {
    const body = bodyObject(ctx);
    const name = typeof body.name === 'string' ? body.name : '';
    if (name === '') throw new RestError(422, 'Validation Failed: a label name is required');
    if (repo.labels.has(name)) throw new RestError(422, 'Validation Failed: the label already exists');
    const color = typeof body.color === 'string' ? body.color.replace(/^#/, '') : 'ededed';
    repo.labels.set(name, { name, color, description: typeof body.description === 'string' ? body.description : null });
    return { status: 201, body: labelJson(repo, name) };
  }
  throw notFound();
}

function issuesRoute(ctx: RestCtx, repo: Repo, rest: string[]): RestResult {
  const { store, method } = ctx;
  if (rest.length === 1) {
    if (method === 'POST') {
      const issue = createIssue(store, repo, bodyObject(ctx));
      return { status: 201, body: issueJson(store, repo, issue) };
    }
    if (method === 'GET') {
      const state = ctx.query.get('state') ?? 'open';
      const direction = ctx.query.get('direction') ?? 'desc';
      let issues = [...repo.threads.values()].filter((thread): thread is Extract<ThreadRec, { kind: 'issue' }> => thread.kind === 'issue');
      if (state === 'open' || state === 'closed') issues = issues.filter((issue) => issue.state === state);
      else if (state !== 'all') throw new RestError(422, `Validation Failed: state "${state}" is not valid`);
      const labelFilter = ctx.query.get('labels');
      if (labelFilter !== null && labelFilter !== '') {
        const wanted = labelFilter.split(',');
        issues = issues.filter((issue) => wanted.every((name) => issue.labels.includes(name)));
      }
      issues.sort((a, b) => (direction === 'asc' ? a.number - b.number : b.number - a.number));
      const { page, headers } = paginate(ctx, issues);
      return { status: 200, body: page.map((issue) => issueJson(store, repo, issue)), headers };
    }
    throw notFound();
  }
  const number = Number(rest[1]);
  if (!Number.isInteger(number) || number <= 0) throw notFound();
  if (rest.length === 2) {
    const thread = threadOr404(repo, number);
    if (method === 'GET') {
      if (thread.kind === 'issue') return { status: 200, text: issueGetText(store, repo, thread) };
      return { status: 200, body: pullJson(store, repo, thread, thread.state === 'open' ? pullMergeable(repo, thread) : thread.merged ? true : null) };
    }
    if (method === 'PATCH') {
      const body = bodyObject(ctx);
      if (typeof body.title === 'string') thread.title = body.title;
      if (typeof body.body === 'string') thread.body = body.body;
      if (body.state === 'open' || body.state === 'closed') thread.state = body.state;
      if (thread.kind === 'issue' && Array.isArray(body.labels)) {
        const names: string[] = [];
        for (const raw of body.labels) {
          const name = typeof raw === 'string' ? raw : '';
          if (!repo.labels.has(name)) throw new RestError(422, `Validation Failed: label "${name}" does not exist`);
          names.push(name);
        }
        thread.labels = names;
      }
      thread.updatedSec = store.tick();
      if (thread.kind === 'issue') return { status: 200, body: issueJson(store, repo, thread) };
      return { status: 200, body: pullJson(store, repo, thread, null) };
    }
    throw notFound();
  }
  if (rest.length === 3 && rest[2] === 'comments') {
    const thread = threadOr404(repo, number);
    if (method === 'GET') {
      const { page, headers } = paginate(ctx, thread.comments);
      return { status: 200, body: page.map((comment) => commentJson(store, repo, thread, comment)), headers };
    }
    if (method === 'POST') {
      const body = bodyObject(ctx);
      const text = typeof body.body === 'string' ? body.body : '';
      if (text === '') throw new RestError(422, 'Validation Failed: a comment body is required');
      const comment = addComment(store, repo, thread, text);
      return { status: 201, body: commentJson(store, repo, thread, comment) };
    }
    throw notFound();
  }
  throw notFound();
}

function pullsRoute(ctx: RestCtx, repo: Repo, rest: string[]): RestResult {
  const { store, method } = ctx;
  if (rest.length === 1) {
    if (method === 'POST') {
      const pull = createPull(store, repo, bodyObject(ctx));
      return { status: 201, body: pullJson(store, repo, pull, pullMergeable(repo, pull)) };
    }
    if (method === 'GET') {
      const state = ctx.query.get('state') ?? 'open';
      let pulls = [...repo.threads.values()].filter((thread): thread is Extract<ThreadRec, { kind: 'pull' }> => thread.kind === 'pull');
      if (state === 'open') pulls = pulls.filter((pull) => pull.state === 'open');
      else if (state === 'closed') pulls = pulls.filter((pull) => pull.state === 'closed');
      else if (state !== 'all') throw new RestError(422, `Validation Failed: state "${state}" is not valid`);
      const direction = ctx.query.get('direction') ?? 'desc';
      pulls.sort((a, b) => (direction === 'asc' ? a.number - b.number : b.number - a.number));
      const { page, headers } = paginate(ctx, pulls);
      return { status: 200, body: page.map((pull) => pullJson(store, repo, pull, pull.state === 'open' ? pullMergeable(repo, pull) : null)), headers };
    }
    throw notFound();
  }
  const number = Number(rest[1]);
  if (!Number.isInteger(number) || number <= 0) throw notFound();
  const thread = repo.threads.get(number);
  if (thread === undefined || thread.kind !== 'pull') throw notFound();
  const pull = thread;
  if (rest.length === 2) {
    if (method === 'GET') {
      return { status: 200, body: pullJson(store, repo, pull, pull.state === 'open' ? pullMergeable(repo, pull) : null) };
    }
    if (method === 'PATCH') {
      const body = bodyObject(ctx);
      if (typeof body.title === 'string') pull.title = body.title;
      if (typeof body.body === 'string') pull.body = body.body;
      if (body.state === 'open' || body.state === 'closed') pull.state = body.state;
      pull.updatedSec = store.tick();
      return { status: 200, body: pullJson(store, repo, pull, null) };
    }
    throw notFound();
  }
  if (rest.length === 3 && rest[2] === 'files' && method === 'GET') {
    const files = pullFiles(repo, pull);
    const { page, headers } = paginate(ctx, files);
    return {
      status: 200,
      body: page.map((file) => ({
        sha: file.after?.oid ?? file.before?.oid ?? ZERO_OID,
        filename: file.path,
        status: file.status === 'modified' ? 'modified' : file.status,
        additions: file.additions,
        deletions: file.deletions,
        changes: file.additions + file.deletions,
      })),
      headers,
    };
  }
  if (rest.length === 3 && rest[2] === 'merge' && method === 'PUT') {
    const body = optionalBody(ctx);
    const raw = typeof body.merge_method === 'string' ? body.merge_method.toLowerCase() : 'merge';
    const mergeMethod: MergeMethod = raw === 'squash' ? 'squash' : raw === 'rebase' ? 'rebase' : 'merge';
    const outcome = mergePull(store, repo, pull, mergeMethod);
    return { status: 200, body: { sha: outcome.sha, merged: true, message: 'Pull Request successfully merged' } };
  }
  throw notFound();
}

function gitRefsRoute(ctx: RestCtx, repo: Repo, rest: string[]): RestResult {
  const { method } = ctx;
  if (rest.length === 0) {
    if (method === 'GET') {
      const all = [...repo.refs.entries()].sort(([a], [b]) => (a < b ? -1 : 1)).map(([ref, oid]) => ({
        ref,
        node_id: `REF_${ref}`,
        url: `/api/v3/repos/${repo.owner}/${repo.name}/git/refs/${ref.replace(/^refs\//, '')}`,
        object: { type: repo.objects.get(oid)?.type ?? 'commit', sha: oid },
      }));
      const { page, headers } = paginate(ctx, all);
      return { status: 200, body: page, headers };
    }
    if (method === 'POST') {
      const body = bodyObject(ctx);
      const ref = typeof body.ref === 'string' ? body.ref : '';
      const sha = typeof body.sha === 'string' ? body.sha : '';
      if (!ref.startsWith('refs/') || !/^[0-9a-f]{40}$/.test(sha) || !repo.objects.has(sha)) {
        throw new RestError(422, 'Validation Failed: the ref or the object is invalid');
      }
      if (ref.startsWith('refs/heads/') && repo.objects.get(sha)?.type !== 'commit') {
        throw new RestError(422, 'Validation Failed: a branch must point at a commit');
      }
      if (repo.refs.has(ref)) throw new RestError(422, 'Reference already exists');
      repo.refs.set(ref, sha);
      return {
        status: 201,
        body: { ref, node_id: `REF_${ref}`, url: '', object: { type: repo.objects.get(sha)?.type ?? 'commit', sha } },
      };
    }
    throw notFound();
  }
  const joined = rest.join('/');
  const full = joined.startsWith('refs/') ? joined : `refs/${joined}`;
  const oid = repo.refs.get(full);
  if (method === 'GET') {
    if (oid === undefined) throw notFound();
    return { status: 200, body: { ref: full, node_id: `REF_${full}`, url: '', object: { type: repo.objects.get(oid)?.type ?? 'commit', sha: oid } } };
  }
  if (method === 'DELETE') {
    if (oid === undefined) throw notFound();
    repo.refs.delete(full);
    return { status: 204 };
  }
  throw notFound();
}
