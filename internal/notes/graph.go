package notes

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// The decision graph (docs/decision-graph.md): a note can be a goal or a
// decision, with a status, a strength and "based on" links to notes and
// transcripts. Plain notes and transcripts are the context. A private note
// is shown only to its author.

// Kinds of note.
const (
	KindGoal     = "goal"
	KindDecision = "decision"
)

// Statuses by kind; the first is the default.
var statuses = map[string][]string{
	KindGoal:     {"active", "achieved", "paused", "dropped"},
	KindDecision: {"considering", "made", "rejected", "reversed"},
}

// Strengths: how firmly a goal or decision is held.
var Strengths = []string{"hard", "firm", "tentative"}

// Statuses lists the statuses of a kind.
func Statuses(kind string) []string { return statuses[kind] }

// Finished reports whether a status means the goal or decision no longer
// applies (hidden in the graph unless asked for).
func Finished(status string) bool {
	switch status {
	case "achieved", "dropped", "rejected", "reversed":
		return true
	}
	return false
}

// Ref is one "based on" link: "note:<id>" or "transcript:<id>", with an
// optional verbatim quote from it.
type Ref struct {
	Ref   string `yaml:"ref" json:"ref"`
	Quote string `yaml:"quote,omitempty" json:"quote,omitempty"`
}

// UnmarshalYAML also accepts a bare "note:<id>".
func (r *Ref) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		r.Ref = n.Value
		return nil
	}
	type plain Ref
	return n.Decode((*plain)(r))
}

// Target splits a ref into its kind ("note" or "transcript") and id.
func (r Ref) Target() (kind, id string) {
	kind, id, _ = strings.Cut(r.Ref, ":")
	return kind, id
}

// Fields are a note's graph fields. All are optional.
type Fields struct {
	Kind     string `json:"kind,omitempty"`
	Status   string `json:"status,omitempty"`
	Strength string `json:"strength,omitempty"`
	BasedOn  []Ref  `json:"based_on,omitempty"`
	Private  bool   `json:"private,omitempty"`
}

// IsNode reports whether the note is a goal or a decision.
func (f Fields) IsNode() bool { return f.Kind != "" }

// Visible reports whether viewer may see n: private notes only to their
// author.
func (n Note) Visible(viewer string) bool { return !n.Private || n.Author == viewer }

var refRe = regexp.MustCompile(`^(note|transcript):([0-9a-f]{12})$`)

// ErrLoop is returned when links would make a note depend on itself.
var ErrLoop = errors.New("that would make a loop")

// normalizeFields checks and tidies graph fields: a known kind with one of
// its statuses (the default if none), a known strength, well-formed refs
// without repeats. Status and strength are cleared on plain notes.
func normalizeFields(f Fields) (Fields, error) {
	f.Kind = strings.ToLower(strings.TrimSpace(f.Kind))
	f.Status = strings.ToLower(strings.TrimSpace(f.Status))
	f.Strength = strings.ToLower(strings.TrimSpace(f.Strength))
	switch f.Kind {
	case "":
		f.Status, f.Strength = "", ""
	case KindGoal, KindDecision:
		if f.Status == "" || !slices.Contains(statuses[f.Kind], f.Status) {
			f.Status = statuses[f.Kind][0]
		}
		if f.Strength != "" && !slices.Contains(Strengths, f.Strength) {
			return f, fmt.Errorf("strength is one of %s", strings.Join(Strengths, ", "))
		}
	default:
		return f, fmt.Errorf("a note is a goal, a decision, or neither (not %q)", f.Kind)
	}
	var refs []Ref
	seen := map[string]bool{}
	for _, r := range f.BasedOn {
		r.Ref = strings.ToLower(strings.TrimSpace(r.Ref))
		if r.Ref == "" {
			continue
		}
		if !refRe.MatchString(r.Ref) {
			return f, fmt.Errorf("%q isn't a link to a note or transcript", r.Ref)
		}
		r.Quote = strings.Join(strings.Fields(r.Quote), " ")
		if rs := []rune(r.Quote); len(rs) > 1000 {
			r.Quote = string(rs[:1000])
		}
		if seen[r.Ref] {
			continue
		}
		seen[r.Ref] = true
		refs = append(refs, r)
	}
	f.BasedOn = refs
	return f, nil
}

