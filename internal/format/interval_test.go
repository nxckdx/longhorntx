package format

import "testing"

func TestInterval(t *testing.T) {
	for in, want := range map[string]string{"5m0s": "5m", "2m": "2m", "1h0m0s": "1h", "1h30m0s": "1h30m", "30s": "30s", "0": "0", "x": "x"} {
		if got := Interval(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}
