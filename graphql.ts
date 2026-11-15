// A compact GraphQL engine for the subset gh and github-mcp-server speak: operations with
// variables, aliases, inline fragments and fragment spreads, input objects and enum literals,
// Relay connections with cursor pagination, node(id:) global ids, __type introspection, and
// batched operations. Validation names unknown fields the way GitHub does (a 200 with an
// `errors` array), because one check feature-detects exactly that.

import { DomainError, addComment, createIssue, createPull, mergePull, pullFiles, pullMergeable } from './domain.ts';
import { globalIdOf, parseGlobalId, type IssueRec, type PullRec, type Repo, type Store, type ThreadRec } from './store.ts';

// ---------------------------------------------------------------------------------------------
// Parser

export type GqlValue =
  | { kind: 'string'; value: string }
  | { kind: 'number'; value: number }
  | { kind: 'boolean'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'list'; items: GqlValue[] }
  | { kind: 'object'; fields: Map<string, GqlValue> }
  | { kind: 'variable'; name: string };

export interface GqlField {
  alias: string | null;
  name: string;
  args: Map<string, GqlValue>;
  selections: GqlSelection[];
}

export type GqlSelection =
  | { kind: 'field'; field: GqlField }
  | { kind: 'inline'; typeName: string; selections: GqlSelection[] }
  | { kind: 'spread'; name: string };

export interface GqlOperation {
  type: 'query' | 'mutation';
  name: string | null;
  selections: GqlSelection[];
}

interface GqlDocument {
  operations: GqlOperation[];
  fragments: Map<string, { typeName: string; selections: GqlSelection[] }>;
}

class GqlSyntaxError extends Error {}

export function parseGraphQL(source: string): GqlDocument {
  let at = 0;
  const len = source.length;
  const peek = (): string => {
    skipIgnored();
    return at < len ? (source[at] as string) : '';
  };
  const skipIgnored = (): void => {
    while (at < len) {
      const ch = source[at] as string;
      if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r' || ch === ',') at += 1;
      else if (ch === '#') {
        while (at < len && source[at] !== '\n') at += 1;
      } else break;
    }
  };
  const expect = (ch: string): void => {
    skipIgnored();
    if (source[at] !== ch) throw new GqlSyntaxError(`expected "${ch}" at offset ${at}`);
    at += 1;
  };
  const readName = (): string => {
    skipIgnored();
    const match = /^[A-Za-z_][A-Za-z0-9_]*/.exec(source.slice(at));
    if (match === null) throw new GqlSyntaxError(`expected a name at offset ${at}`);
    at += match[0].length;
    return match[0];
  };
  const readString = (): string => {
    skipIgnored();
    if (source.startsWith('"""', at)) {
      const end = source.indexOf('"""', at + 3);
      if (end < 0) throw new GqlSyntaxError('unterminated block string');
      const raw = source.slice(at + 3, end);
      at = end + 3;
      return raw;
    }
    if (source[at] !== '"') throw new GqlSyntaxError(`expected a string at offset ${at}`);
    at += 1;
    let out = '';
    while (at < len && source[at] !== '"') {
      const ch = source[at] as string;
      if (ch === '\\') {
        const esc = source[at + 1] as string;
        const map: Record<string, string> = { '"': '"', '\\': '\\', '/': '/', b: '\b', f: '\f', n: '\n', r: '\r', t: '\t' };
        if (esc === 'u') {
          out += String.fromCharCode(parseInt(source.slice(at + 2, at + 6), 16));
          at += 6;
        } else if (map[esc] !== undefined) {
          out += map[esc];
          at += 2;
        } else throw new GqlSyntaxError(`bad escape at offset ${at}`);
      } else {
        out += ch;
        at += 1;
      }
    }
    if (at >= len) throw new GqlSyntaxError('unterminated string');
    at += 1;
    return out;
  };
  const readNumber = (): number => {
    skipIgnored();
    const match = /^-?\d+(\.\d+)?([eE][+-]?\d+)?/.exec(source.slice(at));
    if (match === null) throw new GqlSyntaxError(`expected a number at offset ${at}`);
    at += match[0].length;
    return Number(match[0]);
  };
  const readValue = (): GqlValue => {
    skipIgnored();
    const ch = source[at] as string;
    if (ch === '$') {
      at += 1;
      return { kind: 'variable', name: readName() };
    }
    if (ch === '"') return { kind: 'string', value: readString() };
    if (ch === '[') {
      at += 1;
      const items: GqlValue[] = [];
      while (peek() !== ']') items.push(readValue());
      at += 1;
      return { kind: 'list', items };
    }
    if (ch === '{') {
      at += 1;
      const fields = new Map<string, GqlValue>();
      while (peek() !== '}') {
        const name = readName();
        expect(':');
        fields.set(name, readValue());
      }
      at += 1;
      return { kind: 'object', fields };
    }
    if (ch === '-' || (ch >= '0' && ch <= '9')) return { kind: 'number', value: readNumber() };
    const name = readName();
    if (name === 'true') return { kind: 'boolean', value: true };
    if (name === 'false') return { kind: 'boolean', value: false };
    if (name === 'null') return { kind: 'null' };
    return { kind: 'enum', value: name };
  };
  const readType = (): void => {
    skipIgnored();
    if (source[at] === '[') {
      at += 1;
      readType();
      expect(']');
    } else {
      readName();
    }
    skipIgnored();
    if (source[at] === '!') at += 1;
  };
  const readDirectives = (): void => {
    for (;;) {
      skipIgnored();
      if (source[at] !== '@') return;
      at += 1;
      readName();
      skipIgnored();
      if (source[at] === '(') {
        at += 1;
        while (peek() !== ')') {
          readName();
          expect(':');
          readValue();
        }
        at += 1;
      }
    }
  };
  const readArgs = (): Map<string, GqlValue> => {
    const args = new Map<string, GqlValue>();
    skipIgnored();
    if (source[at] !== '(') return args;
    at += 1;
    while (peek() !== ')') {
      const name = readName();
      expect(':');
      args.set(name, readValue());
    }
    at += 1;
    return args;
  };
  const readSelectionSet = (): GqlSelection[] => {
    expect('{');
    const selections: GqlSelection[] = [];
    while (peek() !== '}') selections.push(readSelection());
    at += 1;
    return selections;
  };
  const readSelection = (): GqlSelection => {
    skipIgnored();
    if (source.startsWith('...', at)) {
      at += 3;
      skipIgnored();
      if (/^on(?![A-Za-z0-9_])/.test(source.slice(at))) {
        at += 2;
        const typeName = readName();
        readDirectives();
        return { kind: 'inline', typeName, selections: readSelectionSet() };
      }
      const name = readName();
      readDirectives();
      return { kind: 'spread', name };
    }
    let name = readName();
    let alias: string | null = null;
    skipIgnored();
    if (source[at] === ':') {
      at += 1;
      alias = name;
      name = readName();
    }
    const args = readArgs();
    readDirectives();
    skipIgnored();
    const selections = source[at] === '{' ? readSelectionSet() : [];
    return { kind: 'field', field: { alias, name, args, selections } };
  };

  const operations: GqlOperation[] = [];
  const fragments = new Map<string, { typeName: string; selections: GqlSelection[] }>();
  for (;;) {
    skipIgnored();
    if (at >= len) break;
    if (source[at] === '{') {
      operations.push({ type: 'query', name: null, selections: readSelectionSet() });
      continue;
    }
    const keyword = readName();
    if (keyword === 'query' || keyword === 'mutation' || keyword === 'subscription') {
      skipIgnored();
      let name: string | null = null;
      if (/[A-Za-z_]/.test(source[at] as string)) name = readName();
      skipIgnored();
      if (source[at] === '(') {
        at += 1;
        while (peek() !== ')') {
          expect('$');
          readName();
          expect(':');
          readType();
          skipIgnored();
          if (source[at] === '=') {
            at += 1;
            readValue();
          }
          readDirectives();
        }
        at += 1;
      }
      readDirectives();
      operations.push({ type: keyword === 'mutation' ? 'mutation' : 'query', name, selections: readSelectionSet() });
    } else if (keyword === 'fragment') {
      const name = readName();
      const on = readName();
      if (on !== 'on') throw new GqlSyntaxError('fragment without "on"');
      const typeName = readName();
      readDirectives();
      fragments.set(name, { typeName, selections: readSelectionSet() });
    } else {
      throw new GqlSyntaxError(`unexpected keyword "${keyword}"`);
    }
  }
  return { operations, fragments };
}

