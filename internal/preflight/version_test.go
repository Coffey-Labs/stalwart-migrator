// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package preflight

import "testing"

func TestParseSemver(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"stalwart 0.15.5\n", "0.15.5", false},
		{"v0.16.14", "0.16.14", false},
		{"Stalwart Mail Server v0.16.0 (build abc123)", "0.16.0", false},
		{"no version here", "", true},
		{"", "", true},
	}
	for _, tc := range cases {
		got, err := parseSemver(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseSemver(%q) = %v, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSemver(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got.String() != tc.want {
			t.Errorf("parseSemver(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestSemverCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.15.5", "0.16.0", -1},
		{"0.16.14", "0.16.14", 0},
		{"0.16.1", "0.15.5", 1},
		{"1.0.0", "0.16.14", 1},
	}
	for _, tc := range cases {
		a, err := parseSemver(tc.a)
		if err != nil {
			t.Fatalf("parseSemver(%q): %v", tc.a, err)
		}
		b, err := parseSemver(tc.b)
		if err != nil {
			t.Fatalf("parseSemver(%q): %v", tc.b, err)
		}
		if got := a.Compare(b); got != tc.want {
			t.Errorf("%s.Compare(%s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
