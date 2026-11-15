// The git object model and smart-HTTP protocol, implemented in-process on Node's stdlib:
// SHA-1 object ids, loose-object semantics over the in-memory store, packfile reading (with
// OFS/REF delta resolution and thin-pack fix-up) and writing (full objects), pkt-line framing,
// protocol v2 (ls-refs + fetch with shallow/deepen) and v0 (advertisement, fetch, receive-pack
// with report-status over side-band-64k). No git binary is ever invoked.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';
import { hashObject, ZERO_OID, type GitObject, type GitObjectType, type Repo } from './store.ts';

// ---------------------------------------------------------------------------------------------
// pkt-lines

export function pktLine(payload: string | Buffer): Buffer {
  const body = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const header = Buffer.from((body.length + 4).toString(16).padStart(4, '0'), 'ascii');
  return Buffer.concat([header, body]);
}

export const PKT_FLUSH = Buffer.from('0000', 'ascii');
export const PKT_DELIM = Buffer.from('0001', 'ascii');

export interface Pkt {
  kind: 'data' | 'flush' | 'delim' | 'end';
  payload: Buffer;
}

/** Parse a whole pkt-line stream; throws on malformed framing (callers turn that into a 400). */
export function parsePkts(buffer: Buffer, offset = 0): { packets: Pkt[]; rest: Buffer } {
  const packets: Pkt[] = [];
  let at = offset;
  while (at < buffer.length) {
    if (at + 4 > buffer.length) throw new Error('pkt-line header is truncated');
    const header = buffer.subarray(at, at + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(header)) throw new Error(`pkt-line header is not hex: ${JSON.stringify(header)}`);
    const length = parseInt(header, 16);
    if (length === 0) {
      packets.push({ kind: 'flush', payload: Buffer.alloc(0) });
      at += 4;
      // A flush ends the pkt-line portion of a receive-pack body; the pack follows raw.
      return { packets, rest: buffer.subarray(at) };
    }
    if (length === 1) {
      packets.push({ kind: 'delim', payload: Buffer.alloc(0) });
      at += 4;
      continue;
    }
    if (length === 2) {
      packets.push({ kind: 'end', payload: Buffer.alloc(0) });
      at += 4;
      continue;
    }
    if (length < 4 || at + length > buffer.length) throw new Error('pkt-line length is out of range');
    packets.push({ kind: 'data', payload: buffer.subarray(at + 4, at + length) });
    at += length;
  }
  return { packets, rest: buffer.subarray(at) };
}

/** Parse a pkt-line stream that must consume the whole buffer (upload-pack requests). */
export function parseAllPkts(buffer: Buffer): Pkt[] {
  const packets: Pkt[] = [];
  let at = 0;
  while (at < buffer.length) {
    if (at + 4 > buffer.length) throw new Error('pkt-line header is truncated');
    const header = buffer.subarray(at, at + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(header)) throw new Error(`pkt-line header is not hex: ${JSON.stringify(header)}`);
    const length = parseInt(header, 16);
    if (length === 0) {
      packets.push({ kind: 'flush', payload: Buffer.alloc(0) });
      at += 4;
      continue;
    }
    if (length === 1) {
      packets.push({ kind: 'delim', payload: Buffer.alloc(0) });
      at += 4;
      continue;
    }
    if (length === 2) {
      packets.push({ kind: 'end', payload: Buffer.alloc(0) });
      at += 4;
      continue;
    }
    if (length < 4 || at + length > buffer.length) throw new Error('pkt-line length is out of range');
    packets.push({ kind: 'data', payload: buffer.subarray(at + 4, at + length) });
    at += length;
  }
  return packets;
}

// ---------------------------------------------------------------------------------------------
// Trees, commits, tags

export interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
}

