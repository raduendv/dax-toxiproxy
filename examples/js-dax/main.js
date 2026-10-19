'use strict';

// This is a test process, not a production routing library. The pinned SDK's
// internal Connector is patched before constructing DAX and restored at shutdown.
// HTTP controls Toxiproxy; DAX protocol bytes travel over separate TCP sockets.
const net = require('node:net');
const assert = require('node:assert/strict');
const { setTimeout: sleep } = require('node:timers/promises');
const { Connector } = require('amazon-dax-client/src/Tube');
const AmazonDaxClient = require('amazon-dax-client');

class Router {
  constructor(api) {
    const address = new URL(api);
    if (address.protocol !== 'http:' || address.hostname !== '127.0.0.1' || !address.port || address.pathname !== '/' || address.search || address.hash || address.username || address.password) {
      throw new Error('TOXIPROXY_API must be http://127.0.0.1:PORT');
    }
    this.api = address.origin;
    this.prefix = `js-dax-${process.pid}-${Date.now()}`;
    this.routes = new Map();
    this.latency = 0;
    this.queue = Promise.resolve();
    this.failure = null;
    this.closed = false;
  }

  async request(method, path, body) {
    const response = await fetch(this.api + path, {
      method, redirect: 'error', signal: AbortSignal.timeout(5000),
      headers: { 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    if (!response.ok) throw new Error(`${method} ${path}: ${response.status} ${(await response.text()).slice(0, 1024)}`);
    const text = await response.text();
    return text ? JSON.parse(text) : undefined;
  }

  // Discovery and pool creation may overlap. Serialize mapping and phase changes
  // so a new listener cannot escape the fault currently being tested.
  exclusive(operation) {
    const result = this.queue.then(async () => {
      if (this.closed) throw new Error('router closed');
      if (this.failure) throw this.failure;
      try { return await operation(); }
      catch (error) { this.failure = error; throw error; }
    });
    this.queue = result.catch(() => {});
    return result;
  }

  toxic(mapping, milliseconds) {
    return this.request('POST', `/proxies/${mapping.name}/toxics`, {
      name: 'example_latency', type: 'latency', stream: 'downstream', toxicity: 1,
      attributes: { latency: milliseconds, jitter: 0 },
    });
  }

  mapping(host, port) {
    return this.exclusive(async () => {
      if (Number(port) !== 8111 || !host || host.includes(':')) throw new Error('This example supports plaintext IPv4 DAX on port 8111 only');
      const upstream = `${host}:8111`;
      if (this.routes.has(upstream)) return this.routes.get(upstream);
      const wanted = { name: `${this.prefix}-${this.routes.size}`, listen: '127.0.0.1:0', upstream, enabled: true };
      // Record the name even if the POST succeeds but its response is lost.
      this.routes.set(upstream, wanted);
      const created = await this.request('POST', '/proxies', wanted);
      if (created.name !== wanted.name || created.upstream !== upstream || !created.enabled || !/^127\.0\.0\.1:[1-9][0-9]*$/.test(created.listen)) throw new Error('Invalid proxy mapping');
      this.routes.set(upstream, created);
      if (this.latency) await this.toxic(created, this.latency);
      console.log(`route ${created.name}: ${created.listen} -> ${upstream}`);
      return created;
    });
  }

  setLatency(milliseconds) {
    return this.exclusive(async () => {
      for (const mapping of this.routes.values()) {
        if (milliseconds) await this.toxic(mapping, milliseconds);
        else if (this.latency) await this.request('DELETE', `/proxies/${mapping.name}/toxics/example_latency`);
      }
      this.latency = milliseconds;
    });
  }

  async close() {
    await this.queue;
    this.closed = true;
    // Never call the global /reset endpoint; delete only names owned by this run.
    const outcomes = await Promise.allSettled([...this.routes.values()].map(mapping => this.request('DELETE', `/proxies/${mapping.name}`)));
    const failures = outcomes.filter(result => result.status === 'rejected').map(result => result.reason);
    if (failures.length) throw new AggregateError(failures, 'Proxy cleanup failed');
  }
}

function installRouting(router) {
  const original = Connector.prototype.connect;
  if (typeof original !== 'function') throw new Error('Unsupported SDK Connector; keep amazon-dax-client pinned');
  Connector.prototype.connect = function connect(callback) {
    // Never forward TLS into a plaintext test or silently connect directly.
    if (this._protocol !== net || !this._connectOps) throw new Error('Only the pinned plaintext DAX connector is supported');
    const { host, port } = this._connectOps;
    // The SDK requires a socket synchronously. Return it now, connect only after
    // the asynchronous HTTP mapping/toxic setup has completed, then notify the SDK.
    const socket = new net.Socket();
    router.mapping(host, port).then(mapping => {
      if (socket.destroyed) return;
      const [listenHost, listenPort] = mapping.listen.split(':');
      socket.connect({ host: listenHost, port: Number(listenPort) }, callback);
    }).catch(error => socket.destroy(error));
    return socket;
  };
  return () => { Connector.prototype.connect = original; };
}

function positiveNumber(name, fallback) {
  const value = Number(process.env[name] || fallback);
  if (!Number.isFinite(value) || value <= 0) throw new Error(`${name} must be positive`);
  return value;
}

async function main() {
  const router = new Router(process.env.TOXIPROXY_API || 'http://127.0.0.1:18474');
  const restore = installRouting(router);
  let client;
  let stopping = false;
  const stop = () => { stopping = true; };
  process.on('SIGINT', stop);
  process.on('SIGTERM', stop);
  try {
    await router.request('GET', '/version');
    if (process.argv.includes('--self-test')) {
      // Exercise the real patched connector, mapping reuse, new-node fault
      // inheritance, and recovery against Toxiproxy, without AWS calls.
      await new Promise((resolve, reject) => {
        const connector = new Connector(false, '127.0.0.1', 8111, false);
        const socket = connector.connect(() => { socket.destroy(); resolve(); });
        socket.on('error', reject);
      });
      await router.mapping('127.0.0.1', 8111);
      assert.equal(router.routes.size, 1);
      await router.setLatency(100);
      await router.mapping('127.0.0.2', 8111);
      await router.setLatency(0);
      assert.equal(router.routes.size, 2);
      console.log('JavaScript routing self-test passed');
      return;
    }
    const endpoint = process.env.DAX_ENDPOINT;
    if (!endpoint || !endpoint.startsWith('dax://') || !process.env.TABLE_NAME) throw new Error('Set DAX_ENDPOINT=dax://... and TABLE_NAME');
    const key = JSON.parse(process.env.DAX_KEY_JSON || '{"pk":{"S":"pk#1"},"sk":{"S":"sk#1"}}');
    const duration = positiveNumber('DAX_DURATION_SECONDS', 10) * 1000;
    const timeout = positiveNumber('DAX_TIMEOUT_MS', 200);
    const latency = positiveNumber('DAX_LATENCY_MS', 1000);
    if (!Number.isInteger(latency)) throw new Error('DAX_LATENCY_MS must be a whole number');
    // Keep the real endpoint: discovery returns node addresses, and the connector
    // adapter must see every one of them. The normal AWS credential chain is used.
    client = new AmazonDaxClient({ endpoints: [endpoint], region: process.env.AWS_REGION || 'eu-west-1', maxRetries: 0, readRetries: 0, writeRetries: 0, requestTimeout: timeout, connectTimeout: timeout });
    for (const phase of ['healthy', 'fault', 'recovery']) {
      if (stopping) throw new Error('Interrupted');
      await router.setLatency(phase === 'fault' ? latency : 0);
      const result = { phase, successes: 0, failures: 0 };
      const end = Date.now() + duration;
      while (Date.now() < end && !stopping) {
        try {
          // Low-level typed key JSON works for S/N keys. Missing items still count
          // as successful requests; this example does not create or write data.
          await client.getItem({ TableName: process.env.TABLE_NAME, Key: key }).promise();
          result.successes++;
        } catch (error) { result.failures++; result.last_error = error.message; }
        await sleep(100);
      }
      console.log(JSON.stringify(result));
      if (stopping) throw new Error('Interrupted');
      if (phase === 'healthy' && (!result.successes || result.successes / (result.successes + result.failures) < 0.95)) throw new Error('Healthy prerequisite failed; check key, credentials, connectivity, and timeout');
      if (phase === 'recovery' && !result.successes) throw new Error('No recovery progress');
    }
  } finally {
    // Stop background discovery before removing proxies or restoring the adapter.
    try { if (client) client.shutdown(); }
    finally {
      try { await router.close(); }
      finally { restore(); process.off('SIGINT', stop); process.off('SIGTERM', stop); }
    }
  }
}

module.exports = { Router, installRouting };
if (require.main === module) main().catch(error => { console.error(error); process.exitCode = 1; });