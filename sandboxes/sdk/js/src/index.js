// gocracker JS/Node SDK.
//
// Zero runtime dependencies — built on Node's stdlib (fetch + net).
// Fetch is global in Node 18+; net.createConnection handles the UDS
// bridge for toolbox-agent calls.
//
// Usage:
//
//   import { Client } from '@gocracker/sdk';
//
//   const client = new Client('http://127.0.0.1:9091');
//   const sb = await client.createSandbox({ image: 'alpine:3.20', kernelPath: '/abs/path' });
//   const result = await sb.toolbox().exec(['echo', 'hello']);
//   console.log(result.stdoutText);
//   await sb.delete();

import { Buffer } from 'node:buffer';
import net from 'node:net';

// ---- Typed errors ------------------------------------------------

/** Base error for sandboxd control-plane calls. */
export class SandboxError extends Error {
  constructor(message, { status = 0, body = '' } = {}) {
    super(message);
    this.name = 'SandboxError';
    this.status = status;
    this.body = body;
  }
}
/** 404 on a sandbox / pool / template endpoint. */
export class SandboxNotFound extends SandboxError {
  constructor(...args) { super(...args); this.name = 'SandboxNotFound'; }
}
/** 400 — malformed request. */
export class SandboxInvalidRequest extends SandboxError {
  constructor(...args) { super(...args); this.name = 'SandboxInvalidRequest'; }
}
/** Raised by sb.process.exec when the command exits non-zero. Carries
 * exitCode + stdout/stderr so callers can log or recover without re-running.
 * Matches the Python SDK's ProcessExitError shape. */
export class ProcessExitError extends SandboxError {
  constructor(exitCode, stdout, stderr) {
    super(`process exited with code ${exitCode}`);
    this.name = 'ProcessExitError';
    this.exitCode = exitCode;
    this.stdout = stdout;
    this.stderr = stderr;
  }
}
/** The named template was not registered with sandboxd. */
export class TemplateNotFound extends SandboxError {
  constructor(...args) { super(...args); this.name = 'TemplateNotFound'; }
}
/** LeaseSandbox when the warm pool has no paused entries. */
export class PoolExhausted extends SandboxError {
  constructor(...args) { super(...args); this.name = 'PoolExhausted'; }
}
/** sandboxd can't reach the gocracker runtime (KVM/jailer missing). */
export class RuntimeUnreachable extends SandboxError {
  constructor(...args) { super(...args); this.name = 'RuntimeUnreachable'; }
}
/** An operation exceeded its deadline. */
export class SandboxTimeout extends SandboxError {
  constructor(...args) { super(...args); this.name = 'SandboxTimeout'; }
}
/** 409 — pool template_id already registered, etc. */
export class SandboxConflict extends SandboxError {
  constructor(...args) { super(...args); this.name = 'SandboxConflict'; }
}
/** Any toolbox-agent UDS/HTTP error. */
export class ToolboxError extends Error {
  constructor(message) { super(message); this.name = 'ToolboxError'; }
}

// ---- Sandbox / Pool / Template / Preview shapes ------------------

/** A sandbox record as returned by sandboxd. */
export class Sandbox {
  constructor(raw, client) {
    this.id = raw.id ?? '';
    this.state = raw.state ?? '';
    this.image = raw.image ?? '';
    this.udsPath = raw.uds_path ?? '';
    this.guestIp = raw.guest_ip ?? '';
    this.runtimeId = raw.runtime_id ?? '';
    this.createdAt = raw.created_at ?? '';
    this.error = raw.error ?? '';
    this._client = client;
  }

  async delete() {
    if (!this._client) throw new SandboxError('Sandbox has no client');
    return this._client.delete(this.id);
  }

  /** Recycle this leased sandbox: tear down the current VM and return
   * a fresh one from the same pool. The old handle (`this`) is dead
   * after this call — use the returned Sandbox. */
  async recycle() {
    if (!this._client) throw new SandboxError('Sandbox has no client');
    return this._client.recycle(this.id);
  }

