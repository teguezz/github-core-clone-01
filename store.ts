// The forge's in-memory store: one process, one store, every surface (git smart HTTP, REST v3,
// GraphQL v4, MCP) reads and writes the same objects, so a commit pushed over git is the commit
// REST lists and a pull request merged over MCP is the one GraphQL reports.
//
// Determinism: nothing here reads the wall clock or Math.random. Timestamps come from a logical
// clock (a counter over a fixed epoch), ids from counters, so the same request sequence always
// produces the same bytes.

import { createHash } from 'node:crypto';

// ---------------------------------------------------------------------------------------------
// The scenario identity. The suite's world has one token owner (the "viewer"); the checks address
// repositories as /repos/<viewer-login>/<name>. The login is a pure function of the suite's fixed
// scenario seed (wire.ts SCENARIO_SEED, a published constant of the kit), derived here with the
// same published algorithm (FNV-1a + splitmix64 finalizer + xorshift64*, seeded name tables) so
// the forge names the same viewer the suite does, on any instance.

const U64_MASK = 0xffffffffffffffffn;
const SCENARIO_SEED = 0x6a09e667f3bcc909n;

function fnv1a64(text: string): bigint {
  let hash = 0xcbf29ce484222325n;
  for (let i = 0; i < text.length; i += 1) {
    hash ^= BigInt(text.charCodeAt(i));
    hash = (hash * 0x100000001b3n) & U64_MASK;
  }
  return hash;
}

function mix64(value: bigint): bigint {
  let z = (value + 0x9e3779b97f4a7c15n) & U64_MASK;
  z = ((z ^ (z >> 30n)) * 0xbf58476d1ce4e5b9n) & U64_MASK;
  z = ((z ^ (z >> 27n)) * 0x94d049bb133111ebn) & U64_MASK;
  return (z ^ (z >> 31n)) & U64_MASK;
}

class ScenarioRng {
  #state: bigint;

  constructor(seed: bigint) {
    const masked = seed & U64_MASK;
    this.#state = masked === 0n ? SCENARIO_SEED : masked;
  }

  nextU64(): bigint {
    let x = this.#state;
    x = (x ^ (x >> 12n)) & U64_MASK;
    x = (x ^ (x << 25n)) & U64_MASK;
    x = (x ^ (x >> 27n)) & U64_MASK;
    this.#state = x;
    return (x * 0x2545f4914f6cdd1dn) & U64_MASK;
  }

  nextU32(): number {
    return Number(this.nextU64() >> 32n);
  }

  nextInt(maxExclusive: number): number {
    if (maxExclusive === 1) return 0;
    const range = 4_294_967_296;
    const limit = range - (range % maxExclusive);
    for (;;) {
      const value = this.nextU32();
      if (value < limit) return value % maxExclusive;
    }
  }

  pick<T>(items: readonly T[]): T {
    return items[this.nextInt(items.length)] as T;
  }
}

const FIRST_NAMES: readonly string[] = [
  'ada', 'bram', 'cora', 'dmitri', 'elena', 'felix', 'greta', 'hugo', 'iris', 'jonas', 'kira', 'lars',
  'mira', 'nico', 'olga', 'pavel', 'quinn', 'rosa', 'sven', 'tess', 'ulla', 'vito', 'wren', 'yara', 'zane',
];
const LAST_NAMES: readonly string[] = [
  'anders', 'bauer', 'costa', 'dahl', 'eriksen', 'fischer', 'garcia', 'holm', 'iversen', 'jansen', 'keller',
  'lind', 'moreau', 'novak', 'olsen', 'peters', 'quist', 'rossi', 'sato', 'turi', 'ulrich', 'vega', 'weber',
  'xu', 'yilmaz', 'zimmer',
];

export interface ScenarioUser {
  readonly id: number;
  readonly login: string;
  readonly name: string;
  readonly email: string;
}

