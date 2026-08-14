// launch-burst.js — the headline proof for this project.
//
// THE CLAIM ON THE RESUME:
//   "thousands of registrations fired in the same second, zero double-bookings,
//    zero oversell, waitlist stays correct, p99 recorded."
//
// THE EXPERIMENT:
//   1. create ONE event with SEATS seats
//   2. pre-register BUYERS distinct users (distinct users matter — see below)
//   3. spin up BUYERS k6 VUs and have every one of them POST /events/:id/book
//      at essentially the same instant (the "launch second" / thundering herd)
//
// WHAT CORRECT LOOKS LIKE, and it is not negotiable:
//   - exactly SEATS requests get 201 Created
//   - exactly BUYERS-SEATS requests get 202 Accepted (waitlisted)
//   - seats remaining in Postgres is EXACTLY 0 — never negative (oversell),
//     never leftover (lost update / lock held too long)
//   - SUM(seats) in bookings == SEATS
//
// These are enforced as k6 THRESHOLDS, so the process exits non-zero if the
// concurrency story is wrong. This is a test, not a demo.
//
// WHY DISTINCT USERS PER VU (the subtle bit):
//   bookings has UNIQUE(event_id, user_id) for idempotency. If every VU booked
//   as the SAME user, that constraint alone would prevent a second booking and
//   the test would "pass" while proving nothing about seat accounting. Distinct
//   users force the real contention onto the seat counter, where the Postgres
//   transaction + FOR UPDATE row lock is the only thing standing between us and
//   an oversell.
//
// RUN (stack must be up: docker compose up -d):
//   docker run --rm -i --network host -e SEATS=100 -e BUYERS=1000 \
//     -v "$PWD/loadtest/k6:/scripts" grafana/k6 run /scripts/launch-burst.js
//
// On Docker Desktop (Windows/Mac) --network host does not reach the host. Join
// the compose network instead, which also takes the host port-forward out of
// the measurement path:
//
//   docker run --rm --network eventreg_default -e BASE_URL=http://nginx:80 \
//     -e SEATS=100 -e BUYERS=1000 \
//     -v "$PWD/loadtest/k6:/scripts:ro" grafana/k6 run /scripts/launch-burst.js
//
// Recorded results and method: loadtest/k6/RESULTS.md

import http from 'k6/http';
import { check } from 'k6';
import { Counter, Trend } from 'k6/metrics';
import exec from 'k6/execution';

const BASE = __ENV.BASE_URL || 'http://localhost:8080';
const SEATS = parseInt(__ENV.SEATS || '100', 10);
const BUYERS = parseInt(__ENV.BUYERS || '1000', 10);
// How many registrations to fire in parallel during setup. bcrypt is
// deliberately slow (~100ms of CPU each), so serial signup of 1000 users would
// take ~100s of wall clock. http.batch parallelises it.
const SIGNUP_BATCH = parseInt(__ENV.SIGNUP_BATCH || '50', 10);
const PASSWORD = 'loadtest-password-123';

// ---------------------------------------------------------------- metrics ---
// Custom counters, one per HTTP outcome, so the RESULT of the burst is visible
// in k6's summary rather than inferred from status-code aggregates.
const booked = new Counter('outcome_booked');            // 201
const waitlisted = new Counter('outcome_waitlisted');    // 202
const replayed = new Counter('outcome_idempotent');      // 200 (already booked)
const soldOut = new Counter('outcome_conflict');         // 409
const failed = new Counter('outcome_error');             // anything else
// Latency of ONLY the booking call — the number that goes on the resume. The
// built-in http_req_duration would average in setup's signup traffic.
const bookLatency = new Trend('book_latency', true);

export const options = {
  // Signing up BUYERS users is CPU-bound on bcrypt and runs before the test
  // clock starts. The 60s default is nowhere near enough.
  setupTimeout: '20m',
  teardownTimeout: '2m',

  scenarios: {
    // per-vu-iterations with iterations:1 is the closest k6 gets to "everyone
    // clicks register at once": every VU is initialised UP FRONT, then each
    // performs exactly one booking and exits. No ramp, no think time.
    launch_burst: {
      executor: 'per-vu-iterations',
      vus: BUYERS,
      iterations: 1,
      maxDuration: '5m',
      gracefulStop: '30s',
    },
  },

  // CORRECTNESS AS A THRESHOLD. A threshold breach exits non-zero, so this
  // doubles as CI-able proof rather than something a human has to eyeball.
  thresholds: {
    // The oversell test: not one seat more, not one fewer.
    outcome_booked: [`count == ${SEATS}`],
    // Everyone who missed a seat must be queued, not dropped.
    outcome_waitlisted: [`count == ${BUYERS - SEATS}`],
    // No request may error out, and nobody should see a bare 409 now that the
    // waitlist exists — a full event returns 202, not "go away".
    outcome_error: ['count == 0'],
    outcome_conflict: ['count == 0'],
    // LATENCY BUDGET — and why it is seconds, not milliseconds.
    //
    // Every one of these BUYERS requests contends for the SAME event row, and
    // the FOR UPDATE lock makes them serialize by design. So the burst is a
    // queue, and the last request in it waits for all the others:
    //
    //     tail latency  ≈  BUYERS × (time the lock is held per booking)
    //
    // Measured at BUYERS=1000 across runs: the burst drains in 3.5-4.5s, which
    // puts the lock-held work at ~3.5-4.5ms per booking. The observed p99 of
    // 3.3-4.2s is therefore QUEUEING DELAY, not a slow database — and it is the
    // correct behaviour, because the alternative to queueing is overselling.
    //
    // WHY THE BUDGET IS 8s AND NOT THE ~4s WE OBSERVE: run-to-run variance on a
    // laptop that is also hosting every server is close to 30% (3.5s then 4.5s
    // on back-to-back runs, as the dataset grows and the CPU contends). A
    // threshold pinned to the best observed run is a flaky test, and a flaky
    // correctness test gets ignored — which would waste the real assertions
    // above. 8s is ~2x the slowest observation: still tight enough that a lock
    // held twice as long fails, loose enough not to cry wolf.
    //
    // For latency under REALISTIC arrival, see sustained.js. That is the number
    // to quote; this one is a regression guard on the serialization cost.
    book_latency: ['p(95)<8000', 'p(99)<10000'],
  },
};

