package transcript

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const valid = `+++
# comment kept
title: Intro call with Acme
date: 2026-11-14T10:00:00-08:00
timezone: America/Los_Angeles
type: Customer_Discovery
participants:
  - name: Jane Doe
    email: jane@acme.com
    company: Acme
  - "Maxim <maxim@melzi.dev>"
  - Antonio
source: granola
source_id: not_123
custom_field: kept
+++

**Jane:** Hello.
`

func TestParseValid(t *testing.T) {
	d, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 14, 18, 0, 0, 0, time.UTC)
	if !d.Start.Equal(want) {
		t.Errorf("Start = %v, want %v", d.Start, want)
	}
	if d.Meta.Type != "customer_discovery" {
		t.Errorf("Type = %q", d.Meta.Type)
	}
	ps := d.Meta.Participants
	if len(ps) != 3 || ps[1].Name != "Maxim" || ps[1].Email != "maxim@melzi.dev" || ps[2].Name != "Antonio" || ps[0].Company != "Acme" {
		t.Errorf("participants = %+v", ps)
	}
	if d.Body != "**Jane:** Hello.\n" {
		t.Errorf("Body = %q", d.Body)
	}
	if got := d.FileName(); got != "2026-11-14-1000-intro-call-with-acme-"+d.ID+".md" {
		t.Errorf("FileName = %q", got)
	}
}

func TestIDStableForSourceID(t *testing.T) {
	a, _ := Parse([]byte(valid))
	b, _ := Parse([]byte(strings.Replace(valid, "Hello.", "Hello, edited.", 1)))
	if a.ID != b.ID {
		t.Error("same source/source_id should give the same id")
	}
	noSrc := strings.Replace(strings.Replace(valid, "source: granola\n", "", 1), "source_id: not_123\n", "", 1)
	c, _ := Parse([]byte(noSrc))
	d, _ := Parse([]byte(strings.Replace(noSrc, "Hello.", "Bye.", 1)))
	if c.ID == d.ID || c.ID == a.ID {
		t.Error("without source_id the id should follow the content")
	}
}

func TestParseVariants(t *testing.T) {
	cases := map[string]struct {
		in    string
		start time.Time
	}{
		"dash delimiters, CRLF": {
			"---\r\ndate: 2026-11-14T10:00:00Z\r\nparticipants: [Jane]\r\n---\r\nhi\r\n",
			time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC),
		},
		"naive date + timezone": {
			"+++\ndate: 2026-07-01 09:30\ntimezone: Europe/Berlin\nparticipants: [Jane]\n+++\nhi\n",
			time.Date(2026, 7, 1, 7, 30, 0, 0, time.UTC),
		},
		"date only + timezone": {
			"+++\ndate: 2026-07-01\ntimezone: UTC\nparticipants: [Jane]\n+++\nhi",
			time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		},
		"quoted date": {
			"+++\ndate: \"2026-07-01T09:30:00+02:00\"\nparticipants: [Jane]\n+++\nhi",
			time.Date(2026, 7, 1, 7, 30, 0, 0, time.UTC),
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d, err := Parse([]byte(c.in))
			if err != nil {
				t.Fatal(err)
			}
			if !d.Start.Equal(c.start) {
				t.Errorf("Start = %v, want %v", d.Start, c.start)
			}
		})
	}
}