  toolbox() {
    if (!this.udsPath) throw new SandboxError('sandbox has no uds_path — not ready?');
    return new ToolboxClient(this.udsPath);
  }

  // ---- Convenience namespaces (v2 parity on v3 runtime) ----
  //
  // const sb = await client.createSandbox({ template: 'base-python' });
  // await sb.process.exec('python -c "print(2+2)"');
  // await sb.fs.writeFile('/tmp/x', Buffer.from('hi'));
  // const url = await sb.previewUrl(8080);
  // await using sb = ... ;   // TC39 explicit-resource-management (Node 24+)

  get process() { return new _ProcessNamespace(this.toolbox()); }
  get fs() { return new _FSNamespace(this.toolbox()); }

  /** Mint a signed preview URL for a guest-side port and return the
   * absolute URL (includes scheme + host + `/previews/<token>/`). */
  async previewUrl(port) {
    if (!this._client) throw new SandboxError('Sandbox has no client');
    const preview = await this._client.mintPreview(this.id, port);
    return `${this._client.baseUrl}${preview.url}`;
  }

  /** Symbol.asyncDispose — supports `await using sb = await client.createSandbox(...)`
   * (TC39 explicit-resource-management proposal, Node 24+). Older
   * runtimes fall back to explicit `await sb.delete()`. */
  async [Symbol.asyncDispose]() {
    try { await this.delete(); } catch (_) { /* swallow double-delete */ }
  }
}

function _normalizeExecCmd(cmd) {
  if (typeof cmd === 'string') return ['/bin/sh', '-c', cmd];
  if (Array.isArray(cmd)) return cmd;
  throw new SandboxError(`exec: cmd must be string or string[], got ${typeof cmd}`);
}

class _ProcessNamespace {
  constructor(tb) { this._tb = tb; }
  async exec(cmd, opts = {}) {
    const res = await this._tb.exec(_normalizeExecCmd(cmd), opts);
    if (res.exitCode !== 0) {
      throw new ProcessExitError(res.exitCode, res.stdoutText ?? '', res.stderrText ?? '');
    }
    return res;
  }
  execStream(cmd, opts = {}) {
    return this._tb.execStream(_normalizeExecCmd(cmd), opts);
  }
  start(cmd, opts = {}) { return this.execStream(cmd, opts); }
}

class _FSNamespace {
  constructor(tb) { this._tb = tb; }
  writeFile(path, data) { return this._tb.upload(path, data); }
  readFile(path) { return this._tb.download(path); }
  listDir(path) { return this._tb.listFiles(path); }
  remove(path) { return this._tb.deleteFile(path); }
  mkdir(path, parents = true) { return this._tb.mkdir(path, { parents }); }
  chmod(path, mode) { return this._tb.chmod(path, mode); }
  rename(src, dst) { return this._tb.rename(src, dst); }
}

// ---- Control-plane client ----------------------------------------

const TOOLBOX_VSOCK_PORT = 10023;

/** Control-plane client for sandboxd. */
export class Client {
  /**
   * @param {string} baseUrl  e.g. 'http://127.0.0.1:9091'
   * @param {object} [opts]
   * @param {number} [opts.timeoutMs=30000]
   */
  constructor(baseUrl, opts = {}) {
    this.baseUrl = baseUrl.replace(/\/$/, '');
    this.timeoutMs = opts.timeoutMs ?? 30_000;
  }