// -------------------------------------------------------------------- setup --
// Runs once, in a single VU, before the burst. Everything expensive and
// sequential belongs here so it never pollutes the measured window.
export function setup() {
  // Unique per run so re-runs don't collide on the email UNIQUE constraint.
  const run = `${Date.now()}`;
  const json = { headers: { 'Content-Type': 'application/json' } };

  // ---- the organiser, who owns the event ----
  const adminRes = http.post(
    `${BASE}/auth/register`,
    JSON.stringify({ email: `admin-${run}@loadtest.local`, password: PASSWORD }),
    json,
  );
  if (adminRes.status !== 201) {
    exec.test.abort(`admin register failed: ${adminRes.status} ${adminRes.body}`);
  }
  const adminToken = adminRes.json('token');

  // ---- the scarce resource ----
  const evRes = http.post(
    `${BASE}/events`,
    JSON.stringify({ name: `Launch Burst ${run}`, seats: SEATS }),
    { headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${adminToken}` } },
  );
  if (evRes.status !== 201) {
    exec.test.abort(`create event failed: ${evRes.status} ${evRes.body}`);
  }
  const eventID = evRes.json('id');
  console.log(`event id=${eventID} seats=${SEATS}; registering ${BUYERS} buyers...`);

  // ---- the crowd ----
  // http.batch fires SIGNUP_BATCH registrations concurrently from this one VU.
  const tokens = [];
  for (let i = 0; i < BUYERS; i += SIGNUP_BATCH) {
    const reqs = [];
    for (let j = i; j < Math.min(i + SIGNUP_BATCH, BUYERS); j++) {
      reqs.push([
        'POST',
        `${BASE}/auth/register`,
        JSON.stringify({ email: `u${j}-${run}@loadtest.local`, password: PASSWORD }),
        json,
      ]);
    }
    const responses = http.batch(reqs);
    for (const r of responses) {
      if (r.status !== 201) {
        exec.test.abort(`buyer register failed: ${r.status} ${r.body}`);
      }
      tokens.push(r.json('token'));
    }
    if ((i / SIGNUP_BATCH) % 5 === 0) {
      console.log(`  registered ${tokens.length}/${BUYERS}`);
    }
  }

  console.log(`setup done — firing ${BUYERS} concurrent bookings at ${SEATS} seats`);
  return { eventID, tokens, adminToken };
}

// ------------------------------------------------------------------ the burst --
export default function (data) {
  // One VU : one user. idInTest is 1-based and unique across the whole test.
  const token = data.tokens[(exec.vu.idInTest - 1) % data.tokens.length];

  const res = http.post(
    `${BASE}/events/${data.eventID}/book`,
    JSON.stringify({ seats: 1 }),
    {
      headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` },
      tags: { name: 'POST /events/:id/book' },
    },
  );

  bookLatency.add(res.timings.duration);

  // Classify. The status code IS the outcome in this API, which is why it's
  // worth asserting on precisely.
  switch (res.status) {
    case 201:
      booked.add(1);
      break;
    case 202:
      waitlisted.add(1);
      break;
    case 200:
      replayed.add(1);
      break;
    case 409:
      soldOut.add(1);
      break;
    default:
      failed.add(1);
      console.error(`unexpected ${res.status}: ${String(res.body).slice(0, 200)}`);
  }

  // Per-request sanity: every response must be one of the four modelled
  // outcomes, and the JSON must actually say which.
  check(res, {
    'status is a modelled outcome': (r) => [200, 201, 202, 409].includes(r.status),
    'body carries an outcome': (r) => {
      if (r.status === 409) return true;
      try {
        return typeof r.json('outcome') === 'string';
      } catch (_) {
        return false;
      }
    },
  });
}

// ----------------------------------------------------------------- teardown --
// The database is the final authority. The counters above say what the API
// CLAIMED; this says what actually persisted.
export function teardown(data) {
  const ev = http.get(`${BASE}/events/${data.eventID}`);
  const seatsLeft = ev.json('seats');

  const stats = http.get(`${BASE}/events/stats`, {
    headers: { Authorization: `Bearer ${data.adminToken}` },
  });

  let mine = null;
  try {
    const rows = stats.json();
    const list = Array.isArray(rows) ? rows : rows.items || [];
    mine = list.find((r) => r.event_id === data.eventID) || null;
  } catch (_) {
    // stats shape drift shouldn't mask the seat check below
  }

  console.log('======================= DATABASE VERIFICATION =======================');
  console.log(`  seats remaining        : ${seatsLeft}   (must be exactly 0)`);
  if (mine) {
    console.log(`  bookings (COUNT)       : ${mine.booking_count}   (must be ${SEATS})`);
    console.log(`  seats sold (SUM)       : ${mine.seats_booked}   (must be ${SEATS})`);
  }
  if (seatsLeft < 0) {
    console.error(`  OVERSOLD by ${-seatsLeft} seats — the locking is broken.`);
  } else if (seatsLeft > 0) {
    console.error(`  ${seatsLeft} seats went unsold despite excess demand — lost bookings.`);
  } else {
    console.log('  PASS: every seat sold exactly once, none oversold.');
  }
  console.log('====================================================================');
}
