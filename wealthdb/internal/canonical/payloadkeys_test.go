package canonical

import (
	"os"
	"strings"
	"testing"
)

// TestTxnEvidenceProjectsEveryKey: the txn_evidence() macro reads each
// cross-source key by the name declared here, so a rename on one side
// shows.
func TestTxnEvidenceProjectsEveryKey(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("../gold/derived/40_flows.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, body, ok := strings.Cut(string(b), "CREATE MACRO txn_evidence()")
	if !ok {
		t.Fatal("no txn_evidence() macro in derived/40_flows.sql")
	}
	body, _, _ = strings.Cut(body, ");\n")
	for _, key := range []string{
		PayloadBankRef, PayloadCounterAccount, PayloadCounterCurrency, PayloadCounterAmount,
		PayloadReturnsFlow, PayloadSourceKind, PayloadFeeFor, PayloadEquityTransferLedger,
	} {
		if !strings.Contains(body, "'$."+key+"'") {
			t.Errorf("txn_evidence() does not read $.%s", key)
		}
	}
}

// TestPrivateLegsReadsTheHoldingKey: private_legs() reads the holding an
// equity-transfer out-leg names by the name declared here.
func TestPrivateLegsReadsTheHoldingKey(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("../gold/derived/30_holdings.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, body, ok := strings.Cut(string(b), "CREATE MACRO private_legs(")
	if !ok {
		t.Fatal("no private_legs() macro in derived/30_holdings.sql")
	}
	body, _, _ = strings.Cut(body, ");\n")
	if !strings.Contains(body, "'$."+PayloadHolding+"'") {
		t.Errorf("private_legs() does not read $.%s", PayloadHolding)
	}
}
