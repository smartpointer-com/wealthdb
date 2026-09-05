package gold

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrUnknownAccount is returned, wrapped, by an AccountResolver for a
// reference that names no account of the source — neither an
// account_external_id nor a nickname. A caller that can tolerate the
// case (a source not loaded yet has no accounts at all) tests for it
// with errors.Is; the ambiguous-nickname error is deliberately not
// this, because two accounts sharing a nickname is a configuration
// fault whatever the load state.
var ErrUnknownAccount = errors.New("account not found")

// AccountResolver maps a user-written account reference — a gold
// account_external_id or an account nickname — to the
// account_external_id, within one source. It is what every
// config-sourced ledger keyed by account uses (the equity-transfer
// ledger, the spending pins), so a nickname resolves the same way
// wherever a person writes it.
type AccountResolver func(account string) (string, error)

// NewAccountResolver reads the source's accounts once and returns the
// resolver over them. An id always wins over a nickname of the same
// spelling; a nickname shared by more than one account is refused as
// ambiguous rather than resolved to either.
func NewAccountResolver(ctx context.Context, tx *sql.Tx, sourceID string) (AccountResolver, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT account_external_id, COALESCE(nickname, '') FROM accounts WHERE silver_source_id = ?`,
		sourceID)
	if err != nil {
		return nil, fmt.Errorf("scan accounts: %w", err)
	}
	defer rows.Close()
	byID := map[string]bool{}
	byNick := map[string]string{}
	nickDup := map[string]bool{}
	for rows.Next() {
		var id, nick string
		if err := rows.Scan(&id, &nick); err != nil {
			return nil, err
		}
		byID[id] = true
		if nick != "" {
			if _, seen := byNick[nick]; seen {
				nickDup[nick] = true
			}
			byNick[nick] = id
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return func(account string) (string, error) {
		if byID[account] {
			return account, nil
		}
		if nickDup[account] {
			return "", fmt.Errorf("account %q (source %s) is an ambiguous nickname; use the account_external_id", account, sourceID)
		}
		if id, ok := byNick[account]; ok {
			return id, nil
		}
		return "", fmt.Errorf("%w: %q in source %s (use its account_external_id or nickname)", ErrUnknownAccount, account, sourceID)
	}, nil
}