  // Sandboxes
  async createSandbox(req) {
    // Template resolution: `createSandbox({template: 'base-python'})`
    // looks up the registered template and fills in image/kernelPath/mem/cpus
    // from its spec. Subsequent request hits the warm-cache restore path.
    if (req.template) {
      let t;
      try {
        t = await this.getTemplate(req.template);
      } catch (err) {
        throw new TemplateNotFound(
          `template ${JSON.stringify(req.template)} is unknown. ` +
          `If you expect base templates (base-python/node/bun/go) to be preregistered, ` +
          `make sure sandboxd was started with -kernel-path or $GOCRACKER_KERNEL set.`,
          { status: err.status ?? 0, body: err.body ?? '' }
        );
      }
      const spec = t.spec ?? {};
      req = {
        ...req,
        image: req.image ?? spec.image,
        kernelPath: req.kernelPath ?? spec.kernel_path,
        memMB: req.memMB ?? spec.mem_mb,
        cpus: req.cpus ?? spec.cpus,
      };
    }
    const body = {
      image: req.image,
      kernel_path: req.kernelPath,
    };
    if (req.memMB) body.mem_mb = req.memMB;
    if (req.cpus) body.cpus = req.cpus;
    if (req.entrypoint) body.entrypoint = req.entrypoint;
    if (req.cmd) body.cmd = req.cmd;
    if (req.env) body.env = req.env;
    if (req.workdir) body.workdir = req.workdir;
    if (req.networkMode) body.network_mode = req.networkMode;
    if (req.jailerMode) body.jailer_mode = req.jailerMode;
    if (req.dockerfile) body.dockerfile = req.dockerfile;
    if (req.context) body.context = req.context;
    const resp = await this._post('/sandboxes', body);
    return new Sandbox(resp.sandbox ?? {}, this);
  }

  async listSandboxes() {
    const resp = await this._get('/sandboxes');
    return (resp.sandboxes ?? []).map((s) => new Sandbox(s, this));
  }

  async getSandbox(id) {
    return new Sandbox(await this._get(`/sandboxes/${id}`), this);
  }

  async delete(id) {
    await this._request('DELETE', `/sandboxes/${id}`, null, [204]);
  }

  /** Recycle a leased sandbox: tear it down and return a fresh one
   * from the same pool in a single round trip. Returns a new Sandbox
   * (different id); the old `id` is gone after this call. */
  async recycle(id) {
    const resp = await this._request('POST', `/sandboxes/${id}/recycle`, null, [200, 201]);
    return new Sandbox(resp.sandbox ?? {}, this);
  }

  // Pools
  async registerPool(req) {
    const body = { template_id: req.templateId };
    for (const [camel, snake] of [
      ['fromTemplate', 'from_template'],
      ['image', 'image'],
      ['kernelPath', 'kernel_path'],
      ['memMB', 'mem_mb'],
      ['cpus', 'cpus'],
      ['jailerMode', 'jailer_mode'],
      ['minPaused', 'min_paused'],
      ['maxPaused', 'max_paused'],
    ]) {
      if (req[camel] !== undefined && req[camel] !== '' && req[camel] !== 0) body[snake] = req[camel];
    }
    return this._post('/pools', body);
  }

  async listPools() {
    const resp = await this._get('/pools');
    return resp.pools ?? [];
  }

  async unregisterPool(templateId) {
    await this._request('DELETE', `/pools/${templateId}`, null, [204]);
  }

  async leaseSandbox(req) {
    const body = { template_id: req.templateId };
    if (req.timeoutNs) body.timeout = req.timeoutNs;
    const resp = await this._post('/sandboxes/lease', body);
    return new Sandbox(resp.sandbox ?? {}, this);
  }

  // Templates
  async createTemplate(req) {
    const body = {};
    for (const [camel, snake] of [
      ['id', 'id'],
      ['image', 'image'],
      ['dockerfile', 'dockerfile'],
      ['context', 'context'],
      ['kernelPath', 'kernel_path'],
      ['memMB', 'mem_mb'],
      ['cpus', 'cpus'],
    ]) {
      if (req[camel] !== undefined && req[camel] !== '' && req[camel] !== 0) body[snake] = req[camel];
    }
    const resp = await this._post('/templates', body);
    return { template: resp.template ?? {}, cacheHit: resp.cache_hit ?? false };
  }

  async listTemplates() {
    const resp = await this._get('/templates');
    return resp.templates ?? [];
  }

  async getTemplate(id) { return this._get(`/templates/${id}`); }

  async deleteTemplate(id) {
    await this._request('DELETE', `/templates/${id}`, null, [204]);
  }

  // Previews
  async mintPreview(sandboxId, port) {
    return this._post(`/sandboxes/${sandboxId}/preview/${port}`, null);
  }