/** Resolve an argument value against the operation's variables into plain JS data. */
export function resolveValue(value: GqlValue, variables: Record<string, unknown>): unknown {
  switch (value.kind) {
    case 'string':
    case 'number':
    case 'boolean':
    case 'enum':
      return value.value;
    case 'null':
      return null;
    case 'list':
      return value.items.map((item) => resolveValue(item, variables));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const [key, inner] of value.fields) out[key] = resolveValue(inner, variables);
      return out;
    }
    case 'variable':
      return variables[value.name];
  }
}

// ---------------------------------------------------------------------------------------------
// Schema model (validation + introspection share it)

const TYPE_FIELDS: Record<string, readonly string[]> = {
  Query: ['viewer', 'repository', 'node', 'nodes', 'rateLimit', '__type', '__schema'],
  Mutation: ['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment', 'reopenIssue'],
  User: ['login', 'id', 'name', 'email', 'url', 'databaseId', 'isViewer'],
  Organization: ['login', 'id', 'name', 'url'],
  Repository: [
    'id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission',
    'hasIssuesEnabled', 'hasProjectsEnabled', 'hasWikiEnabled', 'isPrivate', 'isFork', 'isArchived',
    'description', 'url', 'createdAt', 'updatedAt', 'pushedAt', 'issue', 'issues', 'pullRequest',
    'pullRequests', 'stargazerCount', 'forkCount', 'primaryLanguage',
  ],
  Issue: [
    'id', 'databaseId', 'number', 'title', 'body', 'state', 'author', 'repository', 'labels',
    'comments', 'createdAt', 'updatedAt', 'closedAt', 'url', 'viewerDidAuthor',
  ],
  PullRequest: [
    'id', 'databaseId', 'number', 'title', 'body', 'state', 'merged', 'mergedAt', 'mergeable',
    'headRefName', 'baseRefName', 'headRef', 'baseRef', 'author', 'repository', 'files',
    'comments', 'createdAt', 'updatedAt', 'closedAt', 'url', 'mergeCommit', 'additions',
    'deletions', 'changedFiles', 'isDraft',
  ],
  IssueComment: ['id', 'databaseId', 'body', 'author', 'createdAt', 'updatedAt', 'url'],
  Ref: ['name', 'prefix', 'target', 'repository'],
  Commit: ['oid', 'id', 'message', 'messageHeadline', 'committedDate', 'authoredDate', 'author', 'committer', 'tree'],
  GitActor: ['login', 'name', 'email', 'date'],
  IssueConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  IssueEdge: ['node', 'cursor'],
  PullRequestConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  PullRequestEdge: ['node', 'cursor'],
  PullRequestChangedFileConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  PullRequestChangedFileEdge: ['node', 'cursor'],
  PullRequestChangedFile: ['path', 'additions', 'deletions', 'changeType'],
  PageInfo: ['hasNextPage', 'hasPreviousPage', 'startCursor', 'endCursor'],
  CreateIssuePayload: ['issue', 'clientMutationId'],
  CloseIssuePayload: ['issue', 'clientMutationId'],
  ReopenIssuePayload: ['issue', 'clientMutationId'],
  CreatePullRequestPayload: ['pullRequest', 'clientMutationId'],
  MergePullRequestPayload: ['pullRequest', 'clientMutationId'],
  AddCommentPayload: ['commentEdge', 'clientMutationId', 'subject'],
  CommentEdge: ['node', 'cursor'],
  Node: ['id'],
  RateLimit: ['limit', 'cost', 'remaining', 'used', 'resetAt', 'nodeCount'],
  __Schema: ['types', 'queryType', 'mutationType', 'subscriptionType', 'directives'],
  __Type: ['name', 'kind', 'description', 'fields', 'enumValues', 'possibleTypes', 'ofType', 'inputFields', 'interfaces', 'specifiedByURL'],
  __Field: ['name', 'description', 'args', 'type', 'isDeprecated', 'deprecationReason'],
  __EnumValue: ['name', 'description', 'isDeprecated', 'deprecationReason'],
  __InputValue: ['name', 'description', 'type', 'defaultValue'],
  __Directive: ['name', 'description', 'locations', 'args'],
};

