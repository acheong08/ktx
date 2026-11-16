package config

import (
	"strings"
	"testing"
)

func TestParseTokens(t *testing.T) {
	long := strings.Repeat("a", 32)
	got, err := ParseTokens(" antonio:" + long + " , maxim:" + strings.Repeat("b", 32) + ",")
	if err != nil {
		t.Fatal(err)
	}
	if got[long] != "antonio" || len(got) != 2 {
		t.Errorf("tokens = %v", got)
	}
	if got, err := ParseTokens(""); err != nil || len(got) != 0 {
		t.Errorf("empty: %v %v", got, err)
	}
	for _, bad := range []string{"antonio", "antonio:short", ":" + long, "a:" + long + ",b:" + long} {
		if _, err := ParseTokens(bad); err == nil {
			t.Errorf("ParseTokens(%q) should fail", bad)
		}
	}
}
