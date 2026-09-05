package canonical

import "testing"

// TestDescriptionMemoRoundTrip pins the memo contract: joining and
// splitting are inverses, an empty memo leaves the narrative byte for
// byte as it was, and a memo with nothing before it is still read as a
// memo rather than as a narrative — including one that carries the
// separator in the payer's own words, which is read whole rather than
// cut at its own dash.
func TestDescriptionMemoRoundTrip(t *testing.T) {
	cases := []struct {
		narrative, memo, want string
	}{
		{"EXAMPLE PAYEE; EXAMPLE STREET 1; 9999 EXAMPLETOWN", "THANKS",
			"EXAMPLE PAYEE; EXAMPLE STREET 1; 9999 EXAMPLETOWN — THANKS"},
		{"credit; Ref 7", "see you soon", "credit; Ref 7 — see you soon"},
		{"credit; Ref 7", "", "credit; Ref 7"},
		{"credit; Ref 7", "   ", "credit; Ref 7"},
		{"", "THANKS", "— THANKS"},
		{"", "", ""},
		// A memo with no narrative before it, carrying the separator
		// in the payer's own words: the split reads the whole memo,
		// never a slice of it as a narrative.
		{"", "rent — March", "— rent — March"},
		{"", "— foo", "— — foo"},
		{"", "a — b — c", "— a — b — c"},
	}
	for _, tc := range cases {
		joined := JoinDescriptionMemo(tc.narrative, tc.memo)
		if joined != tc.want {
			t.Errorf("JoinDescriptionMemo(%q, %q) = %q, want %q", tc.narrative, tc.memo, joined, tc.want)
		}
		narrative, memo := SplitDescriptionMemo(joined)
		if narrative != tc.narrative {
			t.Errorf("SplitDescriptionMemo(%q) narrative = %q, want %q", joined, narrative, tc.narrative)
		}
		if wantMemo := trimmed(tc.memo); memo != wantMemo {
			t.Errorf("SplitDescriptionMemo(%q) memo = %q, want %q", joined, memo, wantMemo)
		}
	}
}

// TestJoinDescriptionMemoFoldsTheNarrative pins the invariant that
// gives the separator its one meaning: a narrative that already
// carries it — text copied verbatim from a source that prints a spaced
// em dash — is folded before anything is joined, with or without a
// memo, so a split can never mistake the source's dash for a memo. The
// fold reaches the edges too: a narrative opening on the bare prefix
// would otherwise be read back as all memo, one ending on the bare
// tail would swallow the separator that follows it, one that is
// nothing but the dash becomes the bare prefix as soon as the
// separator is appended, and two adjacent separators overlap so that
// folding once leaves one behind. The memo itself is not folded:
// everything after a memo-only prefix, or after the first separator
// behind a narrative, is memo — dashes included.
func TestJoinDescriptionMemoFoldsTheNarrative(t *testing.T) {
	cases := []struct {
		narrative, memo, want, wantNarrative, wantMemo string
	}{
		{"EXAMPLE PAYEE — EXAMPLE BRANCH", "", "EXAMPLE PAYEE - EXAMPLE BRANCH",
			"EXAMPLE PAYEE - EXAMPLE BRANCH", ""},
		{"EXAMPLE PAYEE — EXAMPLE BRANCH", "THANKS", "EXAMPLE PAYEE - EXAMPLE BRANCH — THANKS",
			"EXAMPLE PAYEE - EXAMPLE BRANCH", "THANKS"},
		{"A — B — C", "", "A - B - C", "A - B - C", ""},
		{"credit; Ref 7", "one — two", "credit; Ref 7 — one — two", "credit; Ref 7", "one — two"},
		{"— leading", "", "- leading", "- leading", ""},
		{"— leading", "memo", "- leading — memo", "- leading", "memo"},
		{"trailing —", "", "trailing -", "trailing -", ""},
		{"trailing —", "memo", "trailing - — memo", "trailing -", "memo"},
		{"A — — B", "", "A - - B", "A - - B", ""},
		{"A — — B", "memo", "A - - B — memo", "A - - B", "memo"},
		// The lone dash: appending the separator would make the whole
		// text open on the bare prefix, and a split reads that as a
		// memo with nothing before it.
		{"—", "memo", "- — memo", "-", "memo"},
		{"—", "", "-", "-", ""},
	}
	for _, tc := range cases {
		joined := JoinDescriptionMemo(tc.narrative, tc.memo)
		if joined != tc.want {
			t.Errorf("JoinDescriptionMemo(%q, %q) = %q, want %q", tc.narrative, tc.memo, joined, tc.want)
		}
		if narrative, memo := SplitDescriptionMemo(joined); narrative != tc.wantNarrative || memo != tc.wantMemo {
			t.Errorf("SplitDescriptionMemo(%q) = (%q, %q), want (%q, %q)",
				joined, narrative, memo, tc.wantNarrative, tc.wantMemo)
		}
	}
}

// TestSplitDescriptionMemoLeavesPlainText pins that a description
// without the separator is all narrative, a hyphen or an unspaced dash
// included: the separator is the spaced em dash and nothing looser.
func TestSplitDescriptionMemoLeavesPlainText(t *testing.T) {
	for _, s := range []string{
		"EXAMPLE PAYEE; EXAMPLE STREET 1",
		"EXAMPLE - PAYEE",
		"EXAMPLE—PAYEE",
		"",
	} {
		if narrative, memo := SplitDescriptionMemo(s); narrative != s || memo != "" {
			t.Errorf("SplitDescriptionMemo(%q) = (%q, %q), want (%q, \"\")", s, narrative, memo, s)
		}
	}
}

func trimmed(s string) string {
	for len(s) > 0 && s[0] == ' ' {
		s = s[1:]
	}
	for len(s) > 0 && s[len(s)-1] == ' ' {
		s = s[:len(s)-1]
	}
	return s
}