export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let at = 0;
  while (at < data.length) {
    const space = data.indexOf(0x20, at);
    if (space < 0) throw new Error('tree entry has no mode terminator');
    const mode = data.subarray(at, space).toString('ascii');
    const nul = data.indexOf(0x00, space + 1);
    if (nul < 0 || nul + 21 > data.length) throw new Error('tree entry is truncated');
    const name = data.subarray(space + 1, nul).toString('utf8');
    const oid = data.subarray(nul + 1, nul + 21).toString('hex');
    entries.push({ mode, name, oid });
    at = nul + 21;
  }
  return entries;
}

/** Git's tree order: by name, with directory names compared as if they ended in '/'. */
function treeSortKey(entry: TreeEntry): string {
  return entry.mode === '40000' ? `${entry.name}/` : entry.name;
}

export function buildTree(entries: readonly TreeEntry[]): Buffer {
  const sorted = [...entries].sort((a, b) => {
    const ka = treeSortKey(a);
    const kb = treeSortKey(b);
    return ka < kb ? -1 : ka > kb ? 1 : 0;
  });
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'));
    parts.push(Buffer.from(entry.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

export interface CommitView {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  message: string;
}

export function parseCommit(data: Buffer): CommitView {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const header = split < 0 ? text : text.slice(0, split);
  const message = split < 0 ? '' : text.slice(split + 2);
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  for (const line of header.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) author = line.slice(7);
    else if (line.startsWith('committer ')) committer = line.slice(10);
  }
  return { tree, parents, author, committer, message };
}

export function buildCommit(fields: {
  tree: string;
  parents: readonly string[];
  author: string;
  committer: string;
  message: string;
}): Buffer {
  let head = `tree ${fields.tree}\n`;
  for (const parent of fields.parents) head += `parent ${parent}\n`;
  head += `author ${fields.author}\ncommitter ${fields.committer}\n\n`;
  return Buffer.from(head + fields.message, 'utf8');
}

export interface TagView {
  object: string;
  type: string;
  tag: string;
  message: string;
}

export function parseTag(data: Buffer): TagView {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const header = split < 0 ? text : text.slice(0, split);
  const message = split < 0 ? '' : text.slice(split + 2);
  let object = '';
  let type = '';
  let tag = '';
  for (const line of header.split('\n')) {
    if (line.startsWith('object ')) object = line.slice(7).trim();
    else if (line.startsWith('type ')) type = line.slice(5).trim();
    else if (line.startsWith('tag ')) tag = line.slice(4).trim();
  }
  return { object, type, tag, message };
}

/** Store an object in the repo and answer its id. */
export function putObject(repo: Repo, type: GitObjectType, data: Buffer): string {
  const oid = hashObject(type, data);
  if (!repo.objects.has(oid)) repo.objects.set(oid, { type, data });
  return oid;
}

// ---------------------------------------------------------------------------------------------
// Packfiles

const TYPE_IDS: Record<GitObjectType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const ID_TYPES: Record<number, GitObjectType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

function packObjectHeader(type: GitObjectType, size: number): Buffer {
  const bytes: number[] = [];
  let first = (TYPE_IDS[type] << 4) | (size & 0x0f);
  size = Math.floor(size / 16);
  if (size > 0) first |= 0x80;
  bytes.push(first);
  while (size > 0) {
    let byte = size & 0x7f;
    size = Math.floor(size / 128);
    if (size > 0) byte |= 0x80;
    bytes.push(byte);
  }
  return Buffer.from(bytes);
}

/** A version-2 pack of full (never delta-compressed) objects, with a valid SHA-1 trailer. */
export function writePack(objects: readonly GitObject[]): Buffer {
  const parts: Buffer[] = [];
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objects.length, 8);
  parts.push(header);
  for (const object of objects) {
    parts.push(packObjectHeader(object.type, object.data.length));
    parts.push(deflateSync(object.data));
  }
  const body = Buffer.concat(parts);
  const trailer = createHash('sha1').update(body).digest();
  return Buffer.concat([body, trailer]);
}

function inflateAt(buffer: Buffer, offset: number): { data: Buffer; next: number } {
  const view = buffer.subarray(offset);
  const result = inflateSync(view, { info: true }) as unknown as { buffer: Buffer; engine: { bytesWritten: number } };
  const written = result.engine.bytesWritten;
  if (!written || written <= 0) throw new Error(`zlib stream at offset ${offset} consumed no bytes`);
  return { data: result.buffer, next: offset + written };
}

function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let offset = 0;
  const readSize = (): number => {
    let value = 0;
    let shift = 0;
    for (;;) {
      if (offset >= delta.length) throw new Error('delta size varint is truncated');
      const byte = delta[offset] as number;
      offset += 1;
      value += (byte & 0x7f) * 2 ** shift;
      shift += 7;
      if (!(byte & 0x80)) break;
    }
    return value;
  };
  const sourceSize = readSize();
  const targetSize = readSize();
  if (sourceSize !== base.length) throw new Error('delta base size mismatch');
  const out = Buffer.alloc(targetSize);
  let written = 0;
  while (offset < delta.length) {
    const op = delta[offset] as number;
    offset += 1;
    if (op & 0x80) {
      let copyOffset = 0;
      let copySize = 0;
      let shift = 0;
      for (let bit = 0; bit < 4; bit += 1) {
        if (op & (1 << bit)) {
          copyOffset += (delta[offset] as number) * 2 ** shift;
          offset += 1;
        }
        shift += 8;
      }
      shift = 0;
      for (let bit = 4; bit < 7; bit += 1) {
        if (op & (1 << bit)) {
          copySize += (delta[offset] as number) * 2 ** shift;
          offset += 1;
        }
        shift += 8;
      }
      if (copySize === 0) copySize = 0x10000;
      base.copy(out, written, copyOffset, copyOffset + copySize);
      written += copySize;
    } else if (op !== 0) {
      const literal = op & 0x7f;
      delta.copy(out, written, offset, offset + literal);
      offset += literal;
      written += literal;
    } else {
      throw new Error('delta opcode 0 is invalid');
    }
  }
  if (written !== targetSize) throw new Error('delta produced the wrong size');
  return out;
}

