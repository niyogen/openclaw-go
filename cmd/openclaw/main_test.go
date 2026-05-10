package main

import "testing"

func TestParseBoolArg(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
		ok   bool
	}{
		{"true", true, true},
		{"TRUE", true, true},
		{"yes", true, true},
		{"1", true, true},
		{"on", true, true},
		{"false", false, true},
		{"no", false, true},
		{"0", false, true},
		{"off", false, true},
		{"maybe", false, false},
		{"", false, false},
	} {
		got, err := parseBoolArg(tc.in)
		if tc.ok {
			if err != nil {
				t.Fatalf("%q: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("%q: got %v want %v", tc.in, got, tc.want)
			}
		} else {
			if err == nil {
				t.Fatalf("%q: expected error", tc.in)
			}
		}
	}
}