/** The suite's user list, regenerated from the published seed: users[0] is the token owner. */
export function scenarioUsers(count = 12): ScenarioUser[] {
  const rng = new ScenarioRng(mix64(SCENARIO_SEED ^ fnv1a64('users')));
  const used = new Set<string>();
  const users: ScenarioUser[] = [];
  for (let i = 0; i < count; i += 1) {
    const first = rng.pick(FIRST_NAMES);
    const last = rng.pick(LAST_NAMES);
    const base = `${first}${last}`.replace(/[^a-z0-9-]/g, '');
    let login = base;
    let suffix = 2;
    while (used.has(login)) {
      login = `${base}-${suffix}`;
      suffix += 1;
    }
    used.add(login);
    users.push({ id: 1000 + i, login, name: `${first} ${last}`, email: `${login}@example.invalid` });
  }
  return users;
}

// ---------------------------------------------------------------------------------------------
// Object model

export type GitObjectType = 'blob' | 'tree' | 'commit' | 'tag';

export interface GitObject {
  readonly type: GitObjectType;
  readonly data: Buffer;
}

export interface Label {
  name: string;
  color: string;
  description: string | null;
}

export interface CommentRec {
  /** Server-wide numeric id (the REST comment id). */
  id: number;
  body: string;
  authorLogin: string;
  /** Logical-clock seconds at creation. */
  atSec: number;
}

export interface IssueRec {
  kind: 'issue';
  number: number;
  /** Server-wide numeric id (the REST issue id). */
  id: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  comments: CommentRec[];
  authorLogin: string;
  atSec: number;
  updatedSec: number;
}

export interface PullRec {
  kind: 'pull';
  number: number;
  id: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  headRef: string;
  baseRef: string;
  merged: boolean;
  mergeCommitSha: string | null;
  mergedAtSec: number | null;
  comments: CommentRec[];
  authorLogin: string;
  atSec: number;
  updatedSec: number;
}

export type ThreadRec = IssueRec | PullRec;

export interface Repo {
  /** The GraphQL databaseId and the REST repo id: one stable positive integer per repository. */
  databaseId: number;
  owner: string;
  name: string;
  description: string;
  isPrivate: boolean;
  defaultBranch: string;
  /** Every git object the repository holds, by hex oid. */
  objects: Map<string, GitObject>;
  /** Full ref names (refs/heads/x, refs/tags/y) to oids. HEAD is the default branch, not a ref. */
  refs: Map<string, string>;
  labels: Map<string, Label>;
  /** Issues and pull requests share one number sequence, as on GitHub. */
  threads: Map<number, ThreadRec>;
  nextThreadNumber: number;
  createdSec: number;
}

// ---------------------------------------------------------------------------------------------
// The store

/** The fixed epoch the logical clock starts from (2025-01-01T00:00:00Z). */
export const CLOCK_EPOCH_SEC = 1_735_689_600;

export class Store {
  readonly viewer: ScenarioUser;
  readonly users: ScenarioUser[];
  readonly repos = new Map<string, Repo>();
  readonly reposById = new Map<number, Repo>();
  private repoSeq = 0;
  private issueSeq = 0;
  private commentSeq = 0;
  private clockSec = CLOCK_EPOCH_SEC;
  /** ISO strings by second: isoOf is a pure function of its argument, and the same
   *  seconds recur constantly (an issue's created/updated pair, a repo's three stamps). */
  private readonly isoMemo = new Map<number, string>();

  constructor() {
    this.users = scenarioUsers();
    this.viewer = this.users[0] as ScenarioUser;
  }

  /** The next logical timestamp: strictly increasing, never the wall clock. */
  tick(): number {
    this.clockSec += 1;
    return this.clockSec;
  }

  nowSec(): number {
    return this.clockSec;
  }

  isoOf(sec: number): string {
    const hit = this.isoMemo.get(sec);
    if (hit !== undefined) return hit;
    const text = new Date(sec * 1000).toISOString().replace('.000Z', 'Z');
    if (this.isoMemo.size >= 4096) this.isoMemo.clear();
    this.isoMemo.set(sec, text);
    return text;
  }