interface PackEntry {
  type: GitObjectType;
  data: Buffer;
  /** For delta entries: the base's pack offset (OFS) or oid (REF). */
  baseOffset?: number;
  baseOid?: string;
  isDelta: boolean;
  offset: number;
}

/**
 * Read a packfile and resolve every object. `resolveExternal` supplies base objects a thin pack
 * leaves out (objects the receiving repository already holds). Throws on any corruption.
 */
export function readPack(pack: Buffer, resolveExternal?: (oid: string) => GitObject | undefined): Map<string, GitObject> {
  if (pack.length < 32) throw new Error('pack is too short');
  if (pack.subarray(0, 4).toString('ascii') !== 'PACK') throw new Error('pack does not start with PACK');
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`pack version ${version} is not supported`);
  const count = pack.readUInt32BE(8);
  const trailer = pack.subarray(pack.length - 20);
  const checksum = createHash('sha1').update(pack.subarray(0, pack.length - 20)).digest();
  if (!checksum.equals(trailer)) throw new Error('pack checksum mismatch');
  const entries = new Map<number, PackEntry>();
  const order: number[] = [];
  let at = 12;
  for (let index = 0; index < count; index += 1) {
    const start = at;
    let byte = pack[at] as number;
    at += 1;
    const typeId = (byte >> 4) & 0x07;
    let size = byte & 0x0f;
    let shift = 4;
    while (byte & 0x80) {
      byte = pack[at] as number;
      at += 1;
      size += (byte & 0x7f) * 2 ** shift;
      shift += 7;
    }
    let baseOffset: number | undefined;
    let baseOid: string | undefined;
    if (typeId === 6) {
      let b = pack[at] as number;
      at += 1;
      let value = b & 0x7f;
      while (b & 0x80) {
        b = pack[at] as number;
        at += 1;
        value = (value + 1) * 128 + (b & 0x7f);
      }
      baseOffset = start - value;
    } else if (typeId === 7) {
      baseOid = pack.subarray(at, at + 20).toString('hex');
      at += 20;
    }
    const inflated = inflateAt(pack, at);
    at = inflated.next;
    if (inflated.data.length !== size) throw new Error('pack object size mismatch');
    const isDelta = typeId === 6 || typeId === 7;
    entries.set(start, {
      type: isDelta ? 'blob' : (ID_TYPES[typeId] as GitObjectType),
      data: inflated.data,
      baseOffset,
      baseOid,
      isDelta,
      offset: start,
    });
    order.push(start);
  }
  const resolved = new Map<string, GitObject>();
  const byOffset = new Map<number, string>();
  const record = (start: number, object: GitObject): void => {
    const oid = hashObject(object.type, object.data);
    resolved.set(oid, object);
    byOffset.set(start, oid);
  };
  const resolveOffset = (start: number, depth: number): GitObject => {
    if (depth > 128) throw new Error('delta chain is too deep');
    const known = byOffset.get(start);
    if (known !== undefined) return resolved.get(known) as GitObject;
    const entry = entries.get(start);
    if (entry === undefined) throw new Error('delta base offset is not in the pack');
    if (!entry.isDelta) {
      const object: GitObject = { type: entry.type, data: entry.data };
      record(start, object);
      return object;
    }
    if (entry.baseOffset !== undefined) {
      const base = resolveOffset(entry.baseOffset, depth + 1);
      const object: GitObject = { type: base.type, data: applyDelta(base.data, entry.data) };
      record(start, object);
      return object;
    }
    // A REF_DELTA: its base is named by oid — a plain or OFS-resolved object in this pack,
    // another REF_DELTA resolved earlier, or an object the caller already holds (a thin pack).
    const baseOid = entry.baseOid as string;
    let base = resolved.get(baseOid);
    if (base === undefined) base = resolveExternal?.(baseOid);
    if (base === undefined) throw new Error(`ref-delta base ${baseOid} is not available`);
    const object: GitObject = { type: base.type, data: applyDelta(base.data, entry.data) };
    record(start, object);
    return object;
  };
  // Plain objects first, then OFS deltas (their bases resolve by offset), then REF deltas in
  // passes, so a REF base that is itself a delta in this pack is resolved before its dependents.
  for (const start of order) {
    const entry = entries.get(start) as PackEntry;
    if (!entry.isDelta) resolveOffset(start, 0);
  }
  for (const start of order) {
    const entry = entries.get(start) as PackEntry;
    if (entry.isDelta && entry.baseOffset !== undefined) resolveOffset(start, 0);
  }
  let pending = order.filter((start) => (entries.get(start) as PackEntry).isDelta && (entries.get(start) as PackEntry).baseOid !== undefined && !byOffset.has(start));
  while (pending.length > 0) {
    const before = pending.length;
    for (const start of pending) {
      const entry = entries.get(start) as PackEntry;
      if (resolved.has(entry.baseOid as string) || resolveExternal?.(entry.baseOid as string) !== undefined) resolveOffset(start, 0);
    }
    pending = pending.filter((start) => !byOffset.has(start));
    if (pending.length === before) {
      const entry = entries.get(pending[0] as number) as PackEntry;
      resolveOffset(pending[0] as number, 0); // throws with the reason
      throw new Error(`ref-delta base ${entry.baseOid} is not available`);
    }
  }
  return resolved;
}

