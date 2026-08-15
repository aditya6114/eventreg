# Event Registration API

**What it is:** a backend for registering people to events, built to survive the moment
a thousand people click "Register" at the same second.

---

## The problem it solves

Imagine a workshop with 100 seats. You announce it. At 10:00:00 sharp, 1,000 people hit
the register button.

Three bad things normally happen:

1. **Double booking.** Two people get the same seat. Both got a confirmation email. One
   of them turns up and there is nowhere to sit.
2. **The site falls over.** Everything is slow, some people get errors, and nobody knows
   whether they got in.
3. **The 900 who missed out just get an error.** They came back later, the event was
   still full, and they gave up.

This project fixes all three:

- It **never sells the same seat twice.** Not "usually" — it is checked by a test that
  fails the build if it ever stops being true.
- It **stays up** under the burst. No 5xx errors, no dropped requests.
- The 900 who missed out are **put in a queue** (a waitlist), not thrown away. If someone
  cancels, the person who has waited longest automatically gets the seat.

Proof, measured on this machine:

| At launch second | Result |
|---|---|
| 1,000 people register for 100 seats | 100 booked, 900 waitlisted |
| Seats sold twice | **0** |
| Seats left unsold | **0** |
| Errors | **0** |
| Whole burst finished in | **3.7 seconds** |

---

## Tech used

| Layer | Tool |
|---|---|
| Language | Go |
| Web framework | Echo |
| Database | PostgreSQL (via `pgx`) |
| Cache / locks / rate limiting | Redis |
| Reverse proxy + load balancer | Nginx |
| Service-to-service calls | gRPC + Protocol Buffers |
| Containers | Docker + Docker Compose |
| Orchestration | Kubernetes (kind) |
| Load testing | k6 |
| Migrations | golang-migrate |
| API docs | Swagger / OpenAPI |
| Logging | Go `log/slog` (JSON) |

---

## Architecture

Simple version, in words:

- Everything comes in through **Nginx**. It is the only door.
- Nginx passes the request to one of the **API copies** (called replicas). They are
  identical and share no memory.
- The API talks to **Postgres** (the real truth) and **Redis** (fast temporary stuff).
- When a booking succeeds, the API tells the **notifier** service over gRPC. If the
  notifier is dead, the booking still works.

```mermaid
flowchart LR
    C["Clients"]

    subgraph edge["Edge"]
      N["<b>Nginx</b> :8080<br/>one public door<br/>round-robin"]
    end

    subgraph app["App tier (stateless)"]
      A1["<b>api1</b> :8082"]
      A2["<b>api2</b> :8082"]
    end

    subgraph data["State"]
      P[("<b>Postgres</b><br/>the truth")]
      R[("<b>Redis</b><br/>cache · holds · rate limit")]
    end

    NT["<b>notifier</b> :50051<br/>gRPC"]

    C --> N
    N --> A1
    N --> A2
    A1 --> P
    A2 --> P
    A1 --> R
    A2 --> R
    A1 -. "gRPC (can fail safely)" .-> NT
    A2 -. gRPC .-> NT
```

### What happens on a read (cache hit vs cache miss)

`GET /events` is the most-used route. It is cached. Here is exactly what happens:

```mermaid
flowchart TD
    S(["GET /events?limit=20"]) --> RL{"Rate limit OK?<br/><i>Redis INCR</i>"}
    RL -->|"no"| R429["429 Too Many Requests"]
    RL -->|"yes"| GEN["Read cache generation<br/><i>Redis GET events:gen</i>"]
    GEN --> LOOK{"Key in Redis?<br/><i>events:list:g7:l20:o0</i>"}

    LOOK -->|"HIT"| HIT["Return cached JSON<br/><b>no database touched</b>"]
    LOOK -->|"MISS"| DB["Query Postgres<br/><i>COUNT(*) + paginated SELECT</i>"]
    DB --> FILL["Write result into Redis<br/>with a 30s TTL"]
    FILL --> OUT["Return JSON"]
    HIT --> OUT

    DOWN{{"Redis unreachable?"}} -.->|"treated as a MISS"| DB

    style HIT fill:#1b5e20,color:#fff
    style DB fill:#4a148c,color:#fff
    style R429 fill:#b71c1c,color:#fff
```

