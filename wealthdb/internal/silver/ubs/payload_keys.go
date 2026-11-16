package ubs

import (
	"encoding/json"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
)

// webTxNumber returns the bank's own "Transaction no." from a web row's
// silver id.
//
// The id IS that number, except on the rows whose number the bank gave
// to several movements at once — a deposit product stamps the number
// derived from its serial on every movement of its life, and a
// cross-border payment carries the correspondent's charge under the
// number of the payment it belongs to. The collector tells those apart
// with a suffix of its own (`_assign_export_txn_ids`), which is not part
// of anything the bank wrote and has to come off wherever the id is read
// AS a number: the reference this adapter publishes for the matcher to
// pair legs on, the PSN seam fold, and the narrative overlay. Left on,
// the row simply matches nothing — silently, because a reference that
// pairs nothing looks exactly like a movement that has no twin.
//
// A statement-era id comes back whole. Those are the collector's own
// content hashes, `#` and all, where the suffix names a LEG of a split
// movement; folding two legs onto one key would be a different bug.
func webTxNumber(txID string) string {
	if isStatementEraID(txID) {
		return txID
	}
	if i := strings.IndexByte(txID, '#'); i >= 0 {
		return txID[:i]
	}
	return txID
}

// bankRefKey is the payload key the MT940 feed already writes its `:61:`
// account-servicing-institution reference to (cashMovementPayload.BankRef),
// so a consumer reads ONE field whichever feed produced the row — the same
// bargain counterAccountKey strikes.
const bankRefKey = `"bank_ref":`

// webBankRef returns the bank reference a web transaction id names, or "" for
// an id that names none.
//
// A web row's silver id IS the bank's own "Transaction no.": the number the
// account statement prints against the entry, which UBS stamps on BOTH sides
// of a move between two accounts of one relationship — the very fact the
// ubs-web silver schema makes its transactions primary key compound to
// accommodate. So the reference is already in hand here and needs no parsing;
// what it needs is to travel as a FIELD, because gold's id for the row is the
// per-leg composition and nothing downstream should have to take that apart
// to find the number inside it.
//
// The statement era is the exception and is refused. Those ids are the
// collector's own content hash of a printed row (statementIDPrefix), not
// anything the bank wrote, so stamping one would claim an identity no bank
// ever asserted. A hash is unique per row and would pair nothing, which makes
// the refusal cheap — but a reference that names nothing has no business
// being offered as one.
func webBankRef(txID string) string {
	if isStatementEraID(txID) {
		return ""
	}
	return webTxNumber(txID)
}

// withBankRef stamps the bank's own reference for the entry onto a row's
// payload, by the same rule and for the same reason as withCounterAccount:
// one key, whichever feed wrote the row.
func withBankRef(payload json.RawMessage, ref string) json.RawMessage {
	return spliceStringField(string(payload), bankRefKey, ref)
}

// spliceStringField writes one string-valued key into a silver payload by
// splicing it in after the opening brace, rather than decoding the object and
// re-marshalling it. The payload is silver's JSON verbatim, and a round trip
// through a map would reorder and re-space every other key, making each row's
// payload churn on a change that added nothing.
//
// Three refusals, all of them leaving the payload byte for byte as it came:
// an empty value (there is nothing to state), a payload that is not a JSON
// object (a row whose payload never decoded is not one to start editing), and
// a payload that already carries the key — a parsed value is the feed's own
// and a derived one must never overwrite it.
//
// The value is marshalled rather than quoted, so a reference or an account id
// carrying a quote or a backslash produces valid JSON instead of a payload
// that no longer parses.
func spliceStringField(payload, key, value string) json.RawMessage {
	trimmed := strings.TrimSpace(payload)
	if value == "" || !strings.HasPrefix(trimmed, "{") || strings.Contains(trimmed, key) {
		return json.RawMessage(payload)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(payload)
	}
	field := key + string(encoded)
	if trimmed == "{}" {
		return json.RawMessage("{" + field + "}")
	}
	return json.RawMessage("{" + field + "," + trimmed[1:])
}

// returnsFlowInternal is the payload key carrying the conduit verdict to the
// returns engine. It is the vehicle for the "silver-side pre-tagging" the UBS
// ReturnsPolicy's ExternalOnly describes. It is a payload key rather than a
// rewritten `kind` because the spending population reads the kind too.
const returnsFlowInternal = `"returns_flow":"internal"`

// withReturnsFlow stamps the conduit verdict onto a row's payload. Only an
// INTERNAL verdict is written: external is the meaning of the key's absence,
// so nothing changes for the rows — every source but this one — that never
// classify a flow at all.
//
// The payload is the silver JSON object verbatim, so the key is spliced after
// the opening brace rather than round-tripped through a map: re-marshalling
// would reorder and re-space every other key and make each row's payload
// differ from the record silver holds, for one added field.
func withReturnsFlow(payload string, internal bool) (json.RawMessage, bool) {
	if !internal {
		return json.RawMessage(payload), true
	}
	trimmed := strings.TrimSpace(payload)
	if trimmed == "{}" {
		return json.RawMessage("{" + returnsFlowInternal + "}"), true
	}
	if strings.HasPrefix(trimmed, "{") {
		return json.RawMessage("{" + returnsFlowInternal + "," + trimmed[1:]), true
	}
	// Not an object — absent, or malformed enough that decoding failed.
	// The verdict has nowhere to live, and a row that silently loses it
	// would be counted as owner capital. Report the failure so the caller
	// falls back to the cruder carrier: the kind itself.
	return json.RawMessage(payload), false
}

// markReturnsInternal stamps the conduit verdict on a row, degrading to the
// kind when the payload cannot hold it. Losing the verdict would let conduit
// churn into the return, which is the worse error of the two: a row demoted
// to `other` is merely absent from spending.
func markReturnsInternal(payload json.RawMessage, kind canonical.TxKind) (json.RawMessage, canonical.TxKind) {
	if out, ok := withReturnsFlow(string(payload), true); ok {
		return out, kind
	}
	return payload, canonical.TxKindOther
}
