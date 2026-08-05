-- Migration 2 (DOWN): reverse migration 2.
--
-- ORDER MATTERS. bookings and waitlist hold foreign keys REFERENCING users,
-- so users cannot be dropped while those tables still point at it — Postgres
-- would refuse. Drop the children first, then the parent.
--
-- Dropping the tables also drops their indexes automatically, so there's no
-- separate DROP INDEX needed here.
DROP TABLE IF EXISTS waitlist;
DROP TABLE IF EXISTS bookings;
DROP TABLE IF EXISTS users;