  async healthz() {
    try {
      const resp = await this._get('/healthz');
      return !!resp.ok;
    } catch { return false; }
  }

  // Internals
  async _get(path) { return this._request('GET', path, null, [200]); }
  async _post(path, body) { return this._request('POST', path, body, [200, 201]); }

  async _request(method, path, body, expectStatus) {
    const url = this.baseUrl + path;
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this.timeoutMs);
    let resp;
    try {
      resp = await fetch(url, {
        method,
        headers: {
          Accept: 'application/json',
          ...(body !== null ? { 'Content-Type': 'application/json' } : {}),
        },
        body: body !== null ? JSON.stringify(body) : undefined,
        signal: controller.signal,
      });
    } catch (e) {
      clearTimeout(timer);
      throw new SandboxError(`${method} ${path}: ${e.message}`);
    }
    clearTimeout(timer);

    const text = await resp.text();
    if (!expectStatus.includes(resp.status)) {
      throw wrapHttpError(resp.status, text, `${method} ${path}`);
    }
    if (resp.status === 204 || text === '') return {};
    try { return JSON.parse(text); } catch { return {}; }
  }
}

function wrapHttpError(status, body, context) {
  let msg = body;
  try {
    const parsed = JSON.parse(body);
    if (parsed && typeof parsed === 'object' && parsed.error) msg = parsed.error;
  } catch {}
  const fullMsg = `${context}: ${msg}`;
  const opts = { status, body };
  if (status === 404) return new SandboxNotFound(fullMsg, opts);
  if (status === 400) return new SandboxInvalidRequest(fullMsg, opts);
  if (status === 409) return new SandboxConflict(fullMsg, opts);
  return new SandboxError(fullMsg, opts);
}

// ---- Toolbox-agent client (UDS + CONNECT) ------------------------

export const TB_CHANNEL_STDIN = 0;
export const TB_CHANNEL_STDOUT = 1;
export const TB_CHANNEL_STDERR = 2;
export const TB_CHANNEL_EXIT = 3;
export const TB_CHANNEL_SIGNAL = 4;

/** Client for the in-guest toolbox agent. */
export class ToolboxClient {
  /**
   * @param {string} udsPath host-visible UDS of the sandbox
   * @param {object} [opts]
   * @param {number} [opts.port=10023]
   * @param {number} [opts.dialTimeoutMs=5000]
   */
  constructor(udsPath, opts = {}) {
    this.udsPath = udsPath;
    this.port = opts.port ?? TOOLBOX_VSOCK_PORT;
    this.dialTimeoutMs = opts.dialTimeoutMs ?? 5_000;
  }

  async health() {
    const { status, body } = await this._request('GET', '/healthz');
    if (status !== 200) throw new ToolboxError(`health: status=${status} body=${body.toString('utf-8')}`);
    return JSON.parse(body.toString('utf-8'));
  }

  /**
   * Run a command in the guest and collect stdout/stderr/exit.
   * @returns {Promise<{exitCode:number, stdout:Buffer, stderr:Buffer, stdoutText:string, stderrText:string}>}
   */
  async exec(cmd, { env, workdir, stdin, timeoutMs = 30_000 } = {}) {
    const stdoutChunks = [];
    const stderrChunks = [];
    let exitCode = -1;
    for await (const { channel, payload } of this.execStream(cmd, { env, workdir, stdin, timeoutMs })) {
      if (channel === TB_CHANNEL_STDOUT) stdoutChunks.push(payload);
      else if (channel === TB_CHANNEL_STDERR) stderrChunks.push(payload);
      else if (channel === TB_CHANNEL_EXIT) exitCode = payload.readInt32BE(0);
    }
    const stdout = Buffer.concat(stdoutChunks);
    const stderr = Buffer.concat(stderrChunks);
    return {
      exitCode,
      stdout, stderr,
      stdoutText: stdout.toString('utf-8'),
      stderrText: stderr.toString('utf-8'),
    };
  }

