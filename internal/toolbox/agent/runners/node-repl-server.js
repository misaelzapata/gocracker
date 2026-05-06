#!/usr/bin/env node
// node-repl-server.js — long-lived V8 REPL listening on a UDS, used by
// the toolbox agent to back the "node-warm" exec mode.
//
// Why: every fresh `node` invocation pays ~25–50 ms of V8 startup on
// alpine/musl. When a sandbox snapshot is taken with this server idle
// on its socket, post-restore eval requests skip V8 init entirely and
// land at ~5–10 ms instead of ~36 ms (host-side restore + dial + V8
// init dominated the latter; only the first two stay in the new path).
//
// Wire format (line-delimited JSON, both directions):
//
//   request:  {"id": <int>, "code": "<string>", "timeout_ms": <int>}
//   response: {"id": <int>, "stdout": "<string>", "stderr": "<string>",
//              "result": "<string|null>", "error": "<string|null>",
//              "exit_code": <int>}
//
// One eval at a time per connection (the REPL is single-threaded V8;
// concurrent connections from the agent serialise on a per-conn queue).
// On error in user code we report `error`+`stderr`, exit_code=1; the
// connection stays open so subsequent requests aren't penalised by the
// reconnect cost. The agent spawns one of these per VM and reuses the
// connection across many exec calls.

'use strict';
const net = require('net');
const fs  = require('fs');
const vm  = require('vm');
const path = require('path');

const SOCK_PATH    = process.env.GOCRACKER_NODE_WARM_SOCK    || '/run/gocracker/warm-node.sock';
const READY_PATH   = process.env.GOCRACKER_NODE_WARM_READY   || '/run/gocracker/warm-node.ready';
const DEFAULT_TIMEOUT_MS = 5000;

try { fs.mkdirSync(path.dirname(SOCK_PATH), { recursive: true }); } catch {}
try { fs.unlinkSync(SOCK_PATH); } catch {}

// Per-connection: read newline-delimited JSON, eval, write response.
// We keep ONE shared sandbox across requests so callers can build up
// state ("global.foo = 1" then later "console.log(global.foo)" —
// returns 1). That mirrors how a long-lived REPL feels and is the
// whole point of the warm path. Callers that want isolation should
// wrap their code in an IIFE or fall back to the regular `node` exec.
const sandbox = vm.createContext({ console, process, require, Buffer, setTimeout, setInterval, clearTimeout, clearInterval });

function handleConnection(conn) {
    let buf = '';
    conn.on('data', (chunk) => {
        buf += chunk.toString('utf8');
        let nl;
        while ((nl = buf.indexOf('\n')) !== -1) {
            const line = buf.slice(0, nl);
            buf = buf.slice(nl + 1);
            if (line.length === 0) continue;
            handleRequest(conn, line);
        }
    });
    conn.on('error', () => { /* ignore — agent will redial */ });
}

function handleRequest(conn, line) {
    let req;
    try { req = JSON.parse(line); }
    catch (e) {
        conn.write(JSON.stringify({ id: 0, stdout: '', stderr: '', result: null, error: 'bad request: ' + e.message, exit_code: 2 }) + '\n');
        return;
    }
    const id = req.id || 0;
    const code = req.code || '';
    const timeout = req.timeout_ms || DEFAULT_TIMEOUT_MS;

    // Capture stdout/stderr written by the eval'd code. We rebind
    // process.stdout.write & .stderr.write rather than swapping
    // console.log — this catches direct writes from native modules
    // and process.exit.callbacks too.
    let stdout = '';
    let stderr = '';
    const origOut = process.stdout.write.bind(process.stdout);
    const origErr = process.stderr.write.bind(process.stderr);
    process.stdout.write = (chunk) => { stdout += String(chunk); return true; };
    process.stderr.write = (chunk) => { stderr += String(chunk); return true; };

    let result = null;
    let error = null;
    let exit_code = 0;
    try {
        const v = vm.runInContext(code, sandbox, { timeout });
        if (v !== undefined) result = String(v);
    } catch (e) {
        error = e && e.stack ? String(e.stack) : String(e);
        exit_code = 1;
    } finally {
        process.stdout.write = origOut;
        process.stderr.write = origErr;
    }

    conn.write(JSON.stringify({
        id, stdout, stderr, result, error, exit_code
    }) + '\n');
}

const server = net.createServer(handleConnection);
server.on('error', (e) => { process.stderr.write('warm-runner listen error: ' + e.message + '\n'); process.exit(1); });

server.listen(SOCK_PATH, () => {
    fs.chmodSync(SOCK_PATH, 0o660);
    // Touch the ready file LAST so any host poll for "is the runner up?"
    // is a one-shot stat() that can't observe a half-initialised state.
    fs.writeFileSync(READY_PATH, String(Date.now()) + '\n');
    process.stdout.write('READY ' + SOCK_PATH + '\n');
});
