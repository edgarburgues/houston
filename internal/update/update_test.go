package update

import (
	"testing"
	"time"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v0.5.0", "v0.4.0", true},
		{"0.4.1", "0.4.0", true},
		{"v1.10.0", "v1.9.0", true}, // numeric, not lexicographic
		{"v0.4.0", "v0.4.0", false},
		{"v0.4.0", "v0.5.0", false},
		{"v0.4.0-rc1", "v0.4.0", false}, // suffix dropped → equal → not newer
		{"v2.0.0", "v1.9.9", true},
	}
	for _, c := range cases {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%q,%q)=%v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestNoticeOnDevBuildIsSilent(t *testing.T) {
	// dev / empty versions must never nag, even though they'd "differ" from a tag.
	if n := Notice("dev", time.Second); n != "" {
		t.Errorf("dev build should never nag: %q", n)
	}
	if n := Notice("", time.Second); n != "" {
		t.Errorf("empty version should never nag: %q", n)
	}
}
