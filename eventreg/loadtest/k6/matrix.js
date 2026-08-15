// matrix.js — a FIXED NUMBER of requests against the hottest read path, so
// results can be quoted as "N requests took T seconds at p95 X ms".
//
// WHY A THIRD SCRIPT, alongside launch-burst.js and sustained.js:
//
//   launch-burst.js  proves CORRECTNESS under contention (no oversell). Its
//                    latency is queueing delay by design and must never be
//                    quoted as service time.
//   sustained.js     proves THROUGHPUT with an open model (constant arrival
//                    rate). Answers "how many req/s before it bends".
//   matrix.js        answers "what does a run of exactly 1k / 10k / 100k
//                    requests cost", and lets the SAME workload be replayed
//                    against 2 replicas and against 1 for a clean comparison.
//
// EXECUTOR: shared-iterations — a CLOSED model. VUS virtual users share a pool
// of REQUESTS iterations and each fires the next request as soon as its
// previous one returns. That is the right model here because the question is
// "how long does a fixed batch of work take", not "can it absorb a fixed
// arrival rate". VUS is held constant across every tier so the tiers are
// comparable to each other; changing it changes the offered concurrency and
// therefore every number below.
//
// RUN:
//   docker run --rm --network eventreg_default -e BASE_URL=http://nginx:80 \
//     -e REQUESTS=10000 -e VUS=50 -e LIMIT=20 \
//     -v "$PWD/loadtest/k6:/scripts:ro" grafana/k6 run /scripts/matrix.js

import http from 'k6/http';
import { check } from 'k6';
import { Trend } from 'k6/metrics';
import exec from 'k6/execution';

const BASE = __ENV.BASE_URL || 'http://localhost:8080';
const REQUESTS = parseInt(__ENV.REQUESTS || '1000', 10);
const VUS = parseInt(__ENV.VUS || '50', 10);
const LIMIT = parseInt(__ENV.LIMIT || '20', 10);   // page size for GET /events
const LABEL = __ENV.LABEL || 'default';
const PASSWORD = 'loadtest-password-123';

const readLatency = new Trend('read_latency', true);

export const options = {
  setupTimeout: '2m',
  // p(99) is NOT in k6's default trend stats (avg/min/med/max/p90/p95), so it
  // has to be requested explicitly or handleSummary reads undefined.
  summaryTrendStats: ['avg', 'min', 'med', 'max', 'p(95)', 'p(99)'],
  scenarios: {
    fixed_batch: {
      executor: 'shared-iterations',
      vus: VUS,
      iterations: REQUESTS,
      // Generous: 100k requests on a laptop takes a while, and a timeout here
      // would truncate the batch and silently report a partial run.
      maxDuration: '20m',
    },
  },
  // No failing thresholds — this script MEASURES. launch-burst.js is the one
  // that asserts. A measurement run that exits non-zero on a slow laptop would
  // just train you to ignore it.
  thresholds: {},
};

export function setup() {
  const run = `${Date.now()}`;
  const json = { headers: { 'Content-Type': 'application/json' } };

  const reg = http.post(
    `${BASE}/auth/register`,
    JSON.stringify({ email: `matrix-${run}@loadtest.local`, password: PASSWORD }),
    json,
  );
  if (reg.status !== 201) {
    exec.test.abort(`register failed: ${reg.status} ${reg.body}`);
  }
  const token = reg.json('token');

  // Seed enough events that a page of 100 is actually a full page. Without
  // this, limit=100 and limit=10 would return the same handful of rows and the
  // pagination sweep would measure nothing.
  const auth = {
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` },
  };
  for (let i = 0; i < 120; i++) {
    http.post(`${BASE}/events`, JSON.stringify({ name: `Matrix ${run} #${i}`, seats: 100 }), auth);
  }

  const url = `${BASE}/events?limit=${LIMIT}&offset=0`;
  // Prime the cache: we want STEADY STATE, not one cold miss averaged in.
  http.get(url);
  return { url };
}

export default function (data) {
  const res = http.get(data.url, { tags: { name: 'GET /events', label: LABEL } });
  readLatency.add(res.timings.duration);
  check(res, { 'status is 200': (r) => r.status === 200 });
}

// One machine-readable line instead of k6's full summary, so a run's numbers
// can be pasted straight into RESULTS.md without transcription errors.
export function handleSummary(data) {
  const lat = data.metrics.read_latency.values;
  const reqs = data.metrics.http_reqs.values;
  const failed = data.metrics.http_req_failed
    ? data.metrics.http_req_failed.values.rate
    : 0;
  const secs = data.state.testRunDurationMs / 1000;

  const row = {
    label: LABEL,
    requests: REQUESTS,
    vus: VUS,
    limit: LIMIT,
    duration_s: +secs.toFixed(2),
    throughput_rps: +(reqs.count / secs).toFixed(1),
    avg_ms: +lat.avg.toFixed(2),
    med_ms: +lat.med.toFixed(2),
    p95_ms: +lat['p(95)'].toFixed(2),
    p99_ms: +lat['p(99)'].toFixed(2),
    max_ms: +lat.max.toFixed(2),
    failed_rate: failed,
  };
  return { stdout: `\nRESULT ${JSON.stringify(row)}\n` };
}
