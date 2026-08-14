# Load test results

Recorded **2026-08-12**. These are the numbers behind the claims in the root README.

**Test bench (state this whenever you quote a figure):** the whole stack —
Nginx, 2 API replicas, Postgres, Redis, notifier — plus the k6 container, all on
**one laptop** (Windows 11, Docker Desktop). k6 runs inside the compose network and hits
Nginx directly, so the host port-forward is out of the measurement path. Single-node
numbers measure *this box*, not production capacity. What they do prove is correctness
under contention and the relative cost of each architectural choice, which is what they
are used for below.

---

## 1. Launch burst — the correctness proof

`launch-burst.js` — 1,000 distinct authenticated users each POST one booking to a
100-seat event, all released at essentially the same instant.

```
docker run --rm --network eventreg_default -e BASE_URL=http://nginx:80 \
  -e SEATS=100 -e BUYERS=1000 -v "$PWD/loadtest/k6:/scripts:ro" \
  grafana/k6 run /scripts/launch-burst.js
```

| Outcome | Result | Required |
|---|---|---|
| `201` booked | **100** | exactly 100 |
| `202` waitlisted | **900** | exactly 900 |
| `409` conflict | **0** | 0 |
| Errors / 5xx | **0** | 0 |
| Failed checks | **0 of 2,000** | 0 |
| **Seats remaining in Postgres** | **0** | exactly 0 — no oversell, none unsold |
| Bookings `COUNT(*)` | **100** | 100 |
| Seats sold `SUM(seats)` | **100** | 100 |

**Zero double-bookings, zero oversell, and every loser queued rather than dropped.**

Timing of the burst itself:

| Metric | Run 1 | Run 2 |
|---|---|---|
| Burst window (1,000 bookings) | **3.5 s** | 4.5 s |
| Booking throughput in the window | **~286 bookings/s** | ~222 bookings/s |
| Booking latency p95 / p99 | 3.25 s / 3.27 s | 4.17 s / 4.20 s |

Both runs were byte-identical on correctness (100 / 900 / 0 / 0, seats = 0). The ~30 %
timing spread between two back-to-back runs is the honest variance of a laptop hosting
every tier *and* the load generator, with a dataset that grows each run. It is why the
threshold in the script is set at 8 s rather than pinned to the best observation — a
flaky correctness test is one that gets ignored.

### Read the latency correctly — this is the interview point

Those seconds look alarming until you notice the spread: **every** request landed between
2.79 s and 3.28 s. Nothing was slow; everything was *queued*. All 1,000 requests contend
for the same event row, and `SELECT … FOR UPDATE` makes them serialize — deliberately,
because the alternative to queueing is overselling.

So the tail is arithmetic, not a symptom:

```
tail latency ≈ requests × lock-held time per booking
3.5 s        ≈ 1000     × ~3.5 ms
```

**~3.5 ms of lock-held work per booking** is the real figure of merit, and it's what a
regression would move. Quote *this* number, plus the sustained figures below — never the
burst p99 as if it were a service-time.

---

## 2. Sustained throughput and latency — the numbers to quote

`sustained.js` — constant-arrival-rate (open model) against the cached
`GET /events` listing. k6 holds the offered rate regardless of how fast responses come
back, so saturation shows up as dropped iterations instead of hiding as reduced load.

| Offered rate | Achieved | p95 | p99 | HTTP failures | Dropped |
|---|---|---|---|---|---|
| 400 req/s | 399 req/s | **2.69 ms** | 3.90 ms | 0 | 0 |
| 2,000 req/s | 1,992 req/s | **6.13 ms** | 14.47 ms | 0 | 7 (0.01 %) |
| 3,000 req/s | 2,977 req/s | 11.10 ms | 33.52 ms | 0 | 317 (0.35 %) |