Two things worth knowing:

- **Redis being down is not an error.** It is treated as a cache miss. The request goes
  to Postgres and works, just a bit slower. A cache is a speed-up, never a dependency.
- **Invalidation is one command.** When someone books a seat, the event list changes.
  Instead of hunting down every cached page, the app bumps a counter (`events:gen`).
  Every old key instantly becomes unreachable and expires on its own.

### What happens on a booking (the important one)

```mermaid
flowchart TD
    S(["POST /events/5/book"]) --> AUTH{"Valid JWT?"}
    AUTH -->|"no"| E401["401 Unauthorized"]
    AUTH -->|"yes"| TX["<b>BEGIN transaction</b>"]
    TX --> LOCK["SELECT ... FOR UPDATE<br/><i>locks this event's row —<br/>everyone else waits here</i>"]
    LOCK --> DUP{"Already booked<br/>by this user?"}
    DUP -->|"yes"| R200["<b>200 OK</b><br/>return the original booking<br/>(safe to retry)"]
    DUP -->|"no"| SEATS{"Enough seats left?"}

    SEATS -->|"yes"| BOOK["INSERT booking<br/>UPDATE seats = seats - n"]
    SEATS -->|"no"| WAIT["INSERT into waitlist<br/>(FIFO queue)"]

    BOOK --> COMMIT["<b>COMMIT</b> — lock released"]
    WAIT --> COMMIT
    COMMIT --> HOLD["Release the Redis hold<br/><i>best effort</i>"]
    HOLD --> NOTIF["Send gRPC notification<br/><i>fire and forget</i>"]
    NOTIF --> R201["<b>201</b> booked / <b>202</b> waitlisted"]

    style LOCK fill:#e65100,color:#fff
    style COMMIT fill:#1b5e20,color:#fff
```

The orange box is the whole trick. `SELECT ... FOR UPDATE` makes 1,000 requests stand in
a single-file line for that one event row. Only one of them can read and change the seat
count at a time. That is why the seat count can never go wrong.

### What happens when a pod dies (Kubernetes self-healing)

```mermaid
flowchart TD
    OK["2 pods running<br/>both serving traffic"] --> CRASH["api-pod-1 crashes"]

    CRASH --> LIVE["<b>Liveness probe</b> fails<br/><i>GET /health</i>"]
    LIVE --> KILL["Kubernetes kills the pod"]
    KILL --> NEW["Scheduler starts a replacement"]
    NEW --> BOOT["New pod boots,<br/>connects to Postgres + Redis"]
    BOOT --> READY{"<b>Readiness probe</b><br/><i>GET /ready</i>"}
    READY -->|"not yet"| WAITP["No traffic sent to it<br/>(it is alive but not useful)"]
    WAITP --> READY
    READY -->|"ready"| SERVE["Added back to the Service.<br/>Traffic resumes."]
    SERVE --> OK

    CRASH -.->|"meanwhile"| POD2["api-pod-2 serves 100% of traffic.<br/><b>Users notice nothing.</b>"]

    style POD2 fill:#1b5e20,color:#fff
    style KILL fill:#b71c1c,color:#fff
```

**Liveness vs readiness — the difference that matters:**

| Probe | Question | If it fails |
|---|---|---|
| `/health` (liveness) | "Is this process alive?" | The pod is **killed and restarted** |
| `/ready` (readiness) | "Can this pod serve a request right now?" | The pod is **removed from load balancing**, not restarted |

This is why `/health` checks almost nothing and `/ready` checks the database. If liveness
checked the database, a 10-second database blip would restart every pod in the fleet —
turning a small problem into a full outage.

---

## What the API is about

It does four jobs.

1. **Accounts.** Register and log in. You get a JWT token. Passwords are hashed with
   bcrypt and never stored as text.
