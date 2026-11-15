// The forge's HTTP shell: one node:http server routing the four surfaces — git smart HTTP,
// REST v3, GraphQL v4 and MCP — over one store, with the protocol discipline the suite checks
// (JSON everywhere under /api/v3, an X-GitHub-Request-Id on every REST reply, 401 without the
// trial's bearer token, a JSON 404 for anything unknown, and no crash ever escaping a handler).

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { GitHttp, type HttpResult } from './githttp.ts';
import { executeGraphQL, type GqlRequest } from './graphql.ts';
import { handleMcp } from './mcp.ts';
import { handleRest, type RestCtx } from './rest.ts';
import { Store } from './store.ts';

const JSON_TYPE = 'application/json; charset=utf-8';
const MAX_BODY_BYTES = 256 * 1024 * 1024;

export function createApp(): Server {
  const store = new Store();
  const git = new GitHttp(store);
  const token = process.env.MAKEFUN_SHOP_TOKEN ?? '';
  let requestSeq = 0;

  const authorized = (req: IncomingMessage): boolean => {
    const header = req.headers.authorization;
    if (header === undefined) return false;
    if (token === '') return true; // no token configured (standalone smoke runs): any credential passes, none does not
    return header === `Bearer ${token}` || header === `token ${token}`;
  };

  const sendBuffer = (res: ServerResponse, status: number, contentType: string, body: Buffer, headers: Record<string, string> = {}): void => {
    res.writeHead(status, { 'content-type': contentType, 'content-length': body.length, ...headers });
    res.end(body);
  };

  const sendJson = (res: ServerResponse, status: number, body: unknown, headers: Record<string, string> = {}): void => {
    if (status === 204) {
      res.writeHead(204, headers);
      res.end();
      return;
    }
    const text = `${JSON.stringify(body)}\n`;
    sendBuffer(res, status, JSON_TYPE, Buffer.from(text, 'utf8'), headers);
  };

  const sendHttpResult = (res: ServerResponse, result: HttpResult): void => {
    sendBuffer(res, result.status, result.contentType, result.body, result.headers ?? {});
  };

  const readBody = async (req: IncomingMessage): Promise<Buffer> => {
    const chunks: Buffer[] = [];
    let total = 0;
    for await (const chunk of req) {
      const buffer = chunk as Buffer;
      total += buffer.length;
      if (total > MAX_BODY_BYTES) throw new Error('request body is too large');
      chunks.push(buffer);
    }
    return Buffer.concat(chunks);
  };

  const handle = async (req: IncomingMessage, res: ServerResponse): Promise<void> => {
    const method = req.method ?? 'GET';
    let url: URL;
    try {
      url = new URL(req.url ?? '/', 'http://entry');
    } catch {
      sendJson(res, 400, { message: 'Bad Request' });
      return;
    }
    const path = url.pathname;
    const segments = path.split('/').filter((piece) => piece !== '').map((piece) => {
      try {
        return decodeURIComponent(piece);
      } catch {
        return piece;
      }
    });

    // The env's boot probe: no token, twice, 200 {"ok":true}.
    if (method === 'GET' && path === '/healthz') {
      sendJson(res, 200, { ok: true });
      return;
    }

    // Git smart HTTP: /<owner>/<repo>.git/...
    if (segments.length >= 2 && (segments[1] as string).endsWith('.git')) {
      const owner = segments[0] as string;
      const repoName = (segments[1] as string).slice(0, -'.git'.length);
      const tail = segments.slice(2).join('/');
      if (!authorized(req)) {
        sendBuffer(res, 401, 'text/plain; charset=utf-8', Buffer.from('authentication required\n', 'utf8'), {
          'www-authenticate': 'Bearer',
        });
        return;
      }
      if (method === 'GET' && tail === 'info/refs') {
        const protocolV2 = (req.headers['git-protocol'] ?? '').includes('version=2');
        sendHttpResult(res, git.infoRefs(owner, repoName, url.searchParams.get('service'), protocolV2));
        return;
      }
      if (method === 'POST' && tail === 'git-upload-pack') {
        sendHttpResult(res, git.uploadPack(owner, repoName, await readBody(req)));
        return;
      }
      if (method === 'POST' && tail === 'git-receive-pack') {
        sendHttpResult(res, git.receivePack(owner, repoName, await readBody(req)));
        return;
      }
      sendJson(res, 404, { message: 'Not Found' });
      return;
    }

    // GraphQL v4.
    if (path === '/api/graphql') {
      if (method !== 'POST') {
        sendJson(res, 404, { message: 'Not Found' });
        return;
      }
      if (!authorized(req)) {
        sendJson(res, 401, { message: 'Bad credentials' });
        return;
      }
      const raw = await readBody(req);
      let parsed: unknown;
      try {
        parsed = JSON.parse(raw.toString('utf8'));
      } catch {
        sendJson(res, 400, { errors: [{ message: 'Problems parsing JSON' }] });
        return;
      }
      const runOne = (entry: unknown): unknown => {
        const item = (typeof entry === 'object' && entry !== null ? entry : {}) as GqlRequest;
        if (typeof item.query !== 'string') return { data: null, errors: [{ message: 'A query is required' }] };
        return executeGraphQL(store, {
          query: item.query,
          variables: (item.variables ?? {}) as Record<string, unknown>,
          operationName: item.operationName,
        });
      };
      if (Array.isArray(parsed)) {
        sendJson(res, 200, parsed.map(runOne));
        return;
      }
      sendJson(res, 200, runOne(parsed));
      return;
    }

    // MCP.
    if (path === '/mcp') {
      if (method === 'DELETE') {
        res.writeHead(200);
        res.end();
        return;
      }
      if (method !== 'POST') {
        sendJson(res, 405, { message: 'Method Not Allowed' });
        return;
      }
      if (!authorized(req)) {
        sendJson(res, 401, { jsonrpc: '2.0', id: null, error: { code: -32001, message: 'authentication required' } });
        return;
      }
      const raw = await readBody(req);
      const result = handleMcp(store, raw);
      if (result.body === undefined) {
        res.writeHead(result.status);
        res.end();
        return;
      }
      sendJson(res, result.status, result.body);
      return;
    }

    // REST v3.
    if (path === '/api/v3' || path.startsWith('/api/v3/')) {
      requestSeq += 1;
      const requestId = `F${requestSeq.toString(16).padStart(6, '0')}`;
      const restSegments = segments.slice(2);
      const isPublic = method === 'GET' && (restSegments.length === 0 || (restSegments.length === 1 && restSegments[0] === 'meta'));
      if (!isPublic && !authorized(req)) {
        sendJson(res, 401, { message: 'Bad credentials' }, { 'x-github-request-id': requestId });
        return;
      }
      let body: unknown;
      let bodyMalformed = false;
      if (method === 'POST' || method === 'PUT' || method === 'PATCH' || method === 'DELETE') {
        const raw = await readBody(req);
        if (raw.length > 0) {
          try {
            body = JSON.parse(raw.toString('utf8'));
          } catch {
            bodyMalformed = true;
          }
        }
      }
      const host = req.headers.host ?? 'entry';
      const ctx: RestCtx = {
        store,
        method,
        segments: restSegments,
        query: url.searchParams,
        body,
        bodyMalformed,
        origin: `http://${host}`,
        path,
      };
      const result = handleRest(ctx);
      const headers = { 'x-github-request-id': requestId, ...(result.headers ?? {}) };
      if (result.text !== undefined) {
        // A handler that already serialized its body (the memoized issue read) skips the
        // stringify pass; the bytes on the wire are the same either way.
        sendBuffer(res, result.status, JSON_TYPE, Buffer.from(result.text, 'utf8'), headers);
        return;
      }
      sendJson(res, result.status, result.body, headers);
      return;
    }

    sendJson(res, 404, { message: 'Not Found' });
  };

  const server = createServer((req, res) => {
    handle(req, res).catch(() => {
      // A handler failure must never take the forge down (the suite probes exactly that).
      try {
        if (!res.headersSent) sendJson(res, 500, { message: 'Internal Server Error' });
        else res.end();
      } catch {
        // The socket is already gone; nothing more to do.
      }
    });
  });
  server.on('clientError', (_err, socket) => {
    socket.destroy();
  });
  return server;
}
