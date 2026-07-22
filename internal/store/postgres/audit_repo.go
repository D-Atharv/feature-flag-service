package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// insertAuditTx writes one flag_audit row inside an existing transaction.
// before/after must be pre-marshalled JSON ([]byte) or nil.
// actorKeyID is stored as NULL when empty.
func insertAuditTx(ctx context.Context, tx pgx.Tx, flagKey, environment, action, actorKeyID string, before, after []byte) error {
	const q = `
		INSERT INTO flag_audit (flag_key, environment, action, actor_key_id, before, after)
		VALUES ($1, $2, $3, $4, $5, $6)`

	var actor any
	if actorKeyID != "" {
		actor = actorKeyID
	}

	_, err := tx.Exec(ctx, q, flagKey, environment, action, actor, before, after)
	if err != nil {
		return fmt.Errorf("insert audit row: %w", err)
	}
	return nil
}
