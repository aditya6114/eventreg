# Launch-Proof Event Registration API

> **The problem:** high-demand event registration systems die on launch day. The site
> falls over when thousands of people hit "register" in the same second, and limited
> workshop seats get double-booked — two people, one seat, and a support ticket.
>
> **This backend is built to survive that stampede.** It never double-books a seat, it
> auto-waitlists when an event fills, it rate-limits abusive clients, and it stays
> correct under a launch-second burst. The claim is not a design opinion — it is a
> load test that fails the build if it stops being true.

Built in Go, with Postgres, Redis, Nginx, gRPC, Docker and Kubernetes.

---

## The headline proof

A k6 burst fires **1,000 concurrent registrations at an event with 100 seats**, all
released at essentially the same instant, each from a distinct authenticated user.

| Measured | Result |
|---|---|
| Bookings created (201) | **100 — exactly the seat count** |
| Waitlisted (202) | **900 — everyone who missed out, queued not dropped** |
| Oversold seats | **0** |
| Seats left unsold | **0** (`seats` in Postgres = exactly 0) |
| Errors / 5xx | **0** |
| Failed checks | **0 of 2,000** |
| Burst drained in | **3.5–4.5 s** across runs (~3.5–4.5 ms of lock-held work per booking) |

Correctness was identical on every run; only the timing varied. The burst's p99 is
*queueing delay by design* — 1,000 requests serializing on one row lock — not service
time, so it is never quoted as latency. That number comes from the sustained test.

And under realistic (open-model) arrival on the cached read path:

| Offered | Achieved | p95 | p99 | Errors |
|---|---|---|---|---|
| 2,000 req/s | **1,992 req/s** | **6.13 ms** | 14.47 ms | **0** |
| 3,000 req/s | 2,977 req/s | 11.10 ms | 33.52 ms | 0 (0.35 % dropped — the knee) |

All tiers on one laptop. Full method, the cache-vs-rate-limiter A/B, and the caveats:
[`eventreg/loadtest/k6/RESULTS.md`](eventreg/loadtest/k6/RESULTS.md).

The correctness properties are encoded as **k6 thresholds**, not printed for a human to
eyeball — so `k6 run` exits non-zero the moment the locking regresses. Reproduce it with:

```bash
cd eventreg
docker compose up -d --build
docker run --rm --network eventreg_default \
  -e BASE_URL=http://nginx:80 -e SEATS=100 -e BUYERS=1000 \
  -v "$PWD/loadtest/k6:/scripts:ro" grafana/k6 run /scripts/launch-burst.js
```

---

## Architecture

```mermaid
flowchart LR
    C["Clients<br/>(1000 at once)"]

    subgraph edge["Edge"]
      N["<b>Nginx</b> :8080<br/>reverse proxy<br/>round-robin LB"]
    end

    subgraph app["Stateless app tier"]
      A1["<b>api1</b> :8082<br/>Go / Echo"]
      A2["<b>api2</b> :8082<br/>Go / Echo"]
    end

    subgraph data["State"]
      P[("<b>Postgres</b><br/>system of record")]
      R[("<b>Redis</b><br/>cache · holds · rate limit")]
    end

    NT["<b>notifier</b> :50051<br/>gRPC service"]

    C --> N
    N --> A1
    N --> A2
    A1 --> P
    A2 --> P
    A1 --> R
    A2 --> R
    A1 -. "gRPC<br/>(degrades gracefully)" .-> NT
    A2 -. gRPC .-> NT
```

**The request's full journey** (worth being able to whiteboard cold):
client → Nginx (picks a replica, sets `X-Forwarded-For`) → API pod → rate limiter (Redis
`INCR`) → JWT verify → Postgres transaction with `SELECT … FOR UPDATE` → release Redis
hold → fire-and-forget gRPC notification → JSON response.

### Why the API is stateless

