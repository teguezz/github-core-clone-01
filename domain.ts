// Domain operations shared by REST, GraphQL and MCP: creating issues and pull requests,
// commenting, and merging (merge / squash / rebase over the in-process three-way tree merge).
// One implementation, so every surface agrees on numbers, states and merge results.

import {
  buildCommit,
  diffCommits,
  isAncestor,
  mergeBase,
  mergeCommits,
  parseCommit,
  putObject,
  type FileChange,
} from './git.ts';
import type { CommentRec, IssueRec, PullRec, Repo, Store, ThreadRec } from './store.ts';

export class DomainError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

export function createIssue(
  store: Store,
  repo: Repo,
  input: { title?: unknown; body?: unknown; labels?: unknown },
): IssueRec {
  const title = typeof input.title === 'string' ? input.title : '';
  if (title === '') throw new DomainError(422, 'Validation Failed: title is required');
  const body = typeof input.body === 'string' ? input.body : '';
  const labels: string[] = [];
  if (input.labels !== undefined) {
    if (!Array.isArray(input.labels)) throw new DomainError(422, 'Validation Failed: labels must be an array');
    for (const raw of input.labels) {
      const name = typeof raw === 'string' ? raw : typeof (raw as { name?: unknown })?.name === 'string' ? ((raw as { name: string }).name as string) : '';
      if (!repo.labels.has(name)) {
        throw new DomainError(422, `Validation Failed: label "${name}" does not exist in ${repo.owner}/${repo.name}`);
      }
      labels.push(name);
    }
  }
  const at = store.tick();
  const issue: IssueRec = {
    kind: 'issue',
    number: repo.nextThreadNumber,
    id: store.nextIssueId(),
    title,
    body,
    state: 'open',
    labels,
    comments: [],
    authorLogin: store.viewer.login,
    atSec: at,
    updatedSec: at,
  };
  repo.nextThreadNumber += 1;
  repo.threads.set(issue.number, issue);
  return issue;
}

export function createPull(
  store: Store,
  repo: Repo,
  input: { title?: unknown; body?: unknown; head?: unknown; base?: unknown },
): PullRec {
  const title = typeof input.title === 'string' ? input.title : '';
  const body = typeof input.body === 'string' ? input.body : '';
  const stripOwner = (ref: string): string => (ref.includes(':') ? (ref.split(':').pop() as string) : ref);
  const head = typeof input.head === 'string' ? stripOwner(input.head) : '';
  const base = typeof input.base === 'string' ? stripOwner(input.base) : '';
  if (head === '' || !repo.refs.has(`refs/heads/${head}`)) {
    throw new DomainError(422, `Validation Failed: head ref "refs/heads/${head}" does not exist`);
  }
  if (base === '' || !repo.refs.has(`refs/heads/${base}`)) {
    throw new DomainError(422, `Validation Failed: base ref "refs/heads/${base}" does not exist`);
  }
  for (const thread of repo.threads.values()) {
    if (thread.kind === 'pull' && thread.state === 'open' && thread.headRef === head && thread.baseRef === base) {
      throw new DomainError(422, `Validation Failed: a pull request already exists for ${head} into ${base}`);
    }
  }
  const at = store.tick();
  const pull: PullRec = {
    kind: 'pull',
    number: repo.nextThreadNumber,
    id: store.nextIssueId(),
    title,
    body,
    state: 'open',
    headRef: head,
    baseRef: base,
    merged: false,
    mergeCommitSha: null,
    mergedAtSec: null,
    comments: [],
    authorLogin: store.viewer.login,
    atSec: at,
    updatedSec: at,
  };
  repo.nextThreadNumber += 1;
  repo.threads.set(pull.number, pull);
  return pull;
}

export function addComment(store: Store, repo: Repo, thread: ThreadRec, body: string): CommentRec {
  const comment: CommentRec = {
    id: store.nextCommentId(),
    body,
    authorLogin: store.viewer.login,
    atSec: store.tick(),
  };
  thread.comments.push(comment);
  thread.updatedSec = comment.atSec;
  void repo;
  return comment;
}

/** The merge-base of a pull request's current head and base tips. */
export function pullMergeBase(repo: Repo, pull: PullRec): string | null {
  const head = repo.refs.get(`refs/heads/${pull.headRef}`);
  const base = repo.refs.get(`refs/heads/${pull.baseRef}`);
  if (head === undefined || base === undefined) return null;
  return mergeBase(repo, base, head);
}

/** Whether the pull request would merge cleanly right now. */
export function pullMergeable(repo: Repo, pull: PullRec): boolean {
  if (pull.state !== 'open') return false;
  const head = repo.refs.get(`refs/heads/${pull.headRef}`);
  const base = repo.refs.get(`refs/heads/${pull.baseRef}`);
  if (head === undefined || base === undefined) return false;
  if (isAncestor(repo, head, base)) return true; // already up to date
  const baseOfMerge = mergeBase(repo, base, head);
  const result = mergeCommits(repo, baseOfMerge, base, head);
  return 'tree' in result;
}

export interface PullFileInfo extends FileChange {
  additions: number;
  deletions: number;
}

