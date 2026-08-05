-- Migration 1 (DOWN): undo migration 1.
--
-- Every .up.sql should have a .down.sql that reverses it. Down migrations are
-- what make a bad deploy recoverable: `migrate down 1` walks the schema back
-- one step instead of leaving you to fix production by hand.
--
-- Write these even if you rarely run them — the discipline of "how would I
-- undo this?" catches migrations that are quietly irreversible (e.g. dropping
-- a column destroys its data; no down migration can bring it back).
DROP TABLE IF EXISTS events;
