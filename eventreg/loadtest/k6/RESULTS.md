# Load test results

Sections 1–3 recorded **2026-08-12**; section 4 (the fixed-request-count matrix and the
replica A/B) recorded **2026-08-15**. These are the numbers behind the claims in the root
README.

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

## 4. Fixed-request-count matrix, and replicas vs no replicas

Recorded **2026-08-15** with `matrix.js`. Closed model (`shared-iterations`), 50 VUs
held constant across every row, against `GET /events`. Rate limiter lifted via
`docker-compose.loadtest.yml` — see the note at the end of this section for why that is
necessary rather than a fudge.

### 4a. Page size sweep — 10,000 requests per row

| Page size | Replicas | Time | Throughput | p95 | p99 | Errors |
|---|---|---|---|---|---|---|
| `limit=10` | 2 | 2.44 s | 4,154 req/s | 15.68 ms | 26.38 ms | 0 |
| `limit=20` | 2 | 2.08 s | 4,858 req/s | 12.81 ms | 16.19 ms | 0 |
| `limit=50` | 2 | 2.50 s | 4,052 req/s | 16.69 ms | 22.45 ms | 0 |
| `limit=100` | 2 | 2.63 s | 3,847 req/s | 18.40 ms | 25.14 ms | 0 |
| `limit=10` | 1 | 2.65 s | 3,819 req/s | 20.97 ms | 34.11 ms | 0 |
| `limit=20` | 1 | 2.03 s | 4,993 req/s | 12.08 ms | 16.24 ms | 0 |
| `limit=50` | 1 | 2.37 s | 4,271 req/s | 14.35 ms | 19.02 ms | 0 |
| `limit=100` | 1 | 2.55 s | 3,964 req/s | 18.92 ms | 25.46 ms | 0 |

Cost grows roughly linearly with page size from `limit=20` up: 20 → 100 is about **+44 %
p95** for 5× the rows.

**The `limit=10` rows are warm-up, not a finding.** Less data cannot cost more; that row
was simply first in each batch and paid for cold connection pools, an empty cache, and
the Go runtime settling. The defensible trend is 20 → 50 → 100.

**`limit=500` and `limit=1000` are not measurable** — `models.MaxLimit` clamps `limit` to
100 by design, so both would silently return 100 rows and the "tier" would be a
relabelled duplicate. The clamp is the DoS guard that makes `?limit=10000000` harmless.

### 4b. Request volume, 2 replicas vs 1 — `limit=20`

| Requests | Replicas | Time | Throughput | p95 | p99 | Errors |
|---|---|---|---|---|---|---|
| 1,000 | 2 | 0.67 s | 1,675 req/s | 13.08 ms | 16.46 ms | 0 |
| 1,000 | 1 | 0.62 s | 1,806 req/s | 10.81 ms | 13.69 ms | 0 |
| 10,000 | 2 | 2.24 s | 4,513 req/s | 14.73 ms | 21.04 ms | 0 |
| 10,000 | 1 | 2.11 s | 4,792 req/s | 13.33 ms | 18.14 ms | 0 |
| 100,000 | 2 | 17.10 s | 5,855 req/s | 13.85 ms | 19.19 ms | 0 |
| 100,000 | 1 | 16.00 s | 6,258 req/s | 13.09 ms | 17.75 ms | 0 |

Repeat of the headline tier, because a surprising result deserves a second run:

| Requests | Replicas | Run 1 | Run 2 |
|---|---|---|---|
| 100,000 | 2 | 17.10 s · 5,855 req/s · p95 13.85 ms | 17.21 s · 5,819 req/s · p95 14.08 ms |
| 100,000 | 1 | 16.00 s · 6,258 req/s · p95 13.09 ms | 16.71 s · 5,992 req/s · p95 13.72 ms |

### 4c. One replica beat two, on every tier

Consistent across both repeats, so this is not noise. It is also not a bug.

"2 replicas" on one laptop means two processes sharing the same cores. The second replica
adds **no CPU**. What it does add is another process competing for those cores, per-request
load-balancing work in Nginx, and a second connection pool against the same Postgres and
Redis. Net cost: about **5 % throughput**.

The case for two replicas was never throughput:

1. **Availability** — kill one, the site stays up. Worth far more than 5 %.
2. **Zero-downtime deploys** — `maxUnavailable: 0` rolls a new version out behind the old.
3. **It proves statelessness** — round-robin only works if any replica can serve any
   request. A token minted by `api1` verifies on `api2`; rate-limit counters live in Redis
   so a limit of 10 stays 10.

Horizontal scaling pays when replicas land on **different machines**. On one box it is an
insurance premium, and this is what the premium costs.

### 4d. The rate limiter blocked the first attempt

The first 100,000-request run returned **52 % failures**. Nothing was broken: the limit is
100,000/min per IP, the run fired 100,000 requests in ~15 s from one IP, and the limiter
worked exactly as designed.

This is the standard load-testing own-goal — the test looks like a single abusive client,
your own defences block it, and you "find" a bug that is a feature. Real traffic at that
rate arrives from tens of thousands of distinct IPs. `docker-compose.loadtest.yml` lifts
the limit for measurement runs only, so the normal stack keeps its defences.

### 4e. Caveats

- Most cells are one run. Treat sub-millisecond differences as noise. The two claims that
  were repeated (100k volume, 1 vs 2 replicas) held.
- The events table grew through the session (each run seeds 120 events; it finished at
  ~3,000 rows), so `COUNT(*)` in the list query got slightly more expensive as the sweep
  went on. That works against later rows, not for them.
- Same single-laptop caveat as every other section: the load generator competes with the
  servers for CPU.

### 4f. Launch burst re-verified the same day

`launch-burst.js` was re-run on this build: **100 booked / 900 waitlisted / 0 conflicts /
seats remaining 0 / 2,000 of 2,000 checks passed**, burst window 3.7 s, p95 3.45 s,
p99 3.46 s — i.e. ~3.5 ms of lock-held work per booking, unchanged.

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

# 4. the fixed-request-count matrix (section 4). Lift the rate limiter first,
#    or a 100k run trips it and reports 52% "failures" that are really 429s.
docker compose -f docker-compose.yml -f docker-compose.loadtest.yml up -d api1 api2
docker run --rm --network eventreg_default -e BASE_URL=http://nginx:80 \
  -e REQUESTS=100000 -e VUS=50 -e LIMIT=20 \
  -v "$PWD/loadtest/k6:/scripts:ro" grafana/k6 run /scripts/matrix.js

# 4b. same again with ONE replica, then restore two
docker compose -f docker-compose.yml -f docker-compose.loadtest.yml \
  -f docker-compose.single.yml up -d nginx api1
docker compose stop api2
# ...rerun matrix.js...
docker compose up -d --force-recreate nginx api1 api2
```

Correctness is enforced by k6 **thresholds**, so `launch-burst.js` exits non-zero if the
locking ever regresses. It is a test, not a demo.
