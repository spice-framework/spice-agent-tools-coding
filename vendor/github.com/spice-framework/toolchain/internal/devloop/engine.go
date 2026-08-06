package devloop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

const (
	defaultQuietPeriod = 150 * time.Millisecond
	defaultMaxDelay    = 2 * time.Second
	defaultStopTimeout = 15 * time.Second
)

// ErrWatcherClosed reports an unexpected end of the filesystem event stream.
var ErrWatcherClosed = errors.New("development file watcher closed")

// Config controls deterministic debounce and graceful-stop bounds.
type Config struct {
	QuietPeriod time.Duration
	MaxDelay    time.Duration
	StopTimeout time.Duration
}

// Engine coordinates change acceptance, isolated preparation, and process
// replacement while preserving the last known good process on preparation
// failures.
type Engine struct {
	config   Config
	watcher  Watcher
	clock    Clock
	pipeline Pipeline
	launcher Launcher
	sink     EventSink
}

// NewEngine validates and constructs a development-loop engine.
func NewEngine(
	config Config,
	watcher Watcher,
	clock Clock,
	pipeline Pipeline,
	launcher Launcher,
	sink EventSink,
) (*Engine, error) {
	config = config.withDefaults()
	if config.QuietPeriod <= 0 {
		return nil, errors.New("development quiet period must be positive")
	}
	if config.MaxDelay < config.QuietPeriod {
		return nil, errors.New("development maximum delay must not be shorter than the quiet period")
	}
	if config.StopTimeout <= 0 {
		return nil, errors.New("development stop timeout must be positive")
	}
	if watcher == nil {
		return nil, errors.New("development watcher must not be nil")
	}
	if clock == nil {
		clock = realClock{}
	}
	if pipeline == nil {
		return nil, errors.New("development pipeline must not be nil")
	}
	if launcher == nil {
		return nil, errors.New("development launcher must not be nil")
	}
	if sink == nil {
		sink = discardSink{}
	}
	return &Engine{
		config:   config,
		watcher:  watcher,
		clock:    clock,
		pipeline: pipeline,
		launcher: launcher,
		sink:     sink,
	}, nil
}

func (config Config) withDefaults() Config {
	if config.QuietPeriod == 0 {
		config.QuietPeriod = defaultQuietPeriod
	}
	if config.MaxDelay == 0 {
		config.MaxDelay = defaultMaxDelay
	}
	if config.StopTimeout == 0 {
		config.StopTimeout = defaultStopTimeout
	}
	return config
}

type preparationResult struct {
	revision  uint64
	candidate Candidate
	err       error
}

type replacementResult struct {
	revision         uint64
	previousRevision uint64
	candidate        Candidate
	process          Process
	previousStopped  bool
	err              error
	warning          error
}

// Run performs the initial preparation and supervises changes until
// cancellation or a terminal watcher failure.
func (engine *Engine) Run(ctx context.Context) (returnErr error) {
	if ctx == nil {
		return errors.New("development context must not be nil")
	}
	debouncer, err := NewDebouncer(
		engine.config.QuietPeriod,
		engine.config.MaxDelay,
	)
	if err != nil {
		return err
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	state := engineState{
		engine:       engine,
		debouncer:    debouncer,
		preparations: make(chan preparationResult),
		replacements: make(chan replacementResult),
		cancels:      make(map[uint64]context.CancelFunc),
	}
	state.startPreparation(runCtx, []FileEvent{{
		Path: ".",
		Kind: ChangeInitial,
	}})
	runErr := state.loop(runCtx)
	cancelRun()
	state.cancelWork()
	stopErr := state.stopActive()
	state.stopDebounceTimer()
	closeErr := engine.watcher.Close()
	if errors.Is(runErr, context.Canceled) {
		runErr = nil
	}
	return errors.Join(runErr, stopErr, closeErr)
}

type engineState struct {
	engine    *Engine
	debouncer *Debouncer

	preparations chan preparationResult
	replacements chan replacementResult
	cancels      map[uint64]context.CancelFunc
	replaceStop  context.CancelFunc

	revision      uint64
	debounceTimer Timer
	debounceC     <-chan time.Time

	active          Process
	activeCandidate Candidate
	activeRevision  uint64
	activeWait      <-chan error
}

func (state *engineState) loop(ctx context.Context) error {
	events := state.engine.watcher.Events()
	watchErrors := state.engine.watcher.Errors()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-events:
			if !ok {
				if err := ctx.Err(); err != nil {
					return err
				}
				return ErrWatcherClosed
			}
			state.acceptFileEvent(event)
		case watchErr, ok := <-watchErrors:
			if !ok {
				watchErrors = nil
				continue
			}
			state.engine.sink.Emit(Event{
				Kind: EventWatchError,
				Err:  watchErr,
			})
		case now := <-state.debounceC:
			state.acceptPending(ctx, now)
		case result := <-state.preparations:
			state.finishPreparation(ctx, result)
		case result := <-state.replacements:
			state.finishReplacement(result)
		case exitErr, ok := <-state.activeWait:
			if !ok {
				exitErr = nil
			}
			state.finishApplication(exitErr)
		}
	}
}