func TestParseInvalid(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"no metadata":       {"hello", `must start with a "---"`},
		"unclosed":          {"+++\ndate: 2026-01-01T00:00:00Z\n", "not closed"},
		"not a mapping":     {"+++\n- a\n+++\nhi", "must be a YAML mapping"},
		"bad yaml":          {"+++\ndate: [\n+++\nhi", "not valid YAML"},
		"missing date":      {"+++\nparticipants: [Jane]\n+++\nhi", "date is required"},
		"naive date no tz":  {"+++\ndate: 2026-01-01 10:00\nparticipants: [Jane]\n+++\nhi", "no UTC offset"},
		"bad timezone":      {"+++\ndate: 2026-01-01T10:00:00Z\ntimezone: PST8\nparticipants: [Jane]\n+++\nhi", "IANA"},
		"no participants":   {"+++\ndate: 2026-01-01T10:00:00Z\n+++\nhi", "at least one"},
		"bad email":         {"+++\ndate: 2026-01-01T10:00:00Z\nparticipants: [{email: nope}]\n+++\nhi", "not a valid address"},
		"empty participant": {"+++\ndate: 2026-01-01T10:00:00Z\nparticipants: [{company: X}]\n+++\nhi", "name or an email"},
		"empty body":        {"+++\ndate: 2026-01-01T10:00:00Z\nparticipants: [Jane]\n+++\n\n  \n", "body"},
		"source_id alone":   {"+++\ndate: 2026-01-01T10:00:00Z\nparticipants: [Jane]\nsource_id: x\n+++\nhi", "requires source"},
		"bad type":          {"+++\ndate: 2026-01-01T10:00:00Z\nparticipants: [Jane]\ntype: a b\n+++\nhi", "type"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(c.in))
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("want ValidationError, got %v", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestReportsAllProblems(t *testing.T) {
	_, err := Parse([]byte("+++\ntitle: x\n+++\n"))
	var verr *ValidationError
	if !errors.As(err, &verr) || len(verr.Problems) != 3 {
		t.Fatalf("want 3 problems (date, participants, body), got %v", err)
	}
}

func TestRender(t *testing.T) {
	d, err := Parse([]byte(strings.Replace(valid, "title:", "id: client-supplied\ntitle:", 1)))
	if err != nil {
		t.Fatal(err)
	}
	d.Set("id", d.ID)
	d.Set("uploaded_by", "maxim")
	d.Unset("custom_field")
	d.Unset("not_there")
	out, err := d.Render()
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"---\n# comment kept\n", "source_id: not_123\nuploaded_by: maxim\n", "id: " + d.ID + "\n", "uploaded_by: maxim\n", "---\n\n**Jane:** Hello.\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered file lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "client-supplied") || strings.Contains(s, "custom_field") {
		t.Error("server-managed id should replace the client value")
	}
	again, err := Parse(out)
	if err != nil {
		t.Fatalf("rendered file does not parse: %v", err)
	}
	if !again.Start.Equal(d.Start) || again.Meta.Title != d.Meta.Title {
		t.Error("round trip changed metadata")
	}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"Intro call w/ Acme!": "intro-call-w-acme",
		"  --Jane, Bob--  ":   "jane-bob",
		"José Müller, café":   "jose-muller-cafe",
		"李雷":                  "",
	} {
		if got := Slugify(in, 60); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Slugify(strings.Repeat("ab ", 40), 10); got != "ab-ab-ab-a" {
		t.Errorf("truncation: %q", got)
	}
}

func TestReadField(t *testing.T) {
	raw := []byte("---\ningested_at: \"2026-11-15T12:00:00Z\"\nunquoted: 2026-11-15T12:00:00Z\nn: 3\n---\nbody\n")
	for key, want := range map[string]string{"ingested_at": "2026-11-15T12:00:00Z", "unquoted": "2026-11-15T12:00:00Z", "n": "3", "missing": ""} {
		if got := ReadField(raw, key); got != want {
			t.Errorf("ReadField(%q) = %q, want %q", key, got, want)
		}
	}
	if got := ReadField([]byte("no front matter"), "n"); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestParseStored(t *testing.T) {
	d, problems, err := ParseStored([]byte("---\nid: abc123\ntitle: Hand edited\ntype: Unsorted\n---\nbody\n"))
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != "abc123" || d.Meta.Type != "" || len(problems) != 2 {
		t.Errorf("id=%q type=%q problems=%v", d.ID, d.Meta.Type, problems)
	}
	if _, _, err := ParseStored([]byte("no front matter")); err == nil {
		t.Error("want error without metadata")
	}
}

func TestNormalizeType(t *testing.T) {
	for in, want := range map[string]string{"": "", "Unsorted": "", " Advisor ": "advisor", "customer_discovery": "customer_discovery"} {
		if got, err := NormalizeType(in); err != nil || got != want {
			t.Errorf("NormalizeType(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := NormalizeType("../etc"); err == nil {
		t.Error("want error for path-like type")
	}
}