2. **Events.** Create events, list them (paginated), see how each one is selling.
3. **Booking.** Book seats, cancel, or get put on the waitlist automatically when full.
4. **Holds.** Temporarily reserve a seat for a few minutes while you finish checkout.

Everything after login needs a header: `Authorization: Bearer <token>`.

### Routes available

| Method | Path | Login needed | What it does |
|---|---|---|---|
| `GET` | `/health` | no | Is the process alive |
| `GET` | `/ready` | no | Can it serve traffic (checks the DB) |
| `POST` | `/auth/register` | no | Create an account, get a token |
| `POST` | `/auth/login` | no | Log in, get a token |
| `GET` | `/auth/me` | yes | Who am I |
| `GET` | `/events` | no | List events, paginated. **Cached.** |
| `GET` | `/events/:id` | no | One event |
| `GET` | `/events/stats` | no | Bookings and seats sold per event |
| `POST` | `/events` | yes | Create an event |
| `POST` | `/events/:id/book` | yes | Book seats |
| `DELETE` | `/events/:id/book` | yes | Cancel, and promote from waitlist |
| `POST` | `/events/:id/hold` | yes | Hold seats for a few minutes |
| `DELETE` | `/events/:id/hold` | yes | Let the hold go early |
| `GET` | `/me/bookings` | yes | My bookings |
| `GET` | `/me/waitlist` | yes | My waitlist entries + my position in the queue |
| `GET` | `/docs/*` | no | Swagger UI (development only) |

**The status code tells you what happened** on a booking:

| Code | Meaning |
|---|---|
| `201` | You got a seat |
| `202` | Event was full — you are on the waitlist |
| `200` | You already had this booking. Nothing changed. Safe to retry. |
| `409` | Not enough seats for the hold you asked for |
| `429` | You are sending too fast |

`202` is chosen on purpose. It means "I took your request, it is not done yet" — which is
honest. A `200` would hide the fact that you have no seat.

---

## Load test results

**Read this first.** Everything below ran on **one laptop** (Windows 11, Docker Desktop)
hosting Nginx, both API replicas, Postgres, Redis, the notifier, *and* k6 itself. These
numbers measure this machine, not production capacity. What they do show reliably is the
**relative** cost of each choice.

Test setup: `loadtest/k6/matrix.js`, 50 virtual users, closed model (each user fires the
next request as soon as the previous returns). Route is `GET /events` — the most-used
route in a launch rush, and the one that is cached.

### 1. Page size sweep — `GET /events?limit=N`

10,000 requests per row.

**With 2 replicas:**

| Page size | Time | Throughput | p95 | p99 |
|---|---|---|---|---|
| `limit=10` | 2.44 s | 4,154 req/s | 15.68 ms | 26.38 ms |
| `limit=20` (default) | 2.08 s | **4,858 req/s** | **12.81 ms** | **16.19 ms** |
| `limit=50` | 2.50 s | 4,052 req/s | 16.69 ms | 22.45 ms |
| `limit=100` (max) | 2.63 s | 3,847 req/s | 18.40 ms | 25.14 ms |

**With 1 replica:**

| Page size | Time | Throughput | p95 | p99 |
|---|---|---|---|---|
| `limit=10` | 2.65 s | 3,819 req/s | 20.97 ms | 34.11 ms |
| `limit=20` (default) | 2.03 s | **4,993 req/s** | **12.08 ms** | **16.24 ms** |
| `limit=50` | 2.37 s | 4,271 req/s | 14.35 ms | 19.02 ms |
| `limit=100` (max) | 2.55 s | 3,964 req/s | 18.92 ms | 25.46 ms |

**What this says:** bigger pages are slower, roughly in a straight line from `limit=20`
upward — more rows to fetch, serialise into JSON, and push through Redis. Going from 20
to 100 costs about **44% more latency at p95** for 5× the data, which is a good trade if
you actually need the rows and a waste if you don't.

### 2. Request volume — 1,000 vs 10,000 vs 100,000 requests

Page size fixed at the default `limit=20`.

