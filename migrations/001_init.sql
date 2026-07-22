-- +goose Up
CREATE TABLE environments (              -- FK target, not free text
  name text PRIMARY KEY CHECK (name ~ '^[a-z0-9-]{1,32}$')
);
INSERT INTO environments VALUES ('dev'), ('staging'), ('prod');

CREATE TABLE flags (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  key                 text NOT NULL CHECK (key ~ '^[a-z0-9][a-z0-9_-]{0,63}$'),
  environment         text NOT NULL REFERENCES environments(name),
  enabled             boolean NOT NULL DEFAULT false,
  rollout_percentage  smallint NOT NULL DEFAULT 0
                      CHECK (rollout_percentage BETWEEN 0 AND 100),
  version             integer NOT NULL DEFAULT 1,       -- optimistic concurrency
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  UNIQUE (key, environment)
);
CREATE INDEX ON flags (environment);

-- Keeps updated_at correct even for out-of-band UPDATEs.
-- +goose StatementBegin
CREATE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
  NEW.updated_at = now();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER flags_set_updated_at
  BEFORE UPDATE ON flags
  FOR EACH ROW
  EXECUTE FUNCTION set_updated_at();

-- Latency accelerator only; the Phase 6 poller is the correctness guarantee.
-- STATEMENT-level since the listener does a full refresh regardless of row
-- count. pg_notify(), not NOTIFY: PL/pgSQL can't call NOTIFY directly.

-- +goose StatementBegin
CREATE FUNCTION notify_flags_changed() RETURNS trigger AS $$
BEGIN
  PERFORM pg_notify('flags_changed', '');
  RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER flags_notify_changed
  AFTER INSERT OR UPDATE OR DELETE ON flags
  FOR EACH STATEMENT
  EXECUTE FUNCTION notify_flags_changed();

CREATE TABLE api_keys (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name              text NOT NULL,
  key_hash          bytea NOT NULL UNIQUE,   -- sha256(raw). Raw is never stored.
  key_prefix        text NOT NULL,           -- "lo_live_a1b2" — for display/audit only
  is_admin          boolean NOT NULL DEFAULT false,  -- false: evaluate-only. true: full CRUD.
  rate_limit_rps    real NOT NULL DEFAULT 50,
  rate_limit_burst  integer NOT NULL DEFAULT 100,
  active            boolean NOT NULL DEFAULT true,
  created_at        timestamptz NOT NULL DEFAULT now(),
  last_used_at      timestamptz
);

-- +goose Down
DROP TABLE api_keys;
DROP TABLE flags;
DROP FUNCTION notify_flags_changed();
DROP FUNCTION set_updated_at();
DROP TABLE environments;
