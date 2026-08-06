package devloop

import (
	"errors"
	"sort"
	"time"
)

// Debouncer deterministically coalesces file-event bursts.
type Debouncer struct {
	quietPeriod time.Duration
	maxDelay    time.Duration
	first       time.Time
	last        time.Time
	pending     map[string]FileEvent
}

// NewDebouncer creates a quiet-period debouncer with a starvation bound.
func NewDebouncer(quietPeriod, maxDelay time.Duration) (*Debouncer, error) {
	if quietPeriod <= 0 {
		return nil, errors.New("development quiet period must be positive")
	}
	if maxDelay < quietPeriod {
		return nil, errors.New("development maximum delay must not be shorter than the quiet period")
	}
	return &Debouncer{
		quietPeriod: quietPeriod,
		maxDelay:    maxDelay,
		pending:     make(map[string]FileEvent),
	}, nil
}

// Add coalesces one validated event into the pending burst.
func (debouncer *Debouncer) Add(now time.Time, event FileEvent) error {
	if _, err := NewFileEvent(event.Path, event.Kind); err != nil {
		return err
	}
	if len(debouncer.pending) == 0 {
		debouncer.first = now
	}
	debouncer.last = now
	if previous, found := debouncer.pending[event.Path]; found {
		event.Kind = mergeChange(previous.Kind, event.Kind)
	}
	debouncer.pending[event.Path] = event
	return nil
}

// Pending reports whether a change burst is waiting for acceptance.
func (debouncer *Debouncer) Pending() bool {
	return len(debouncer.pending) != 0
}

// Deadline returns the next quiet or maximum-delay deadline.
func (debouncer *Debouncer) Deadline() (time.Time, bool) {
	if len(debouncer.pending) == 0 {
		return time.Time{}, false
	}
	quietDeadline := debouncer.last.Add(debouncer.quietPeriod)
	maximumDeadline := debouncer.first.Add(debouncer.maxDelay)
	if maximumDeadline.Before(quietDeadline) {
		return maximumDeadline, true
	}
	return quietDeadline, true
}

// Take accepts a ready burst as an immutable batch.
func (debouncer *Debouncer) Take(now time.Time, revision uint64) (Batch, bool) {
	deadline, found := debouncer.Deadline()
	if !found || now.Before(deadline) {
		return Batch{}, false
	}
	paths := make([]string, 0, len(debouncer.pending))
	for path := range debouncer.pending {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	changes := make([]FileEvent, 0, len(paths))
	for _, path := range paths {
		changes = append(changes, debouncer.pending[path])
	}
	clear(debouncer.pending)
	debouncer.first = time.Time{}
	debouncer.last = time.Time{}
	return newBatch(revision, changes), true
}

func mergeChange(previous, next ChangeKind) ChangeKind {
	switch {
	case previous == ChangeRemove && next == ChangeCreate:
		return ChangeWrite
	case previous == ChangeCreate && next == ChangeWrite:
		return ChangeCreate
	case next == ChangeRemove:
		return ChangeRemove
	default:
		return next
	}
}