const ENUM_VALUES: Record<string, readonly string[]> = {
  IssueState: ['OPEN', 'CLOSED'],
  PullRequestState: ['OPEN', 'CLOSED', 'MERGED'],
  MergeableState: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'],
  PullRequestMergeMethod: ['MERGE', 'SQUASH', 'REBASE'],
  RepositoryPermission: ['ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'],
  SearchType: ['ISSUE', 'USER', 'REPOSITORY', 'DISCUSSION'],
  IssueOrderField: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'],
  PullRequestOrderField: ['CREATED_AT', 'UPDATED_AT'],
  OrderDirection: ['ASC', 'DESC'],
  CommentAuthorAssociation: ['OWNER', 'MEMBER', 'COLLABORATOR', 'NONE'],
  StatusState: ['EXPECTED', 'ERROR', 'FAILURE', 'PENDING', 'SUCCESS'],
};

/** The child type a field resolves to, for validation walks (null: a leaf). */
const FIELD_CHILD: Record<string, Record<string, string | null>> = {
  Query: { viewer: 'User', repository: 'Repository', node: 'Node', __type: '__Type', __schema: '__Schema' },
  Mutation: {
    createIssue: 'CreateIssuePayload', closeIssue: 'CloseIssuePayload', reopenIssue: 'ReopenIssuePayload',
    createPullRequest: 'CreatePullRequestPayload', mergePullRequest: 'MergePullRequestPayload', addComment: 'AddCommentPayload',
  },
  Repository: {
    owner: 'User', defaultBranchRef: 'Ref', issue: 'Issue', issues: 'IssueConnection',
    pullRequest: 'PullRequest', pullRequests: 'PullRequestConnection',
  },
  Issue: { author: 'User', repository: 'Repository' },
  PullRequest: {
    author: 'User', repository: 'Repository', files: 'PullRequestChangedFileConnection',
    headRef: 'Ref', baseRef: 'Ref', mergeCommit: 'Commit',
  },
  IssueComment: { author: 'User' },
  Ref: { target: 'Commit', repository: 'Repository' },
  Commit: { author: 'GitActor', committer: 'GitActor', tree: 'Tree' },
  IssueConnection: { nodes: 'Issue', edges: 'IssueEdge', pageInfo: 'PageInfo' },
  IssueEdge: { node: 'Issue' },
  PullRequestConnection: { nodes: 'PullRequest', edges: 'PullRequestEdge', pageInfo: 'PageInfo' },
  PullRequestEdge: { node: 'PullRequest' },
  PullRequestChangedFileConnection: { nodes: 'PullRequestChangedFile', edges: 'PullRequestChangedFileEdge', pageInfo: 'PageInfo' },
  PullRequestChangedFileEdge: { node: 'PullRequestChangedFile' },
  CreateIssuePayload: { issue: 'Issue' },
  CloseIssuePayload: { issue: 'Issue' },
  ReopenIssuePayload: { issue: 'Issue' },
  CreatePullRequestPayload: { pullRequest: 'PullRequest' },
  MergePullRequestPayload: { pullRequest: 'PullRequest' },
  AddCommentPayload: { commentEdge: 'CommentEdge', subject: 'Issue' },
  CommentEdge: { node: 'IssueComment' },
  __Type: { fields: '__Field', enumValues: '__EnumValue', possibleTypes: '__Type', ofType: '__Type', inputFields: '__InputValue', interfaces: '__Type' },
  __Field: { type: '__Type', args: '__InputValue' },
  __Schema: { types: '__Type', queryType: '__Type', mutationType: '__Type' },
};

const SCALAR_TYPES = new Set(['String', 'Int', 'Float', 'Boolean', 'ID', 'DateTime', 'URI', 'GitObjectID', 'Base64String']);

// ---------------------------------------------------------------------------------------------
// Execution

export class GqlError extends Error {}

type Obj = { __typename: string; [key: string]: unknown };

