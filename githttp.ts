// Git smart HTTP: info/refs advertisements (protocol v2 capabilities for upload-pack when the
// client asks, v0 otherwise and always for receive-pack), the v2 ls-refs and fetch commands,
// the v0 fetch, and receive-pack with report-status. Repositories live under /<owner>/<repo>.git.

import {
  PKT_DELIM,
  PKT_FLUSH,
  isAncestor,
  listRefs,
  parseAllPkts,
  parsePkts,
  pktLine,
  planFetch,
  readPack,
  writePack,
  type Pkt,
} from './git.ts';
import { ZERO_OID, type Repo, type Store } from './store.ts';

export interface HttpResult {
  status: number;
  contentType: string;
  body: Buffer;
  headers?: Record<string, string>;
}

const UPLOAD_CAPS_V0 = 'side-band-64k multi_ack_detailed thin-pack ofs-delta shallow';
const RECEIVE_CAPS = 'report-status delete-refs side-band-64k ofs-delta';
const AGENT = 'github-core-clone/1.0';

function text(status: number, body: string): HttpResult {
  return { status, contentType: 'text/plain; charset=utf-8', body: Buffer.from(body, 'utf8') };
}

function gitResult(service: 'git-upload-pack' | 'git-receive-pack', kind: 'advertisement' | 'result', body: Buffer): HttpResult {
  return {
    status: 200,
    contentType: `application/x-${service}-${kind}`,
    body,
    headers: { 'cache-control': 'no-cache, max-age=0, must-revalidate' },
  };
}

/** Split a sideband payload into pkt-lines of at most 65520 bytes total, band byte included. */
function sidebandPackets(band: number, data: Buffer): Buffer[] {
  const out: Buffer[] = [];
  const CHUNK = 65512;
  if (data.length === 0) return out;
  for (let at = 0; at < data.length; at += CHUNK) {
    const slice = data.subarray(at, Math.min(data.length, at + CHUNK));
    out.push(pktLine(Buffer.concat([Buffer.from([band]), slice])));
  }
  return out;
}

export class GitHttp {
  private readonly store: Store;

  constructor(store: Store) {
    this.store = store;
  }

  private repo(owner: string, name: string): Repo | null {
    return this.store.repoOf(owner, name);
  }

  // -------------------------------------------------------------------------------------------
  // info/refs

  infoRefs(owner: string, name: string, service: string | null, protocolV2: boolean): HttpResult {
    if (service !== 'git-upload-pack' && service !== 'git-receive-pack') {
      return text(400, 'unknown service\n');
    }
    const repo = this.repo(owner, name);
    if (repo === null) return text(404, 'repository not found\n');
    if (service === 'git-upload-pack' && protocolV2) {
      const parts: Buffer[] = [
        pktLine('# service=git-upload-pack\n'),
        PKT_FLUSH,
        pktLine('version 2\n'),
        pktLine(`agent=${AGENT}\n`),
        pktLine('ls-refs=unborn\n'),
        pktLine('fetch=shallow wait-for-done\n'),
        pktLine('object-format=sha1\n'),
        PKT_FLUSH,
      ];
      return gitResult('git-upload-pack', 'advertisement', Buffer.concat(parts));
    }
    const caps = service === 'git-upload-pack' ? `${UPLOAD_CAPS_V0} agent=${AGENT}` : `${RECEIVE_CAPS} agent=${AGENT}`;
    const parts: Buffer[] = [pktLine(`# service=${service}\n`), PKT_FLUSH];
    const refs = listRefs(repo);
    const headRef = `refs/heads/${repo.defaultBranch}`;
    const lines: string[] = [];
    if (service === 'git-upload-pack') {
      const head = refs.find((ref) => ref.name === 'HEAD');
      if (repo.refs.size === 0) {
        lines.push(`${ZERO_OID} capabilities^{}`);
      } else if (head !== undefined) {
        lines.push(`${head.oid} HEAD`);
        for (const ref of refs) {
          if (ref.name === 'HEAD') continue;
          lines.push(`${ref.oid} ${ref.name}`);
          if (ref.peeled !== undefined) lines.push(`${ref.peeled} ${ref.name}^{}`);
        }
      }
      const withCaps = lines.map((line, index) => (index === 0 ? `${line}\0${caps} symref=HEAD:${headRef}` : line));
      for (const line of withCaps) parts.push(pktLine(`${line}\n`));
    } else {
      const real = refs.filter((ref) => ref.name !== 'HEAD');
      if (real.length === 0) {
        parts.push(pktLine(`${ZERO_OID} capabilities^{}\0${caps}\n`));
      } else {
        real.forEach((ref, index) => {
          const suffix = index === 0 ? `\0${caps}` : '';
          parts.push(pktLine(`${ref.oid} ${ref.name}${suffix}\n`));
          if (ref.peeled !== undefined) parts.push(pktLine(`${ref.peeled} ${ref.name}^{}\n`));
        });
      }
    }
    parts.push(PKT_FLUSH);
    return gitResult(service, 'advertisement', Buffer.concat(parts));
  }

