package canonical

import "strings"

// DescriptionMemoSeparator separates a transaction description's
// narrative from its memo: the payer's own free text about the row,
// where a source carries one. An adapter emits the two apart —
// TransactionChange.Description and TransactionChange.Memo — and the
// gold writer composes the stored description with
// JoinDescriptionMemo, which puts the memo AFTER everything the bank
// wrote and folds any separator the narrative itself carried. The
// separator therefore has one meaning in gold: what follows it is the
// payer's words rather than the payee's identity, and the spending
// tiers read it accordingly. A memo is shown wherever the description
// is shown and a config rule can key on it — what a payment was for —
// but it never enters the merchant signature (spending.Normalize cuts
// the description at the separator), never fires a built-in rule, and
// is never sent as a model descriptor. A narrative is keyed the same
// whether or not a memo follows it.
const DescriptionMemoSeparator = " — "

// foldedMemoSeparator replaces a separator found inside a narrative:
// the same dash shape, so the text reads as it did, but not the
// separator.
const foldedMemoSeparator = " - "

// memoOnlyPrefix opens a description that is a memo with no narrative
// before it: the separator without its leading space, so the text
// does not begin with a blank.
var memoOnlyPrefix = strings.TrimLeft(DescriptionMemoSeparator, " ")

// The separator's edge shapes, each beside the folded form it
// becomes: the bare prefix a memo-only description opens with, the
// bare tail a text can end on, and the lone dash, which is both at
// once — appending the separator's leading space to it makes the bare
// prefix. A split reads any of them as a separator, so a narrative
// carrying one at its edge has to lose it like any other.
var (
	foldedMemoOnlyPrefix    = strings.TrimLeft(foldedMemoSeparator, " ")
	memoSeparatorTail       = strings.TrimRight(DescriptionMemoSeparator, " ")
	foldedMemoSeparatorTail = strings.TrimRight(foldedMemoSeparator, " ")
	bareMemoSeparator       = strings.TrimSpace(DescriptionMemoSeparator)
	foldedBareMemoSeparator = strings.TrimSpace(foldedMemoSeparator)
)

// JoinDescriptionMemo composes a stored description: the narrative,
// then memo after DescriptionMemoSeparator. The narrative is folded
// first, and the fold covers every shape a split would read as a
// separator: the separator itself wherever it occurs, the overlap two
// adjacent separators leave, a leading bare prefix, a trailing bare
// tail, and a narrative that is nothing but the dash, which the
// separator's leading space would turn into the bare prefix. So a
// narrative copied verbatim from a source that happens to print a
// spaced em dash can never be read back as carrying a memo, wherever
// in the text the dash sits. The fold runs with or without a memo, so
// a narrative is keyed the same either way; one carrying none of those
// shapes is returned byte for byte when the memo is empty. Every
// folded shape is what makes the bare prefix mean memo-only and
// nothing else: an empty narrative yields the memo behind it, and
// SplitDescriptionMemo reads that as a memo and no narrative rather
// than a memo posing as one.
func JoinDescriptionMemo(narrative, memo string) string {
	// Looping is what folds two adjacent separators: the second
	// overlaps the first and survives a single ReplaceAll. Each pass
	// removes at least one separator, so it terminates.
	for strings.Contains(narrative, DescriptionMemoSeparator) {
		narrative = strings.ReplaceAll(narrative, DescriptionMemoSeparator, foldedMemoSeparator)
	}
	if strings.HasPrefix(narrative, memoOnlyPrefix) {
		narrative = foldedMemoOnlyPrefix + narrative[len(memoOnlyPrefix):]
	} else if narrative == bareMemoSeparator {
		narrative = foldedBareMemoSeparator
	}
	if strings.HasSuffix(narrative, memoSeparatorTail) {
		narrative = narrative[:len(narrative)-len(memoSeparatorTail)] + foldedMemoSeparatorTail
	}
	memo = strings.TrimSpace(memo)
	if memo == "" {
		return narrative
	}
	if narrative == "" {
		return memoOnlyPrefix + memo
	}
	return narrative + DescriptionMemoSeparator + memo
}

// SplitDescriptionMemo splits a stored description at its memo
// separator into the narrative before it and the memo after it. A
// description without one is all narrative; one that opens with the
// bare separator is all memo.
//
// The memo-only prefix is tested FIRST, before the separator is looked
// for anywhere in the text: a memo may itself carry a spaced em dash —
// nothing folds the memo — and searching for the separator first would
// cut inside it, handing part of the payer's words back as the
// narrative. JoinDescriptionMemo folds every other shape that could
// open on the bare prefix, so opening on it means memo-only and
// nothing else.
func SplitDescriptionMemo(description string) (narrative, memo string) {
	if strings.HasPrefix(description, memoOnlyPrefix) {
		return "", description[len(memoOnlyPrefix):]
	}
	if i := strings.Index(description, DescriptionMemoSeparator); i >= 0 {
		return description[:i], description[i+len(DescriptionMemoSeparator):]
	}
	return description, ""
}
