package cli

import (
	"testing"
	"time"
)

func TestParseStaleDuration(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: "", want: 0},
		{in: "7d", want: 7 * 24 * time.Hour},
		{in: "1w", want: 7 * 24 * time.Hour},
		{in: "24h", want: 24 * time.Hour},
		{in: "90m", want: 90 * time.Minute},
		{in: "0d", wantErr: true},
		{in: "bogus", wantErr: true},
		{in: "0s", wantErr: true},
	}
	for _, tt := range tests {
		got, err := parseStaleDuration(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseStaleDuration(%q) = %v, want error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseStaleDuration(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseStaleDuration(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
