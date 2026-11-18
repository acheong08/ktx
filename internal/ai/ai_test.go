package ai

import (
	"strings"
	"testing"
)

func TestParseDescription(t *testing.T) {
	defs := []LabelDef{{Name: "slop"}, {Name: "competitors"}}
	for reply, want := range map[string]string{
		"Title: Rival pricing\nLabels: none":                     "Rival pricing|",
		"**Title:** \"Rival pricing\"\n**Labels:** Slop, other.": "Rival pricing|slop",
		"Rival pricing": "Rival pricing|",
		"Title: A\nLabels: competitors, slop, slop": "A|competitors,slop",
	} {
		title, labels := parseDescription(reply, defs)
		if got := title + "|" + strings.Join(labels, ","); got != want {
			t.Errorf("%q: got %q, want %q", reply, got, want)
		}
	}
}