// ---------------------------------------------------------------------------------------------
// Graph walks over a repository's objects

/** Every object reachable from `tips` (commits, their trees and blobs, tags), as an oid set. */
export function closureOf(repo: Repo, tips: readonly string[], boundary?: ReadonlySet<string>): Set<string> {
  const seen = new Set<string>();
  const stack = [...tips];
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (seen.has(oid)) continue;
    const object = repo.objects.get(oid);
    if (object === undefined) continue;
    seen.add(oid);
    if (object.type === 'commit') {
      const view = parseCommit(object.data);
      stack.push(view.tree);
      // A boundary commit is held shallowly: its tree counts as held, its parents do not.
      if (boundary === undefined || !boundary.has(oid)) {
        for (const parent of view.parents) stack.push(parent);
      }
    } else if (object.type === 'tree') {
      for (const entry of parseTree(object.data)) stack.push(entry.oid);
    } else if (object.type === 'tag') {
      stack.push(parseTag(object.data).object);
    }
  }
  return seen;
}

/** The commit a (possibly annotated-tag) oid points at, or null. */
export function peelToCommit(repo: Repo, oid: string): string | null {
  let current = oid;
  for (let hops = 0; hops < 16; hops += 1) {
    const object = repo.objects.get(current);
    if (object === undefined) return null;
    if (object.type === 'commit') return current;
    if (object.type === 'tag') {
      current = parseTag(object.data).object;
      continue;
    }
    return null;
  }
  return null;
}