func (state *engineState) acceptFileEvent(event FileEvent) {
	if err := state.debouncer.Add(state.engine.clock.Now(), event); err != nil {
		state.engine.sink.Emit(Event{
			Kind: EventWatchError,
			Err:  err,
		})
		return
	}
	state.cancelPreparations()
	if state.replaceStop != nil {
		state.replaceStop()
	}
	state.resetDebounceTimer()
	state.engine.sink.Emit(Event{
		Kind:  EventChangeDetected,
		Paths: []string{event.Path},
		Stale: state.active != nil,
	})
}

func (state *engineState) acceptPending(ctx context.Context, now time.Time) {
	batch, ready := state.debouncer.Take(now, state.revision+1)
	if !ready {
		state.resetDebounceTimer()
		return
	}
	state.debounceC = nil
	state.startPreparation(ctx, batch.Changes())
}

func (state *engineState) startPreparation(
	ctx context.Context,
	changes []FileEvent,
) {
	state.revision++
	batch := newBatch(state.revision, changes)
	paths := make([]string, len(changes))
	for index, change := range changes {
		paths[index] = change.Path
	}
	slices.Sort(paths)
	state.engine.sink.Emit(Event{
		Kind:     EventAnalysisStarted,
		Revision: state.revision,
		Paths:    paths,
		Stale:    state.active != nil,
	})

	prepareCtx, cancel := context.WithCancel(ctx)
	state.cancels[state.revision] = cancel
	go state.prepare(ctx, prepareCtx, batch)
}

func (state *engineState) prepare(
	runCtx context.Context,
	prepareCtx context.Context,
	batch Batch,
) {
	candidate, err := state.engine.pipeline.Prepare(prepareCtx, batch)
	result := preparationResult{
		revision:  batch.Revision(),
		candidate: candidate,
		err:       err,
	}
	select {
	case state.preparations <- result:
	case <-runCtx.Done():
		if candidate.Dispose() != nil {
			return
		}
	}
}

func (state *engineState) finishPreparation(
	ctx context.Context,
	result preparationResult,
) {
	if cancel, found := state.cancels[result.revision]; found {
		cancel()
		delete(state.cancels, result.revision)
	}
	if result.revision != state.revision || state.debouncer.Pending() {
		disposeErr := result.candidate.Dispose()
		state.engine.sink.Emit(Event{
			Kind:     EventCandidateDiscarded,
			Revision: result.revision,
			Err:      errors.Join(result.err, disposeErr),
			Stale:    state.active != nil,
		})
		return
	}
	if result.err != nil {
		if errors.Is(result.err, context.Canceled) {
			return
		}
		state.engine.sink.Emit(Event{
			Kind:     EventPreparationFailed,
			Revision: result.revision,
			Err:      result.err,
			Stale:    state.active != nil,
		})
		if state.active != nil {
			state.engine.sink.Emit(Event{
				Kind:     EventLastKnownGood,
				Revision: state.activeRevision,
				Err:      result.err,
				Stale:    true,
			})
		}
		return
	}
	state.engine.sink.Emit(Event{
		Kind:     EventCandidateReady,
		Revision: result.revision,
		Stale:    state.active != nil,
	})
	state.startReplacement(ctx, result)
}

func (state *engineState) startReplacement(
	ctx context.Context,
	result preparationResult,
) {
	if state.replaceStop != nil {
		state.replaceStop()
	}
	replaceCtx, cancel := context.WithCancel(ctx)
	state.replaceStop = cancel
	previous := state.active
	previousRevision := state.activeRevision
	if previous != nil {
		state.engine.sink.Emit(Event{
			Kind:     EventRestartRequested,
			Revision: result.revision,
			Stale:    true,
		})
	}
	go state.replace(
		replaceCtx,
		result.revision,
		result.candidate,
		previousRevision,
		previous,
	)
}

func (state *engineState) replace(
	ctx context.Context,
	revision uint64,
	candidate Candidate,
	previousRevision uint64,
	previous Process,
) {
	result := replacementResult{
		revision:         revision,
		previousRevision: previousRevision,
		candidate:        candidate,
	}
	if previous != nil {
		stopCtx, stopCancel := context.WithTimeout(
			context.WithoutCancel(ctx),
			state.engine.config.StopTimeout,
		)
		result.err = previous.Stop(stopCtx)
		stopCancel()
		result.previousStopped = true
		if errors.Is(result.err, ErrGracefulStopTimeout) {
			result.warning = result.err
			result.err = nil
		}
	}
	if result.err == nil {
		result.err = ctx.Err()
	}
	if result.err == nil {
		result.process, result.err = state.engine.launcher.Start(ctx, candidate)
	}
	if result.process != nil && ctx.Err() != nil {
		stopCtx, stopCancel := context.WithTimeout(
			context.WithoutCancel(ctx),
			state.engine.config.StopTimeout,
		)
		stopErr := result.process.Stop(stopCtx)
		stopCancel()
		result.err = errors.Join(ctx.Err(), stopErr)
		result.process = nil
	}
	select {
	case state.replacements <- result:
	case <-ctx.Done():
		var cleanupErr error
		if result.process != nil {
			stopCtx, stopCancel := context.WithTimeout(
				context.WithoutCancel(ctx),
				state.engine.config.StopTimeout,
			)
			cleanupErr = result.process.Stop(stopCtx)
			stopCancel()
		}
		cleanupErr = errors.Join(cleanupErr, candidate.Dispose())
		if cleanupErr != nil {
			return
		}
	}
}

