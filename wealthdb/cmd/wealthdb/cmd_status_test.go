package main

import "testing"

func TestFormatWatermark(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{-1, "-1"},
		{0, "0"},
		{1750000000, "1750000000 (2025-06-15 15:06:40Z)"},
	}
	for _, tc := range cases {
		if got := formatWatermark(tc.in); got != tc.want {
			t.Errorf("formatWatermark(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