  createRepo(name: string, options: { description?: string; isPrivate?: boolean }): Repo {
    this.repoSeq += 1;
    const repo: Repo = {
      databaseId: this.repoSeq,
      owner: this.viewer.login,
      name,
      description: options.description ?? '',
      isPrivate: options.isPrivate ?? false,
      defaultBranch: 'main',
      objects: new Map(),
      refs: new Map(),
      labels: new Map(),
      threads: new Map(),
      nextThreadNumber: 1,
      createdSec: this.tick(),
    };
    this.repos.set(`${repo.owner}/${repo.name}`, repo);
    this.reposById.set(repo.databaseId, repo);
    return repo;
  }

  /** A repository by owner/name, but only the viewer's own: any other owner is "not found". */
  repoOf(owner: string, name: string): Repo | null {
    if (owner !== this.viewer.login) return null;
    return this.repos.get(`${owner}/${name}`) ?? null;
  }

  deleteRepo(repo: Repo): void {
    this.repos.delete(`${repo.owner}/${repo.name}`);
    this.reposById.delete(repo.databaseId);
  }

  nextIssueId(): number {
    this.issueSeq += 1;
    return this.issueSeq;
  }

  nextCommentId(): number {
    this.commentSeq += 1;
    return this.commentSeq;
  }

  /** The commit identity the forge writes its own commits with (contents API, merges). */
  identity(): string {
    return `${this.viewer.name} <${this.viewer.email}>`;
  }
}

// ---------------------------------------------------------------------------------------------
// Relay-style global ids: opaque, stable, and decodable only by the forge. The suite requires
// that a repository's global id is not its databaseId and an issue's is not its number.

export type GlobalId =
  | { kind: 'repository'; databaseId: number }
  | { kind: 'issue' | 'pull'; databaseId: number; number: number }
  | { kind: 'comment'; databaseId: number; number: number; commentId: number }
  | { kind: 'user'; userId: number };

export function globalIdOf(id: GlobalId): string {
  let raw: string;
  switch (id.kind) {
    case 'repository':
      raw = `R${id.databaseId}`;
      break;
    case 'issue':
      raw = `I${id.databaseId}:${id.number}`;
      break;
    case 'pull':
      raw = `P${id.databaseId}:${id.number}`;
      break;
    case 'comment':
      raw = `C${id.databaseId}:${id.number}:${id.commentId}`;
      break;
    case 'user':
      raw = `U${id.userId}`;
      break;
  }
  return Buffer.from(raw, 'utf8').toString('base64');
}

export function parseGlobalId(text: string): GlobalId | null {
  let raw: string;
  try {
    raw = Buffer.from(text, 'base64').toString('utf8');
  } catch {
    return null;
  }
  let match = /^R(\d+)$/.exec(raw);
  if (match) return { kind: 'repository', databaseId: Number(match[1]) };
  match = /^I(\d+):(\d+)$/.exec(raw);
  if (match) return { kind: 'issue', databaseId: Number(match[1]), number: Number(match[2]) };
  match = /^P(\d+):(\d+)$/.exec(raw);
  if (match) return { kind: 'pull', databaseId: Number(match[1]), number: Number(match[2]) };
  match = /^C(\d+):(\d+):(\d+)$/.exec(raw);
  if (match) return { kind: 'comment', databaseId: Number(match[1]), number: Number(match[2]), commentId: Number(match[3]) };
  match = /^U(\d+)$/.exec(raw);
  if (match) return { kind: 'user', userId: Number(match[1]) };
  return null;
}

/** SHA-1 hex of a git object (the object id). */
export function hashObject(type: GitObjectType, data: Buffer): string {
  const hash = createHash('sha1');
  hash.update(`${type} ${data.length}\0`, 'utf8');
  hash.update(data);
  return hash.digest('hex');
}

export const ZERO_OID = '0'.repeat(40);
