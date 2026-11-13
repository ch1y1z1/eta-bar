import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const bar = readFileSync(new URL('./eta.bar', import.meta.url), 'utf8');
const script = bar.match(/<script(?: apis="localStorage")?><!\[CDATA\[([\s\S]*?)\]\]><\/script>/)[1];
const fixture = JSON.parse(readFileSync(new URL('../examples/demo/monitor.jsonl', import.meta.url), 'utf8').trim());
const envelope = data => ({ data, collection: { state: data ? 'ok' : 'unreachable',
  observed_at: new Date().toISOString(), last_success_at: data ? new Date().toISOString() : null,
  error: data ? null : { code: 'source_unavailable', message: 'Source is unavailable' }, window: 'bounded_tail' } });
const response = data => ({ ok: true, json: async () => envelope(data) });
function worker(fetch, localStorage = memoryStorage(), clock = { now: Date.now() }) {
  class Clock extends Date {
    constructor(...args) { super(...(args.length ? args : [clock.now])); }
    static now() { return clock.now; }
  }
  const context = vm.createContext({ URL, AbortSignal, fetch, localStorage, Date: Clock,
    $metrix: { secret: name => { assert.equal(name, 'token'); return 'fixture-token'; } } });
  vm.runInContext(script.replace('export default {', 'globalThis.dashboard = {') +
    '\nglobalThis.current = () => ({snapshot, collection, history, storageNotice});' +
    '\nglobalThis.inspect = { cleanHistory, historySeries };', context);
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

function memoryStorage(initial = null) {
  let value = initial;
  return { getItem: () => value, setItem: (key, next) => { assert.equal(key, 'eta-history-v1'); value = next; } };
}
function fresh(clock, seq = 1, seconds = 480, etaTime = clock.now) {
  const data = structuredClone(fixture);
  data.seq = seq;
  data.timestamp = new Date(clock.now).toISOString();
  for (const task of data.tasks) task.eta.updated_at = new Date(etaTime).toISOString();
  data.tasks[1].eta.remaining_seconds = seconds;
  return data;
}
const runs = w => w.current().history.sources.flatMap(s => s.runs);
const points = w => runs(w).flatMap(r => r.points);
const chart = view => {
  if (view.type === 'Chart' && view.props.type === 'line') return view;
  for (const child of view.children || []) { const found = chart(child); if (found) return found; }
};

test('ETA timestamp/value dedupe ignores heartbeat and seq; renewed fixed demo is flat', async () => {
  const clock = { now: Date.now() }, etaTime = clock.now;
  let data = fresh(clock);
  const w = worker(async () => response(data), memoryStorage(), clock);
  await w.dashboard.scheduled(env());
  clock.now += 30000;
  data = fresh(clock, 2, 480, etaTime);
  await w.dashboard.scheduled(env());
  assert.equal(points(w).length, 1);
  clock.now += 30000;
  data = fresh(clock, 3);
  await w.dashboard.scheduled(env());
  assert.equal(points(w).length, 2);
  assert.equal(points(w)[0].seconds, points(w)[1].seconds);
  const demo = worker(() => assert.fail('offline demo'), memoryStorage(), clock);
  await demo.dashboard.scheduled({ inputs: { mode: 'demo' } });
  clock.now += 30000;
  await demo.dashboard.scheduled({ inputs: { mode: 'demo' } });
  assert.deepEqual(Array.from(points(demo), p => p.seconds), [1380, 1380]);
});

test('unknown, invalid, unreachable and stale observations break spans, without recording frozen ETA', async () => {
  for (const failure of ['unknown', 'invalid', 'unreachable', 'stale', 'eta-stale', 'future']) {
    const clock = { now: Date.now() };
    let data = fresh(clock), unreachable = false;
    const w = worker(async () => unreachable ? { ok: false } : response(data), memoryStorage(), clock);
    await w.dashboard.scheduled(env());
    clock.now += 30000;
    data = fresh(clock, 2);
    if (failure === 'unknown') data.tasks[1].eta = { remaining_seconds: null, method: 'unknown', updated_at: data.timestamp };
    if (failure === 'invalid') data.schema_version = 'broken';
    if (failure === 'unreachable') unreachable = true;
    if (failure === 'stale') data.timestamp = new Date(clock.now - 121000).toISOString();
    if (failure === 'eta-stale') data.tasks[1].eta.updated_at = new Date(clock.now - 121000).toISOString();
    if (failure === 'future') data.timestamp = new Date(clock.now + 10000).toISOString();
    await w.dashboard.scheduled(env());
    assert.equal(points(w).length, 1, failure);
    clock.now += 30000;
    data = fresh(clock, 3); unreachable = false;
    await w.dashboard.scheduled(env());
    assert.equal(points(w).length, 2, failure);
    assert.equal(points(w)[1].gap, true, failure);
    assert.equal(chart(w.dashboard.render(env()).panel).props.series.length, 2, failure);
  }
});

test('time axis uses real observation milliseconds and long gaps split across worker reload', async () => {
  const clock = { now: Date.now() }, storage = memoryStorage();
  let data = fresh(clock);
  let w = worker(async () => response(data), storage, clock);
  await w.dashboard.scheduled(env());
  clock.now += 30000; data = fresh(clock, 2);
  await w.dashboard.scheduled(env());
  clock.now += 90000; data = fresh(clock, 3);
  await w.dashboard.scheduled(env());
  let props = chart(w.dashboard.render(env()).panel).props;
  assert.equal(props.xKind, 'time');
  const xs = props.series[0].points.map(p => p.x);
  assert.equal(xs[2] - xs[1], 3 * (xs[1] - xs[0]));
  clock.now += 180000; data = fresh(clock, 4);
  w = worker(async () => response(data), storage, clock);
  await w.dashboard.scheduled(env());
  props = chart(w.dashboard.render(env()).panel).props;
  assert.equal(points(w).length, 4);
  assert.equal(props.series.length, 2);
});

test('rendering stale data adds no points; renewed identical estimate keeps pending gap', async () => {
  const clock = { now: Date.now() }, etaTime = clock.now;
  let data = fresh(clock);
  const w = worker(async () => response(data), memoryStorage(), clock);
  await w.dashboard.scheduled(env());
  clock.now += 121000;
  w.dashboard.render(env());
  assert.equal(points(w).length, 1);
  data = fresh(clock, 2, 480, etaTime);
  await w.dashboard.scheduled(env({ etaValiditySeconds: 1000 }));
  assert.equal(points(w).length, 1);
  clock.now += 30000; data = fresh(clock, 3);
  await w.dashboard.scheduled(env());
  assert.equal(points(w)[1].gap, true);
});

test('source, run and attempt histories remain isolated and reload survives', async () => {
  const clock = { now: Date.now() }, storage = memoryStorage();
  let data = fresh(clock);
  const w = worker(async () => response(data), storage, clock);
  await w.dashboard.scheduled(env());
  clock.now += 30000; data = fresh(clock, 2); data.attempt_id = 'retry';
  await w.dashboard.scheduled(env());
  clock.now += 30000; data = fresh(clock); data.run_id = 'another-run';
  await w.dashboard.scheduled(env({ logPath: '/logs/another.jsonl' }));
  assert.equal(w.current().history.sources.length, 2);
  assert.equal(runs(w).length, 3);
  assert.equal(chart(w.dashboard.render(env({ logPath: '/logs/another.jsonl' })).panel).props.series[0].points.length, 1);
  const reloaded = worker(async () => response(data), storage, clock);
  await reloaded.dashboard.scheduled(env({ logPath: '/logs/another.jsonl' }));
  assert.equal(points(reloaded).length, 3);
  assert.doesNotMatch(storage.getItem(), /fixture-token|must-not-use-env-input/);
});

test('confirmed terminal success contributes zero once, failure never does', async () => {
  for (const state of ['succeeded', 'failed']) {
    const clock = { now: Date.now() }, storage = memoryStorage();
    let data = fresh(clock);
    let w = worker(async () => response(data), storage, clock);
    await w.dashboard.scheduled(env());
    clock.now += 30000; data = fresh(clock, 2); data.state = state;
    if (state === 'succeeded') for (const t of data.tasks) { t.state = state; t.progress.completed = t.progress.total; }
    await w.dashboard.scheduled(env());
    clock.now += 30000; data.seq += 1; data.timestamp = new Date(clock.now).toISOString();
    w = worker(async () => response(data), storage, clock);
    await w.dashboard.scheduled(env());
    assert.equal(points(w).filter(p => p.seconds === 0).length, state === 'succeeded' ? 1 : 0);
  }
});

test('corrupt, unavailable and full storage do not break live progress or claim persistence', async () => {
  const clock = { now: Date.now() };
  for (const [storage, writable] of [[memoryStorage('{bad json'), true], [null, false],
    [{ getItem() { throw Error('denied'); }, setItem() { throw Error('quota'); } }, false]]) {
    const w = worker(async () => response(fresh(clock)), storage, clock);
    assert.match(w.current().storageNotice, /不可读/);
    await w.dashboard.scheduled(env());
    assert.ok(w.current().snapshot);
    assert.equal(points(w).length, 1);
    if (writable) {
      // A repaired JSON store can persist the new observation normally.
      assert.equal(w.current().storageNotice, '');
    } else assert.match(w.current().storageNotice, /未保存/);
  }
  const w = worker(async () => response(fresh(clock)), memoryStorage(), clock);
  const dirty = { version: 1, sources: Array.from({ length: 10 }, (_, s) => ({ key: `source-${s}`,
    runs: Array.from({ length: 10 }, (_, r) => ({ id: `run-${r}`, lastObserved: clock.now,
      points: Array.from({ length: 800 }, (_, i) => ({ t: clock.now - 800000 + i * 1000,
        seconds: i, estimate: `estimate-${i}`, gap: false })) })) })) };
  dirty.sources[0].runs[0].points.push({ t: clock.now + 100000, seconds: -1, estimate: 'bad' });
  const cleaned = w.inspect.cleanHistory(dirty, clock.now);
  assert.ok(cleaned.sources.length <= 8);
  for (const source of cleaned.sources) {
    assert.ok(source.runs.length <= 8);
    for (const run of source.runs) assert.ok(run.points.length <= 720);
  }
  assert.ok(JSON.stringify(cleaned).length <= 1000000);
  assert.equal(w.inspect.cleanHistory(dirty, clock.now + 86401000).sources.length, 0);
});

test('A to B to A async switching rejects the first A generation and preserves new history', async () => {
  const clock = { now: Date.now() };
  let resolveOld, first = true;
  const w = worker(async () => {
    if (first) { first = false; return new Promise(resolve => { resolveOld = resolve; }); }
    return response(fresh(clock));
  }, memoryStorage(), clock);
  const old = w.dashboard.scheduled(env());
  await w.dashboard.scheduled(env({ logPath: '/logs/b.jsonl' }));
  clock.now += 30000;
  await w.dashboard.scheduled(env());
  const before = JSON.stringify(w.current().history);
  const obsolete = fresh(clock, 99, 999);
  resolveOld(response(obsolete)); await old;
  assert.equal(w.current().snapshot.seq, 1);
  assert.equal(JSON.stringify(w.current().history), before);
  assert.equal(points(w).length, 2);
  assert.equal((JSON.stringify(w.dashboard.render(env()).panel).match(/"type":"Button"/g) || []).length, 1);
});

test('failed first collection after reload preserves an explicit gap even in a short outage', async () => {
  const clock = { now: Date.now() }, storage = memoryStorage();
  let data = fresh(clock), offline = false;
  const fetch = async () => offline ? { ok: false } : response(data);
  let w = worker(fetch, storage, clock);
  await w.dashboard.scheduled(env());
  clock.now += 30000; offline = true;
  w = worker(fetch, storage, clock);
  await w.dashboard.scheduled(env());
  assert.equal(w.current().snapshot, null);
  assert.equal(runs(w)[0].gap, true);
  clock.now += 30000; offline = false; data = fresh(clock, 2);
  await w.dashboard.scheduled(env());
  assert.equal(chart(w.dashboard.render(env()).panel).props.series.length, 2);
});
