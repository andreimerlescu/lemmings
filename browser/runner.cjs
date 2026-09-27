'use strict';

// Real browser evidence is deliberately a separate, small cohort. Its requests
// are additional to Go traffic, and never enter the Go HTTP latency histogram.
const { chromium } = require('playwright');
const fs = require('node:fs/promises');
const path = require('node:path');
const { once } = require('node:events');
const { createWriteStream } = require('node:fs');

const safeURL = raw => { try { const u = new URL(raw); u.username = ''; u.password = ''; u.search = ''; u.hash = ''; return u.href; } catch { return '[invalid URL]'; } };
const clip = value => String(value).slice(0, 500);
const cleanMessage = value => clip(String(value).replace(/https?:\/\/[^\s)"']+/g, safeURL));
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));

async function launch() {
  return chromium.launch({ headless: true, ...(process.env.LEMMINGS_CHROMIUM_PATH ? { executablePath: process.env.LEMMINGS_CHROMIUM_PATH } : {}) });
}

async function main() {
  if (process.argv.includes('--check')) {
    const browser = await launch();
    await browser.close();
    process.stdout.write('Chromium available\n');
    return;
  }
  let input = '';
  for await (const chunk of process.stdin) { input += chunk; if (input.length > 1 << 20) throw new Error('configuration exceeds 1 MiB'); }
  const cfg = JSON.parse(input);
  if (!Number.isInteger(cfg.users) || cfg.users < 1 || cfg.users > 32) throw new Error('users must be 1..32');
  const origin = new URL(cfg.url).origin;
  if (!/^https?:$/.test(new URL(cfg.url).protocol)) throw new Error('HTTP(S) target required');
  await fs.mkdir(cfg.output, { recursive: true });
  // Unique run subdirectory avoids overwriting earlier screenshots or traces.
  const output = await fs.mkdtemp(path.join(cfg.output, 'run-'));
  const trace = createWriteStream(path.join(output, 'visits.jsonl'), { flags: 'wx', mode: 0o600 });
  let traceError;
  trace.on('error', err => { traceError = err; });
  const report = { engine: 'chromium', sessions: cfg.users, visits: 0, failed: 0, cancelled: 0, response_codes: {}, samples: [], omitted: 0, output };
  const browser = await launch();
  if (cfg.stream_ready) process.stdout.write(JSON.stringify({ ready: true }) + "\n");
  let stopping = false;
  const contexts = new Set();
  const stop = () => { stopping = true; for (const c of contexts) c.close().catch(() => {}); };
  process.on('SIGINT', stop); process.on('SIGTERM', stop);
  try {
    const results = await Promise.allSettled(Array.from({ length: cfg.users }, async (_, user) => {
      const context = await browser.newContext({ viewport: user % 3 === 0 ? { width: 390, height: 844 } : { width: 1440, height: 900 }, isMobile: user % 3 === 0, hasTouch: user % 3 === 0, locale: 'en-US', serviceWorkers: 'block' });
      contexts.add(context);
      const page = await context.newPage();
      const deadline = Date.now() + cfg.duration_ms;
      let active, nextURL = cfg.url;
      // Do not load analytics, CDNs or off-origin redirects implicitly. Report
      // blocked dependencies so the user knows the verification scope.
      await context.route('**/*', route => {
        const u = route.request().url();
        if (/^https?:/.test(u) && new URL(u).origin !== origin) {
          if (active) active.blocked_external++;
          return route.abort('blockedbyclient');
        }
        return route.continue();
      });
      await context.addInitScript(() => {
        window.__lemmings = { lcp_ms: 0, cls: 0 };
        try { new PerformanceObserver(list => { for (const e of list.getEntries()) window.__lemmings.lcp_ms = e.startTime; }).observe({ type: 'largest-contentful-paint', buffered: true }); } catch {}
        try { new PerformanceObserver(list => { for (const e of list.getEntries()) if (!e.hadRecentInput) window.__lemmings.cls += e.value; }).observe({ type: 'layout-shift', buffered: true }); } catch {}
      });
      const add = (field, value) => { if (active && active[field].length < 20) active[field].push(cleanMessage(value)); };
      page.on('pageerror', err => add('page_errors', err.message));
      page.on('console', msg => { if (msg.type() === 'error') add('console_errors', msg.text()); });
      page.on('requestfailed', req => add('failed_resources', `${safeURL(req.url())}: ${req.failure()?.errorText || 'failed'}`));
      page.on('response', res => {
        report.response_codes[res.status()] = (report.response_codes[res.status()] || 0) + 1;
        if (res.status() >= 400 && res.request().resourceType() !== 'document') add('failed_resources', `${res.status()} ${safeURL(res.url())}`);
      });
      try {
        for (let sequence = 0; !stopping && Date.now() < deadline; sequence++) {
          if (cfg.max_pages > 0 && sequence >= cfg.max_pages) break;
          if (cfg.scenario && sequence >= cfg.scenario.steps.length) break;
          const step = cfg.scenario?.steps[sequence];
          if (step) nextURL = new URL(step.path, cfg.url).href;
          if (new URL(nextURL).origin !== origin) throw new Error('journey leaves origin');
          const expected = step?.expect || {};
          const started = Date.now();
          const v = active = { session: `browser-${user + 1}`, sequence: sequence + 1, step: step?.name || '', url: safeURL(nextURL), final_url: '', status: 0, title: '', failed: false, cancelled: false, checks: [], console_errors: [], page_errors: [], failed_resources: [], blocked_external: 0, screenshot: '', duration_ms: 0, performance: {} };
          const check = (name, passed, detail = '') => v.checks.push({ name, passed, detail });
          const remaining = () => Math.max(1, Math.min(cfg.timeout_ms || 10000, deadline - Date.now()));
          try {
            const response = await page.goto(nextURL, { waitUntil: 'domcontentloaded', timeout: remaining() });
            v.status = response?.status() || 0;
            v.final_url = safeURL(page.url());
            check('status', expected.status?.length ? expected.status.includes(v.status) : v.status >= 200 && v.status < 300, `received ${v.status}`);
            if (expected.content_type) check('content-type', (response?.headers()['content-type'] || '').split(';')[0].trim() === expected.content_type);
            for (const selector of expected.selectors || []) {
              try { await page.locator(selector).first().waitFor({ state: 'visible', timeout: remaining() }); check(`selector:${selector}`, true); }
              catch { check(`selector:${selector}`, false, 'not visible before timeout'); }
            }
            // An explicit short observation window avoids hanging on sites with
            // long polling. Performance values describe this window, not a full
            // Web Vitals field measurement or an interaction score such as INP.
            await delay(Math.max(0, Math.min(500, deadline - Date.now())));
            v.title = clip(await page.title());
            const text = await page.locator('body').innerText({ timeout: remaining() });
            const visible = await page.locator('body').evaluate(body => {
              const r = body.getBoundingClientRect(), s = getComputedStyle(body);
              return r.width > 0 && r.height > 0 && s.display !== 'none' && s.visibility !== 'hidden' && Number(s.opacity) > 0 && (body.innerText.trim().length > 0 || [...body.querySelectorAll('img,canvas,svg,video')].some(e => e.getBoundingClientRect().width > 0));
            });
            check('visible-content', visible, 'body has visible dimensions and text or media');
            for (const [i, value] of (expected.contains || []).entries()) check(`contains[${i}]`, text.includes(value));
            for (const [i, value] of (expected.not_contains || []).entries()) check(`not-contains[${i}]`, !text.includes(value));
            if (expected.title_contains) check('title', v.title.includes(expected.title_contains));
            const broken = await page.locator('img').evaluateAll(images => images.filter(i => i.complete && i.currentSrc && i.naturalWidth === 0).length);
            check('images', broken === 0, `${broken} completed images failed to decode`);
            check('javascript', v.page_errors.length === 0, `${v.page_errors.length} uncaught errors (up to 20 retained)`);
            check('console', v.console_errors.length === 0, `${v.console_errors.length} console errors (up to 20 retained)`);
            check('resources', v.failed_resources.length === 0 && v.blocked_external === 0, `${v.failed_resources.length} failed resources retained, ${v.blocked_external} blocked external requests`);
            v.performance = await page.evaluate(() => {
              const n = performance.getEntriesByType('navigation')[0];
              return { dom_content_loaded_ms: n?.domContentLoadedEventEnd || 0, load_ms: n?.loadEventEnd || 0, ttfb_ms: n?.responseStart || 0, fcp_ms: performance.getEntriesByName('first-contentful-paint')[0]?.startTime || 0, ...window.__lemmings };
            });
            if (!step) {
              const links = await page.locator('a[href]').evaluateAll(els => els.map(e => e.href).filter(h => { try { return new URL(h).origin === location.origin && /^https?:/.test(h); } catch { return false; } }).slice(0, 1000));
              if (links.length) nextURL = links[Math.floor(Math.random() * links.length)];
            }
          } catch (err) {
            v.cancelled = stopping || Date.now() >= deadline;
            if (!v.cancelled) check('navigation', false, cleanMessage(err.message));
          }
          v.duration_ms = Date.now() - started;
          v.failed = !v.cancelled && v.checks.some(c => !c.passed);
          if (v.failed && !page.isClosed()) {
            try { v.screenshot = path.join(output, `${v.session}-${v.sequence}.png`); await page.screenshot({ path: v.screenshot, timeout: 3000 }); }
            catch { v.screenshot = ''; }
          }
          active = undefined;
          report.visits++; if (v.failed) report.failed++; if (v.cancelled) report.cancelled++;
          if (report.samples.length === 200) { report.samples.shift(); report.omitted++; }
          report.samples.push(v);
          if (traceError) throw traceError;
          if (!trace.write(JSON.stringify(v) + '\n')) await once(trace, 'drain');
          const pause = cfg.think_min_ms + Math.random() * Math.max(0, cfg.think_max_ms - cfg.think_min_ms);
          await delay(Math.max(0, Math.min(pause, deadline - Date.now())));
        }
      } finally { contexts.delete(context); await context.close(); }
    }));
    for (const result of results) if (result.status === 'rejected') report.error = cleanMessage(result.reason?.message || result.reason);
  } catch (err) { report.error = cleanMessage(err.message); }
  finally {
    await browser.close();
    trace.end();
    if (!traceError) await once(trace, 'finish');
    if (traceError) report.error = cleanMessage(traceError.message);
    await fs.writeFile(path.join(output, 'summary.json'), JSON.stringify(report, null, 2), { mode: 0o600 });
    process.stdout.write(JSON.stringify(report));
  }
}
main().catch(err => { process.stderr.write(`${err.message}\n`); process.exitCode = 1; });