// checkRefs checks that note id's links point at notes and transcripts
// that exist and don't make a loop. all is every note; transcriptExists may
// be nil (transcript links are then taken on trust).
func checkRefs(id string, refs []Ref, all []Note, transcriptExists func(string) bool) error {
	byID := map[string]Note{}
	for _, n := range all {
		byID[n.ID] = n
	}
	for _, r := range refs {
		kind, tid := r.Target()
		switch kind {
		case "note":
			if tid == id {
				return fmt.Errorf("%w: a note can't be based on itself", ErrLoop)
			}
			if _, ok := byID[tid]; !ok {
				return fmt.Errorf("there's no note %s", tid)
			}
		case "transcript":
			if transcriptExists != nil && !transcriptExists(tid) {
				return fmt.Errorf("there's no transcript %s", tid)
			}
		}
	}
	// A loop: one of the notes this one is based on is (indirectly) based
	// on this one.
	seen := map[string]bool{}
	var reaches func(from string) bool
	reaches = func(from string) bool {
		if from == id {
			return true
		}
		if seen[from] {
			return false
		}
		seen[from] = true
		for _, r := range byID[from].BasedOn {
			if k, t := r.Target(); k == "note" && reaches(t) {
				return true
			}
		}
		return false
	}
	for _, r := range refs {
		if k, t := r.Target(); k == "note" && reaches(t) {
			return fmt.Errorf("%w: %q is already based on this note", ErrLoop, byID[t].Title)
		}
	}
	return nil
}

// BasedOnThis lists the notes viewer can see that are based on ref
// ("note:<id>" or "transcript:<id>").
func BasedOnThis(all []Note, ref, viewer string) []Note {
	var out []Note
	for _, n := range all {
		if !n.Visible(viewer) {
			continue
		}
		for _, r := range n.BasedOn {
			if r.Ref == ref {
				out = append(out, n)
				break
			}
		}
	}
	return out
}

// GraphNode is one goal or decision in the outline.
type GraphNode struct {
	Note     Note
	Children []*GraphNode
	// Again is set where the node was already shown in full under another
	// parent; it's shown collapsed here.
	Again bool
	// Parents are the other goals and decisions it's based on (titles of
	// the visible ones), for "also under".
	Parents []Note
}

// Outline arranges the goals and decisions viewer can see: at the top the
// ones not based on another shown goal or decision, and under each the
// ones based on it. withFinished includes achieved, dropped, rejected and
// reversed ones. A node with several parents is in full under the first
// and marked Again under the others.
func Outline(all []Note, viewer string, withFinished bool) []*GraphNode {
	shown := map[string]Note{}
	var order []Note // newest first, as List returns
	for _, n := range all {
		if n.IsNode() && n.Visible(viewer) && !n.Pending && (withFinished || !Finished(n.Status)) {
			shown[n.ID] = n
			order = append(order, n)
		}
	}
	parentsOf := func(n Note) []Note {
		var out []Note
		for _, r := range n.BasedOn {
			if k, id := r.Target(); k == "note" {
				if p, ok := shown[id]; ok {
					out = append(out, p)
				}
			}
		}
		return out
	}
	children := map[string][]Note{}
	var roots []Note
	for i := len(order) - 1; i >= 0; i-- { // oldest first reads better in an outline
		n := order[i]
		ps := parentsOf(n)
		if len(ps) == 0 {
			roots = append(roots, n)
		}
		for _, p := range ps {
			children[p.ID] = append(children[p.ID], n)
		}
	}
	done := map[string]bool{}
	var build func(n Note, path map[string]bool) *GraphNode
	build = func(n Note, path map[string]bool) *GraphNode {
		g := &GraphNode{Note: n, Parents: parentsOf(n)}
		if done[n.ID] || path[n.ID] {
			g.Again = true
			return g
		}
		done[n.ID] = true
		path[n.ID] = true
		for _, c := range children[n.ID] {
			g.Children = append(g.Children, build(c, path))
		}
		delete(path, n.ID)
		return g
	}
	var out []*GraphNode
	for _, r := range roots {
		out = append(out, build(r, map[string]bool{}))
	}
	// Nodes only reachable through a loop (refused when saving, but files
	// can be edited by hand) still show up.
	for i := len(order) - 1; i >= 0; i-- {
		if n := order[i]; !done[n.ID] {
			out = append(out, build(n, map[string]bool{}))
		}
	}
	return out
}