  /**
   * Yield framed exec responses as they arrive.
   * @returns {AsyncIterableIterator<{channel:number, payload:Buffer}>}
   */
  async *execStream(cmd, { env, workdir, stdin, timeoutMs = 30_000 } = {}) {
    const body = { cmd };
    if (env) body.env = env;
    if (workdir) body.workdir = workdir;
    const bodyBytes = Buffer.from(JSON.stringify(body), 'utf-8');

    const sock = await this._dialConnect();
    sock.setTimeout(timeoutMs);
    try {
      const req = [
        'POST /exec HTTP/1.0',
        'Host: x',
        `Content-Length: ${bodyBytes.length}`,
        'Content-Type: application/json',
        'Connection: close',
        '', '',
      ].join('\r\n');
      sock.write(req);
      sock.write(bodyBytes);
      if (stdin) {
        sock.write(frameHeader(TB_CHANNEL_STDIN, stdin.length));
        sock.write(stdin);
        sock.write(frameHeader(TB_CHANNEL_STDIN, 0));
      }

      const reader = new StreamReader(sock);
      // Drain HTTP response line + headers.
      const statusLine = await reader.readLine();
      if (!statusLine.startsWith('HTTP/')) throw new ToolboxError(`unexpected response: ${statusLine}`);
      for (;;) {
        const line = await reader.readLine();
        if (line === '' || line === '\r') break;
      }
      // Stream frames.
      for (;;) {
        const hdr = await reader.readBytes(5);
        if (!hdr || hdr.length < 5) return;
        const channel = hdr[0];
        const n = hdr.readUInt32BE(1);
        let payload = Buffer.alloc(0);
        if (n > 0) {
          payload = await reader.readBytes(n);
          if (!payload || payload.length < n) return;
        }
        yield { channel, payload };
        if (channel === TB_CHANNEL_EXIT) return;
      }
    } finally {
      sock.destroy();
    }
  }

  // ---- Files ----
  async listFiles(path) {
    const { status, body } = await this._request('GET', `/files?path=${encodeURIComponent(path)}`);
    if (status !== 200) throw new ToolboxError(`list_files: status=${status}`);
    const parsed = JSON.parse(body.toString('utf-8'));
    return (parsed.entries ?? []).map((e) => ({
      name: e.name ?? '',
      path: e.path ?? '',
      size: e.size ?? 0,
      isDir: e.kind === 'dir',
    }));
  }

  async download(path) {
    const { status, body } = await this._request('GET', `/files/download?path=${encodeURIComponent(path)}`);
    if (status !== 200) throw new ToolboxError(`download: status=${status}`);
    return body;
  }

  async upload(path, data) {
    const bytes = data instanceof Buffer ? data : Buffer.from(data);
    const { status } = await this._request(
      'POST', `/files/upload?path=${encodeURIComponent(path)}`, bytes, 'application/octet-stream'
    );
    if (status !== 200 && status !== 201) throw new ToolboxError(`upload: status=${status}`);
  }

  async deleteFile(path) {
    const { status } = await this._request('DELETE', `/files?path=${encodeURIComponent(path)}`);
    if (status !== 200 && status !== 204) throw new ToolboxError(`delete_file: status=${status}`);
  }

  async mkdir(path, parents = false) {
    const { status } = await this._request('POST', '/files/mkdir',
      Buffer.from(JSON.stringify({ path, all: parents }), 'utf-8'));
    if (status !== 200) throw new ToolboxError(`mkdir: status=${status}`);
  }

  async rename(src, dst) {
    const { status } = await this._request('POST', '/files/rename',
      Buffer.from(JSON.stringify({ old_path: src, new_path: dst }), 'utf-8'));
    if (status !== 200) throw new ToolboxError(`rename: status=${status}`);
  }

  async chmod(path, mode) {
    const { status } = await this._request('POST', '/files/chmod',
      Buffer.from(JSON.stringify({ path, mode }), 'utf-8'));
    if (status !== 200) throw new ToolboxError(`chmod: status=${status}`);
  }

