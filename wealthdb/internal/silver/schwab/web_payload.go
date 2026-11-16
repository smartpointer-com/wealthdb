package schwab

import (
	"encoding/json"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/silver"
)

// webNarrative is the row's narrative for gold: the movement, then the
// security it concerns — the fidelity adapter's action-then-security
// reading. The security alone cannot tell a dividend from the withholding
// taken from it, or an ADR's pass-through fee from either: all three name
// the same security. The movement is the row's action column: the
// transaction history's `Action` ("NRA Tax Adj", "ADR Mgmt Fee"), or on a
// statement row its `action` or `category`, where it states one.
func webNarrative(movement string, security *string) *string {
	sec := ""
	if security != nil {
		sec = *security
	}
	if movement != "" && strings.HasPrefix(strings.ToUpper(sec), strings.ToUpper(movement)) {
		movement = "" // the narrative already leads with it
	}
	return silver.StrPtrIfNonEmpty(strings.TrimSpace(movement + " " + sec))
}

// distributionIsCash reports whether a third_party_distribution row is a cash
// transfer (payload.transfer_kind == "cash"). Cash transfers may overlap a
// statement cash debit and are deduped against it; securities transfers
// (transfer_kind == "securities") are new data and are left untouched.
func distributionIsCash(payload string) bool {
	var p struct {
		TransferKind string `json:"transfer_kind"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return false
	}
	return p.TransferKind == "cash"
}