Two replicas share **no memory**. Everything a request needs is either in the JWT it
carries or in Postgres/Redis. That is what makes round-robin load balancing safe: a token
minted by `api1` verifies on `api2` because both hold the same signing secret, and the
rate limiter counts in Redis so a limit of 10 stays 10 instead of drifting to 20 with a
second replica.

---

## The double-booking answer, in four layers

This is the core of the project. Each layer solves something the layer above cannot.

| Layer | Mechanism | What it guarantees |
|---|---|---|
| **1. Postgres transaction** | `BEGIN` → `SELECT … FOR UPDATE` → `UPDATE` → `COMMIT` | The durable, final guarantee. Concurrent bookers **serialize** on the event row, so the seat count can never be read stale and written back. |
| **2. Unique constraint** | `UNIQUE (event_id, user_id)` | Idempotency the database enforces. A retried request returns the *original* booking instead of creating a second one — application-level "check then insert" cannot promise this under concurrency. |
| **3. Redis hold** | Lua-atomic `SETNX` + TTL | A *temporary* reservation for the checkout window, visible to every replica, that **releases itself** if the user abandons. No sweeper cron to own. |
| **4. Waitlist** | FIFO queue + promotion inside the cancel transaction | A full event returns `202 Accepted`, not a dead end. Cancellations promote the longest-waiting user atomically. |

**Why a `sync.Mutex` is not the answer** (the interview trap): a mutex coordinates
goroutines inside *one process*. Two replicas behind Nginx have two independent mutexes
that know nothing about each other, so both would happily sell the same seat. The
coordination point must be somewhere both replicas can see — Postgres for durability,
Redis for speed.

---

## What each technology does, and why it's here

| Technology | Role | Why this and not the alternative |
|---|---|---|
| **Go + Echo** | HTTP API | Goroutine-per-request handles a burst without a thread pool to tune. Echo adds routing and middleware without becoming a framework that owns your architecture. |
| **Postgres** (`pgx`) | System of record | Needs real transactions and row locks — the oversell guarantee lives here. `pgx` over `database/sql` for native Postgres types and a faster binary protocol. |
| **Redis** | Cache, seat holds, rate limiting | Chosen for one property Postgres lacks: **native TTL**. A hold that deletes itself is why abandoned checkouts can't strangle a popular event. Also the only place a limiter shared across replicas can live. |
| **Nginx** | Reverse proxy, load balancer | One public entry point; backends unreachable from outside. Round-robin across replicas proves the app is stateless. Also the natural TLS-termination point. |
| **gRPC** (notifier) | Service-to-service | A typed contract (protobuf) and HTTP/2 for an internal call where REST's human-readability buys nothing. Demonstrates deadline propagation and graceful degradation. |
| **Docker Compose** | Local stack | The whole topology in one checked-in file, reproducible on a fresh machine. Healthchecks + `depends_on: service_healthy` remove the "works if the DB wins the race" class of bug. |
| **Kubernetes** (kind) | Orchestration | What Compose can't do: self-healing, rolling updates with `maxUnavailable: 0`, liveness *vs* readiness probes, declarative replica count. |
| **k6** | The proof | Correctness asserted as thresholds, so the concurrency claim is CI-able rather than anecdotal. |
| **golang-migrate** | Schema versioning | Schema changes are code, reviewed and ordered, with a down path. |
| **`log/slog`** | Structured logging | JSON logs a aggregator can filter by field. `fmt.Println` produces text nobody can query at 3am. |
| **Swagger / OpenAPI** | Contract | Generated from annotations next to the handlers, so the docs can't drift from the code. Mounted dev-only. |

