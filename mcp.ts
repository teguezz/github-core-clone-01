// The MCP endpoint (/mcp): stateless streamable-HTTP JSON-RPC. It speaks both protocol eras the
// suite accepts — the modern `server/discover` revision (2026-07-28) and the legacy
// initialize handshake (2025-11-25 and earlier) — and exposes the forge's tools over the same
// store as REST and GraphQL, so an issue created here is the same object everywhere.

import { DomainError, createIssue, createPull, mergePull, pullMergeable } from './domain.ts';
import { issueJson, pullJson, repoJson } from './shapes.ts';
import type { Repo, Store } from './store.ts';

const SERVER_INFO = { name: 'github-core-clone', version: '1.0.0' };
const SUPPORTED_VERSIONS = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];

interface ToolDef {
  name: string;
  description: string;
  inputSchema: Record<string, unknown>;
}

function objectSchema(properties: Record<string, unknown>, required: string[]): Record<string, unknown> {
  return { type: 'object', properties, required, additionalProperties: false };
}

const STRING = { type: 'string' } as const;

export const TOOLS: readonly ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository for the authenticated user.',
    inputSchema: objectSchema(
      {
        name: { ...STRING, description: 'The repository name.' },
        description: { ...STRING, description: 'A short description of the repository.' },
        private: { type: 'boolean', description: 'Whether the repository is private.' },
      },
      ['name'],
    ),
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: objectSchema(
      {
        owner: { ...STRING, description: 'The repository owner login.' },
        repo: { ...STRING, description: 'The repository name.' },
      },
      ['owner', 'repo'],
    ),
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: objectSchema(
      {
        owner: { ...STRING, description: 'The repository owner login.' },
        repo: { ...STRING, description: 'The repository name.' },
        title: { ...STRING, description: 'The issue title.' },
        body: { ...STRING, description: 'The issue body.' },
      },
      ['owner', 'repo', 'title'],
    ),
  },
  {
    name: 'list_issues',
    description: 'List the issues of a repository, optionally filtered by state (open, closed, all).',
    inputSchema: objectSchema(
      {
        owner: { ...STRING, description: 'The repository owner login.' },
        repo: { ...STRING, description: 'The repository name.' },
        state: { ...STRING, description: 'Issue state filter: open, closed or all.' },
      },
      ['owner', 'repo'],
    ),
  },
  {
    name: 'get_issue',
    description: 'Get one issue of a repository by its number.',
    inputSchema: objectSchema(
      {
        owner: { ...STRING, description: 'The repository owner login.' },
        repo: { ...STRING, description: 'The repository name.' },
        issue_number: { type: 'integer', description: 'The issue number.' },
      },
      ['owner', 'repo', 'issue_number'],
    ),
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request from a head branch into a base branch.',
    inputSchema: objectSchema(
      {
        owner: { ...STRING, description: 'The repository owner login.' },
        repo: { ...STRING, description: 'The repository name.' },
        title: { ...STRING, description: 'The pull request title.' },
        body: { ...STRING, description: 'The pull request body.' },
        head: { ...STRING, description: 'The head branch name.' },
        base: { ...STRING, description: 'The base branch name.' },
      },
      ['owner', 'repo', 'title', 'head', 'base'],
    ),
  },
  {
    name: 'list_pull_requests',
    description: 'List the pull requests of a repository, optionally filtered by state.',
    inputSchema: objectSchema(
      {
        owner: { ...STRING, description: 'The repository owner login.' },
        repo: { ...STRING, description: 'The repository name.' },
        state: { ...STRING, description: 'Pull request state filter: open, closed or all.' },
      },
      ['owner', 'repo'],
    ),
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request by number.',
    inputSchema: objectSchema(
      {
        owner: { ...STRING, description: 'The repository owner login.' },
        repo: { ...STRING, description: 'The repository name.' },
        pull_number: { type: 'integer', description: 'The pull request number.' },
      },
      ['owner', 'repo', 'pull_number'],
    ),
  },
];

export interface McpResult {
  status: number;
  /** The JSON body; undefined answers with no body (notifications). */
  body?: unknown;
}

interface ToolOutcome {
  value: unknown;
  isError?: boolean;
}

function toolText(value: unknown): string {
  return typeof value === 'string' ? value : JSON.stringify(value, null, 2);
}

