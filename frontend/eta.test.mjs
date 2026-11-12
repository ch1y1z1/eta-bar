import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const bar = readFileSync(new URL('./eta.bar', import.meta.url), 'utf8');
const script = bar.match(/<script><!\[CDATA\[([\s\S]*?)\]\]><\/script>/)[1];
const fixture = JSON.parse(readFileSync(new URL('../examples/demo/monitor.jsonl', import.meta.url), 'utf8').trim());
const envelope = data => ({ data, collection: { state: data ? 'ok' : 'unreachable',
  observed_at: new Date().toISOString(), last_success_at: data ? new Date().toISOString() : null,
  error: data ? null : { code: 'source_unavailable', message: 'Source is unavailable' }, window: 'bounded_tail' } });
const response = data => ({ ok: true, json: async () => envelope(data) });
function worker(fetch) {
  const context = vm.createContext({ URL, AbortSignal, fetch,
    $metrix: { secret: name => { assert.equal(name, 'token'); return 'fixture-token'; } } });
  vm.runInContext(script.replace('export default {', 'globalThis.dashboard = {') +
    '\nglobalThis.current = () => ({snapshot, collection});', context);
  return context;
}
const env = (extra = {}) => ({ inputs: { mode: 'api', baseURL: 'http://backend.example.org:8080',
  sourceHost: 'local', logPath: '/logs/one monitor.jsonl', runID: 'unused-legacy-id',
  token: 'must-not-use-env-input', ...extra } });

test('path source encodes host/path, discovers run ID and reads native secret', async () => {
  let requested;
  const w = worker(async (url, options) => { requested = { url: new URL(url), options }; return response(fixture); });
  await w.dashboard.scheduled(env());
  assert.equal(requested.url.pathname, '/v1/snapshot');
  assert.equal(requested.url.searchParams.get('path'), '/logs/one monitor.jsonl');
  assert.equal(requested.url.searchParams.get('host'), 'local');
  assert.equal(requested.url.searchParams.has('runID'), false);
  assert.equal(requested.options.headers.Authorization, 'Bearer fixture-token');
  assert.equal(requested.options.redirect, 'error');
  assert.equal(w.current().snapshot.run_id, fixture.run_id);
  assert.match(bar, /<network origin="\{\{ inputs.baseURL \}\}" allow-http="true"/);
});

test('path and host changes clear data, ignore in-flight old responses and isolate failures', async () => {
  let resolve;
  let pending = false;
  let failed = false;
  const w = worker(async () => pending ? new Promise(done => { resolve = done; }) : response(failed ? null : fixture));
  const original = env();
  await w.dashboard.scheduled(original);
  assert.ok(w.current().snapshot);
  pending = true;
  const oldRequest = w.dashboard.scheduled(original);
  const next = env({ logPath: '/logs/two.jsonl' });
  w.dashboard.render(next);
  assert.equal(w.current().snapshot, null);
  resolve(response(fixture));
  await oldRequest;
  assert.equal(w.current().snapshot, null);
  pending = false;
  failed = true;
  await w.dashboard.scheduled(next);
  assert.equal(w.current().snapshot, null);
  assert.equal(w.current().collection.state, 'unreachable');
  failed = false;
  await w.dashboard.scheduled(next);
  assert.ok(w.current().snapshot);
  w.dashboard.render(env({ logPath: next.inputs.logPath, sourceHost: 'cluster' }));
  assert.equal(w.current().snapshot, null);
});

test('blank path retains legacy route and run matching; demo stays offline', async () => {
  let url;
  const w = worker(async value => { url = value; return response(fixture); });
  await w.dashboard.scheduled(env({ logPath: '', runID: fixture.run_id }));
  assert.equal(new URL(url).pathname, `/v1/runs/${fixture.run_id}/snapshot`);
  assert.ok(w.current().snapshot);
  await w.dashboard.scheduled(env({ logPath: '', runID: 'different' }));
  assert.equal(w.current().snapshot, null);
  assert.equal(w.current().collection.error.code, 'INVALID_ENVELOPE');
  const demo = worker(() => { throw new Error('demo must not fetch'); });
  await demo.dashboard.scheduled({ inputs: { mode: 'demo' } });
  assert.equal(demo.current().snapshot.tasks[1].progress.completed, 17);
});
