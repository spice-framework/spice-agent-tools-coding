package process

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
)

const maximumRetryClassificationDepth = 128

// Operation classifies a provider-neutral process operation.
type Operation string

const (
	OperationLaunch      Operation = "launch"
	OperationResolve     Operation = "resolve"
	OperationResult      Operation = "result"
	OperationRequestStop Operation = "request_stop"
	OperationForceKill   Operation = "force_kill"
	OperationWait        Operation = "wait"
)

// Failure wraps an implementation failure while keeping its formatted and
// serialized form free of command, path, environment, and platform details.
// Unwrap preserves cancellation and implementation-specific error identity for
// deliberate programmatic inspection.
type Failure struct {
	operation Operation
	cause     error
}

// NewFailure returns nil for a nil cause and otherwise creates a redacted
// typed failure. Unknown operations are represented safely as an empty
// Operation rather than copied into human-facing output.
func NewFailure(operation Operation, cause error) error {
	if cause == nil {
		return nil
	}
	if !validOperation(operation) {
		operation = ""
	}
	return &Failure{operation: operation, cause: cause}
}

func (failure *Failure) Error() string {
	if failure == nil || !validOperation(failure.operation) {
		return "process operation failed"
	}
	return "process " + string(failure.operation) + " failed"
}

func (failure *Failure) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.cause
}

func (failure *Failure) Operation() Operation {
	if failure == nil {
		return ""
	}
	return failure.operation
}

// Retryable delegates explicit retry classification to the wrapped cause. An
// unclassified failure remains retryable so a failed containment observation
// cannot silently surrender process ownership.
func (failure *Failure) Retryable() bool {
	if failure == nil {
		return false
	}
	return !containsNonRetryable(failure.cause, 0)
}

func containsNonRetryable(err error, depth int) bool {
	if err == nil {
		return false
	}
	if depth >= maximumRetryClassificationDepth {
		// A cyclic or adversarial error graph cannot prove that cleanup is safe
		// to repeat, so fail closed.
		return true
	}
	// Direct traversal avoids recursively invoking Failure.Retryable and still
	// inspects every joined branch; errors.As cannot provide that guarantee.
	if nested, ok := err.(*Failure); ok { //nolint:errorlint // Avoid recursive Retryable while walking every branch.
		if nested == nil {
			return false
		}
		return containsNonRetryable(nested.cause, depth+1)
	}
	// Direct structural traversal is required because errors.As observes only
	// one joined branch and a later terminal classifier must dominate.
	if classified, ok := err.(interface{ Retryable() bool }); ok &&
		classificationIsTerminal(classified) {
		return true
	}
	children, safe := retryChildren(err)
	if !safe {
		return true
	}
	for _, cause := range children {
		if containsNonRetryable(cause, depth+1) {
			return true
		}
	}
	return false
}

func classificationIsTerminal(classified interface{ Retryable() bool }) (terminal bool) {
	terminal = true
	defer func() {
		if recover() != nil {
			terminal = true
		}
	}()
	return !classified.Retryable()
}

func retryChildren(err error) (children []error, safe bool) {
	defer func() {
		if recover() != nil {
			children = nil
			safe = false
		}
	}()
	// Direct structural traversal is required to inspect every joined branch.
	switch wrapped := err.(type) { //nolint:errorlint // errors.As cannot inspect every joined branch.
	case interface{ Unwrap() []error }:
		return wrapped.Unwrap(), true
	case interface{ Unwrap() error }:
		return []error{wrapped.Unwrap()}, true
	default:
		return nil, true
	}
}

func (failure *Failure) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, failure.Error())
}

func (failure *Failure) MarshalJSON() ([]byte, error) {
	return json.Marshal(failure.Error())
}

func validOperation(operation Operation) bool {
	switch operation {
	case OperationLaunch, OperationResolve, OperationResult, OperationRequestStop, OperationForceKill, OperationWait:
		return true
	default:
		return false
	}
}

// OutcomeKind classifies the provider-neutral root-process outcome.
type OutcomeKind string

