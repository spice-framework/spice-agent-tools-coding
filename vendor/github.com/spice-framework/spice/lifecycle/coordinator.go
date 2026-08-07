package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
)

// State is the observable state of one generated application coordinator.
type State string

const (
	// StateInvalid is returned for an uninitialized coordinator.
	StateInvalid State = ""
	// StateConstructed means providers exist but lifecycle hooks have not started.
	StateConstructed State = "constructed"
	// StateStarting means start hooks are executing.
	StateStarting State = "starting"
	// StateReady means every start hook completed successfully.
	StateReady State = "ready"
	// StateStopping means stop hooks or construction cleanups are executing.
	StateStopping State = "stopping"
	// StateStopped means normal stop and cleanup completed.
	StateStopped State = "stopped"
	// StateFailed means construction abort or startup rollback completed.
	StateFailed State = "failed"
)

// ErrInvalidTransition identifies an operation that is not legal in the
// coordinator's current state.
var ErrInvalidTransition = errors.New("invalid application lifecycle transition")

// TransitionError reports an invalid lifecycle state transition.
type TransitionError struct {
	Operation string
	State     State
}

// Error describes the rejected operation and current state.
func (e *TransitionError) Error() string {
	if e == nil {
		return ErrInvalidTransition.Error()
	}
	state := e.State
	if state == StateInvalid {
		return fmt.Sprintf("%s: %v: state is invalid", e.Operation, ErrInvalidTransition)
	}
	return fmt.Sprintf("%s: %v: state is %s", e.Operation, ErrInvalidTransition, state)
}

// Unwrap supports errors.Is(err, ErrInvalidTransition).
func (e *TransitionError) Unwrap() error {
	return ErrInvalidTransition
}

// Hook contains explicit generated start and optional stop callbacks for one
// provider-owned component.
type Hook struct {
	ID     string
	Module string
	Start  Cleanup
	Stop   Cleanup
}

// Operation identifies the lifecycle callback being observed.
type Operation string

const (
	// OperationStart identifies a component start hook.
	OperationStart Operation = "start"
	// OperationStop identifies a component stop hook.
	OperationStop Operation = "stop"
	// OperationCleanup identifies a provider construction cleanup.
	OperationCleanup Operation = "cleanup"
)

// Phase identifies whether a callback is about to run or has completed.
type Phase string

const (
	// PhaseBegin is emitted immediately before callback invocation.
	PhaseBegin Phase = "begin"
	// PhaseEnd is emitted immediately after callback invocation.
	PhaseEnd Phase = "end"
)

// Observation is one synchronous lifecycle callback observation. Err is set
// only on an end event when the callback failed.
type Observation struct {
	Module    string
	Component string
	Operation Operation
	Phase     Phase
	Err       error
}

// Observer receives lifecycle observations on the callback-executing
// goroutine. It has no error return and must not panic or block indefinitely.
type Observer func(context.Context, Observation)

// ContextFactory creates a caller-owned context and release function when
// shutdown begins. It allows Run to obtain a fresh shutdown deadline only after
// the run context is canceled.
type ContextFactory func() (context.Context, context.CancelFunc)

type callback struct {
	id     string
	module string
	fn     Cleanup
}

// Coordinator implements generic lifecycle state and callback ordering for one
// generated application. It performs no discovery or dependency resolution.
type Coordinator struct {
	mu        sync.Mutex
	state     State
	cleanups  []callback
	started   []Hook
	observers []Observer
	done      chan struct{}
	stopErr   error
}

// NewCoordinator returns a coordinator ready to accept construction cleanups.
func NewCoordinator() *Coordinator {
	return &Coordinator{
		state: StateConstructed,
		done:  make(chan struct{}),
	}
}

