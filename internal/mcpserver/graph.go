package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/acheong08/ktx/internal/calendar"
	"github.com/acheong08/ktx/internal/notes"
)

type getGraphIn struct {
	Intent   string `json:"intent" jsonschema:"One sentence: what the user asked for and why this call helps. Logged for the people whose data this is."`
	Finished bool   `json:"include_finished,omitempty" jsonschema:"also show achieved, dropped, rejected and reversed goals and decisions"`
}

type getGraphOut struct {
	Outline string `json:"outline" jsonschema:"one line per goal or decision, indented under what it's based on; ids are note:<id>, readable with get_note"`
	Count   int    `json:"count" jsonschema:"goals and decisions shown"`
}

// getGraph renders the goals and decisions the caller can see as an
// indented outline, with their context and tasks, for an agent's context.
func (s *Server) getGraph(_ context.Context, in getGraphIn, c caller) (getGraphOut, error) {
	all, err := s.notes.List()
	if err != nil {
		return getGraphOut{}, err
	}
	who := c.user.Display()
	byID := map[string]notes.Note{}
	for _, n := range all {
		byID[n.ID] = n
	}
	titles := map[string]string{}
	entries, err := s.store.List()
	if err != nil {
		return getGraphOut{}, err
	}
	for _, e := range entries {
		titles[e.ID] = e.Label()
	}
	tasks := map[string][]calendar.ToDo{}
	if s.cal != nil {
		cals, err := s.cal.Calendars()
		if err != nil {
			return getGraphOut{}, err
		}
		ts, _, err := s.cal.ToDos(cals)
		if err != nil {
			return getGraphOut{}, err
		}
		for _, t := range ts {
			if t.For != "" {
				tasks[t.For] = append(tasks[t.For], t)
			}
		}
	}

	var b strings.Builder
	count := 0
	var walk func(ns []*notes.GraphNode, depth int, parent string)
	walk = func(ns []*notes.GraphNode, depth int, parent string) {
		pad := strings.Repeat("  ", depth)
		for _, g := range ns {
			n := g.Note
			tags := n.Kind + " · " + n.Status
			if n.Strength != "" {
				tags += " · " + n.Strength
			}
			if n.Private {
				tags += " · private"
			}
			fmt.Fprintf(&b, "%s- [%s] %s (note:%s)", pad, tags, n.Title, n.ID)
			var others []string
			for _, p := range g.Parents {
				if p.ID != parent {
					others = append(others, fmt.Sprintf("%q", p.Title))
				}
			}
			if len(others) > 0 {
				fmt.Fprintf(&b, "; also under %s", strings.Join(others, ", "))
			}
			if g.Again {
				b.WriteString("; shown in full above\n")
				continue
			}
			b.WriteString("\n")
			count++
			var context []string
			for _, r := range n.BasedOn {
				kind, id := r.Target()
				var desc string
				switch kind {
				case "note":
					t, ok := byID[id]
					if !ok || !t.Visible(who) {
						desc = "a note that's gone or private (" + r.Ref + ")"
					} else if t.IsNode() {
						continue // shown by the outline itself
					} else {
						desc = fmt.Sprintf("note %q (%s)", t.Title, r.Ref)
						if len(t.Labels) > 0 {
							desc += " [" + strings.Join(t.Labels, ", ") + "]"
						}
					}
				case "transcript":
					desc = fmt.Sprintf("call %q (%s)", titles[id], r.Ref)
				}
				if r.Quote != "" {
					desc += fmt.Sprintf(": %q", r.Quote)
				}
				context = append(context, desc)
			}
			if len(context) > 0 {
				fmt.Fprintf(&b, "%s  based on: %s\n", pad, strings.Join(context, "; "))
			}
			if ts := tasks["note:"+n.ID]; len(ts) > 0 {
				var parts []string
				for _, t := range ts {
					box := "[ ]"
					if t.Done {
						box = "[x]"
					}
					part := box + " " + t.Summary
					if !t.Done && !t.Due.IsZero() {
						part += " (due " + t.Due.In(calendar.DefaultZone()).Format("2006-01-02") + ")"
					}
					parts = append(parts, part)
				}
				fmt.Fprintf(&b, "%s  tasks: %s\n", pad, strings.Join(parts, "; "))
			}
			walk(g.Children, depth+1, n.ID)
		}
	}
	walk(notes.Outline(all, who, in.Finished), 0, "")
	if count == 0 {
		b.WriteString("No goals or decisions yet.\n")
	}
	return getGraphOut{Outline: b.String(), Count: count}, nil
}
