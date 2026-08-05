-- Migration 2 (UP): users, bookings, and the waitlist.
--
-- This is where the app stops being "a counter of seats" and becomes a real
-- registration system that knows WHO registered for WHAT.

-- ---------------------------------------------------------------- users ----
CREATE TABLE users (
    id            SERIAL PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,   -- UNIQUE = the DB rejects duplicate signups
    -- We store a bcrypt HASH, never the password itself. Named _hash so the
    -- intent is unmissable in every query that touches it.
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ------------------------------------------------------------- bookings ----
CREATE TABLE bookings (
    id       SERIAL PRIMARY KEY,

    -- FOREIGN KEY: event_id must point at a real events row. The database
    -- enforces referential integrity — you cannot create a booking for an
    -- event that doesn't exist.
    -- ON DELETE CASCADE: deleting an event automatically deletes its bookings,
    -- so you never end up with orphaned rows pointing at nothing.
    event_id INT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    user_id  INT NOT NULL REFERENCES users(id)  ON DELETE CASCADE,

    seats      INT NOT NULL CHECK (seats > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- IDEMPOTENCY, ENFORCED BY THE DATABASE.
    -- A checklist cross-cutting item: "retrying a booking request must not
    -- double-book." If a student's browser retries a request that already
    -- succeeded, this constraint makes the second INSERT fail instead of
    -- silently creating a second booking. The app catches that specific
    -- violation and returns the ORIGINAL booking — a retry becomes a no-op.
    -- Application-level "check then insert" cannot guarantee this under
    -- concurrency; a UNIQUE constraint can, because the DB checks it atomically.
    UNIQUE (event_id, user_id)
);

-- INDEXES — the checklist's "indexes" item, with the reasoning that matters.
--
-- An index is a lookup structure (a B-tree) that lets Postgres FIND rows
-- without scanning the whole table. Without one, "all bookings for event 7"
-- reads every row in bookings (a sequential scan) — fine at 100 rows,
-- catastrophic at 10 million.
--
-- Rule of thumb: index the columns you FILTER by (WHERE) and JOIN on.
-- Foreign keys are prime candidates, because you constantly query "all the
-- children of this parent".
--
-- The trade-off (and the interview answer): indexes make reads faster but
-- writes slower, because every INSERT/UPDATE must also update the index. You
-- index deliberately, not everywhere.
CREATE INDEX idx_bookings_event_id ON bookings(event_id);
CREATE INDEX idx_bookings_user_id  ON bookings(user_id);

-- ------------------------------------------------------------- waitlist ----
-- The differentiator feature: when an event is full, students join a queue
-- instead of just getting a 409. If someone cancels, the earliest waitlisted
-- user gets promoted.
CREATE TABLE waitlist (
    id       SERIAL PRIMARY KEY,
    event_id INT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    user_id  INT NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
    seats    INT NOT NULL CHECK (seats > 0),

    -- FIFO fairness: position in the queue is decided by arrival time, so
    -- "first to try, first to get promoted". created_at IS the queue order —
    -- no separate position column to keep in sync when rows are removed.
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (event_id, user_id)  -- can't join the same waitlist twice
);

-- Promotion always asks "who's been waiting longest for THIS event?", i.e.
-- WHERE event_id = $1 ORDER BY created_at. A composite index matching that
-- exact access pattern lets Postgres answer it without sorting the table.
CREATE INDEX idx_waitlist_event_created ON waitlist(event_id, created_at);