| Requests | Replicas | Time | Throughput | p95 | p99 | Errors |
|---|---|---|---|---|---|---|
| 1,000 | **2** | 0.67 s | 1,675 req/s | 13.08 ms | 16.46 ms | 0 |
| 1,000 | **1** | 0.62 s | 1,806 req/s | 10.81 ms | 13.69 ms | 0 |
| 10,000 | **2** | 2.24 s | 4,513 req/s | 14.73 ms | 21.04 ms | 0 |
| 10,000 | **1** | 2.11 s | 4,792 req/s | 13.33 ms | 18.14 ms | 0 |
| 100,000 | **2** | 17.10 s | 5,855 req/s | 13.85 ms | 19.19 ms | 0 |
| 100,000 | **1** | 16.00 s | **6,258 req/s** | 13.09 ms | 17.75 ms | 0 |

Because the 100,000 result is the headline, it was run twice on each setup:

| Requests | Replicas | Run 1 | Run 2 |
|---|---|---|---|
| 100,000 | 2 | 17.10 s · 5,855 req/s · p95 13.85 | 17.21 s · 5,819 req/s · p95 14.08 |
| 100,000 | 1 | 16.00 s · 6,258 req/s · p95 13.09 | 16.71 s · 5,992 req/s · p95 13.72 |

**Single request** (no load, for reference). Measured with `curl` against a warm cache,
10 samples:

| Measure | Value |
|---|---|
| End-to-end, including a fresh TCP connection | **~5.5 ms** (median 5.5, worst 9.1) |
| Server-side only, under light load (400 req/s) | **~2.7 ms** p95 |
| Fastest response ever observed | **199 µs** |

The gap between the first two rows is mostly connection setup — `curl` opens a new TCP
connection every time, k6 reuses them. Worth remembering before comparing a `curl` number
to a load-test number and concluding something is wrong.

### 3. Peak p95 and p99

| Setup | Worst p95 seen | Worst p99 seen | Where it happened |
|---|---|---|---|
| 2 replicas | 18.40 ms | 26.38 ms | `limit=100` / warm-up run |
| 1 replica | 20.97 ms | 34.11 ms | warm-up run at `limit=10` |
| 2 replicas (volume tiers only) | 14.73 ms | 21.04 ms | 10,000 requests |
| 1 replica (volume tiers only) | 13.33 ms | 18.14 ms | 10,000 requests |

Under the launch burst — 1,000 people fighting for 100 seats — p95 is **3.45 s**. That
number is **queueing, not slowness**, and it is explained in the next section.

### 4. The surprising result: 2 replicas were NOT faster than 1

Look again at the volume table. One replica beat two, consistently, on every tier.

**This is real and it is not a bug.** The reason is that "2 replicas" here means two
processes on **one laptop, sharing the same CPU cores**. Adding a second replica does not
add any CPU. It only adds:

- another process competing for the same cores,
- Nginx doing load-balancing work on every request,
- more open connections to the same Postgres and Redis.

So on one machine, replicas cost about **5% throughput and buy nothing**.

**Then why have two replicas at all?** Because throughput was never the reason:

1. **Survival.** Kill one replica and the site stays up. That is worth far more than 5%.
2. **Zero-downtime deploys.** With `maxUnavailable: 0`, a new version rolls out while the
   old one still serves.
3. **It proves the app is stateless.** Round-robin only works if any replica can serve
   any request. A token minted by `api1` verifies on `api2`. Rate limit counters live in
   Redis, so a limit of 10 stays 10 instead of drifting to 20.

Replicas pay off for real **when they land on different machines**. On one laptop, they
are an insurance policy you pay 5% for — not a speed-up.

### 5. What the rate limiter taught us

The first attempt at the 100,000-request run came back with **52% failures**. Nothing was
broken. The rate limit is 100,000 requests per minute per IP, the run fired 100,000
requests in 15 seconds from **one** IP, and the limiter did exactly its job.

This is the classic load-testing own-goal: your test looks like one abusive client, so
your own defences block you and you "discover" a bug that is actually a feature. Real
traffic at that rate arrives from tens of thousands of different IPs and would never come
close.

