# syntax=docker/dockerfile:1
#
# MULTI-STAGE BUILD.
#
# The idea: use a big image with the full Go toolchain to COMPILE, then copy
# only the resulting binary into a tiny image to RUN. The build tools never
# ship.
#
#   golang:1.26-alpine   ~250 MB   (compiler, stdlib source, module cache)
#   final image          ~15 MB    (alpine + one static binary)
#
# Why that 235 MB matters beyond disk:
#   - PULL SPEED. Every deploy, every node, every autoscale event downloads it.
#   - ATTACK SURFACE. A compiler, shell utilities, and package manager in
#     production are tools an attacker can use after a breakout. If they're not
#     in the image, they can't be used.
#   - CVE NOISE. Every package is something to patch. Fewer packages, fewer
#     security bulletins that apply to you.


# ============================================================ STAGE 1: build
# `AS builder` names the stage so the final stage can copy from it.
FROM golang:1.26-alpine AS builder

WORKDIR /src

# ---- LAYER CACHING: THE MOST IMPORTANT IDEA IN THIS FILE ----
#
# Docker caches each instruction as a layer. On rebuild it reuses a layer if
# that instruction AND its inputs are unchanged. The catch: once one layer is
# invalidated, EVERY layer after it is rebuilt too.
#
# So order instructions from LEAST to MOST frequently changed.
#
# Dependencies change rarely; your source changes constantly. Copying go.mod
# and go.sum FIRST and downloading modules in their own layer means editing a
# handler reuses the cached download. Doing `COPY . .` first instead would
# re-download every module on every single build — minutes wasted, every time.
COPY go.mod go.sum ./
RUN go mod download

# NOW the source. This layer is invalidated by any code change — which is
# fine, because only the compile step below repeats.
COPY . .

# ---- CGO_ENABLED=0: the flag that makes a tiny image possible ----
#
# By default Go may link against the system C library (for DNS resolution and
# user lookups). That produces a DYNAMICALLY linked binary which needs libc at
# runtime — and alpine uses musl while the builder might use glibc, giving you
# the classic "no such file or directory" error on a binary that's clearly
# there. CGO_ENABLED=0 forces a pure-Go, STATICALLY linked binary that depends
# on nothing. It's also what makes a FROM scratch image possible at all.
#
# -ldflags="-s -w" strips the symbol table and DWARF debug info: a few MB off,
# at the cost of readable stack traces in a debugger. Worth it for a service
# you'd debug from logs and traces anyway.
#
# -trimpath removes absolute filesystem paths from the binary, making builds
# reproducible and not leaking your directory layout.
# TWO BINARIES from one build stage (lesson 20). The API and the notification
# service share this module — the same models, config, and logging packages —
# so building both here costs almost nothing (the compile cache is shared) and
# guarantees they're built from identical source.
#
# One IMAGE ships both; compose picks which to run by overriding ENTRYPOINT.
# The alternative — a separate image per service — is more "correct" for
# independent deployment, but for two services in one repo this keeps the build
# simple and the versions impossible to skew.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/api ./cmd/api \
 && CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/notifier ./cmd/notifier


# ============================================================ STAGE 2: run
# A fresh, minimal base. NOTHING from the builder carries over except what we
# explicitly COPY --from.
FROM alpine:3.20

# ca-certificates: needed for any outbound TLS (a managed Postgres with
# sslmode=require, or the notification service later). Without it you get
# "x509: certificate signed by unknown authority" — a confusing error for
# something as mundane as a missing root cert bundle.
RUN apk add --no-cache ca-certificates && \
    # ---- RUN AS A NON-ROOT USER ----
    # Containers run as root by default. If someone finds a way out of the
    # process, root inside the container is a much better starting position
    # than an unprivileged user — especially combined with a mounted volume.
    # Costs nothing; closes a whole class of escalation.
    adduser -D -u 10001 -h /app appuser

WORKDIR /app

# Copy ONLY the binary from the builder stage. The Go toolchain, module cache,
# and source code are all left behind.
COPY --from=builder /out/api      /app/api
COPY --from=builder /out/notifier /app/notifier

USER appuser

# EXPOSE is DOCUMENTATION ONLY — it publishes nothing. It records the port the
# image expects to serve on, for humans and for tools like `docker run -P`.
# Actual publishing happens with `-p host:container` or compose's `ports:`.
EXPOSE 8082

# ENTRYPOINT (exec form, a JSON array) runs the binary as PID 1 DIRECTLY, with
# no shell wrapping it.
#
# THIS IS WHY GRACEFUL SHUTDOWN WORKS. In the shell form (ENTRYPOINT ./api),
# /bin/sh becomes PID 1 and does NOT forward SIGTERM to your program — so
# `docker stop` waits 10 seconds, then SIGKILLs, and every in-flight request
# dies. The exec form makes your process PID 1, so it receives SIGTERM
# directly and lesson 14's shutdown code actually runs.
#
# A subtle, high-impact detail: same Dockerfile, one pair of brackets, and the
# difference between a clean deploy and dropped traffic on every restart.
ENTRYPOINT ["/app/api"]