**Headline: ~2,000 req/s sustained at p95 6 ms / p99 14 ms with zero errors**, on a
single laptop running every tier. At 3,000 the offered rate stops being met — throughput
holds near 2,977 req/s and drops appear — so that's the saturation knee, not a clean
result. 179,370 checks passed, 0 failed, across the 3,000 rps run.

---

## 3. What the architecture actually costs and buys

Disabling Redis (`docker-compose.nocache.yml`) turns off **both** the cache and the rate
limiter, so a naive before/after would confound two changes. `GET /events/:id` is not
cached (`cachedStore.Get` is a pass-through to Postgres) while `GET /events` is, which
gives a control group and a treatment group on the same stack.

All rows: 400 req/s, 30 s, p95 in milliseconds.

| Endpoint | Redis ON | Redis OFF | Delta |
|---|---|---|---|
| `GET /events/:id` — **uncached (control)** | 2.54 | 1.92 | **+0.62** |
| `GET /events` — **cached (treatment)** | 2.69 | 2.41 | **+0.28** |

**The rate limiter costs ~0.62 ms per request.** That is the control delta: on the
uncached path, the only thing Redis does is `INCR`/`EXPIRE` for throttling.

**The cache saves ~0.34 ms p95** (`0.62 − 0.28`) — the treatment delta is smaller than
the control's by exactly the cache's contribution. Supporting evidence that this is
structural and not noise: the *fastest* cached response was **199 µs** against **717 µs**
uncached, a 3.6× lower floor.

### The honest conclusion

**At this scale, Redis is a net latency loss on the read path — roughly +0.3 ms.** The
limiter costs more than the cache saves. That is not a bug and not a reason to remove it:

- **The cache's value scales with query cost, not request count.** Here it front-runs a
  handful of rows out of a warm local Postgres — a query so cheap there is almost nothing
  to save. Point it at a `JOIN`/`GROUP BY` over a large table, or a database on another
  host, and the saving grows while the ~0.2 ms Redis round-trip stays fixed.
- **The rate limiter isn't bought for latency.** It's bought for surviving a credential
  stuffing run against a bcrypt endpoint, where each guess costs ~100 ms of *our* CPU.
  Paying 0.62 ms per request to prevent that is a trade worth making explicitly.
- **A known, fixable inefficiency:** every cached read does **two sequential Redis
  round-trips** — `GET generation`, then `GET key` (`internal/storage/cached.go`).
  Pipelining them, or caching the generation in-process for a second, would remove one
  full RTT and likely flip the cached path to a net win.

### Caveats, stated rather than buried

- Sub-millisecond deltas from **one run per cell**. The direction is consistent and
  corroborated by the floor measurement, but treat ±0.1 ms as noise.
- Everything shares one machine, so the client competes with the servers for CPU.
- Redis and Postgres are one hop away on a local bridge network. Real deployments have
  higher and more variable network latency, which shifts the trade toward the cache.

---

## Reproducing

```bash
cd eventreg
docker compose up -d --build

# 1. correctness
docker run --rm --network eventreg_default -e BASE_URL=http://nginx:80 \
  -e SEATS=100 -e BUYERS=1000 -v "$PWD/loadtest/k6:/scripts:ro" \
  grafana/k6 run /scripts/launch-burst.js

# 2. throughput / latency
docker run --rm --network eventreg_default -e BASE_URL=http://nginx:80 \
  -e RATE=2000 -e DURATION=30s -e ENDPOINT=list \
  -v "$PWD/loadtest/k6:/scripts:ro" grafana/k6 run /scripts/sustained.js

# 3. the A/B: repeat step 2 with ENDPOINT=event, then again for both with
#    Redis disabled, restoring the stack afterwards
docker compose -f docker-compose.yml -f docker-compose.nocache.yml up -d api1 api2
docker compose up -d api1 api2
```

Correctness is enforced by k6 **thresholds**, so `launch-burst.js` exits non-zero if the
locking ever regresses. It is a test, not a demo.