/** Whether `ancestor` is reachable from `descendant` through commit parents (or equal). */
export function isAncestor(repo: Repo, ancestor: string, descendant: string): boolean {
  const seen = new Set<string>();
  const stack = [descendant];
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (oid === ancestor) return true;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const object = repo.objects.get(oid);
    if (object === undefined || object.type !== 'commit') continue;
    for (const parent of parseCommit(object.data).parents) stack.push(parent);
  }
  return false;
}

/** Commit ancestors of `oid` with their shortest distance (edges) from it. */
function ancestorDistances(repo: Repo, oid: string): Map<string, number> {
  const distances = new Map<string, number>();
  let frontier = [oid];
  let distance = 0;
  while (frontier.length > 0) {
    const next: string[] = [];
    for (const current of frontier) {
      if (distances.has(current)) continue;
      distances.set(current, distance);
      const object = repo.objects.get(current);
      if (object !== undefined && object.type === 'commit') {
        for (const parent of parseCommit(object.data).parents) next.push(parent);
      }
    }
    frontier = next;
    distance += 1;
  }
  return distances;
}

/** The best common ancestor of two commits, or null when they share no history. */
export function mergeBase(repo: Repo, a: string, b: string): string | null {
  if (a === b) return a;
  const fromA = ancestorDistances(repo, a);
  const fromB = ancestorDistances(repo, b);
  let best: string | null = null;
  let bestScore = Number.POSITIVE_INFINITY;
  for (const [oid, da] of fromA) {
    const db = fromB.get(oid);
    if (db === undefined) continue;
    const score = da + db;
    if (score < bestScore) {
      bestScore = score;
      best = oid;
    }
  }
  return best;
}

/** Commits reachable from `tip`, newest first (breadth by generation), capped. */
export function historyOf(repo: Repo, tip: string, cap = 1000): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  let frontier = [tip];
  while (frontier.length > 0 && out.length < cap) {
    const next: string[] = [];
    for (const oid of frontier) {
      if (seen.has(oid)) continue;
      seen.add(oid);
      out.push(oid);
      const object = repo.objects.get(oid);
      if (object !== undefined && object.type === 'commit') {
        for (const parent of parseCommit(object.data).parents) next.push(parent);
      }
    }
    frontier = next;
  }
  return out;
}

// ---------------------------------------------------------------------------------------------
// Trees as file maps, diffs and three-way merges (the REST/GraphQL/MCP pull-request machinery)

export interface FileEntry {
  oid: string;
  mode: string;
}

/** Flatten a tree (by oid) into path -> blob entry. */
export function flattenTree(repo: Repo, treeOid: string, prefix = '', out = new Map<string, FileEntry>()): Map<string, FileEntry> {
  const object = repo.objects.get(treeOid);
  if (object === undefined || object.type !== 'tree') return out;
  for (const entry of parseTree(object.data)) {
    const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
    if (entry.mode === '40000') flattenTree(repo, entry.oid, path, out);
    else out.set(path, { oid: entry.oid, mode: entry.mode });
  }
  return out;
}