  // -------------------------------------------------------------------------------------------
  // upload-pack POST: v2 ls-refs / v2 fetch / v0 fetch

  uploadPack(owner: string, name: string, body: Buffer): HttpResult {
    const repo = this.repo(owner, name);
    if (repo === null) return text(404, 'repository not found\n');
    let packets: Pkt[];
    try {
      packets = parseAllPkts(body);
    } catch {
      return text(400, 'malformed pkt-line request\n');
    }
    const first = packets.find((packet) => packet.kind === 'data');
    const firstLine = first?.payload.toString('utf8') ?? '';
    try {
      if (firstLine.startsWith('command=ls-refs')) return this.lsRefs(repo, packets);
      if (firstLine.startsWith('command=fetch')) return this.fetchV2(repo, packets);
      return this.fetchV0(repo, packets);
    } catch (err) {
      return text(400, `upload-pack failed: ${err instanceof Error ? err.message : String(err)}\n`);
    }
  }

  private lsRefs(repo: Repo, packets: Pkt[]): HttpResult {
    let peel = false;
    let symrefs = false;
    let unborn = false;
    const prefixes: string[] = [];
    let afterDelim = false;
    for (const packet of packets) {
      if (packet.kind === 'delim') {
        afterDelim = true;
        continue;
      }
      if (!afterDelim || packet.kind !== 'data') continue;
      const line = packet.payload.toString('utf8').replace(/\n$/, '');
      if (line === 'peel') peel = true;
      else if (line === 'symrefs') symrefs = true;
      else if (line === 'unborn') unborn = true;
      else if (line.startsWith('ref-prefix ')) prefixes.push(line.slice('ref-prefix '.length));
    }
    const parts: Buffer[] = [];
    for (const ref of listRefs(repo)) {
      if (prefixes.length > 0 && !prefixes.some((prefix) => ref.name.startsWith(prefix))) continue;
      let line: string;
      const attrs: string[] = [];
      if (symrefs && ref.symrefTarget !== undefined) attrs.push(`symref-target:${ref.symrefTarget}`);
      if (peel && ref.peeled !== undefined) attrs.push(`peeled:${ref.peeled}`);
      if (ref.name === 'HEAD' && ref.oid === ZERO_OID && unborn) {
        line = `unborn HEAD${attrs.length > 0 ? ` ${attrs.join(' ')}` : ''}`;
      } else {
        line = `${ref.oid} ${ref.name}${attrs.length > 0 ? ` ${attrs.join(' ')}` : ''}`;
      }
      parts.push(pktLine(`${line}\n`));
    }
    parts.push(PKT_FLUSH);
    return gitResult('git-upload-pack', 'result', Buffer.concat(parts));
  }