interface ExecCtx {
  store: Store;
  variables: Record<string, unknown>;
  fragments: GqlDocument['fragments'];
  errors: Array<{ message: string }>;
}

export interface GqlRequest {
  query: string;
  variables?: Record<string, unknown>;
  operationName?: string;
}

export function executeGraphQL(store: Store, request: GqlRequest): { data: unknown; errors?: Array<{ message: string }> } {
  let document: GqlDocument;
  try {
    document = parseGraphQL(request.query);
  } catch (err) {
    return { data: null, errors: [{ message: err instanceof Error ? err.message : 'syntax error' }] };
  }
  let operation: GqlOperation | undefined;
  if (request.operationName !== undefined && request.operationName !== '') {
    operation = document.operations.find((candidate) => candidate.name === request.operationName);
    if (operation === undefined) return { data: null, errors: [{ message: `Unknown operation "${request.operationName}"` }] };
  } else if (document.operations.length === 1) {
    operation = document.operations[0];
  } else {
    operation = document.operations[0];
    if (operation === undefined) return { data: null, errors: [{ message: 'The document holds no operation' }] };
  }
  const ctx: ExecCtx = { store, variables: request.variables ?? {}, fragments: document.fragments, errors: [] };
  const rootType = operation.type === 'mutation' ? 'Mutation' : 'Query';
  const validationErrors = validateSelections(ctx, rootType, operation.selections);
  if (validationErrors.length > 0) {
    return { data: null, errors: validationErrors.map((message) => ({ message })) };
  }
  const root: Obj = { __typename: rootType };
  const data = execSelections(ctx, rootType, root, operation.selections);
  return ctx.errors.length > 0 ? { data, errors: ctx.errors } : { data };
}

function validateSelections(ctx: ExecCtx, typeName: string, selections: GqlSelection[]): string[] {
  const errors: string[] = [];
  const fields = TYPE_FIELDS[typeName];
  for (const selection of selections) {
    if (selection.kind === 'inline') {
      errors.push(...validateSelections(ctx, selection.typeName, selection.selections));
      continue;
    }
    if (selection.kind === 'spread') {
      const fragment = ctx.fragments.get(selection.name);
      if (fragment !== undefined) errors.push(...validateSelections(ctx, fragment.typeName, fragment.selections));
      continue;
    }
    const { field } = selection;
    if (field.name === '__typename') continue;
    if (fields === undefined) continue; // an unmodelled type: nothing to check against
    if (!fields.includes(field.name)) {
      errors.push(`Field '${field.name}' doesn't exist on type '${typeName}'`);
      continue;
    }
    const child = FIELD_CHILD[typeName]?.[field.name];
    if (child !== undefined && child !== null && field.selections.length > 0) {
      errors.push(...validateSelections(ctx, child, field.selections));
    }
  }
  return errors;
}

function expandSelections(ctx: ExecCtx, typeName: string, selections: GqlSelection[]): Array<{ field: GqlField }> {
  const out: Array<{ field: GqlField }> = [];
  for (const selection of selections) {
    if (selection.kind === 'field') out.push({ field: selection.field });
    else if (selection.kind === 'inline') {
      if (selection.typeName === typeName) out.push(...expandSelections(ctx, typeName, selection.selections));
    } else {
      const fragment = ctx.fragments.get(selection.name);
      if (fragment !== undefined && fragment.typeName === typeName) {
        out.push(...expandSelections(ctx, typeName, fragment.selections));
      }
    }
  }
  return out;
}

function execSelections(ctx: ExecCtx, typeName: string, source: Obj, selections: GqlSelection[]): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const { field } of expandSelections(ctx, typeName, selections)) {
    const key = field.alias ?? field.name;
    if (field.name === '__typename') {
      out[key] = typeName;
      continue;
    }
    let value: unknown;
    try {
      value = resolveField(ctx, typeName, source, field);
    } catch (err) {
      ctx.errors.push({ message: err instanceof Error ? err.message : String(err) });
      value = null;
    }
    out[key] = completeValue(ctx, typeName, field, value);
  }
  return out;
}

function completeValue(ctx: ExecCtx, parentType: string, field: GqlField, value: unknown): unknown {
  if (value === null || value === undefined) return null;
  if (Array.isArray(value)) return value.map((item) => completeValue(ctx, parentType, field, item));
  if (typeof value === 'object' && (value as Obj).__typename !== undefined) {
    const obj = value as Obj;
    if (field.selections.length === 0) return obj;
    return execSelections(ctx, obj.__typename, obj, field.selections);
  }
  return value;
}

// ---------------------------------------------------------------------------------------------
// Object constructors

function userObj(store: Store, login: string): Obj {
  const user = store.users.find((candidate) => candidate.login === login) ?? store.viewer;
  return { __typename: 'User', login: user.login, id: globalIdOf({ kind: 'user', userId: user.id }), databaseId: user.id, name: user.name, email: user.email };
}

function repoObj(repo: Repo): Obj {
  return { __typename: 'Repository', repo };
}

function issueObj(repo: Repo, rec: IssueRec): Obj {
  return { __typename: 'Issue', repo, rec };
}

function pullObj(repo: Repo, rec: PullRec): Obj {
  return { __typename: 'PullRequest', repo, rec };
}

function commentObj(repo: Repo, thread: ThreadRec, comment: ThreadRec['comments'][number]): Obj {
  return { __typename: 'IssueComment', repo, thread, comment };
}

function cursorOf(number: number): string {
  return Buffer.from(`cursor:${number}`, 'utf8').toString('base64');
}

