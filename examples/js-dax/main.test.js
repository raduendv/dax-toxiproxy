'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const http = require('node:http');
const { Router } = require('./main');

test('mapping fails closed when control API fails', async () => {
  const server = http.createServer((request, response) => { response.writeHead(503); response.end('unavailable'); });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  try {
    const router = new Router(`http://127.0.0.1:${server.address().port}`);
    await assert.rejects(router.mapping('node.example', 8111), /503/);
    await assert.rejects(router.mapping('other.example', 8111), /503/);
    assert.equal(router.routes.size, 1);
  } finally { await new Promise(resolve => server.close(resolve)); }
});

test('remote control APIs are refused', () => {
  assert.throws(() => new Router('http://192.0.2.1:8474'));
  assert.throws(() => new Router('http://127.0.0.1:8474/path'));
});