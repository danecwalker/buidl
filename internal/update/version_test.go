package update

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		in     string
		want   Version
		wantOK bool
	}{
		{in: "v0.1.6", want: Version{Minor: 1, Patch: 6}, wantOK: true},
		{in: "0.1.6", want: Version{Minor: 1, Patch: 6}, wantOK: true},
		{in: "v1.2.3", want: Version{Major: 1, Minor: 2, Patch: 3}, wantOK: true},
		{in: "v0.1.6-3-gabcdef", want: Version{Minor: 1, Patch: 6, Commits: 3}, wantOK: true},
		{in: "v0.1.6-3-gabcdef-dirty", want: Version{Minor: 1, Patch: 6, Commits: 3}, wantOK: true},
		{in: "v0.1.6-dirty", want: Version{Minor: 1, Patch: 6}, wantOK: true},
		{in: "v0.1", want: Version{Minor: 1}, wantOK: true},
		{in: "  v0.1.6  ", want: Version{Minor: 1, Patch: 6}, wantOK: true},
		{in: "dev", wantOK: false},
		{in: "", wantOK: false},
		{in: "v0.1.6-rc.1", wantOK: false},
		{in: "not-a-version", wantOK: false},
	}
	for _, tt := range tests {
		got, ok := Parse(tt.in)
		if ok != tt.wantOK {
			t.Errorf("Parse(%q) ok=%v, want %v", tt.in, ok, tt.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if got != tt.want {
			t.Errorf("Parse(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestNewer(t *testing.T) {
	tests := []struct {
		current, latest string
		want            bool
	}{
		{"v0.1.6", "v0.1.7", true},
		{"v0.1.6", "v0.2.0", true},
		{"v0.1.6", "v1.0.0", true},
		{"v0.1.6", "v0.1.6", false},
		{"v0.1.7", "v0.1.6", false},
		{"v0.1.6-3-gabc", "v0.1.6", false},
		{"v0.1.6-3-gabc", "v0.1.7", true},
		{"dev", "v0.1.7", false},
		{"v0.1.6", "dev", false},
		{"", "v0.1.7", false},
	}
	for _, tt := range tests {
		if got := Newer(tt.current, tt.latest); got != tt.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", tt.current, tt.latest, got, tt.want)
		}
	}
}

func TestParseable(t *testing.T) {
	if !Parseable("v0.1.6") {
		t.Error("v0.1.6 should be parseable")
	}
	if Parseable("dev") {
		t.Error("dev should not be parseable")
	}
}