// State returns the coordinator's current state.
func (c *Coordinator) State() State {
	if c == nil {
		return StateInvalid
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// RegisterCleanup arms one provider cleanup. Generated constructors call this
// immediately after the corresponding provider succeeds. A nil cleanup is a
// valid no-op.
func (c *Coordinator) RegisterCleanup(id string, cleanup Cleanup) error {
	return c.RegisterModuleCleanup("", id, cleanup)
}

// RegisterModuleCleanup arms one provider cleanup with optional module
// ownership metadata. Generated constructors use this form.
func (c *Coordinator) RegisterModuleCleanup(module, id string, cleanup Cleanup) error {
	if c == nil {
		return &TransitionError{Operation: "register cleanup", State: StateInvalid}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != StateConstructed {
		return &TransitionError{Operation: "register cleanup", State: c.state}
	}
	if cleanup == nil {
		return nil
	}
	if id == "" {
		return errors.New("register cleanup: callback ID is required")
	}
	c.cleanups = append(c.cleanups, callback{id: id, module: module, fn: cleanup})
	return nil
}

// RegisterObserver adds a synchronous observer before lifecycle execution.
func (c *Coordinator) RegisterObserver(observer Observer) error {
	if c == nil {
		return &TransitionError{Operation: "register lifecycle observer", State: StateInvalid}
	}
	if observer == nil {
		return errors.New("register lifecycle observer: observer is nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != StateConstructed {
		return &TransitionError{Operation: "register lifecycle observer", State: c.state}
	}
	c.observers = append(c.observers, observer)
	return nil
}

// Abort records a construction failure, runs every armed cleanup in reverse
// construction order, and joins rollback failures with cause.
func (c *Coordinator) Abort(ctx context.Context, cause error) error {
	if ctx == nil {
		return errors.New("abort application construction: context is nil")
	}
	if cause == nil {
		return errors.New("abort application construction: cause is nil")
	}
	if c == nil {
		return errors.Join(
			cause,
			&TransitionError{Operation: "abort application construction", State: StateInvalid},
		)
	}
	if err := c.beginTerminal("abort application construction", StateConstructed); err != nil {
		return errors.Join(cause, err)
	}
	rollbackErr := c.runCallbacks(ctx, nil)
	return c.completeTerminal(StateFailed, errors.Join(cause, rollbackErr))
}

// Start executes explicit hooks serially in the supplied dependency-first
// order. A failure stops previously successful hooks, then runs every
// construction cleanup.
func (c *Coordinator) Start(ctx context.Context, hooks []Hook) error {
	if ctx == nil {
		return errors.New("start application: context is nil")
	}
	if c == nil {
		return &TransitionError{Operation: "start application", State: StateInvalid}
	}
	if err := validateHooks(hooks); err != nil {
		return err
	}

	c.mu.Lock()
	if c.state != StateConstructed {
		state := c.state
		c.mu.Unlock()
		return &TransitionError{Operation: "start application", State: state}
	}
	c.state = StateStarting
	c.mu.Unlock()

	if cause := context.Cause(ctx); cause != nil {
		return c.failStart(ctx, fmt.Errorf("start application: %w", cause))
	}
	for _, hook := range hooks {
		if cause := context.Cause(ctx); cause != nil {
			return c.failStart(ctx, fmt.Errorf("start component %s: %w", hook.ID, cause))
		}
		c.observe(ctx, hook.Module, hook.ID, OperationStart, PhaseBegin, nil)
		err := hook.Start(ctx)
		c.observe(ctx, hook.Module, hook.ID, OperationStart, PhaseEnd, err)
		if err != nil {
			return c.failStart(ctx, fmt.Errorf("start component %s: %w", hook.ID, err))
		}
		c.mu.Lock()
		c.started = append(c.started, hook)
		c.mu.Unlock()
	}

	c.mu.Lock()
	c.state = StateReady
	c.mu.Unlock()
	return nil
}

// Stop is idempotent. It stops successfully started components in reverse
// order and then runs construction cleanups in reverse order. Concurrent Stop
// calls wait for the in-progress stop or their own context cancellation.
func (c *Coordinator) Stop(ctx context.Context) error {
	if ctx == nil {
		return errors.New("stop application: context is nil")
	}
	if c == nil {
		return &TransitionError{Operation: "stop application", State: StateInvalid}
	}

	c.mu.Lock()
	switch c.state {
	case StateConstructed, StateReady:
		c.state = StateStopping
		c.mu.Unlock()
		stopErr := c.runCallbacks(ctx, c.takeStarted())
		return c.completeTerminal(StateStopped, stopErr)
	case StateStopping:
		done := c.done
		c.mu.Unlock()
		select {
		case <-done:
			c.mu.Lock()
			err := c.stopErr
			c.mu.Unlock()
			return err
		case <-ctx.Done():
			return fmt.Errorf("wait for application stop: %w", context.Cause(ctx))
		}
	case StateStopped, StateFailed:
		err := c.stopErr
		c.mu.Unlock()
		return err
	case StateStarting:
		c.mu.Unlock()
		return &TransitionError{Operation: "stop application", State: StateStarting}
	case StateInvalid:
		c.mu.Unlock()
		return &TransitionError{Operation: "stop application", State: StateInvalid}
	}
	state := c.state
	c.mu.Unlock()
	return &TransitionError{Operation: "stop application", State: state}
}

// Run starts the application, waits for ctx cancellation, obtains a fresh
// caller-owned shutdown context, and stops the application. Cancellation of the
// run context is the normal shutdown signal and is not returned as an error.
func (c *Coordinator) Run(ctx context.Context, hooks []Hook, shutdown ContextFactory) error {
	if ctx == nil {
		return errors.New("run application: context is nil")
	}
	if shutdown == nil {
		return errors.New("run application: shutdown context factory is nil")
	}
	if err := c.Start(ctx, hooks); err != nil {
		return err
	}
	<-ctx.Done()
	shutdownContext, cancel := shutdown()
	if shutdownContext == nil || cancel == nil {
		factoryErr := errors.New("run application: shutdown context factory returned a nil context or cancel function")
		return errors.Join(factoryErr, c.Stop(ctx))
	}
	defer cancel()
	return c.Stop(shutdownContext)
}

func (c *Coordinator) beginTerminal(operation string, allowed State) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != allowed {
		return &TransitionError{Operation: operation, State: c.state}
	}
	c.state = StateStopping
	return nil
}

func (c *Coordinator) failStart(ctx context.Context, cause error) error {
	c.mu.Lock()
	c.state = StateStopping
	started := c.takeStartedLocked()
	c.mu.Unlock()
	rollbackErr := c.runCallbacks(ctx, started)
	return c.completeTerminal(StateFailed, errors.Join(cause, rollbackErr))
}

func (c *Coordinator) takeStarted() []Hook {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.takeStartedLocked()
}

func (c *Coordinator) takeStartedLocked() []Hook {
	started := append([]Hook(nil), c.started...)
	c.started = nil
	return started
}

func (c *Coordinator) runCallbacks(ctx context.Context, started []Hook) error {
	var failures []error
	for _, hook := range slices.Backward(started) {
		if hook.Stop == nil {
			continue
		}
		c.observe(ctx, hook.Module, hook.ID, OperationStop, PhaseBegin, nil)
		err := hook.Stop(ctx)
		c.observe(ctx, hook.Module, hook.ID, OperationStop, PhaseEnd, err)
		if err != nil {
			failures = append(failures, fmt.Errorf("stop component %s: %w", hook.ID, err))
		}
	}

	c.mu.Lock()
	cleanups := append([]callback(nil), c.cleanups...)
	c.cleanups = nil
	c.mu.Unlock()
	for _, item := range slices.Backward(cleanups) {
		c.observe(ctx, item.module, item.id, OperationCleanup, PhaseBegin, nil)
		err := item.fn(ctx)
		c.observe(ctx, item.module, item.id, OperationCleanup, PhaseEnd, err)
		if err != nil {
			failures = append(failures, fmt.Errorf("cleanup provider %s: %w", item.id, err))
		}
	}
	return errors.Join(failures...)
}

func (c *Coordinator) observe(
	ctx context.Context,
	module string,
	component string,
	operation Operation,
	phase Phase,
	err error,
) {
	c.mu.Lock()
	observers := append([]Observer(nil), c.observers...)
	c.mu.Unlock()
	observation := Observation{
		Module:    module,
		Component: component,
		Operation: operation,
		Phase:     phase,
		Err:       err,
	}
	for _, observer := range observers {
		observer(ctx, observation)
	}
}

func (c *Coordinator) completeTerminal(state State, result error) error {
	c.mu.Lock()
	c.state = state
	c.stopErr = result
	close(c.done)
	c.mu.Unlock()
	return result
}

func validateHooks(hooks []Hook) error {
	seen := make(map[string]struct{}, len(hooks))
	for index, hook := range hooks {
		if hook.ID == "" {
			return fmt.Errorf("start application: hook %d has no ID", index)
		}
		if hook.Start == nil {
			return fmt.Errorf("start application: hook %s has no start callback", hook.ID)
		}
		if _, duplicate := seen[hook.ID]; duplicate {
			return fmt.Errorf("start application: duplicate hook ID %q", hook.ID)
		}
		seen[hook.ID] = struct{}{}
	}
	return nil
}
