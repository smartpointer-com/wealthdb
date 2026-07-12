package canonical

import (
	"encoding/json"
	"testing"
)

func TestNewDecimalFromString(t *testing.T) {
	cases := []struct {
		in    string
		ok    bool
		canon string // canonical form expected from String()
	}{
		{"0", true, "0"},
		{"1.5", true, "1.5"},
		{"-12345.6789", true, "-12345.6789"},
		{"1e10", true, "10000000000"},
		{"", false, ""},
		{"abc", false, ""},
	}
	for _, c := range cases {
		d, err := NewDecimalFromString(c.in)
		if (err == nil) != c.ok {
			t.Errorf("NewDecimalFromString(%q) err=%v, want ok=%v", c.in, err, c.ok)
			continue
		}
		if c.ok && d.String() != c.canon {
			t.Errorf("NewDecimalFromString(%q).String() = %q, want %q", c.in, d.String(), c.canon)
		}
	}
}

func TestDecimalJSONRoundTrip(t *testing.T) {
	d, err := NewDecimalFromString("123456789.987654321")
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var got Decimal
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Equal(d) {
		t.Errorf("round-trip mismatch: in=%s out=%s", d, got)
	}
}

func TestNewDecimalFromInt(t *testing.T) {
	d := NewDecimalFromInt(-42)
	if d.String() != "-42" {
		t.Errorf("NewDecimalFromInt(-42).String() = %q, want %q", d.String(), "-42")
	}
}
