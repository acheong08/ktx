package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"

	"github.com/acheong08/ktx/internal/gitrepo"
)

// A person's manual order of their own tasks, and whether their task list
// uses it, kept in git next to their calendars:
//
//	calendar/<owner>/task-order.json   {"sort": "manual", "order": ["personal/x.ics", ...]}
//
// Tasks are named "<calendar id>/<file name>". The file isn't a calendar
// (calendars are directories), so CalDAV and the calendar list ignore it.
// Task files aren't touched by reordering, so phones don't re-download them.

// Sort modes.
const (
	SortDue    = "due"    // by due date (the default)
	SortManual = "manual" // by the person's own order
)

// TaskOrder is one person's task order.
type TaskOrder struct {
	Sort  string   `json:"sort"`
	Order []string `json:"order"` // "<calendar id>/<file name>", first first
}

func taskOrderPath(owner string) string { return path.Join(Root, owner, "task-order.json") }

// TaskKey names a task in a TaskOrder.
func TaskKey(t ToDo) string { return t.Calendar.ID + "/" + t.Name }

// TaskOrder reads a person's task order (SortDue and no order if unset).
func (s *Service) TaskOrder(owner string) (TaskOrder, error) {
	o := TaskOrder{Sort: SortDue}
	if !ValidID(owner) {
		return o, nil
	}
	err := s.repo.View(func(root string) error {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(taskOrderPath(owner))))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &o); err != nil {
			return fmt.Errorf("%s: %w", taskOrderPath(owner), err)
		}
		return nil
	})
	if o.Sort != SortManual {
		o.Sort = SortDue
	}
	return o, err
}

// SetTaskSort switches a person's list between SortDue and SortManual.
func (s *Service) SetTaskSort(ctx context.Context, owner, mode, user string) error {
	if mode != SortManual {
		mode = SortDue
	}
	return s.updateTaskOrder(ctx, owner, user, "sort by "+mode, func(o *TaskOrder) { o.Sort = mode })
}

// SetTaskOrder puts the given tasks first, in this order, and switches to
// SortManual. Tasks ordered before but not given (e.g. done ones) keep
// their relative order after them, so reopening one puts it back near
// where it was.
func (s *Service) SetTaskOrder(ctx context.Context, owner string, keys []string, user string) error {
	return s.updateTaskOrder(ctx, owner, user, "reorder", func(o *TaskOrder) {
		seen := map[string]bool{}
		var out []string
		for _, k := range append(slices.Clone(keys), o.Order...) {
			if k != "" && !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
		o.Sort, o.Order = SortManual, out
	})
}

func (s *Service) updateTaskOrder(ctx context.Context, owner, user, what string, fn func(*TaskOrder)) error {
	if !ValidID(owner) {
		return fmt.Errorf("%w: owner %q", ErrInvalid, owner)
	}
	rel := taskOrderPath(owner)
	_, err := s.repo.Update(ctx, func(root string) (gitrepo.Change, error) {
		o := TaskOrder{Sort: SortDue}
		abs := filepath.Join(root, filepath.FromSlash(rel))
		old, err := os.ReadFile(abs)
		if err == nil {
			_ = json.Unmarshal(old, &o)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return gitrepo.Change{}, err
		}
		// Forget tasks that no longer exist.
		var live []string
		for _, k := range o.Order {
			if _, err := os.Stat(filepath.Join(root, Root, owner, filepath.FromSlash(k))); err == nil {
				live = append(live, k)
			}
		}
		o.Order = live
		fn(&o)
		b, err := json.MarshalIndent(o, "", "  ")
		if err != nil {
			return gitrepo.Change{}, err
		}
		b = append(b, '\n')
		if string(b) == string(old) {
			return gitrepo.Change{}, nil
		}
		ch := gitrepo.Change{Paths: []string{rel}, Message: "Tasks: " + what + " (" + owner + ")\n\nChanged-By: " + user}
		return ch, writeFile(root, rel, b)
	})
	return err
}

// SortManually orders tasks by a TaskOrder: open tasks not in it first
// (new ones, or all of them right after switching to manual), in due-date
// order among themselves, then the ordered ones; done tasks after, as
// SortToDos puts them.
func SortManually(ts []ToDo, o TaskOrder) {
	SortToDos(ts)
	pos := map[string]int{}
	for i, k := range o.Order {
		pos[k] = i
	}
	sort.SliceStable(ts, func(i, j int) bool {
		a, b := ts[i], ts[j]
		if a.Done != b.Done {
			return !a.Done
		}
		if a.Done {
			return a.Completed.After(b.Completed)
		}
		pa, oka := pos[TaskKey(a)]
		pb, okb := pos[TaskKey(b)]
		switch {
		case oka != okb:
			return !oka
		case !oka:
			return false // both unplaced: keep due-date order
		default:
			return pa < pb
		}
	})
}