function numberOfCursor(cursor: string): number | null {
  try {
    const raw = Buffer.from(cursor, 'base64').toString('utf8');
    const match = /^cursor:(\d+)$/.exec(raw);
    return match ? Number(match[1]) : null;
  } catch {
    return null;
  }
}

interface ConnectionSpec<T> {
  items: T[];
  first: number;
  after: string | null;
  keyOf: (item: T) => number;
}

function paginateConnection<T>(spec: ConnectionSpec<T>): { page: T[]; hasNextPage: boolean; endCursor: string | null } {
  let start = 0;
  if (spec.after !== null) {
    const afterNumber = numberOfCursor(spec.after);
    const index = afterNumber === null ? -1 : spec.items.findIndex((item) => spec.keyOf(item) === afterNumber);
    start = index >= 0 ? index + 1 : 0;
  }
  const page = spec.items.slice(start, start + Math.max(0, spec.first));
  return {
    page,
    hasNextPage: start + page.length < spec.items.length,
    endCursor: page.length > 0 ? cursorOf(spec.keyOf(page[page.length - 1] as T)) : spec.after,
  };
}

function connectionObj(typename: string, edgeTypename: string, nodes: Obj[], total: number, hasNextPage: boolean, endCursor: string | null): Obj {
  return {
    __typename: typename,
    nodesRaw: nodes,
    edgesRaw: nodes.map((node) => ({ __typename: edgeTypename, node, cursor: endCursor })),
    pageInfo: { __typename: 'PageInfo', hasNextPage, hasPreviousPage: false, startCursor: endCursor, endCursor },
    totalCount: total,
  };
}

function argsOf(ctx: ExecCtx, field: GqlField): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [name, value] of field.args) out[name] = resolveValue(value, ctx.variables);
  return out;
}

function inputOf(ctx: ExecCtx, field: GqlField): Record<string, unknown> {
  const args = argsOf(ctx, field);
  const input = args.input;
  return typeof input === 'object' && input !== null ? (input as Record<string, unknown>) : {};
}

function repoByGlobalId(store: Store, id: unknown): Repo {
  const parsed = typeof id === 'string' ? parseGlobalId(id) : null;
  if (parsed === null || parsed.kind !== 'repository') throw new GqlError('the repository id is not recognized');
  const repo = store.reposById.get(parsed.databaseId);
  if (repo === undefined) throw new GqlError('the repository does not exist');
  return repo;
}

function threadByGlobalId(store: Store, id: unknown, kinds: Array<'issue' | 'pull'>): { repo: Repo; thread: ThreadRec } {
  const parsed = typeof id === 'string' ? parseGlobalId(id) : null;
  if (parsed === null || (parsed.kind !== 'issue' && parsed.kind !== 'pull')) throw new GqlError('the id is not recognized');
  if (!kinds.includes(parsed.kind)) throw new GqlError('the id names the wrong kind of object');
  const repo = store.reposById.get(parsed.databaseId);
  if (repo === undefined) throw new GqlError('the repository does not exist');
  const thread = repo.threads.get(parsed.number);
  if (thread === undefined || thread.kind !== parsed.kind) throw new GqlError('the object does not exist');
  return { repo, thread };
}

// ---------------------------------------------------------------------------------------------
// Field resolvers

