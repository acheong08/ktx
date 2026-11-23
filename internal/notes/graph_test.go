package notes

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestGoalsAndDecisions(t *testing.T) {
	e := setup(t)
	e.svc.SetTranscriptCheck(func(id string) bool { return id == "f144dccdc869" })
	e.start(t)
	ctx := context.Background()
	add := func(title, author string, f Fields) Note {
		t.Helper()
		id, err := e.svc.SubmitNote(Submission{Body: title + " because reasons.", Author: author, Title: title, Fields: f})
		if err != nil {
			t.Fatalf("%s: %v", title, err)
		}
		return e.wait(t, id)
	}
	// Bad input is refused.
	for _, f := range []Fields{
		{Kind: "wish"},
		{Kind: KindGoal, Strength: "very"},
		{BasedOn: []Ref{{Ref: "note:nothex"}}},
		{BasedOn: []Ref{{Ref: "note:000000000000"}}},
		{BasedOn: []Ref{{Ref: "transcript:aaaaaaaaaaaa"}}},
	} {
		if _, err := e.svc.SubmitNote(Submission{Body: "x", Author: "antonio", Fields: f}); err == nil {
			t.Errorf("accepted %+v", f)
		}
	}

	retire := add("Retire by 30", "antonio", Fields{Kind: KindGoal, Strength: "hard", Private: true})
	if retire.Status != "active" || !retire.Private {
		t.Fatalf("defaults: %+v", retire.Fields)
	}
	file := gitShow(t, e.remote, "main:"+retire.Path)
	if !strings.Contains(file, "kind: goal\n") || !strings.Contains(file, "private: true\n") || !strings.Contains(file, "status: active\n") {
		t.Fatalf("file:\n%s", file)
	}
	backgrounds := add("Our backgrounds", "antonio", Fields{}) // context: a plain note
	cyber := add("Work in cybersecurity", "antonio", Fields{Kind: KindDecision, Status: "made", Strength: "firm",
		BasedOn: []Ref{{Ref: "note:" + backgrounds.ID}, {Ref: "note:" + retire.ID}}})
	idea := add("Abliterated models for pentesters", "maxim", Fields{Kind: KindDecision, BasedOn: []Ref{
		{Ref: "note:" + cyber.ID}, {Ref: "transcript:f144dccdc869", Quote: "  Nicholas reached\n$3M ARR  "}}})
	if idea.Status != "considering" || idea.BasedOn[1].Quote != "Nicholas reached $3M ARR" {
		t.Fatalf("idea: %+v", idea.Fields)
	}

	// A loop is refused: retire can't be based on something based on it.
	_, err := e.svc.Edit(ctx, retire.ID, Changes{Title: retire.Title, Body: retire.Body, Fields: Fields{Kind: KindGoal, BasedOn: []Ref{{Ref: "note:" + idea.ID}}}}, "antonio")
	if !errors.Is(err, ErrLoop) {
		t.Fatalf("loop: %v", err)
	}
	// Editing fields alone is a change.
	idea, err = e.svc.Edit(ctx, idea.ID, Changes{Title: idea.Title, Body: idea.Body, Fields: Fields{Kind: KindDecision, Status: "made", BasedOn: idea.BasedOn}}, "maxim")
	if err != nil || idea.Status != "made" {
		t.Fatalf("edit: %+v %v", idea.Fields, err)
	}

	all, _ := e.svc.List()
	// What's based on what, as each person sees it.
	if got := BasedOnThis(all, "transcript:f144dccdc869", "maxim"); len(got) != 1 || got[0].ID != idea.ID {
		t.Fatalf("based on transcript: %v", got)
	}
	if got := BasedOnThis(all, "note:"+backgrounds.ID, "antonio"); len(got) != 1 {
		t.Fatalf("based on backgrounds: %v", got)
	}

	// Antonio sees his private goal at the top; Maxim doesn't.
	outline := func(viewer string, finished bool) string {
		var b strings.Builder
		var walk func(ns []*GraphNode, depth int)
		walk = func(ns []*GraphNode, depth int) {
			for _, n := range ns {
				b.WriteString(strings.Repeat("  ", depth) + n.Note.Title)
				if n.Again {
					b.WriteString(" (again)")
				}
				b.WriteString("\n")
				walk(n.Children, depth+1)
			}
		}
		walk(Outline(all, viewer, finished), 0)
		return b.String()
	}
	if got := outline("antonio", false); got != "Retire by 30\n  Work in cybersecurity\n    Abliterated models for pentesters\n" {
		t.Fatalf("antonio's outline:\n%s", got)
	}
	if got := outline("maxim", false); got != "Work in cybersecurity\n  Abliterated models for pentesters\n" {
		t.Fatalf("maxim's outline:\n%s", got)
	}
	// A second parent: shown in full once, "again" under the other.
	strat := add("Prefer a decent chance of success", "antonio", Fields{Kind: KindGoal, Strength: "tentative"})
	idea, _ = e.svc.Edit(ctx, idea.ID, Changes{Title: idea.Title, Body: idea.Body, Fields: Fields{Kind: KindDecision, Status: "made",
		BasedOn: append(idea.BasedOn, Ref{Ref: "note:" + strat.ID})}}, "maxim")
	all, _ = e.svc.List()
	if got := outline("maxim", false); got != "Work in cybersecurity\n  Abliterated models for pentesters\nPrefer a decent chance of success\n  Abliterated models for pentesters (again)\n" {
		t.Fatalf("two parents:\n%s", got)
	}
	// Finished ones are hidden unless asked for; their children stay under
	// the parents still shown.
	cyber, _ = e.svc.Edit(ctx, cyber.ID, Changes{Title: cyber.Title, Body: cyber.Body, Fields: Fields{Kind: KindDecision, Status: "reversed", BasedOn: cyber.BasedOn}}, "antonio")
	all, _ = e.svc.List()
	if got := outline("maxim", false); got != "Prefer a decent chance of success\n  Abliterated models for pentesters\n" {
		t.Fatalf("hidden finished:\n%s", got)
	}
	if got := outline("maxim", true); !strings.HasPrefix(got, "Work in cybersecurity\n") {
		t.Fatalf("with finished:\n%s", got)
	}
	// Plain notes have no status or strength.
	n, _ := e.svc.Edit(ctx, backgrounds.ID, Changes{Title: backgrounds.Title, Body: backgrounds.Body, Fields: Fields{Status: "made", Strength: "hard"}}, "antonio")
	if n.Status != "" || n.Strength != "" {
		t.Fatalf("plain note fields: %+v", n.Fields)
	}
}
