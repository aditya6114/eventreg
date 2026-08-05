-- Migration 1 (UP): the events table.
--
-- This mirrors the schema that postgresStore.init() used to create by hand.
-- IF NOT EXISTS makes it safe to apply to a database that already has the
-- table from those earlier lesson-8 runs.
--
-- Naming: golang-migrate requires <version>_<name>.up.sql / .down.sql.
-- The version prefix defines the ORDER migrations run in, and is recorded in
-- a schema_migrations table so each one is applied exactly once.
CREATE TABLE IF NOT EXISTS events (
    id    SERIAL PRIMARY KEY,
    name  TEXT   NOT NULL,
    -- CHECK is a DATABASE-level guarantee: even a buggy app (or someone with
    -- a psql prompt) cannot drive seats below zero. Defence in depth behind
    -- the application's own logic.
    seats INT    NOT NULL CHECK (seats >= 0)
);