function resolveField(ctx: ExecCtx, typeName: string, source: Obj, field: GqlField): unknown {
  const { store } = ctx;
  const args = argsOf(ctx, field);
  switch (typeName) {
    case 'Query': {
      if (field.name === 'viewer') return userObj(store, store.viewer.login);
      if (field.name === 'repository') {
        const owner = typeof args.owner === 'string' ? args.owner : '';
        const name = typeof args.name === 'string' ? args.name : '';
        const repo = store.repoOf(owner, name);
        return repo === null ? null : repoObj(repo);
      }
      if (field.name === 'node') return nodeById(store, args.id);
      if (field.name === '__type') return typeObj(typeof args.name === 'string' ? args.name : '');
      if (field.name === '__schema') return { __typename: '__Schema' };
      if (field.name === 'rateLimit') return { __typename: 'RateLimit', limit: 5000, cost: 1, remaining: 5000, resetAt: store.isoOf(store.nowSec()) };
      break;
    }
    case 'Mutation': {
      return resolveMutation(ctx, field);
    }
    case '__Schema': {
      if (field.name === 'queryType') return typeObj('Query');
      if (field.name === 'mutationType') return typeObj('Mutation');
      if (field.name === 'types') return Object.keys(TYPE_FIELDS).map((name) => typeObj(name));
      return null;
    }
    case '__Type': {
      return resolveTypeField(source, field);
    }
    case '__Field': {
      if (field.name === 'name') return source.name;
      if (field.name === 'description') return null;
      if (field.name === 'isDeprecated') return false;
      if (field.name === 'deprecationReason') return null;
      if (field.name === 'type') return { __typename: '__Type', name: 'String', kind: 'SCALAR' };
      if (field.name === 'args') return [];
      break;
    }
    case '__EnumValue': {
      if (field.name === 'name') return source.name;
      if (field.name === 'description') return null;
      if (field.name === 'isDeprecated') return false;
      if (field.name === 'deprecationReason') return null;
      break;
    }
    case 'User':
    case 'Organization': {
      const login = source.login as string;
      if (field.name === 'login') return login;
      if (field.name === 'id') return source.id;
      if (field.name === 'databaseId') return source.databaseId;
      if (field.name === 'name') return source.name;
      if (field.name === 'email') return source.email;
      if (field.name === 'isViewer') return login === store.viewer.login;
      if (field.name === 'url') return `https://forge.invalid/${login}`;
      break;
    }
    case 'Repository':
      return resolveRepositoryField(ctx, source.repo as Repo, field, args);
    case 'Issue':
      return resolveIssueField(ctx, source, field);
    case 'PullRequest':
      return resolvePullField(ctx, source, field, args);
    case 'IssueComment': {
      const comment = source.comment as ThreadRec['comments'][number];
      if (field.name === 'id') {
        return globalIdOf({ kind: 'comment', databaseId: (source.repo as Repo).databaseId, number: (source.thread as ThreadRec).number, commentId: comment.id });
      }
      if (field.name === 'databaseId') return comment.id;
      if (field.name === 'body') return comment.body;
      if (field.name === 'author') return userObj(store, comment.authorLogin);
      if (field.name === 'createdAt' || field.name === 'updatedAt') return store.isoOf(comment.atSec);
      break;
    }
    case 'Ref': {
      if (field.name === 'name') return source.name;
      if (field.name === 'prefix') return 'refs/heads/';
      if (field.name === 'target') return { __typename: 'Commit', repo: source.repo, oid: source.oid };
      if (field.name === 'repository') return repoObj(source.repo as Repo);
      break;
    }
    case 'Commit': {
      if (field.name === 'oid' || field.name === 'id') return source.oid;
      return null;
    }
    case 'IssueConnection':
    case 'PullRequestConnection':
    case 'PullRequestChangedFileConnection': {
      if (field.name === 'nodes') return source.nodesRaw;
      if (field.name === 'edges') return source.edgesRaw;
      if (field.name === 'pageInfo') return source.pageInfo;
      if (field.name === 'totalCount') return source.totalCount;
      break;
    }
    case 'IssueEdge':
    case 'PullRequestEdge':
    case 'PullRequestChangedFileEdge':
    case 'CommentEdge': {
      if (field.name === 'node') return source.node;
      if (field.name === 'cursor') return source.cursor;
      break;
    }
    case 'PageInfo': {
      if (field.name in source) return source[field.name];
      return null;
    }
    case 'CreateIssuePayload':
    case 'CloseIssuePayload':
    case 'ReopenIssuePayload': {
      if (field.name === 'issue') return source.issue;
      if (field.name === 'clientMutationId') return null;
      break;
    }
    case 'CreatePullRequestPayload':
    case 'MergePullRequestPayload': {
      if (field.name === 'pullRequest') return source.pullRequest;
      if (field.name === 'clientMutationId') return null;
      break;
    }
    case 'AddCommentPayload': {
      if (field.name === 'commentEdge') return source.commentEdge;
      if (field.name === 'subject') return source.subject;
      if (field.name === 'clientMutationId') return null;
      break;
    }
    case 'PullRequestChangedFile': {
      if (field.name in source) return source[field.name];
      return null;
    }
  }
  // Plain data carried on a wrapper object (PageInfo flags, changed-file rows, rate limits).
  if (field.name in source) return source[field.name];
  // A modelled field with no resolver answers null rather than failing the whole operation.
  if ((TYPE_FIELDS[typeName] ?? []).includes(field.name)) return null;
  throw new GqlError(`Field '${field.name}' doesn't exist on type '${typeName}'`);
}

function typeObj(name: string): Obj | null {
  if (TYPE_FIELDS[name] !== undefined) return { __typename: '__Type', name, kind: 'OBJECT' };
  if (ENUM_VALUES[name] !== undefined) return { __typename: '__Type', name, kind: 'ENUM' };
  if (SCALAR_TYPES.has(name)) return { __typename: '__Type', name, kind: 'SCALAR' };
  if (name === 'Tree') return { __typename: '__Type', name, kind: 'OBJECT' };
  if (name === 'RateLimit') return { __typename: '__Type', name, kind: 'OBJECT' };
  return null;
}

function resolveTypeField(source: Obj, field: GqlField): unknown {
  const name = source.name as string;
  if (field.name === 'name') return name;
  if (field.name === 'kind') return source.kind;
  if (field.name === 'description') return null;
  if (field.name === 'fields') {
    const fields = TYPE_FIELDS[name];
    if (fields === undefined) return null;
    return fields.map((fieldName) => ({ __typename: '__Field', name: fieldName, parentType: name }));
  }
  if (field.name === 'enumValues') {
    const values = ENUM_VALUES[name];
    if (values === undefined) return null;
    return values.map((value) => ({ __typename: '__EnumValue', name: value }));
  }
  if (field.name === 'possibleTypes' || field.name === 'interfaces') return [];
  if (field.name === 'ofType' || field.name === 'inputFields') return null;
  return null;
}

function nodeById(store: Store, id: unknown): Obj | null {
  if (typeof id !== 'string') return null;
  const parsed = parseGlobalId(id);
  if (parsed === null) return null;
  switch (parsed.kind) {
    case 'repository': {
      const repo = store.reposById.get(parsed.databaseId);
      return repo === undefined ? null : repoObj(repo);
    }
    case 'issue':
    case 'pull': {
      const repo = store.reposById.get(parsed.databaseId);
      if (repo === undefined) return null;
      const thread = repo.threads.get(parsed.number);
      if (thread === undefined || thread.kind !== parsed.kind) return null;
      return thread.kind === 'issue' ? issueObj(repo, thread) : pullObj(repo, thread);
    }
    case 'comment': {
      const repo = store.reposById.get(parsed.databaseId);
      if (repo === undefined) return null;
      const thread = repo.threads.get(parsed.number);
      const comment = thread?.comments.find((candidate) => candidate.id === parsed.commentId);
      return thread === undefined || comment === undefined ? null : commentObj(repo, thread, comment);
    }
    case 'user': {
      const user = store.users.find((candidate) => candidate.id === parsed.userId);
      return user === undefined ? null : userObj(store, user.login);
    }
  }
}

