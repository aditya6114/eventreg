// sustained.js — throughput and latency under REALISTIC arrival, plus the
// measured impact of the Redis cache.
//
// WHY THIS EXISTS ALONGSIDE launch-burst.js:
//   launch-burst.js releases 1000 requests simultaneously. Every one of them
//   queues behind the same Postgres row lock, so its p99 is dominated by
//   QUEUEING DELAY, not by how fast the system serves a request. That is the
//   right way to test CORRECTNESS and the wrong way to quote latency.
//
//   This script instead uses a CONSTANT ARRIVAL RATE — requests arrive at a
//   fixed rate independent of how fast previous ones finished, which is how
//   real traffic behaves (an open model). The p95/p99 it reports is a number
//   you can defend in an interview.
//
// WHAT IT MEASURES: a read path, selected with ENDPOINT.
//
//   ENDPOINT=list   GET /events        <- CACHED (cachedStore.List)
//   ENDPOINT=event  GET /events/:id    <- NOT cached (cachedStore.Get is a
//                                         pass-through to Postgres)
//
// That asymmetry is what makes a clean experiment possible, and it is worth
// understanding before trusting any number here. Disabling Redis turns OFF
// BOTH the cache AND the rate limiter, so a naive cache A/B actually measures
// two changes at once. The uncached endpoint is therefore the CONTROL: run it
// with and without Redis and the delta is the rate limiter's round-trip alone.
// Subtract that from the cached endpoint's delta and what remains is the
// cache's real contribution. See RESULTS.md for the worked numbers.
//
// RUN (cache ON — the default stack):
//   docker run --rm --network eventreg_default -e BASE_URL=http://nginx:80 \
//     -v "$PWD/loadtest/k6:/scripts:ro" grafana/k6 run /scripts/sustained.js
//
// RUN (cache OFF, for the A/B):
//   docker compose -f docker-compose.yml -f docker-compose.nocache.yml up -d api1 api2
//   ...same k6 command, with -e LABEL=nocache
//   docker compose up -d api1 api2      # restore
//
// See RESULTS.md for the recorded comparison.

import http from 'k6/http';
import { check } from 'k6';
import { Trend } from 'k6/metrics';
import exec from 'k6/execution';

const BASE = __ENV.BASE_URL || 'http://localhost:8080';
const RATE = parseInt(__ENV.RATE || '400', 10);       // requests per second
const DURATION = __ENV.DURATION || '30s';
const LABEL = __ENV.LABEL || 'default';
const ENDPOINT = __ENV.ENDPOINT || 'event';   // 'list' (cached) | 'event' (uncached)
const PASSWORD = 'loadtest-password-123';

const readLatency = new Trend('read_latency', true);

export const options = {
  setupTimeout: '2m',
  scenarios: {
    // constant-arrival-rate = OPEN model. k6 adds VUs as needed to hold the
    // arrival rate; if the system can't keep up, k6 reports dropped iterations
    // instead of silently slowing down the offered load. That distinction is
    // the whole point: a closed model (constant-vus) hides saturation by
    // sending less traffic, which flatters the numbers.
    steady_reads: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: 100,
      maxVUs: 800,
    },
  },
  thresholds: {
    // A read served from cache should be single-digit milliseconds; allow
    // generous headroom so this catches real regressions, not laptop noise.
    read_latency: ['p(95)<500', 'p(99)<1000'],
    http_req_failed: ['rate<0.01'],
    // Saturation guard: if k6 cannot keep up with the offered rate, the
    // throughput figure is meaningless and the run must not pass quietly.
    dropped_iterations: ['count == 0'],
  },
};

export function setup() {
  const run = `${Date.now()}`;
  const json = { headers: { 'Content-Type': 'application/json' } };

  const reg = http.post(
    `${BASE}/auth/register`,
    JSON.stringify({ email: `read-${run}@loadtest.local`, password: PASSWORD }),
    json,
  );
  if (reg.status !== 201) {
    exec.test.abort(`register failed: ${reg.status} ${reg.body}`);
  }
  const token = reg.json('token');

  const ev = http.post(
    `${BASE}/events`,
    JSON.stringify({ name: `Sustained Read ${run}`, seats: 500 }),
    { headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` } },
  );
  if (ev.status !== 201) {
    exec.test.abort(`create event failed: ${ev.status} ${ev.body}`);
  }

  const eventID = ev.json('id');
  const url = ENDPOINT === 'list'
    ? `${BASE}/events?limit=20&offset=0`
    : `${BASE}/events/${eventID}`;

  // Prime the cache so we measure STEADY STATE, not the first cold miss. The
  // cold-start cost is real but it happens once per TTL, not per request, and
  // averaging it in would understate the cache and overstate the database.
  http.get(url);
  console.log(`[${LABEL}] ENDPOINT=${ENDPOINT} url=${url}; offering ${RATE} req/s for ${DURATION}`);
  return { url };
}

export default function (data) {
  const res = http.get(data.url, {
    tags: { name: ENDPOINT === 'list' ? 'GET /events' : 'GET /events/:id', label: LABEL },
  });
  readLatency.add(res.timings.duration);
  check(res, {
    'status is 200': (r) => r.status === 200,
    'body parses': (r) => {
      try {
        return r.json() !== null;
      } catch (_) {
        return false;
      }
    },
  });
}