  // ---- Git ----
  async gitClone(repository, directory, ref = '') {
    const body = { repository, directory };
    if (ref) body.ref = ref;
    const { status, body: resp } = await this._request('POST', '/git/clone',
      Buffer.from(JSON.stringify(body), 'utf-8'));
    if (status !== 200) throw new ToolboxError(`git_clone: status=${status}`);
    return JSON.parse(resp.toString('utf-8'));
  }

  async gitStatus(directory) {
    const { status, body: resp } = await this._request('POST', '/git/status',
      Buffer.from(JSON.stringify({ directory }), 'utf-8'));
    if (status !== 200) throw new ToolboxError(`git_status: status=${status}`);
    return JSON.parse(resp.toString('utf-8'));
  }

  // ---- Secrets ----
  async setSecret(name, value) {
    const { status } = await this._request('POST', '/secrets',
      Buffer.from(JSON.stringify({ name, value }), 'utf-8'));
    if (status !== 200 && status !== 201) throw new ToolboxError(`set_secret: status=${status}`);
  }

  async listSecrets() {
    const { status, body } = await this._request('GET', '/secrets');
    if (status !== 200) throw new ToolboxError(`list_secrets: status=${status}`);
    return JSON.parse(body.toString('utf-8')).secrets ?? [];
  }

  async deleteSecret(name) {
    const { status } = await this._request('DELETE', `/secrets/${name}`);
    if (status !== 200 && status !== 204) throw new ToolboxError(`delete_secret: status=${status}`);
  }

  // ---- Internals ----
  //
  // Per-call UDS + CONNECT is ~5 ms of round trips in practice. The
  // agent speaks HTTP/1.0 with `Connection: close`, so we can't keep
  // the socket open across requests. What we CAN do is pipeline the
  // CONNECT header and the HTTP request into a single write so the
  // guest agent sees both bytes together — saves one round trip
  // (the "wait for OK\n before sending HTTP") per call. On a warm
  // pool that's a ~5 ms drop in p50 exec latency.
  async _dialPipelined(httpHead, body = null) {
    return new Promise((resolve, reject) => {
      const sock = net.createConnection({ path: this.udsPath });
      const timer = setTimeout(() => { sock.destroy(); reject(new ToolboxError(`dial timeout: ${this.udsPath}`)); }, this.dialTimeoutMs);

      sock.once('error', (e) => { clearTimeout(timer); reject(new ToolboxError(`dial ${this.udsPath}: ${e.message}`)); });
      sock.once('connect', async () => {
        try {
          sock.write(`CONNECT ${this.port}\n` + httpHead);
          if (body) sock.write(body);
          const reader = new StreamReader(sock);
          const line = await reader.readLine();
          clearTimeout(timer);
          if (!line.startsWith('OK')) {
            sock.destroy();
            reject(new ToolboxError(`CONNECT rejected: ${line}`));
            return;
          }
          sock.__gcReader = reader;
          resolve(sock);
        } catch (err) {
          clearTimeout(timer);
          sock.destroy();
          reject(err);
        }
      });
    });
  }

  // _dialConnect is the classic round-trip-then-send dial used by exec
  // (which streams a body in multiple writes). Kept separate from
  // _dialPipelined so the simpler one-shot request path can use the
  // faster one.
  async _dialConnect() {
    return new Promise((resolve, reject) => {
      const sock = net.createConnection({ path: this.udsPath });
      const timer = setTimeout(() => { sock.destroy(); reject(new ToolboxError(`dial timeout: ${this.udsPath}`)); }, this.dialTimeoutMs);

      sock.once('error', (e) => { clearTimeout(timer); reject(new ToolboxError(`dial ${this.udsPath}: ${e.message}`)); });
      sock.once('connect', async () => {
        try {
          sock.write(`CONNECT ${this.port}\n`);
          const reader = new StreamReader(sock);
          const line = await reader.readLine();
          clearTimeout(timer);
          if (!line.startsWith('OK')) {
            sock.destroy();
            reject(new ToolboxError(`CONNECT rejected: ${line}`));
            return;
          }
          sock.__gcReader = reader;
          resolve(sock);
        } catch (err) {
          clearTimeout(timer);
          sock.destroy();
          reject(err);
        }
      });
    });
  }