function callTool(store: Store, name: string, args: Record<string, unknown>): ToolOutcome | 'unknown' {
  const repoOf = (): Repo => {
    const owner = typeof args.owner === 'string' ? args.owner : '';
    const repoName = typeof args.repo === 'string' ? args.repo : '';
    const repo = store.repoOf(owner, repoName);
    if (repo === null) throw new DomainError(404, `repository ${owner}/${repoName} not found`);
    return repo;
  };
  const need = (key: string): string => {
    const value = args[key];
    if (typeof value !== 'string' || value === '') throw new DomainError(422, `missing required argument: ${key}`);
    return value;
  };
  try {
    switch (name) {
      case 'create_repo': {
        const repoName = need('name');
        if (store.repos.has(`${store.viewer.login}/${repoName}`)) {
          return { value: `repository ${repoName} already exists`, isError: true };
        }
        const repo = store.createRepo(repoName, {
          description: typeof args.description === 'string' ? args.description : '',
          isPrivate: args.private === true,
        });
        return { value: repoJson(store, repo) };
      }
      case 'get_repo': {
        need('owner');
        need('repo');
        return { value: repoJson(store, repoOf()) };
      }
      case 'create_issue': {
        need('owner');
        need('repo');
        const title = need('title');
        const repo = repoOf();
        const issue = createIssue(store, repo, { title, body: typeof args.body === 'string' ? args.body : '' });
        return { value: issueJson(store, repo, issue) };
      }
      case 'list_issues': {
        need('owner');
        need('repo');
        const repo = repoOf();
        const state = typeof args.state === 'string' ? args.state : 'open';
        const issues = [...repo.threads.values()]
          .filter((thread) => thread.kind === 'issue' && (state === 'all' || thread.state === state))
          .sort((a, b) => a.number - b.number)
          .map((issue) => issueJson(store, repo, issue as never));
        return { value: issues };
      }
      case 'get_issue': {
        need('owner');
        need('repo');
        const repo = repoOf();
        const number = typeof args.issue_number === 'number' ? args.issue_number : Number(args.issue_number);
        const thread = repo.threads.get(number);
        if (thread === undefined || thread.kind !== 'issue') throw new DomainError(404, `issue ${number} not found`);
        return { value: issueJson(store, repo, thread) };
      }
      case 'create_pull_request': {
        need('owner');
        need('repo');
        const repo = repoOf();
        const pull = createPull(store, repo, {
          title: need('title'),
          body: typeof args.body === 'string' ? args.body : '',
          head: need('head'),
          base: need('base'),
        });
        return { value: pullJson(store, repo, pull, pullMergeable(repo, pull)) };
      }
      case 'list_pull_requests': {
        need('owner');
        need('repo');
        const repo = repoOf();
        const state = typeof args.state === 'string' ? args.state : 'open';
        const pulls = [...repo.threads.values()]
          .filter((thread) => thread.kind === 'pull' && (state === 'all' || thread.state === state))
          .sort((a, b) => a.number - b.number)
          .map((pull) => pullJson(store, repo, pull as never, null));
        return { value: pulls };
      }
      case 'merge_pull_request': {
        need('owner');
        need('repo');
        const repo = repoOf();
        const number = typeof args.pull_number === 'number' ? args.pull_number : Number(args.pull_number);
        const thread = repo.threads.get(number);
        if (thread === undefined || thread.kind !== 'pull') throw new DomainError(404, `pull request ${number} not found`);
        const outcome = mergePull(store, repo, thread, 'merge');
        return { value: { merged: true, sha: outcome.sha, message: 'Pull Request successfully merged' } };
      }
      default:
        return 'unknown';
    }
  } catch (err) {
    if (err instanceof DomainError) return { value: err.message, isError: true };
    throw err;
  }
}

/** Handle one MCP HTTP request body. Auth is the app shell's job (401 before this runs). */
export function handleMcp(store: Store, rawBody: Buffer): McpResult {
  let parsed: unknown;
  try {
    parsed = JSON.parse(rawBody.toString('utf8'));
  } catch {
    return {
      status: 400,
      body: { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } },
    };
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
    return { status: 400, body: { jsonrpc: '2.0', id: null, error: { code: -32600, message: 'Invalid Request' } } };
  }
  const request = parsed as { id?: unknown; method?: unknown; params?: unknown };
  const isNotification = request.id === undefined;
  if (isNotification) return { status: 202 };
  const id = request.id ?? null;
  const method = typeof request.method === 'string' ? request.method : '';
  const params = (typeof request.params === 'object' && request.params !== null ? request.params : {}) as Record<string, unknown>;
  const ok = (result: unknown): McpResult => ({ status: 200, body: { jsonrpc: '2.0', id, result } });
  const fail = (code: number, message: string): McpResult => ({ status: 200, body: { jsonrpc: '2.0', id, error: { code, message } } });

  switch (method) {
    case 'server/discover':
      return ok({
        resultType: 'complete',
        supportedVersions: SUPPORTED_VERSIONS,
        capabilities: { tools: {} },
        serverInfo: SERVER_INFO,
      });
    case 'initialize': {
      const requested = typeof params.protocolVersion === 'string' ? params.protocolVersion : '';
      const protocolVersion = SUPPORTED_VERSIONS.includes(requested) ? requested : '2025-11-25';
      return ok({ protocolVersion, capabilities: { tools: {} }, serverInfo: SERVER_INFO });
    }
    case 'ping':
      return ok({});
    case 'tools/list':
      return ok({ tools: TOOLS });
    case 'tools/call': {
      const toolName = typeof params.name === 'string' ? params.name : '';
      const toolArgs = (typeof params.arguments === 'object' && params.arguments !== null ? params.arguments : {}) as Record<string, unknown>;
      const outcome = callTool(store, toolName, toolArgs);
      if (outcome === 'unknown') return fail(-32602, `Invalid params: unknown tool "${toolName}"`);
      const result: Record<string, unknown> = {
        content: [{ type: 'text', text: toolText(outcome.value) }],
        structuredContent: Array.isArray(outcome.value) ? { items: outcome.value } : outcome.value,
      };
      if (outcome.isError === true) result.isError = true;
      return ok(result);
    }
    default:
      return fail(-32601, `Method not found: ${method}`);
  }
}