export function treeOfCommit(repo: Repo, commitOid: string): string | null {
  const object = repo.objects.get(commitOid);
  if (object === undefined || object.type !== 'commit') return null;
  return parseCommit(object.data).tree;
}

/** The flattened file map of a commit. */
export function filesOfCommit(repo: Repo, commitOid: string): Map<string, FileEntry> {
  const tree = treeOfCommit(repo, commitOid);
  if (tree === null) return new Map();
  return flattenTree(repo, tree);
}

export interface FileChange {
  path: string;
  status: 'added' | 'modified' | 'removed';
  before: FileEntry | null;
  after: FileEntry | null;
}

/** The file-level diff between two commits' trees. */
export function diffCommits(repo: Repo, beforeOid: string | null, afterOid: string | null): FileChange[] {
  const before = beforeOid === null ? new Map<string, FileEntry>() : filesOfCommit(repo, beforeOid);
  const after = afterOid === null ? new Map<string, FileEntry>() : filesOfCommit(repo, afterOid);
  const changes: FileChange[] = [];
  const paths = new Set([...before.keys(), ...after.keys()]);
  for (const path of [...paths].sort()) {
    const b = before.get(path) ?? null;
    const a = after.get(path) ?? null;
    if (b !== null && a !== null && b.oid === a.oid && b.mode === a.mode) continue;
    changes.push({ path, status: b === null ? 'added' : a === null ? 'removed' : 'modified', before: b, after: a });
  }
  return changes;
}

/** Set (or, with blobOid null, remove) a path in a tree, rebuilding the tree chain; returns the new tree oid. */
export function treeWithPath(repo: Repo, treeOid: string | null, segments: readonly string[], blob: { oid: string; mode: string } | null): string {
  const entries: TreeEntry[] = treeOid === null ? [] : parseTree((repo.objects.get(treeOid) as GitObject).data);
  const head = segments[0] as string;
  const rest = segments.slice(1);
  const kept = entries.filter((entry) => entry.name !== head);
  if (rest.length === 0) {
    if (blob !== null) kept.push({ mode: blob.mode, name: head, oid: blob.oid });
  } else {
    const existing = entries.find((entry) => entry.name === head && entry.mode === '40000');
    const childOid = treeWithPath(repo, existing?.oid ?? null, rest, blob);
    // An emptied subtree disappears, as in git.
    const child = repo.objects.get(childOid);
    if (child !== undefined && parseTree(child.data).length > 0) {
      kept.push({ mode: '40000', name: head, oid: childOid });
    }
  }
  return putObject(repo, 'tree', buildTree(kept));
}

/**
 * Three-way merge of two commits against their merge base, at whole-file granularity with
 * subtree recursion. Answers the merged tree oid, or the first conflicting path.
 */
export function mergeCommits(repo: Repo, baseOid: string | null, oursOid: string, theirsOid: string): { tree: string } | { conflict: string } {
  const baseTree = baseOid === null ? null : treeOfCommit(repo, baseOid);
  const oursTree = treeOfCommit(repo, oursOid);
  const theirsTree = treeOfCommit(repo, theirsOid);
  if (oursTree === null || theirsTree === null) return { conflict: '' };
  const merged = mergeTrees(repo, baseTree, oursTree, theirsTree, '');
  return merged;
}

