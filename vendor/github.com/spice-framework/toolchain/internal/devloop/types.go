// @import { Module } from "github.com/spice-framework/spice/annotation/modulith"

// Package devloop coordinates deterministic application rebuilds and restarts
// for development tools.
//
// @Module
package devloop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// ErrGracefulStopTimeout reports that graceful shutdown exceeded its deadline
// but process escalation completed successfully.
var ErrGracefulStopTimeout = errors.New("development graceful stop timed out")

// ChangeKind identifies the filesystem transition represented by a file event.
type ChangeKind string

const (
	// ChangeInitial requests the initial application candidate.
	ChangeInitial ChangeKind = "initial"
	// ChangeCreate reports a newly created path.
	ChangeCreate ChangeKind = "create"
	// ChangeWrite reports changed contents or metadata.
	ChangeWrite ChangeKind = "write"
	// ChangeRemove reports a removed path.
	ChangeRemove ChangeKind = "remove"
	// ChangeRename reports a renamed path.
	ChangeRename ChangeKind = "rename"
)

// Valid reports whether the kind is a supported development-loop transition.
func (kind ChangeKind) Valid() bool {
	switch kind {
	case ChangeInitial, ChangeCreate, ChangeWrite, ChangeRemove, ChangeRename:
		return true
	default:
		return false
	}
}

// FileEvent is a normalized, workspace-relative filesystem change.
type FileEvent struct {
	Path string
	Kind ChangeKind
}

// NewFileEvent validates and constructs a normalized file event.
func NewFileEvent(path string, kind ChangeKind) (FileEvent, error) {
	if path == "" {
		return FileEvent{}, errors.New("development file event path must not be empty")
	}
	if !kind.Valid() {
		return FileEvent{}, fmt.Errorf("unsupported development file event kind %q", kind)
	}
	return FileEvent{Path: path, Kind: kind}, nil
}

// Batch is an immutable, deterministically ordered set of accepted changes.
type Batch struct {
	revision uint64
	changes  []FileEvent
}

func newBatch(revision uint64, changes []FileEvent) Batch {
	return Batch{
		revision: revision,
		changes:  slices.Clone(changes),
	}
}

// Revision returns the monotonically increasing preparation revision.
func (batch Batch) Revision() uint64 {
	return batch.revision
}

// Changes returns a defensive copy of the accepted changes.
func (batch Batch) Changes() []FileEvent {
	return slices.Clone(batch.changes)
}

// Watcher produces normalized relevant file events until it is closed.
type Watcher interface {
	Events() <-chan FileEvent
	Errors() <-chan error
	Close() error
}

// Timer is the resettable timer contract used by the debounce coordinator.
type Timer interface {
	C() <-chan time.Time
	Reset(time.Duration) bool
	Stop() bool
}

// Clock supplies time to the development loop and permits deterministic tests.
type Clock interface {
	Now() time.Time
	NewTimer(time.Duration) Timer
}

// Pipeline analyzes, generates, and builds one isolated application candidate.
//
// Implementations must honor cancellation and must not replace a running
// artifact. Any returned candidate is owned by the caller.
type Pipeline interface {
	Prepare(context.Context, Batch) (Candidate, error)
}

// Process is one launched application process. Stop must request graceful
// shutdown, escalate within the supplied deadline when needed, and return only
// after the process has exited.
type Process interface {
	Wait() <-chan error
	Stop(context.Context) error
}

// Launcher starts a fully prepared candidate without invoking compilation.
type Launcher interface {
	Start(context.Context, Candidate) (Process, error)
}

// EventKind identifies one structured development-loop observation.
type EventKind string

const (
	// EventChangeDetected reports a relevant source change.
	EventChangeDetected EventKind = "change_detected"
	// EventAnalysisStarted reports that a debounced batch entered preparation.
	EventAnalysisStarted EventKind = "analysis_started"
	// EventGenerationComplete reports guarded generated-file application.
	EventGenerationComplete EventKind = "generation_complete"
	// EventBuildComplete reports creation of a unique executable candidate.
	EventBuildComplete EventKind = "build_complete"
	// EventCandidateReady reports a completely prepared build candidate.
	EventCandidateReady EventKind = "candidate_ready"
	// EventPreparationFailed reports an analysis, generation, or build failure.
	EventPreparationFailed EventKind = "preparation_failed"
	// EventLastKnownGood reports that an existing process remains active.
	EventLastKnownGood EventKind = "last_known_good"
	// EventCandidateDiscarded reports that an obsolete candidate was rejected.
	EventCandidateDiscarded EventKind = "candidate_discarded"
	// EventRestartRequested reports the start of graceful replacement.
	EventRestartRequested EventKind = "restart_requested"
	// EventApplicationStarted reports a successful candidate launch.
	EventApplicationStarted EventKind = "application_started"
	// EventApplicationExited reports application process completion.
	EventApplicationExited EventKind = "application_exited"
	// EventWatchError reports a recoverable watcher failure.
	EventWatchError EventKind = "watch_error"
	// EventCleanupFailed reports failure to release an obsolete artifact.
	EventCleanupFailed EventKind = "cleanup_failed"
	// EventShutdownTimedOut reports successful escalation after a stop deadline.
	EventShutdownTimedOut EventKind = "shutdown_timed_out"
)

// Event is a concise structured development-loop observation.
type Event struct {
	Kind     EventKind
	Revision uint64
	Paths    []string
	Err      error
	Stale    bool
	Duration time.Duration
	Reused   bool
}

// EventSink receives serialized observations from the engine event loop.
type EventSink interface {
	Emit(Event)
}

// Candidate owns one unique build artifact and its cleanup operation.
//
// Candidate copies share an idempotent cleanup state.
type Candidate struct {
	artifact string
	state    *candidateState
}

type candidateState struct {
	dispose func() error
	once    sync.Once
	err     error
}

// NewCandidate constructs an owned build candidate.
func NewCandidate(artifact string, dispose func() error) (Candidate, error) {
	if artifact == "" {
		return Candidate{}, errors.New("development candidate artifact must not be empty")
	}
	if dispose == nil {
		return Candidate{}, errors.New("development candidate cleanup must not be nil")
	}
	return Candidate{
		artifact: artifact,
		state:    &candidateState{dispose: dispose},
	}, nil
}

// Artifact returns the candidate's opaque launch artifact identity.
func (candidate Candidate) Artifact() string {
	return candidate.artifact
}

// Dispose releases the candidate exactly once.
func (candidate Candidate) Dispose() error {
	if candidate.state == nil {
		return nil
	}
	candidate.state.once.Do(func() {
		candidate.state.err = candidate.state.dispose()
	})
	return candidate.state.err
}

type realClock struct{}

func (realClock) Now() time.Time {
	return time.Now()
}

func (realClock) NewTimer(duration time.Duration) Timer {
	return realTimer{timer: time.NewTimer(duration)}
}

type realTimer struct {
	timer *time.Timer
}

func (timer realTimer) C() <-chan time.Time {
	return timer.timer.C
}

func (timer realTimer) Reset(duration time.Duration) bool {
	return timer.timer.Reset(duration)
}

func (timer realTimer) Stop() bool {
	return timer.timer.Stop()
}

type discardSink struct{}

func (discardSink) Emit(Event) {}