func (state *engineState) finishReplacement(result replacementResult) {
	if state.replaceStop != nil {
		state.replaceStop()
		state.replaceStop = nil
	}
	if result.previousStopped && state.activeRevision == result.previousRevision {
		disposeErr := state.activeCandidate.Dispose()
		state.emitCleanupFailure(state.activeRevision, disposeErr)
		state.active = nil
		state.activeCandidate = Candidate{}
		state.activeRevision = 0
		state.activeWait = nil
	}
	if result.warning != nil {
		state.engine.sink.Emit(Event{
			Kind:     EventShutdownTimedOut,
			Revision: result.previousRevision,
			Err:      result.warning,
			Stale:    true,
		})
	}
	if result.revision != state.revision || state.debouncer.Pending() {
		state.discardReplacement(result)
		return
	}
	if result.err != nil {
		state.discardReplacement(result)
		if !errors.Is(result.err, context.Canceled) {
			state.engine.sink.Emit(Event{
				Kind:     EventPreparationFailed,
				Revision: result.revision,
				Err:      fmt.Errorf("replace application: %w", result.err),
				Stale:    state.active != nil,
			})
		}
		return
	}
	state.active = result.process
	state.activeCandidate = result.candidate
	state.activeRevision = result.revision
	state.activeWait = result.process.Wait()
	state.engine.sink.Emit(Event{
		Kind:     EventApplicationStarted,
		Revision: result.revision,
	})
}

func (state *engineState) discardReplacement(result replacementResult) {
	if result.process != nil {
		stopCtx, stopCancel := context.WithTimeout(
			context.Background(),
			state.engine.config.StopTimeout,
		)
		stopErr := result.process.Stop(stopCtx)
		stopCancel()
		result.err = errors.Join(result.err, stopErr)
	}
	result.err = errors.Join(result.err, result.candidate.Dispose())
	state.engine.sink.Emit(Event{
		Kind:     EventCandidateDiscarded,
		Revision: result.revision,
		Err:      result.err,
		Stale:    state.active != nil,
	})
}

func (state *engineState) finishApplication(err error) {
	revision := state.activeRevision
	err = errors.Join(err, state.activeCandidate.Dispose())
	state.active = nil
	state.activeCandidate = Candidate{}
	state.activeRevision = 0
	state.activeWait = nil
	state.engine.sink.Emit(Event{
		Kind:     EventApplicationExited,
		Revision: revision,
		Err:      err,
	})
}

func (state *engineState) resetDebounceTimer() {
	deadline, found := state.debouncer.Deadline()
	if !found {
		state.debounceC = nil
		return
	}
	delay := deadline.Sub(state.engine.clock.Now())
	delay = max(0, delay)
	if state.debounceTimer == nil {
		state.debounceTimer = state.engine.clock.NewTimer(delay)
	} else {
		stopAndDrainTimer(state.debounceTimer)
		state.debounceTimer.Reset(delay)
	}
	state.debounceC = state.debounceTimer.C()
}

func (state *engineState) cancelPreparations() {
	for revision, cancel := range state.cancels {
		cancel()
		delete(state.cancels, revision)
	}
}

func (state *engineState) cancelWork() {
	state.cancelPreparations()
	if state.replaceStop != nil {
		state.replaceStop()
		state.replaceStop = nil
	}
}

func (state *engineState) stopActive() error {
	if state.active == nil {
		return state.activeCandidate.Dispose()
	}
	stopCtx, stopCancel := context.WithTimeout(
		context.Background(),
		state.engine.config.StopTimeout,
	)
	stopErr := state.active.Stop(stopCtx)
	stopCancel()
	disposeErr := state.activeCandidate.Dispose()
	state.active = nil
	state.activeCandidate = Candidate{}
	state.activeRevision = 0
	state.activeWait = nil
	return errors.Join(stopErr, disposeErr)
}

func (state *engineState) stopDebounceTimer() {
	if state.debounceTimer == nil {
		return
	}
	stopAndDrainTimer(state.debounceTimer)
	state.debounceTimer = nil
	state.debounceC = nil
}

func (state *engineState) emitCleanupFailure(revision uint64, err error) {
	if err == nil {
		return
	}
	state.engine.sink.Emit(Event{
		Kind:     EventCleanupFailed,
		Revision: revision,
		Err:      err,
		Stale:    state.active != nil,
	})
}

func stopAndDrainTimer(timer Timer) {
	if timer.Stop() {
		return
	}
	select {
	case <-timer.C():
	default:
	}
}