function resolveRepositoryField(ctx: ExecCtx, repo: Repo, field: GqlField, args: Record<string, unknown>): unknown {
  const { store } = ctx;
  switch (field.name) {
    case 'id':
      return globalIdOf({ kind: 'repository', databaseId: repo.databaseId });
    case 'databaseId':
      return repo.databaseId;
    case 'name':
      return repo.name;
    case 'nameWithOwner':
      return `${repo.owner}/${repo.name}`;
    case 'owner':
      return userObj(store, repo.owner);
    case 'defaultBranchRef': {
      const oid = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
      return oid === undefined ? null : { __typename: 'Ref', repo, name: repo.defaultBranch, oid };
    }
    case 'viewerPermission':
      return 'ADMIN';
    case 'hasIssuesEnabled':
    case 'hasProjectsEnabled':
    case 'hasWikiEnabled':
      return true;
    case 'isPrivate':
      return repo.isPrivate;
    case 'isFork':
    case 'isArchived':
      return false;
    case 'description':
      return repo.description === '' ? null : repo.description;
    case 'url':
      return `https://forge.invalid/${repo.owner}/${repo.name}`;
    case 'createdAt':
    case 'updatedAt':
    case 'pushedAt':
      return store.isoOf(repo.createdSec);
    case 'stargazerCount':
    case 'forkCount':
      return 0;
    case 'issue': {
      const thread = repo.threads.get(Number(args.number));
      return thread !== undefined && thread.kind === 'issue' ? issueObj(repo, thread) : null;
    }
    case 'issues': {
      const states = Array.isArray(args.states) ? (args.states as string[]) : null;
      let issues = [...repo.threads.values()].filter((thread): thread is IssueRec => thread.kind === 'issue');
      if (states !== null) {
        const wanted = states.map((state) => state.toLowerCase());
        issues = issues.filter((issue) => wanted.includes(issue.state));
      }
      const orderBy = (args.orderBy ?? {}) as { field?: string; direction?: string };
      const descending = typeof orderBy.direction === 'string' ? orderBy.direction === 'DESC' : false;
      issues.sort((a, b) => (descending ? b.number - a.number : a.number - b.number));
      const first = typeof args.first === 'number' ? Math.min(100, args.first) : 30;
      const { page, hasNextPage, endCursor } = paginateConnection({ items: issues, first, after: typeof args.after === 'string' ? args.after : null, keyOf: (issue) => issue.number });
      return connectionObj('IssueConnection', 'IssueEdge', page.map((issue) => issueObj(repo, issue)), issues.length, hasNextPage, endCursor);
    }
    case 'pullRequest': {
      const thread = repo.threads.get(Number(args.number));
      return thread !== undefined && thread.kind === 'pull' ? pullObj(repo, thread) : null;
    }
    case 'pullRequests': {
      const states = Array.isArray(args.states) ? (args.states as string[]) : null;
      let pulls = [...repo.threads.values()].filter((thread): thread is PullRec => thread.kind === 'pull');
      if (states !== null) {
        pulls = pulls.filter((pull) => {
          const gqlState = pull.merged ? 'MERGED' : pull.state.toUpperCase();
          return states.includes(gqlState);
        });
      }
      pulls.sort((a, b) => b.number - a.number);
      const first = typeof args.first === 'number' ? Math.min(100, args.first) : 30;
      const { page, hasNextPage, endCursor } = paginateConnection({ items: pulls, first, after: typeof args.after === 'string' ? args.after : null, keyOf: (pull) => pull.number });
      return connectionObj('PullRequestConnection', 'PullRequestEdge', page.map((pull) => pullObj(repo, pull)), pulls.length, hasNextPage, endCursor);
    }
  }
  return null;
}

function resolveIssueField(ctx: ExecCtx, source: Obj, field: GqlField): unknown {
  const { store } = ctx;
  const repo = source.repo as Repo;
  const rec = source.rec as IssueRec;
  switch (field.name) {
    case 'id':
      return globalIdOf({ kind: 'issue', databaseId: repo.databaseId, number: rec.number });
    case 'databaseId':
      return rec.id;
    case 'number':
      return rec.number;
    case 'title':
      return rec.title;
    case 'body':
      return rec.body;
    case 'state':
      return rec.state.toUpperCase();
    case 'author':
      return userObj(store, rec.authorLogin);
    case 'repository':
      return repoObj(repo);
    case 'createdAt':
      return store.isoOf(rec.atSec);
    case 'updatedAt':
      return store.isoOf(rec.updatedSec);
    case 'closedAt':
      return rec.state === 'closed' ? store.isoOf(rec.updatedSec) : null;
    case 'url':
      return `https://forge.invalid/${repo.owner}/${repo.name}/issues/${rec.number}`;
    case 'viewerDidAuthor':
      return rec.authorLogin === store.viewer.login;
  }
  return null;
}