function mergeTrees(repo: Repo, base: string | null, ours: string, theirs: string, prefix: string): { tree: string } | { conflict: string } {
  if (ours === theirs) return { tree: ours };
  if (base !== null && base === ours) return { tree: theirs };
  if (base !== null && base === theirs) return { tree: ours };
  const baseEntries = new Map((base === null ? [] : parseTree((repo.objects.get(base) as GitObject).data)).map((e) => [e.name, e]));
  const oursEntries = new Map(parseTree((repo.objects.get(ours) as GitObject).data).map((e) => [e.name, e]));
  const theirsEntries = new Map(parseTree((repo.objects.get(theirs) as GitObject).data).map((e) => [e.name, e]));
  const names = new Set([...baseEntries.keys(), ...oursEntries.keys(), ...theirsEntries.keys()]);
  const out: TreeEntry[] = [];
  for (const name of names) {
    const b = baseEntries.get(name);
    const o = oursEntries.get(name);
    const t = theirsEntries.get(name);
    const same = (x?: TreeEntry, y?: TreeEntry): boolean => x === undefined && y === undefined ? true : x !== undefined && y !== undefined && x.oid === y.oid && x.mode === y.mode;
    if (same(o, t)) {
      if (o !== undefined) out.push(o);
      continue;
    }
    if (same(b, o)) {
      if (t !== undefined) out.push(t);
      continue;
    }
    if (same(b, t)) {
      if (o !== undefined) out.push(o);
      continue;
    }
    if (o !== undefined && t !== undefined && o.mode === '40000' && t.mode === '40000') {
      const sub = mergeTrees(repo, b?.mode === '40000' ? b.oid : null, o.oid, t.oid, `${prefix}${name}/`);
      if ('conflict' in sub) return sub;
      out.push({ mode: '40000', name, oid: sub.tree });
      continue;
    }
    return { conflict: `${prefix}${name}` };
  }
  return { tree: putObject(repo, 'tree', buildTree(out)) };
}

// ---------------------------------------------------------------------------------------------
// Refs

export interface RefListing {
  name: string;
  oid: string;
  /** For HEAD: the ref it points at. */
  symrefTarget?: string;
  /** For annotated tags: the oid the tag object points at. */
  peeled?: string;
}

/** Every ref the forge advertises for a repo: HEAD first, then refs in name order. */
export function listRefs(repo: Repo): RefListing[] {
  const out: RefListing[] = [];
  const headRef = `refs/heads/${repo.defaultBranch}`;
  const headOid = repo.refs.get(headRef);
  out.push({ name: 'HEAD', oid: headOid ?? ZERO_OID, symrefTarget: headRef });
  for (const name of [...repo.refs.keys()].sort()) {
    const oid = repo.refs.get(name) as string;
    const entry: RefListing = { name, oid };
    const object = repo.objects.get(oid);
    if (object !== undefined && object.type === 'tag') entry.peeled = parseTag(object.data).object;
    out.push(entry);
  }
  return out;
}

// ---------------------------------------------------------------------------------------------
// Fetch set computation

export interface FetchPlan {
  /** Objects to pack, in a deterministic order. */
  objects: GitObject[];
  shallow: string[];
  unshallow: string[];
  acks: string[];
  nak: boolean;
}

/** Every tree and blob under a tree oid (the tree itself included). */
export function treeClosureOf(repo: Repo, treeOid: string): Set<string> {
  const seen = new Set<string>();
  const stack = [treeOid];
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (seen.has(oid)) continue;
    const object = repo.objects.get(oid);
    if (object === undefined) continue;
    seen.add(oid);
    if (object.type === 'tree') {
      for (const entry of parseTree(object.data)) stack.push(entry.oid);
    }
  }
  return seen;
}