function lineDiff(before: string, after: string): { additions: number; deletions: number } {
  const a = before === '' ? [] : before.split('\n');
  const b = after === '' ? [] : after.split('\n');
  if (a.length > 0 && a[a.length - 1] === '') a.pop();
  if (b.length > 0 && b[b.length - 1] === '') b.pop();
  // Longest-common-subsequence over lines; the blobs in play are small.
  const m = a.length;
  const n = b.length;
  const dp: number[] = new Array((m + 1) * (n + 1)).fill(0);
  for (let i = m - 1; i >= 0; i -= 1) {
    for (let j = n - 1; j >= 0; j -= 1) {
      dp[i * (n + 1) + j] = a[i] === b[j] ? (dp[(i + 1) * (n + 1) + (j + 1)] as number) + 1 : Math.max(dp[(i + 1) * (n + 1) + j] as number, dp[i * (n + 1) + (j + 1)] as number);
    }
  }
  const common = dp[0] as number;
  return { additions: n - common, deletions: m - common };
}

/** The changed files of a pull request, diffed from its merge base to its head. */
export function pullFiles(repo: Repo, pull: PullRec): PullFileInfo[] {
  const head = repo.refs.get(`refs/heads/${pull.headRef}`);
  if (head === undefined) return [];
  const base = pullMergeBase(repo, pull);
  const changes = diffCommits(repo, base, head);
  return changes.map((change) => {
    const beforeText = change.before === null ? '' : (repo.objects.get(change.before.oid)?.data.toString('utf8') ?? '');
    const afterText = change.after === null ? '' : (repo.objects.get(change.after.oid)?.data.toString('utf8') ?? '');
    const { additions, deletions } = lineDiff(beforeText, afterText);
    return { ...change, additions, deletions };
  });
}

export type MergeMethod = 'merge' | 'squash' | 'rebase';

export interface MergeOutcome {
  sha: string;
}

/**
 * Merge a pull request into its base branch. Throws DomainError 404/405 in GitHub's shape:
 * a conflicted (or already merged, or closed) pull is a 405. On success the base ref moves to
 * the merge commit and the pull is closed with merged: true.
 */
export function mergePull(store: Store, repo: Repo, pull: PullRec, method: MergeMethod): MergeOutcome {
  if (pull.merged) throw new DomainError(405, 'Pull Request is not mergeable: it has already been merged');
  if (pull.state !== 'open') throw new DomainError(405, 'Pull Request is not mergeable: it is closed');
  const headOid = repo.refs.get(`refs/heads/${pull.headRef}`);
  const baseOid = repo.refs.get(`refs/heads/${pull.baseRef}`);
  if (headOid === undefined || baseOid === undefined) throw new DomainError(405, 'Pull Request is not mergeable: a ref is missing');
  const identity = `${store.identity()} ${store.tick()} +0000`;
  let mergeOid: string;

  if (isAncestor(repo, headOid, baseOid)) {
    // Nothing to merge: the head is already contained in the base.
    mergeOid = baseOid;
  } else if (method === 'rebase') {
    mergeOid = rebaseOnto(repo, baseOid, headOid, identity);
    if (mergeOid === '') throw new DomainError(405, 'Pull Request is not mergeable: rebase conflict');
  } else {
    const baseOfMerge = mergeBase(repo, baseOid, headOid);
    const merged = mergeCommits(repo, baseOfMerge, baseOid, headOid);
    if ('conflict' in merged) {
      throw new DomainError(405, `Pull Request is not mergeable: merge conflict in ${merged.conflict || 'the tree'}`);
    }
    if (method === 'squash') {
      const message = `${pull.title}\n\n(squash merge of pull request #${pull.number})`;
      mergeOid = putObject(repo, 'commit', buildCommit({ tree: merged.tree, parents: [baseOid], author: identity, committer: identity, message }));
    } else if (isAncestor(repo, baseOid, headOid)) {
      // A fast-forward presented as a merge method still lands the head commit itself.
      mergeOid = headOid;
    } else {
      const message = `Merge pull request #${pull.number} from ${pull.headRef}\n\n${pull.title}`;
      mergeOid = putObject(repo, 'commit', buildCommit({ tree: merged.tree, parents: [baseOid, headOid], author: identity, committer: identity, message }));
    }
  }

  repo.refs.set(`refs/heads/${pull.baseRef}`, mergeOid);
  const at = store.tick();
  pull.state = 'closed';
  pull.merged = true;
  pull.mergeCommitSha = mergeOid;
  pull.mergedAtSec = at;
  pull.updatedSec = at;
  return { sha: mergeOid };
}

/** Replay the head-only commits of a pull request onto the base tip; '' on conflict. */
function rebaseOnto(repo: Repo, baseOid: string, headOid: string, identity: string): string {
  const baseOfMerge = mergeBase(repo, baseOid, headOid);
  if (baseOfMerge === null) return '';
  // Head-only commits, oldest first.
  const chain: string[] = [];
  const seen = new Set<string>();
  let cursor: string | null = headOid;
  while (cursor !== null && cursor !== baseOfMerge) {
    if (seen.has(cursor)) return '';
    seen.add(cursor);
    chain.push(cursor);
    const object = repo.objects.get(cursor);
    if (object === undefined || object.type !== 'commit') return '';
    const parents = parseCommit(object.data).parents;
    cursor = parents[0] ?? null;
  }
  chain.reverse();
  let newBase = baseOid;
  let newBaseTreeBase: string | null = baseOfMerge;
  for (const oid of chain) {
    const view = parseCommit((repo.objects.get(oid) as { data: Buffer }).data);
    const merged = mergeCommits(repo, newBaseTreeBase, newBase, oid);
    if ('conflict' in merged) return '';
    newBase = putObject(repo, 'commit', buildCommit({ tree: merged.tree, parents: [newBase], author: view.author, committer: identity, message: view.message }));
    newBaseTreeBase = oid;
  }
  return newBase;
}