const (
	OutcomeExited   OutcomeKind = "exited"
	OutcomeSignaled OutcomeKind = "signaled"
	OutcomeUnknown  OutcomeKind = "unknown"

	// MaximumExitCode preserves every unsigned 32-bit Windows exit code while
	// also covering Unix exit statuses.
	MaximumExitCode int64 = 1<<32 - 1
)

// Outcome is an immutable root-process result. It says nothing about whether
// descendants or platform containment resources have joined; Wait owns that
// separate fact.
type Outcome struct {
	kind     OutcomeKind
	exitCode int64
	hasCode  bool
}

// NewExitedOutcome creates an ordinary process-exit outcome.
func NewExitedOutcome(exitCode int64) (Outcome, error) {
	if exitCode < 0 || exitCode > MaximumExitCode {
		return Outcome{}, &OutcomeError{problem: "exit_code_out_of_range"}
	}
	return Outcome{kind: OutcomeExited, exitCode: exitCode, hasCode: true}, nil
}

// NewSignaledOutcome reports termination by a platform signal or analogous
// forced mechanism without exposing a platform-specific signal value.
func NewSignaledOutcome() Outcome { return Outcome{kind: OutcomeSignaled} }

// NewUnknownOutcome reports that the root is known to have terminated but no
// portable exit classification is available. Unknown is never successful.
func NewUnknownOutcome() Outcome { return Outcome{kind: OutcomeUnknown} }

// Validate rejects a zero or corrupted outcome.
func (outcome Outcome) Validate() error {
	switch outcome.kind {
	case OutcomeExited:
		if !outcome.hasCode || outcome.exitCode < 0 || outcome.exitCode > MaximumExitCode {
			return &OutcomeError{problem: "invalid_exited_outcome"}
		}
	case OutcomeSignaled, OutcomeUnknown:
		if outcome.hasCode || outcome.exitCode != 0 {
			return &OutcomeError{problem: "unexpected_exit_code"}
		}
	default:
		return &OutcomeError{problem: "unsupported_kind"}
	}
	return nil
}

func (outcome Outcome) Kind() OutcomeKind { return outcome.kind }
func (outcome Outcome) Successful() bool {
	return outcome.kind == OutcomeExited && outcome.hasCode && outcome.exitCode == 0
}
func (outcome Outcome) ExitCode() (int64, bool) { return outcome.exitCode, outcome.hasCode }

// OutcomeError is a typed, secret-safe invalid-outcome failure.
type OutcomeError struct{ problem string }

func (failure *OutcomeError) Error() string {
	if failure == nil || failure.problem == "" {
		return "invalid process outcome"
	}
	return "invalid process outcome: " + failure.problem
}

func (failure *OutcomeError) Problem() string {
	if failure == nil {
		return ""
	}
	return failure.problem
}

// Launcher is an injected provider-neutral process constructor. Start's
// context bounds launch only; a successfully returned Process has an
// independent lifetime. When Start returns both a Process and an error,
// ownership of that Process still transfers to the caller and must be joined.
// Implementations must never return a nil Process with a nil error.
type Launcher interface {
	Start(context.Context, Spec) (Process, error)
}

// LauncherFunc adapts an ordinary function for constructor injection and
// ordered policy decoration.
type LauncherFunc func(context.Context, Spec) (Process, error)

func (launcher LauncherFunc) Start(ctx context.Context, spec Spec) (Process, error) {
	return launcher(ctx, spec)
}

// Process owns one launched root and its implementation-defined containment
// resources. Done closes when the root outcome becomes stable. Result is then
// deterministic and returns a validated Outcome; its error is only an
// observation failure, never a containment/join failure.
//
// RequestStop and ForceKill request graceful and forced termination
// respectively. They are concurrency-safe and idempotent after success. A
// failure does not poison a later retry. Wait honors its context and returns
// nil only when all owned descendants and containment resources are safe to
// release. A canceled, unclassified, or explicitly retryable Wait retains
// ownership and a later Wait must perform a fresh join attempt. A Wait error
// implementing Retryable() bool with a false result declares that repeating
// containment cleanup is unsafe; ownership remains for manual recovery.
type Process interface {
	Done() <-chan struct{}
	Result() (Outcome, error)
	RequestStop(context.Context) error
	ForceKill(context.Context) error
	Wait(context.Context) error
}