The fix is `docker-compose.loadtest.yml`, which lifts the limit for measurement runs only.

### 6. Caveats, said out loud

- One machine hosts everything, including the load generator. The client competes with
  the servers for CPU.
- Most cells are a **single run**. Treat differences under ~1 ms as noise. The two claims
  that were repeated (100,000 requests, 1 vs 2 replicas) held up.
- The events table **grew during the sweep** (each run seeds 120 events; it ended at
  ~3,000 rows), so the `COUNT(*)` in the list query got slightly more expensive as the
  session went on. This works against later rows, not for them.
- Redis and Postgres are one network hop away on a local bridge. Real deployments have
  slower, more variable networks, which shifts the trade in the cache's favour.

Full method, plus the separate cache-vs-rate-limiter A/B:
[`eventreg/loadtest/k6/RESULTS.md`](eventreg/loadtest/k6/RESULTS.md).

### Reproduce it

```bash
cd eventreg
docker compose -f docker-compose.yml -f docker-compose.loadtest.yml up -d --build

# the correctness proof — exits non-zero if locking ever regresses
docker run --rm --network eventreg_default -e BASE_URL=http://nginx:80 \
  -e SEATS=100 -e BUYERS=1000 \
  -v "$PWD/loadtest/k6:/scripts:ro" grafana/k6 run /scripts/launch-burst.js

# the matrix above
docker run --rm --network eventreg_default -e BASE_URL=http://nginx:80 \
  -e REQUESTS=100000 -e VUS=50 -e LIMIT=20 \
  -v "$PWD/loadtest/k6:/scripts:ro" grafana/k6 run /scripts/matrix.js

# 1 replica instead of 2
docker compose -f docker-compose.yml -f docker-compose.loadtest.yml \
  -f docker-compose.single.yml up -d && docker compose stop api2
```

---

## Booking queues — how it works

There are actually **two** queues in this system. People mix them up, so here they are
side by side.

### Queue 1: the lock queue (invisible, lasts milliseconds)

When 1,000 people book the same event, they do not all run at once. Postgres makes them
line up:

```mermaid
sequenceDiagram
    participant U1 as User 1
    participant U2 as User 2
    participant U3 as User 3
    participant PG as Postgres (event row)

    U1->>PG: BEGIN, SELECT FOR UPDATE
    Note over PG: row locked by User 1
    U2->>PG: SELECT FOR UPDATE
    Note over U2,PG: waiting...
    U3->>PG: SELECT FOR UPDATE
    Note over U3,PG: waiting...
    U1->>PG: seats 100 → 99, COMMIT
    Note over PG: lock released
    PG-->>U2: your turn
    U2->>PG: seats 99 → 98, COMMIT
    PG-->>U3: your turn
```

Each person holds the lock for about **3.5 milliseconds**. So:

```
total time ≈ number of people × time each one holds the lock
3.5 seconds ≈ 1,000          × 3.5 ms
```

That is why the burst p95 is 3.45 seconds. **Nobody was served slowly** — every single
request landed between 3.04 s and 3.48 s. They were all just *waiting in line*. The
alternative to waiting in line is overselling, so this queue is a feature.

The number to actually watch is **3.5 ms of lock-held work per booking**. If that gets
worse, something regressed.

### Queue 2: the waitlist (visible, lasts days)

When the seats run out, you do not get an error. You get put in a line.

```mermaid
flowchart TD
    FULL["Event is full"] --> W["You are added to the waitlist<br/><b>202 Accepted</b>"]
    W --> POS["You can check your position<br/><i>GET /me/waitlist</i>"]

    CANCEL["Someone cancels their booking<br/><i>DELETE /events/:id/book</i>"] --> TX["<b>One single transaction:</b>"]
    TX --> T1["1 — free their seats"]
    T1 --> T2["2 — find longest-waiting people who fit<br/><i>ORDER BY created_at</i>"]
    T2 --> T3["3 — turn them into real bookings"]
    T3 --> T4["4 — remove them from the waitlist"]
    T4 --> T5["<b>COMMIT</b>"]
    T5 --> NOTE["Email them —<br/>they are not watching the page"]

    style TX fill:#e65100,color:#fff
    style T5 fill:#1b5e20,color:#fff
```