  private fetchV2(repo: Repo, packets: Pkt[]): HttpResult {
    const wants: string[] = [];
    const haves: string[] = [];
    const shallow: string[] = [];
    let depth: number | null = null;
    let done = false;
    let afterDelim = false;
    for (const packet of packets) {
      if (packet.kind === 'delim') {
        afterDelim = true;
        continue;
      }
      if (!afterDelim || packet.kind !== 'data') continue;
      const line = packet.payload.toString('utf8').replace(/\n$/, '');
      if (line.startsWith('want ')) wants.push(line.slice(5).trim());
      else if (line.startsWith('have ')) haves.push(line.slice(5).trim());
      else if (line.startsWith('shallow ')) shallow.push(line.slice(8).trim());
      else if (line.startsWith('deepen ')) depth = Number(line.slice(7).trim());
      else if (line === 'done') done = true;
    }
    void done;
    for (const want of wants) {
      if (!repo.objects.has(want)) {
        return gitResult('git-upload-pack', 'result', Buffer.concat([pktLine(`ERR upload-pack: not our ref ${want}\n`), PKT_FLUSH]));
      }
    }
    const plan = planFetch(repo, wants, haves, shallow, depth);
    const parts: Buffer[] = [];
    parts.push(pktLine('acknowledgments\n'));
    if (plan.acks.length === 0) parts.push(pktLine('NAK\n'));
    else for (const ack of plan.acks) parts.push(pktLine(`ACK ${ack}\n`));
    if (plan.objects.length > 0) parts.push(pktLine('ready\n'));
    parts.push(PKT_DELIM);
    if (plan.shallow.length > 0 || plan.unshallow.length > 0) {
      parts.push(pktLine('shallow-info\n'));
      for (const oid of plan.shallow) parts.push(pktLine(`shallow ${oid}\n`));
      for (const oid of plan.unshallow) parts.push(pktLine(`unshallow ${oid}\n`));
      parts.push(PKT_DELIM);
    }
    if (plan.objects.length > 0) {
      parts.push(pktLine('packfile\n'));
      const pack = writePack(plan.objects);
      for (const packet of sidebandPackets(1, pack)) parts.push(packet);
    }
    parts.push(PKT_FLUSH);
    return gitResult('git-upload-pack', 'result', Buffer.concat(parts));
  }

  private fetchV0(repo: Repo, packets: Pkt[]): HttpResult {
    const wants: string[] = [];
    const haves: string[] = [];
    const shallow: string[] = [];
    let depth: number | null = null;
    let section = 0;
    for (const packet of packets) {
      if (packet.kind === 'flush') {
        section += 1;
        continue;
      }
      if (packet.kind !== 'data') continue;
      const line = packet.payload.toString('utf8').replace(/\n$/, '');
      if (section === 0) {
        if (line.startsWith('want ')) wants.push(line.slice(5).trim().split(' ')[0] as string);
        else if (line.startsWith('shallow ')) shallow.push(line.slice(8).trim());
        else if (line.startsWith('deepen ')) depth = Number(line.slice(7).trim());
      } else if (line.startsWith('have ')) {
        haves.push(line.slice(5).trim());
      }
    }
    const plan = planFetch(repo, wants, haves, shallow, depth);
    const parts: Buffer[] = [];
    for (const oid of plan.shallow) parts.push(pktLine(`shallow ${oid}\n`));
    for (const oid of plan.unshallow) parts.push(pktLine(`unshallow ${oid}\n`));
    parts.push(pktLine('NAK\n'));
    if (plan.objects.length > 0) {
      const pack = writePack(plan.objects);
      for (const packet of sidebandPackets(1, pack)) parts.push(packet);
    }
    parts.push(PKT_FLUSH);
    return gitResult('git-upload-pack', 'result', Buffer.concat(parts));
  }

  // -------------------------------------------------------------------------------------------
  // receive-pack POST

