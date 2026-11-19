package notes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/acheong08/ktx/internal/gitrepo"
)

// LabelsFile holds the label definitions.
var LabelsFile = path.Join(Root, "labels.yaml")

// Label is a label notes can have, with what it means. The meaning is
// shown to people and agents, and the model labels new notes by it.
type Label struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Count       int    `yaml:"-"` // notes that have it; set by LabelsInUse
}

var labelName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,29}$`)

// ErrLabelInUse is returned when deleting a label notes still have.
var ErrLabelInUse = errors.New("label in use")

// NormalizeLabel lowercases a name and checks it: letters, digits, - and _,
// at most 30.
func NormalizeLabel(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !labelName.MatchString(name) {
		return "", fmt.Errorf("%q isn't a valid label name: use lowercase letters, digits, - and _ (at most 30)", name)
	}
	return name, nil
}

// Labels returns the defined labels, in file order.
func (s *Service) Labels() ([]Label, error) {
	var out []Label
	err := s.repo.View(func(root string) error {
		var err error
		out, err = readLabels(root)
		return err
	})
	return out, err
}

// LabelsInUse returns the defined labels with how many notes have each.
func (s *Service) LabelsInUse() ([]Label, error) {
	defs, err := s.Labels()
	if err != nil {
		return nil, err
	}
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	for i := range defs {
		for _, n := range all {
			if slices.Contains(n.Labels, defs[i].Name) {
				defs[i].Count++
			}
		}
	}
	return defs, nil
}

// PutLabel adds a label, or changes the meaning of an existing one, in
// one commit.
func (s *Service) PutLabel(ctx context.Context, name, description, user string) (Label, error) {
	name, err := NormalizeLabel(name)
	if err != nil {
		return Label{}, err
	}
	description = strings.Join(strings.Fields(description), " ")
	if description == "" {
		return Label{}, errors.New("say what the label means: people and the AI label notes by it")
	}
	if len([]rune(description)) > 500 {
		return Label{}, errors.New("keep the meaning under 500 characters")
	}
	l := Label{Name: name, Description: description}
	_, err = s.repo.Update(ctx, func(root string) (gitrepo.Change, error) {
		defs, err := readLabels(root)
		if err != nil {
			return gitrepo.Change{}, err
		}
		action := "Add note label"
		if i := slices.IndexFunc(defs, func(d Label) bool { return d.Name == name }); i >= 0 {
			if defs[i].Description == description {
				return gitrepo.Change{}, nil
			}
			defs[i].Description, action = description, "Change note label"
		} else {
			defs = append(defs, l)
		}
		ch := gitrepo.Change{Paths: []string{LabelsFile}, Message: action + ": " + name + "\n\nChanged-By: " + user}
		return ch, writeLabels(root, defs)
	})
	return l, err
}

// DeleteLabel removes a label's definition in one commit. A label that
// notes still have can't be deleted (ErrLabelInUse).
func (s *Service) DeleteLabel(ctx context.Context, name, user string) error {
	defs, err := s.LabelsInUse()
	if err != nil {
		return err
	}
	if i := slices.IndexFunc(defs, func(d Label) bool { return d.Name == name }); i >= 0 && defs[i].Count > 0 {
		return fmt.Errorf("%w: %d notes are labelled %s; take it off them first", ErrLabelInUse, defs[i].Count, name)
	}
	_, err = s.repo.Update(ctx, func(root string) (gitrepo.Change, error) {
		defs, err := readLabels(root)
		if err != nil {
			return gitrepo.Change{}, err
		}
		i := slices.IndexFunc(defs, func(d Label) bool { return d.Name == name })
		if i < 0 {
			return gitrepo.Change{}, nil
		}
		defs = slices.Delete(defs, i, i+1)
		ch := gitrepo.Change{Paths: []string{LabelsFile}, Message: "Delete note label: " + name + "\n\nChanged-By: " + user}
		return ch, writeLabels(root, defs)
	})
	return err
}

// checkLabels validates picked labels against the definitions.
func (s *Service) checkLabels(picked, keep []string) ([]string, error) {
	defs, err := s.Labels()
	if err != nil {
		return nil, err
	}
	return checkLabelsIn(picked, defs, keep)
}

// checkLabelsIn dedupes picked labels and puts them in definition order.
// Each must be defined, or in keep (labels a note already had).
func checkLabelsIn(picked []string, defs []Label, keep []string) ([]string, error) {
	var out []string
	for _, p := range picked {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" || slices.Contains(out, p) {
			continue
		}
		if !slices.ContainsFunc(defs, func(d Label) bool { return d.Name == p }) && !slices.Contains(keep, p) {
			return nil, fmt.Errorf("there's no label %q; define it in Settings → Labels", p)
		}
		out = append(out, p)
	}
	return ordered(out, defs), nil
}

// ordered dedupes labels and sorts them in definition order; undefined
// ones go last.
func ordered(labels []string, defs []Label) []string {
	var out []string
	for _, d := range defs {
		if slices.Contains(labels, d.Name) {
			out = append(out, d.Name)
		}
	}
	for _, l := range labels {
		if !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

func readLabels(root string) ([]Label, error) {
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(LabelsFile)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var defs []Label
	if err := yaml.Unmarshal(b, &defs); err != nil {
		return nil, fmt.Errorf("%s: %w", LabelsFile, err)
	}
	return defs, nil
}

func writeLabels(root string, defs []Label) error {
	var b bytes.Buffer
	b.WriteString("# Labels for notes: what each means. Edited in ktx (Settings → Labels);\n")
	b.WriteString("# the AI labels new notes by these meanings.\n")
	if len(defs) > 0 {
		enc := yaml.NewEncoder(&b)
		enc.SetIndent(2)
		if err := enc.Encode(defs); err != nil {
			return err
		}
	}
	return writeFile(root, LabelsFile, b.Bytes())
}

// EnsureLabel defines a label with a meaning if it isn't defined yet, and
// leaves an existing one as it is.
func (s *Service) EnsureLabel(ctx context.Context, name, description, user string) error {
	defs, err := s.Labels()
	if err != nil {
		return err
	}
	if slices.ContainsFunc(defs, func(d Label) bool { return d.Name == name }) {
		return nil
	}
	_, err = s.PutLabel(ctx, name, description, user)
	return err
}
