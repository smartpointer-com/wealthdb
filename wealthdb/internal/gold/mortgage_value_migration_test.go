package gold

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

// A gold whose mortgage rule rows read internal_transfer, migrated,
// holds what the pass writes now: mortgage_transfer on each row the
// rule placed, whether a link gave it the class or the source named its
// far account, and on no other row. The class leaves transfer_links.
func TestTheMortgageRuleRowsMoveToTheirValue(t *testing.T) {
	t.Parallel()
	ms, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(ms, func(m migration) bool { return strings.HasSuffix(m.name, "_mortgage_rule_value.sql") })
	if i < 1 {
		t.Fatal("no mortgage rule migration above another")
	}
	db, ctx := openAtVersion(t, ms[i-1].version)
	seedResolutionFixture(t, db, ctx)
	mustExec(t, db, `INSERT INTO spend_account_scope VALUES ('cf', 'FENCED', 'exclude');
	    INSERT INTO transactions (silver_source_id, transaction_external_id, occurred_at,
	        account_external_id, kind, currency, net_amount, counterparty, description) VALUES
	    ('cf', 'M-CLASS',   1728000, 'CASH',   'withdrawal', 'USD', -900, NULL, 'instalment'),
	    ('cf', 'M-STATED',  1728000, 'CASH',   'withdrawal', 'USD', -900, 'Hypothek AG', 'June'),
	    ('cf', 'M-PHRASE',  1728000, 'CASH',   'withdrawal', 'USD', -900, NULL, 'home  loan 06'),
	    ('cf', 'M-MEMO',    1728000, 'CASH',   'withdrawal', 'USD', -900, NULL, 'wire — for the mortgage'),
	    ('cf', 'M-WORD',    1728000, 'CASH',   'withdrawal', 'USD', -900, NULL, 'MORTGAGES2'),
	    ('cf', 'M-DECL',    1728000, 'CASH',   'withdrawal', 'USD', -300, NULL, 'to savings'),
	    ('cf', 'M-INBOUND', 1728000, 'CASH',   'deposit',    'USD',  300, NULL, 'mortgage refund'),
	    ('cf', 'M-FENCED',  1728000, 'FENCED', 'withdrawal', 'USD', -900, NULL, 'mortgage'),
	    ('cf', 'M-PAIRED',  1728000, 'CASH',   'withdrawal', 'USD', -900, NULL, 'mortgage');
	    INSERT INTO spend_txn_enrichment (silver_source_id, transaction_external_id,
	        merchant_signature, signature_version, spend_detailed, provenance, assigned_at) VALUES
	    ('cf', 'M-CLASS',   'INSTALMENT', 1, 'internal_transfer', 'rule', 100),
	    ('cf', 'M-STATED',  'HYPOTHEK AG', 1, 'internal_transfer', 'rule', 100),
	    ('cf', 'M-PHRASE',  'HOME LOAN', 1, 'internal_transfer', 'rule', 100),
	    ('cf', 'M-MEMO',    'WIRE', 1, 'internal_transfer', 'rule', 100),
	    ('cf', 'M-WORD',    'MORTGAGES', 1, 'internal_transfer', 'rule', 100),
	    ('cf', 'M-DECL',    'TO SAVINGS', 1, 'internal_transfer', 'rule', 100),
	    ('cf', 'M-INBOUND', 'MORTGAGE REFUND', 1, 'internal_transfer', 'rule', 100),
	    ('cf', 'M-FENCED',  'MORTGAGE', 1, 'internal_transfer', 'rule', 100),
	    ('cf', 'M-PAIRED',  'MORTGAGE', 1, 'internal_transfer', 'matcher', 100);
	    INSERT INTO transfer_links (silver_source_id, transaction_external_id, far_silver_source_id,
	        far_account_external_id, far_transaction_external_id, far_class, evidence, measure) VALUES
	    ('cf', 'M-CLASS',   NULL, NULL, NULL, 'mortgage', 'rule', 'cash'),
	    ('cf', 'M-STATED',  'cf', 'MORT', NULL, NULL, 'stated-account', 'cash'),
	    ('cf', 'M-DECL',    'declared', 'DECL1', NULL, NULL, 'rule', 'cash'),
	    ('cf', 'M-INBOUND', 'declared', 'DECL1', NULL, NULL, 'rule', 'cash'),
	    ('cf', 'M-PAIRED',  'cf', 'MORT', 'M-OTHER', NULL, 'amount', 'cash')`)
	if err := applyMigration(ctx, db, ms[i]); err != nil {
		t.Fatalf("apply %s: %v", ms[i].name, err)
	}
	got := scanPairs(t, db, `SELECT transaction_external_id, spend_detailed FROM spend_txn_enrichment`)
	want := map[string]string{
		// The rule's rows: by the class, by a stated far account, by its
		// phrase, and by its token in the description before the memo.
		"M-CLASS": "mortgage_transfer", "M-STATED": "mortgage_transfer", "M-PHRASE": "mortgage_transfer",
		// The memo is no narrative of the built-ins', a token is a whole
		// word, a config rule's row is its own, a leg outside the
		// population or the scope meets no built-in, and the matcher's
		// pair is its own.
		"M-MEMO": "internal_transfer", "M-WORD": "internal_transfer", "M-DECL": "internal_transfer",
		"M-INBOUND": "internal_transfer", "M-FENCED": "internal_transfer", "M-PAIRED": "internal_transfer",
	}
	if !maps.Equal(got, want) {
		t.Errorf("migrated verdicts = %v, want %v", got, want)
	}
	links := scanPairs(t, db, `SELECT transaction_external_id, concat_ws(' ', far_silver_source_id,
	        far_account_external_id, far_transaction_external_id, evidence) FROM transfer_links`)
	wantLinks := map[string]string{
		"M-STATED": "cf MORT stated-account", "M-DECL": "declared DECL1 rule",
		"M-INBOUND": "declared DECL1 rule", "M-PAIRED": "cf MORT M-OTHER amount",
	}
	if !maps.Equal(links, wantLinks) {
		t.Errorf("migrated links = %v, want %v", links, wantLinks)
	}
}