Three details that matter:

1. **It is FIFO — first come, first served.** Order is by `created_at`. Your position is
   worked out at read time with `ROW_NUMBER()`, so it is never stale.
2. **Cancel and promote happen in ONE transaction.** If they were separate, there would
   be a gap where the seat is free and a brand-new visitor could grab it before the
   person who has been waiting three days. Doing both inside one transaction closes that
   gap completely.
3. **Promoted people must be told.** They are not sitting on the page refreshing. This is
   the one notification that genuinely matters, and it is why the gRPC notifier exists.

### Related: seat holds (the checkout timer)

A hold is a short "this seat is mine while I type my card details" reservation. It lives
in Redis with a TTL.

- Set atomically with `SETNX` + TTL, using a Lua script.
- Visible to **every** replica, because it is in Redis, not in one process's memory.
- **It deletes itself.** If you close the tab, the seat comes back automatically. There
  is no cleanup job to write, own, or debug.

Booking successfully releases your hold early. If that release fails, nothing breaks —
the TTL cleans it up anyway. That is the whole point of using a TTL.

### Why not just use a mutex?

This is the trap. A `sync.Mutex` locks goroutines inside **one process**. Two replicas
behind Nginx have **two separate mutexes** that know nothing about each other. Both would
happily sell the same seat.

The coordination point has to be somewhere **both replicas can see**: Postgres for the
durable guarantee, Redis for the fast temporary one.

---

## Why each technology was used

Honest verdicts. Not everything here was necessary.

| Tech | Verdict | Why |
|---|---|---|
| **Go + Echo** | **Need** | One goroutine per request handles a burst with no thread pool to tune. Echo adds routing and middleware without taking over the architecture. |
| **Postgres** | **Need** | The no-double-booking guarantee lives here. Real transactions and row locks. Nothing else in the stack can replace this. |
| **`pgx` over `database/sql`** | Debatable | Faster binary protocol and native Postgres types. `database/sql` would have worked fine. |
| **Redis — holds & rate limiting** | **Need** | Two things Postgres cannot do well: keys that **delete themselves** (TTL), and a counter that two replicas share. An in-memory counter would let a limit of 10 become 20 with two replicas. |
| **Redis — the cache** | Debatable | Measured: at this scale it is a **net latency loss** of about +0.3 ms. The rate limiter costs ~0.62 ms per request and the cache only saves ~0.34 ms. It pays off when the query is expensive or the database is on another host — not against a warm local Postgres. Kept, with the numbers stated. |
| **Nginx — one entry point** | **Need** | The API containers publish no ports at all. One door, TLS terminates here, backends invisible from outside. |
| **Nginx — load balancing** | No need *(here)* | Measured above: on one laptop, 2 replicas are ~5% **slower** than 1. Real value only once replicas sit on different machines. |
| **gRPC + the notifier** | Debatable | Genuinely useful for showing typed contracts, deadline propagation, and graceful degradation. But notifications should really go through a **queue** — fire-and-forget means a failed notification is lost forever. Named as a known gap, not pretended solved. |
| **Docker Compose** | **Need** | The whole stack in one file that works on a fresh machine. Healthchecks + `depends_on: service_healthy` kill the "works if the database wins the race" bug class. |
| **Kubernetes** | No need *(for this scale)* | Compose runs this project fine. Kubernetes is here for what Compose genuinely cannot do: self-healing, rolling updates, liveness vs readiness. Honest reason: learning value. |
| **k6** | **Need** | The correctness claims are k6 **thresholds**, so `k6 run` exits non-zero the moment locking regresses. Without it, "it doesn't double book" is just an opinion. |
| **golang-migrate** | **Need** | Schema changes are versioned, reviewed, ordered, and reversible. |
| **`log/slog` JSON** | **Need** | Logs you can filter by field. `fmt.Println` gives you text nobody can query at 3am. |
| **Swagger / OpenAPI** | Debatable | Generated from annotations next to the handlers, so docs cannot drift. Nice, not essential. Dev-only. |


