package schwab

import (
	"encoding/json"
	"strings"

	"github.com/smartpointer-com/wealthdb/wealthdb/internal/canonical"
	"github.com/smartpointer-com/wealthdb/wealthdb/internal/lots"
)

// lotPolicy is the lot policy (docs/LOTS.md §6): the statement
// lots are the anchors and the documents' realized lots the stated
// sales; the engine replays the history around them. What a row does
// to the lots, where its kind does not say, is its action
// (setLotActions).
func lotPolicy() *lots.Policy {
	p := lots.TradingPolicy()
	return &p
}

// setLotActions states the action of each row whose kind does not say
// what it does to the lots: an other or a corporate action. It reads
// the row's own words, Schwab's type (the payload's "type") and action
// (its "Action", else its "action"), and its narrative.
func setLotActions(txs []canonical.TransactionChange) {
	for i := range txs {
		tx := &txs[i]
		if tx.Kind != canonical.TxKindOther && tx.Kind != canonical.TxKindCorporateAction {
			continue
		}
		typ, action := payloadVerbs(tx.Payload)
		var d string
		if tx.Description != nil {
			d = *tx.Description
		}
		tx.Action = lotAction(tx.Kind, typ, action, d)
	}
}

// lotAction is what a row does to the lots where its kind does not say
// it. A TRADE moving no cash (kindFor's other) restates a holding the
// account keeps: no event. A corporate action reads by its action, else
// its description. Cash-only actions (rights redemptions, litigation
// proceeds, option exchanges) move no lot. Cash in lieu of a fraction
// is cash in lieu in both feeds: the web feed's "Cash In Lieu" action,
// and an api JOURNAL named so by its description (journalShapes).
func lotAction(kind canonical.TxKind, typ, action, description string) canonical.TxAction {
	switch kind {
	case canonical.TxKindOther:
		if typ == "TRADE" {
			return canonical.TxActionIgnore
		}
		return ""
	case canonical.TxKindCorporateAction:
	default:
		return ""
	}
	a, d := strings.ToLower(action), strings.ToLower(description)
	switch {
	case a == "return of capital":
		return canonical.TxActionReturnOfCapital
	case a == "cash in lieu", typ == "JOURNAL" && cashInLieu.MatchString(d):
		return canonical.TxActionCashInLieu
	case a == "unissued rights redemption", a == "litigation", strings.HasPrefix(d, "exchange "):
		return canonical.TxActionCashOnly
	case strings.HasPrefix(d, "removed due to expiration"):
		return canonical.TxActionExpiry
	}
	return canonical.TxActionReorganisation
}

// payloadVerbs reads a row's type and action out of its payload, each
// as the string the payload states, or "" where it states none.
func payloadVerbs(payload json.RawMessage) (typ, action string) {
	var m map[string]json.RawMessage
	if json.Unmarshal(payload, &m) != nil {
		return "", ""
	}
	typ, _ = payloadString(m, "type")
	if a, ok := payloadString(m, "Action"); ok {
		return typ, a
	}
	action, _ = payloadString(m, "action")
	return typ, action
}

// payloadString is m's value at key as text, false where the key is
// absent or null.
func payloadString(m map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := m[key]
	if !ok || string(raw) == "null" {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	return string(raw), true
}