Deliberately **out of scope** (know they exist, they're the "what next" answer): Kafka,
Helm, Argo CD, HPA autoscaling, service mesh, distributed sagas, Prometheus/Grafana.

---

## API

Auth is a bearer JWT: `Authorization: Bearer <token>`.

| Method | Path | Auth | Notes |
|---|---|---|---|
| `GET` | `/health` | — | Liveness: is the process up |
| `GET` | `/ready` | — | Readiness: are dependencies usable |
| `POST` | `/auth/register` | — | `201` + token (bcrypt hash stored, never the password) |
| `POST` | `/auth/login` | — | `200` + token; stricter rate limit — it's a password oracle |
| `GET` | `/auth/me` | ✔ | Current user from the verified token |
| `GET` | `/events` | — | Paginated `Page[T]` envelope (`limit`/`offset`, clamped) |
| `GET` | `/events/:id` | — | Cache-aside read through Redis |
| `GET` | `/events/stats` | — | `JOIN` + `GROUP BY`: bookings count and seats sold per event |
| `POST` | `/events` | ✔ | `201` |
| `POST` | `/events/:id/book` | ✔ | **`201`** booked · **`202`** waitlisted · **`200`** idempotent replay |
| `DELETE` | `/events/:id/book` | ✔ | Cancels and promotes from the waitlist in one transaction |
| `POST` | `/events/:id/hold` | ✔ | Temporary seat hold with a TTL countdown (Redis-only) |
| `DELETE` | `/events/:id/hold` | ✔ | Release early |
| `GET` | `/me/bookings` | ✔ | |
| `GET` | `/me/waitlist` | ✔ | Includes queue position via `ROW_NUMBER()` |
| `GET` | `/docs/*` | — | Swagger UI, development only |

**The status codes carry the meaning.** `202 Accepted` for a waitlisted request is the
deliberate choice: it's honest about "I took this, it isn't fulfilled yet", where `200`
would hide that you have no seat and `409` would imply failure when the system did
something useful for you.

---

## Running it

### Docker Compose (the full stack)

```bash
cd eventreg
docker compose up -d --build     # postgres, redis, api1, api2, notifier, nginx
curl localhost:8080/health       # -> ok
curl localhost:8080/ready        # -> {"status":"ready"}
open  localhost:8080/docs/       # Swagger UI
docker compose logs -f api1
docker compose down              # keeps the data volume; -v also deletes it
```

Everything enters through Nginx on **:8080**. The API replicas publish no host port —
that's the point of a reverse proxy.

### Kubernetes (kind)

```bash
cd eventreg
kind create cluster --config k8s/kind-cluster.yaml
docker build -t eventreg-api:local .
kind load docker-image eventreg-api:local      # no registry needed
kubectl apply -f k8s/
kubectl get pods -w
```

Manifests: [`k8s/`](eventreg/k8s) — namespace + ConfigMap/Secret, Postgres & Redis, the
API Deployment (2 replicas, liveness + readiness probes, `maxUnavailable: 0`), the
notifier Deployment, and an Ingress.

### Without Docker

```bash
cd eventreg
export DATABASE_URL='postgres://eventgo:secret@localhost:5432/eventgo?sslmode=disable'
export REDIS_URL='redis://localhost:6379/0'
export JWT_SECRET='local-dev-secret-at-least-32-characters-long'
go run ./cmd/api
```

Migrations run automatically at boot. Config is 12-factor: every knob is an environment
variable, validated once at startup, and the process **refuses to boot** on invalid
config rather than failing on the first request. Notably, unsetting `REDIS_URL` disables
caching entirely and the app still works — a cache is an optimization, never a dependency.

---

## Repository layout

```
eventreg/                  the real project
  cmd/api/                 the server
  cmd/notifier/            the gRPC notification service
  cmd/loadtest/            Go concurrency harness (predates the k6 suite)
  internal/
    handlers/              HTTP layer: routing, binding, status codes
    storage/               Store interface; postgres.go holds the locking logic
    cache/ holds/ ratelimit/   the three Redis responsibilities, separated
    auth/                  bcrypt + JWT issue/verify
    config/ logging/       12-factor config, slog setup
    db/migrations/         versioned SQL
  loadtest/k6/             the launch-burst proof + recorded results
  k8s/ nginx/ proto/       manifests, proxy config, protobuf contract
```