export function planFetch(
  repo: Repo,
  wants: readonly string[],
  haves: readonly string[],
  clientShallow: readonly string[],
  depth: number | null,
): FetchPlan {
  const shallowSet = new Set(clientShallow);
  // In a depth fetch the client's declared boundary caps what it can hold: the have closure
  // stops at shallow commits. A plain fetch keeps the usual invariant (a have's whole
  // history is held), so the full closure applies there.
  const haveSet = closureOf(
    repo,
    haves.filter((oid) => repo.objects.has(oid)),
    depth !== null ? shallowSet : undefined,
  );
  const haveCommits = new Set([...haveSet].filter((oid) => repo.objects.get(oid)?.type === 'commit'));
  const send = new Set<string>();
  const acks: string[] = [];
  for (const have of haves) if (repo.objects.has(have)) acks.push(have);

  // Tag wants contribute their tag objects; the commit beneath is what depth counts from.
  const wantCommits: string[] = [];
  for (const want of wants) {
    let current = want;
    for (let hops = 0; hops < 16; hops += 1) {
      const object = repo.objects.get(current);
      if (object === undefined || object.type !== 'tag') break;
      if (!haveSet.has(current)) send.add(current);
      current = parseTag(object.data).object;
    }
    const commit = peelToCommit(repo, want);
    if (commit !== null) wantCommits.push(commit);
  }

  const includedCommits = new Set<string>();
  if (depth === null) {
    for (const oid of closureOf(repo, wants)) {
      if (!haveSet.has(oid)) send.add(oid);
    }
    for (const oid of send) {
      if (repo.objects.get(oid)?.type === 'commit') includedCommits.add(oid);
    }
  } else {
    // Breadth-first by distance below each want: a commit joins the pack when its distance is
    // below `depth` (the suite's deepen is an absolute target depth — deepen 2 on a depth-1
    // clone adds exactly one generation). The walk passes through commits the client already
    // holds, shallow ones included, but never delivers them again.
    const distance = new Map<string, number>();
    const queue: Array<{ oid: string; d: number }> = [];
    for (const commit of wantCommits) queue.push({ oid: commit, d: 0 });
    while (queue.length > 0) {
      const { oid, d } = queue.shift() as { oid: string; d: number };
      if (d >= depth) continue;
      if ((distance.get(oid) ?? Number.POSITIVE_INFINITY) <= d) continue;
      distance.set(oid, d);
      const object = repo.objects.get(oid);
      if (object === undefined || object.type !== 'commit') continue;
      for (const parent of parseCommit(object.data).parents) queue.push({ oid: parent, d: d + 1 });
    }
    for (const oid of distance.keys()) {
      if (!haveSet.has(oid)) includedCommits.add(oid);
    }
    for (const commit of includedCommits) {
      send.add(commit);
      const view = parseCommit((repo.objects.get(commit) as GitObject).data);
      for (const oid of treeClosureOf(repo, view.tree)) {
        if (!haveSet.has(oid)) send.add(oid);
      }
    }
  }

  // Shallow bookkeeping: a commit is at the cut when one of its parents was not delivered and is
  // not already fully held. A declared boundary whose parents all arrived becomes unshallow.
  const shallow: string[] = [];
  const unshallow: string[] = [];
  if (depth !== null) {
    // A parent counts as present only when it was delivered now or is held *unshallow*: a
    // parent that is itself a declared boundary leaves its child at the cut too.
    const present = new Set<string>([
      ...includedCommits,
      ...[...haveCommits].filter((oid) => !shallowSet.has(oid)),
    ]);
    const candidates = new Set<string>([...includedCommits, ...shallowSet]);
    for (const oid of candidates) {
      const object = repo.objects.get(oid);
      if (object === undefined || object.type !== 'commit') continue;
      const parents = parseCommit(object.data).parents;
      if (parents.length === 0) continue;
      const cut = parents.some((parent) => !present.has(parent) && repo.objects.has(parent));
      if (cut) shallow.push(oid);
      else if (shallowSet.has(oid)) unshallow.push(oid);
    }
  }

  const objects: GitObject[] = [];
  const byType: Record<GitObjectType, string[]> = { commit: [], tree: [], blob: [], tag: [] };
  for (const oid of send) {
    const object = repo.objects.get(oid);
    if (object !== undefined) byType[object.type].push(oid);
  }
  for (const type of ['commit', 'tree', 'blob', 'tag'] as const) {
    for (const oid of byType[type].sort()) objects.push(repo.objects.get(oid) as GitObject);
  }
  return { objects, shallow: shallow.sort(), unshallow: unshallow.sort(), acks, nak: acks.length === 0 };
}
