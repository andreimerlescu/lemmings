'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const http = require('node:http');
const fs = require('node:fs/promises');
const os = require('node:os');
const path = require('node:path');
const { spawn } = require('node:child_process');
const { once } = require('node:events');

async function run(cfg) {
  const child = spawn(process.execPath, [path.join(__dirname, 'runner.cjs')], { stdio: ['pipe', 'pipe', 'pipe'] });
  let out = '', err = '';
  child.stdout.on('data', b => out += b); child.stderr.on('data', b => err += b);
  child.stdin.end(JSON.stringify(cfg));
  const [code] = await once(child, 'close');
  assert.equal(code, 0, err);
  return JSON.parse(out);
}

test('real Chromium detects broken experiences and preserves browser sessions', { timeout: 45000 }, async () => {
  const requests = [];
  const server = http.createServer((req, res) => {
    requests.push({ url: req.url, cookie: req.headers.cookie, ua: req.headers['user-agent'] });
    res.setHeader('Content-Type', 'text/html');
    if (req.url === '/') {
      res.setHeader('Set-Cookie', 'session=private-cookie-value; Path=/; HttpOnly');
      return res.end('<html><head><title>Catalog</title></head><body><main id="app">Welcome</main><a href="/next">Next</a></body></html>');
    }
    if (req.url === '/next') return res.end('<html><head><title>Next</title></head><body><main id="app">Next page</main></body></html>');
    if (req.url === '/broken') return res.end('<html><head><title>Broken</title></head><body><main>Oops</main><img src="/missing.png"><script>throw new Error("application exploded")</script></body></html>');
    if (req.url === '/blank') return res.end('<html><head><title>Blank</title></head><body></body></html>');
    res.writeHead(404); res.end('missing');
  });
  server.listen(0, '127.0.0.1'); await once(server, 'listening');
  const root = `http://127.0.0.1:${server.address().port}`;
  const output = await fs.mkdtemp(path.join(os.tmpdir(), 'lemmings-browser-test-'));
  try {
    const report = await run({ url: root, users: 1, duration_ms: 15000, think_min_ms: 0, think_max_ms: 0, timeout_ms: 1000, max_pages: 4, output, scenario: { name: 'regression', steps: [
      { name: 'home', path: '/', expect: { status: [200], contains: ['Welcome'], selectors: ['#app'] } },
      { name: 'next', path: '/next', expect: { contains: ['Next page'] } },
      { name: 'broken', path: '/broken', expect: { contains: ['Welcome'] } },
      { name: 'blank', path: '/blank', expect: {} },
    ] } });
    assert.equal(report.visits, 4); assert.equal(report.failed, 2); assert.equal(report.cancelled, 0); assert.ok(!report.error, report.error);
    assert.equal(report.samples[0].failed, false); assert.equal(report.samples[1].failed, false);
    const broken = report.samples[2];
    assert.ok(broken.page_errors.some(x => x.includes('application exploded')));
    assert.ok(broken.failed_resources.some(x => x.includes('404')));
    assert.ok(broken.checks.some(x => x.name === 'contains[0]' && !x.passed));
    assert.ok((await fs.stat(broken.screenshot)).size > 0);
    assert.ok(report.samples[3].checks.some(x => x.name === 'visible-content' && !x.passed));
    const first = requests.find(x => x.url === '/'), next = requests.find(x => x.url === '/next');
    assert.equal(first.ua, next.ua); assert.ok(next.cookie.includes('private-cookie-value'));
    assert.ok(!JSON.stringify(report).includes('private-cookie-value'));
    assert.ok(report.response_codes[404] >= 1);
    assert.equal((await fs.readFile(path.join(report.output, 'visits.jsonl'), 'utf8')).trim().split('\n').length, 4);
  } finally { await new Promise(resolve => server.close(resolve)); await fs.rm(output, { recursive: true, force: true }); }
});