function resolvePullField(ctx: ExecCtx, source: Obj, field: GqlField, args: Record<string, unknown>): unknown {
  const { store } = ctx;
  const repo = source.repo as Repo;
  const rec = source.rec as PullRec;
  switch (field.name) {
    case 'id':
      return globalIdOf({ kind: 'pull', databaseId: repo.databaseId, number: rec.number });
    case 'databaseId':
      return rec.id;
    case 'number':
      return rec.number;
    case 'title':
      return rec.title;
    case 'body':
      return rec.body;
    case 'state':
      return rec.merged ? 'MERGED' : rec.state.toUpperCase();
    case 'merged':
      return rec.merged;
    case 'mergedAt':
      return rec.mergedAtSec === null ? null : store.isoOf(rec.mergedAtSec);
    case 'mergeable': {
      if (rec.state !== 'open') return 'UNKNOWN';
      return pullMergeable(repo, rec) ? 'MERGEABLE' : 'CONFLICTING';
    }
    case 'headRefName':
      return rec.headRef;
    case 'baseRefName':
      return rec.baseRef;
    case 'headRef': {
      const oid = repo.refs.get(`refs/heads/${rec.headRef}`);
      return oid === undefined ? null : { __typename: 'Ref', repo, name: rec.headRef, oid };
    }
    case 'baseRef': {
      const oid = repo.refs.get(`refs/heads/${rec.baseRef}`);
      return oid === undefined ? null : { __typename: 'Ref', repo, name: rec.baseRef, oid };
    }
    case 'author':
      return userObj(store, rec.authorLogin);
    case 'repository':
      return repoObj(repo);
    case 'createdAt':
      return store.isoOf(rec.atSec);
    case 'updatedAt':
      return store.isoOf(rec.updatedSec);
    case 'closedAt':
      return rec.state === 'closed' ? store.isoOf(rec.updatedSec) : null;
    case 'url':
      return `https://forge.invalid/${repo.owner}/${repo.name}/pull/${rec.number}`;
    case 'isDraft':
      return false;
    case 'files': {
      const files = pullFiles(repo, rec);
      const first = typeof args.first === 'number' ? Math.min(100, args.first) : 30;
      const { page, hasNextPage, endCursor } = paginateConnection({
        items: files.map((file, index) => ({ ...file, index })),
        first,
        after: typeof args.after === 'string' ? args.after : null,
        keyOf: (file) => file.index,
      });
      const nodes = page.map((file) => ({
        __typename: 'PullRequestChangedFile',
        path: file.path,
        additions: file.additions,
        deletions: file.deletions,
        changeType: file.status === 'added' ? 'ADDED' : file.status === 'removed' ? 'DELETED' : 'MODIFIED',
      }));
      return connectionObj('PullRequestChangedFileConnection', 'PullRequestChangedFileEdge', nodes, files.length, hasNextPage, endCursor);
    }
    case 'additions':
      return pullFiles(repo, rec).reduce((sum, file) => sum + file.additions, 0);
    case 'deletions':
      return pullFiles(repo, rec).reduce((sum, file) => sum + file.deletions, 0);
    case 'changedFiles':
      return pullFiles(repo, rec).length;
    case 'mergeCommit':
      return rec.mergeCommitSha === null ? null : { __typename: 'Commit', repo, oid: rec.mergeCommitSha };
  }
  return null;
}

function resolveMutation(ctx: ExecCtx, field: GqlField): unknown {
  const { store } = ctx;
  const input = inputOf(ctx, field);
  try {
    switch (field.name) {
      case 'createIssue': {
        const repo = repoByGlobalId(store, input.repositoryId);
        const issue = createIssue(store, repo, { title: input.title, body: input.body, labels: input.labelIds !== undefined ? [] : undefined });
        return { __typename: 'CreateIssuePayload', issue: issueObj(repo, issue) };
      }
      case 'closeIssue':
      case 'reopenIssue': {
        const { repo, thread } = threadByGlobalId(store, input.issueId, ['issue']);
        const issue = thread as IssueRec;
        issue.state = field.name === 'closeIssue' ? 'closed' : 'open';
        issue.updatedSec = store.tick();
        return { __typename: field.name === 'closeIssue' ? 'CloseIssuePayload' : 'ReopenIssuePayload', issue: issueObj(repo, issue) };
      }
      case 'createPullRequest': {
        const repo = repoByGlobalId(store, input.repositoryId);
        const pull = createPull(store, repo, {
          title: input.title,
          body: input.body,
          head: input.headRefName,
          base: input.baseRefName,
        });
        return { __typename: 'CreatePullRequestPayload', pullRequest: pullObj(repo, pull) };
      }
      case 'mergePullRequest': {
        const { repo, thread } = threadByGlobalId(store, input.pullRequestId, ['pull']);
        const pull = thread as PullRec;
        const raw = typeof input.mergeMethod === 'string' ? input.mergeMethod.toUpperCase() : 'MERGE';
        mergePull(store, repo, pull, raw === 'SQUASH' ? 'squash' : raw === 'REBASE' ? 'rebase' : 'merge');
        return { __typename: 'MergePullRequestPayload', pullRequest: pullObj(repo, pull) };
      }
      case 'addComment': {
        const { repo, thread } = threadByGlobalId(store, input.subjectId, ['issue', 'pull']);
        const body = typeof input.body === 'string' ? input.body : '';
        if (body === '') throw new GqlError('a comment body is required');
        const comment = addComment(store, repo, thread, body);
        const node = commentObj(repo, thread, comment);
        return {
          __typename: 'AddCommentPayload',
          commentEdge: { __typename: 'CommentEdge', node, cursor: cursorOf(thread.number) },
          subject: thread.kind === 'issue' ? issueObj(repo, thread) : pullObj(repo, thread),
        };
      }
    }
  } catch (err) {
    if (err instanceof DomainError) throw new GqlError(err.message);
    throw err;
  }
  throw new GqlError(`Field '${field.name}' doesn't exist on type 'Mutation'`);
}
