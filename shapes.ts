// The JSON shapes every surface agrees on: a repository, issue, pull request, comment, label or
// branch serialized once here, so REST, GraphQL cross-checks and MCP parity all see the same
// objects with the same stable ids.

import { parseCommit, treeOfCommit } from './git.ts';
import { globalIdOf, type CommentRec, type IssueRec, type PullRec, type Repo, type Store, type ThreadRec } from './store.ts';

export type Json = Record<string, unknown>;

export function userJson(store: Store, login: string): Json {
  const user = store.users.find((candidate) => candidate.login === login) ?? store.viewer;
  return {
    login: user.login,
    id: user.id,
    node_id: globalIdOf({ kind: 'user', userId: user.id }),
    type: 'User',
    site_admin: false,
  };
}

export function repoJson(store: Store, repo: Repo): Json {
  return {
    id: repo.databaseId,
    node_id: globalIdOf({ kind: 'repository', databaseId: repo.databaseId }),
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    private: repo.isPrivate,
    owner: userJson(store, repo.owner),
    description: repo.description,
    fork: false,
    html_url: `https://forge.invalid/${repo.owner}/${repo.name}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}`,
    git_url: `git://forge.invalid/${repo.owner}/${repo.name}.git`,
    ssh_url: `git@forge.invalid:${repo.owner}/${repo.name}.git`,
    clone_url: `https://forge.invalid/${repo.owner}/${repo.name}.git`,
    default_branch: repo.defaultBranch,
    has_issues: true,
    has_projects: true,
    has_wiki: true,
    has_pages: false,
    has_downloads: true,
    archived: false,
    disabled: false,
    visibility: repo.isPrivate ? 'private' : 'public',
    created_at: store.isoOf(repo.createdSec),
    updated_at: store.isoOf(repo.createdSec),
    pushed_at: store.isoOf(repo.createdSec),
    size: 0,
    stargazers_count: 0,
    watchers_count: 0,
    forks_count: 0,
    open_issues_count: [...repo.threads.values()].filter((thread) => thread.kind === 'issue' && thread.state === 'open').length,
    language: null,
    license: null,
    topics: [],
  };
}

export function labelJson(repo: Repo, name: string): Json {
  const label = repo.labels.get(name);
  return {
    id: labelId(repo, name),
    node_id: `LA_${repo.databaseId}_${labelId(repo, name)}`,
    name,
    color: label?.color ?? 'ededed',
    description: label?.description ?? null,
    default: false,
  };
}

/** A stable per-repo numeric id for a label name (derived, so it never shifts between reads). */
export function labelId(repo: Repo, name: string): number {
  let hash = repo.databaseId * 131;
  for (let i = 0; i < name.length; i += 1) hash = (hash * 31 + name.charCodeAt(i)) % 1_000_000_007;
  return hash + 1;
}

export function commentJson(store: Store, repo: Repo, thread: ThreadRec, comment: CommentRec): Json {
  return {
    id: comment.id,
    node_id: globalIdOf({ kind: 'comment', databaseId: repo.databaseId, number: thread.number, commentId: comment.id }),
    body: comment.body,
    user: userJson(store, comment.authorLogin),
    created_at: store.isoOf(comment.atSec),
    updated_at: store.isoOf(comment.atSec),
    author_association: 'OWNER',
  };
}

export function issueJson(store: Store, repo: Repo, issue: IssueRec): Json {
  return {
    id: issue.id,
    node_id: globalIdOf({ kind: 'issue', databaseId: repo.databaseId, number: issue.number }),
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    locked: false,
    labels: issue.labels.map((name) => labelJson(repo, name)),
    user: userJson(store, issue.authorLogin),
    assignee: null,
    assignees: [],
    comments: issue.comments.length,
    created_at: store.isoOf(issue.atSec),
    updated_at: store.isoOf(issue.updatedSec),
    closed_at: issue.state === 'closed' ? store.isoOf(issue.updatedSec) : null,
    author_association: 'OWNER',
  };
}

export interface PullSides {
  headSha: string | null;
  baseSha: string | null;
}

/** The current head/base tips of a pull request, read from the refs it names. */
export function pullSides(repo: Repo, pull: PullRec): PullSides {
  return {
    headSha: repo.refs.get(`refs/heads/${pull.headRef}`) ?? null,
    baseSha: repo.refs.get(`refs/heads/${pull.baseRef}`) ?? null,
  };
}

export function pullJson(store: Store, repo: Repo, pull: PullRec, mergeable: boolean | null): Json {
  const sides = pullSides(repo, pull);
  const side = (ref: string, sha: string | null): Json => ({
    label: `${repo.owner}:${ref}`,
    ref,
    sha,
    user: userJson(store, repo.owner),
    repo: repoJson(store, repo),
  });
  return {
    id: pull.id,
    node_id: globalIdOf({ kind: 'pull', databaseId: repo.databaseId, number: pull.number }),
    number: pull.number,
    state: pull.state,
    locked: false,
    title: pull.title,
    body: pull.body,
    user: userJson(store, pull.authorLogin),
    labels: [],
    head: side(pull.headRef, sides.headSha),
    base: side(pull.baseRef, sides.baseSha),
    merged: pull.merged,
    merged_at: pull.mergedAtSec === null ? null : store.isoOf(pull.mergedAtSec),
    merge_commit_sha: pull.mergeCommitSha,
    mergeable,
    mergeable_state: mergeable === null ? 'unknown' : mergeable ? 'clean' : 'dirty',
    comments: pull.comments.length,
    commits: 0,
    additions: 0,
    deletions: 0,
    changed_files: 0,
    draft: false,
    created_at: store.isoOf(pull.atSec),
    updated_at: store.isoOf(pull.updatedSec),
    closed_at: pull.state === 'closed' ? store.isoOf(pull.updatedSec) : null,
    author_association: 'OWNER',
  };
}

export function branchJson(repo: Repo, name: string): Json {
  const sha = repo.refs.get(`refs/heads/${name}`) ?? null;
  return {
    name,
    commit: { sha, url: `/api/v3/repos/${repo.owner}/${repo.name}/commits/${sha}` },
    protected: false,
  };
}

export function commitJson(store: Store, repo: Repo, oid: string): Json | null {
  const object = repo.objects.get(oid);
  if (object === undefined || object.type !== 'commit') return null;
  const view = parseCommit(object.data);
  const person = (line: string): Json => {
    const match = /^(.*)<([^>]*)>\s+(\d+)\s+([+-]\d{4})$/.exec(line.trim());
    if (match === null) return { name: line, email: '', date: store.isoOf(store.nowSec()) };
    return {
      name: (match[1] as string).trim(),
      email: match[2] as string,
      date: new Date(Number(match[3]) * 1000).toISOString().replace('.000Z', 'Z'),
    };
  };
  return {
    sha: oid,
    node_id: `C_${oid}`,
    commit: {
      author: person(view.author),
      committer: person(view.committer),
      message: view.message,
      tree: { sha: view.tree, url: '' },
      comment_count: 0,
    },
    author: null,
    committer: null,
    parents: view.parents.map((parent) => ({ sha: parent, url: '' })),
  };
}

export function treeShaOf(repo: Repo, commitOid: string): string | null {
  return treeOfCommit(repo, commitOid);
}