---

## How to run

### Option 1 — Docker Compose (easiest, gives you everything)

```bash
cd eventreg
docker compose up -d --build     # postgres, redis, api1, api2, notifier, nginx

curl localhost:8080/health       # -> ok
curl localhost:8080/ready        # -> {"status":"ready"}
open  localhost:8080/docs/       # Swagger UI

docker compose logs -f api1
docker compose down              # stop, keep the data
docker compose down -v           # stop and delete the data too
```

Everything goes through Nginx on **port 8080**. The API replicas publish no host port at
all — that is the point of a reverse proxy.

Try it end to end:

```bash
TOKEN=$(curl -s -X POST localhost:8080/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"email":"me@example.com","password":"a-long-enough-password"}' | jq -r .token)

curl -s -X POST localhost:8080/events \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"Go Workshop","seats":2}'

curl -s -X POST localhost:8080/events/1/book \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"seats":1}'
```

### Option 2 — Kubernetes (kind)

```bash
cd eventreg
kind create cluster --config k8s/kind-cluster.yaml
docker build -t eventreg-api:local .
kind load docker-image eventreg-api:local     # no registry needed
kubectl apply -f k8s/
kubectl get pods -w
```

Watch self-healing for yourself:

```bash
kubectl get pods -n eventreg                        # two api pods
kubectl delete pod -n eventreg -l app=api --wait=false
kubectl get pods -n eventreg -w                     # replacements appear on their own
```

`replicas: 2` is **desired state**, not a one-time instruction. Delete a pod and the
controller notices reality no longer matches and creates a replacement. Nobody told it to.

### Option 3 — no Docker at all

You need Postgres and Redis running locally.

```bash
cd eventreg
export DATABASE_URL='postgres://eventgo:secret@localhost:5432/eventgo?sslmode=disable'
export REDIS_URL='redis://localhost:6379/0'
export JWT_SECRET='local-dev-secret-at-least-32-characters-long'
go run ./cmd/api
```

Migrations run automatically at boot. Config is 12-factor: every setting is an environment
variable, checked once at startup, and the app **refuses to start** on bad config rather
than failing on the first request.

Try this: unset `REDIS_URL`. The app still works — caching, holds and rate limiting just
switch off. A cache is an optimisation, never a dependency.

---

## Repository layout

```
eventreg/                        the actual project
  cmd/
    api/                         the HTTP server
    notifier/                    the gRPC notification service
    loadtest/                    an older Go concurrency harness (k6 replaced it)
  internal/
    handlers/                    HTTP layer: routes, binding, status codes
    storage/
      storage.go                 the Store interface
      postgres.go                the locking logic — the heart of the project
      cached.go                  the Redis cache decorator
      users.go                   user queries
    cache/                       Redis client setup
    holds/                       temporary seat reservations (Lua + TTL)
    ratelimit/                   fixed-window limiter (INCR + EXPIRE)
    auth/                        bcrypt hashing + JWT issue/verify
    config/                      12-factor config, validated at startup
    logging/                     slog setup
    models/                      Event, Booking, Waitlist, User, Page[T]
    db/migrations/               versioned SQL, up and down
  loadtest/k6/
    launch-burst.js              the correctness proof (asserts via thresholds)
    sustained.js                 open-model throughput and latency
    matrix.js                    the fixed-request-count matrix in this README
    RESULTS.md                   full numbers and method
  k8s/                           namespace, config, Postgres/Redis, API, notifier, ingress
  nginx/
    nginx.conf                   2 replicas, round-robin
    nginx-single.conf            1 replica, for the A/B test
  proto/                         the protobuf contract
  docker-compose.yml             the normal stack
  docker-compose.loadtest.yml    lifts the rate limit for measurement runs
  docker-compose.single.yml      drops to 1 replica for the A/B test
  docker-compose.nocache.yml     disables Redis, for the cache A/B