  async _request(method, path, body = null, contentType = 'application/json') {
    // Pipeline: CONNECT + HTTP headers + body in one shot. Saves the
    // OK round trip on every request.
    const hdrs = [`${method} ${path} HTTP/1.0`, 'Host: x', 'Connection: close'];
    if (body) {
      hdrs.push(`Content-Length: ${body.length}`);
      hdrs.push(`Content-Type: ${contentType}`);
    }
    const httpHead = hdrs.join('\r\n') + '\r\n\r\n';
    const sock = await this._dialPipelined(httpHead, body);
    try {
      const reader = sock.__gcReader ?? new StreamReader(sock);
      const statusLine = await reader.readLine();
      if (!statusLine.startsWith('HTTP/')) throw new ToolboxError(`unexpected response: ${statusLine}`);
      const parts = statusLine.split(' ');
      const status = parseInt(parts[1], 10);
      const headers = {};
      for (;;) {
        const line = await reader.readLine();
        if (line === '' || line === '\r') break;
        const idx = line.indexOf(':');
        if (idx > 0) headers[line.slice(0, idx).trim().toLowerCase()] = line.slice(idx + 1).trim();
      }
      let bodyOut = Buffer.alloc(0);
      if (status !== 204 && status !== 205 && status !== 304) {
        const cl = headers['content-length'];
        if (cl && !isNaN(parseInt(cl, 10))) {
          const n = parseInt(cl, 10);
          if (n > 0) bodyOut = await reader.readBytes(n);
        } else {
          bodyOut = await reader.readToEnd();
        }
      }
      return { status, headers, body: bodyOut };
    } finally {
      sock.destroy();
    }
  }
}

// ---- Stream reader helper ---------------------------------------

class StreamReader {
  constructor(sock) {
    this.sock = sock;
    this.chunks = [];
    this.ended = false;
    this._resolvers = [];
    sock.on('data', (c) => { this.chunks.push(c); this._notify(); });
    sock.on('end', () => { this.ended = true; this._notify(); });
    sock.on('close', () => { this.ended = true; this._notify(); });
    sock.on('timeout', () => { this.sock.destroy(new Error('socket timeout')); });
  }

  _notify() {
    const r = this._resolvers.shift();
    if (r) r();
  }

  async _wait() {
    if (this.chunks.length > 0 || this.ended) return;
    await new Promise((resolve) => this._resolvers.push(resolve));
  }

  _available() {
    return this.chunks.reduce((n, c) => n + c.length, 0);
  }

  _take(n) {
    const out = [];
    let left = n;
    while (left > 0 && this.chunks.length > 0) {
      const c = this.chunks[0];
      if (c.length <= left) { out.push(c); left -= c.length; this.chunks.shift(); }
      else { out.push(c.subarray(0, left)); this.chunks[0] = c.subarray(left); left = 0; }
    }
    return Buffer.concat(out);
  }

  async readBytes(n) {
    while (this._available() < n) {
      if (this.ended) break;
      await this._wait();
    }
    return this._take(Math.min(n, this._available()));
  }

  async readLine() {
    for (;;) {
      const idx = this._findNewline();
      if (idx >= 0) {
        const buf = this._take(idx + 1);
        return buf.toString('utf-8').replace(/\r?\n$/, '');
      }
      if (this.ended) {
        return this._take(this._available()).toString('utf-8');
      }
      await this._wait();
    }
  }

  _findNewline() {
    let offset = 0;
    for (const c of this.chunks) {
      const i = c.indexOf(0x0a); // \n
      if (i >= 0) return offset + i;
      offset += c.length;
    }
    return -1;
  }

  async readToEnd() {
    while (!this.ended) await this._wait();
    return this._take(this._available());
  }
}

function frameHeader(channel, length) {
  const b = Buffer.alloc(5);
  b[0] = channel;
  b.writeUInt32BE(length, 1);
  return b;
}
