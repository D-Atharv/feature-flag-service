-- +goose Up
CREATE TABLE flag_audit (
  id          bigserial PRIMARY KEY,
  flag_key    text        NOT NULL,
  environment text        NOT NULL,
  action      text        NOT NULL CHECK (action IN ('created', 'updated', 'deleted')),
  actor_key_id uuid,                -- NULL for seed/out-of-band writes
  before      jsonb,
  after       jsonb,
  at          timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE flag_audit;