  receivePack(owner: string, name: string, body: Buffer): HttpResult {
    const repo = this.repo(owner, name);
    if (repo === null) return text(404, 'repository not found\n');
    let packets: Pkt[];
    let packBytes: Buffer;
    try {
      const parsed = parsePkts(body);
      packets = parsed.packets;
      packBytes = parsed.rest;
    } catch {
      return text(400, 'malformed pkt-line request\n');
    }
    interface Command {
      oldOid: string;
      newOid: string;
      ref: string;
    }
    const commands: Command[] = [];
    let capabilities = new Set<string>();
    for (const packet of packets) {
      if (packet.kind !== 'data') continue;
      let line = packet.payload.toString('utf8').replace(/\n$/, '');
      const nul = line.indexOf('\0');
      if (nul >= 0) {
        capabilities = new Set(line.slice(nul + 1).split(' ').filter(Boolean).map((cap) => cap.split('=')[0] as string));
        line = line.slice(0, nul);
      }
      const match = /^([0-9a-f]{40}) ([0-9a-f]{40}) (\S+)$/.exec(line);
      if (match) commands.push({ oldOid: match[1] as string, newOid: match[2] as string, ref: match[3] as string });
    }
    const useSideband = capabilities.has('side-band-64k');
    const useReport = capabilities.has('report-status') || capabilities.has('report-status-v2');

    let unpackError: string | null = null;
    if (packBytes.length > 0) {
      try {
        const objects = readPack(packBytes, (oid) => repo.objects.get(oid));
        for (const [oid, object] of objects) {
          if (!repo.objects.has(oid)) repo.objects.set(oid, object);
        }
      } catch (err) {
        unpackError = err instanceof Error ? err.message : String(err);
      }
    }

    const results: Array<{ ref: string; ok: boolean; reason: string }> = [];
    for (const command of commands) {
      const current = repo.refs.get(command.ref) ?? ZERO_OID;
      const fail = (reason: string): void => {
        results.push({ ref: command.ref, ok: false, reason });
      };
      if (unpackError !== null) {
        fail('unpack failed');
        continue;
      }
      if (command.newOid === ZERO_OID) {
        if (current === ZERO_OID) fail('reference does not exist');
        else if (command.oldOid !== current) fail('failed to lock');
        else {
          repo.refs.delete(command.ref);
          results.push({ ref: command.ref, ok: true, reason: '' });
        }
        continue;
      }
      const object = repo.objects.get(command.newOid);
      if (object === undefined) {
        fail('bad object');
        continue;
      }
      if (command.ref.startsWith('refs/heads/') && object.type !== 'commit') {
        fail('bad object');
        continue;
      }
      if (current === ZERO_OID) {
        if (command.oldOid !== ZERO_OID) fail('failed to lock');
        else {
          repo.refs.set(command.ref, command.newOid);
          results.push({ ref: command.ref, ok: true, reason: '' });
        }
        continue;
      }
      if (command.oldOid !== current) {
        fail('failed to lock');
        continue;
      }
      if (command.ref.startsWith('refs/heads/') && !isAncestor(repo, current, command.newOid)) {
        fail('non-fast-forward');
        continue;
      }
      repo.refs.set(command.ref, command.newOid);
      results.push({ ref: command.ref, ok: true, reason: '' });
    }

    if (!useReport) {
      return gitResult('git-receive-pack', 'result', Buffer.alloc(0));
    }
    const reportParts: Buffer[] = [pktLine(unpackError === null ? 'unpack ok\n' : `unpack ${unpackError}\n`)];
    for (const result of results) {
      reportParts.push(pktLine(result.ok ? `ok ${result.ref}\n` : `ng ${result.ref} ${result.reason}\n`));
    }
    reportParts.push(PKT_FLUSH);
    const report = Buffer.concat(reportParts);
    if (useSideband) {
      const wrapped = [...sidebandPackets(1, report), PKT_FLUSH];
      return gitResult('git-receive-pack', 'result', Buffer.concat(wrapped));
    }
    return gitResult('git-receive-pack', 'result', report);
  }
}
