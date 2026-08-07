package coding

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spice-framework/spice-agent/process"
)

const (
	processStopGrace   = 500 * time.Millisecond
	processForceWait   = 5 * time.Second
	processRetryWait   = 10 * time.Millisecond
	maximumOwnedShells = 128
)

type processOutcome struct {
	result     process.Outcome
	hasResult  bool
	resultErr  error
	launchErr  error
	contextErr error
	controlErr error
	waitErr    error
	timedOut   bool
	started    bool
}

type ownedProcess struct {
	process  process.Process
	terminal error
}

type executionLease struct {
	shell    *shellTool
	reserved bool
}

func (shell *shellTool) beginExecution() (*executionLease, error) {
	shell.ownershipMu.Lock()
	defer shell.ownershipMu.Unlock()
	if shell.closing {
		return nil, executionFailure("shell_closing", "shell tool cleanup has begun")
	}
	if len(shell.owned)+shell.reservations >= maximumOwnedShells {
		return nil, executionFailure("process_limit", "shell tool process ownership limit has been reached")
	}
	shell.active++
	shell.reservations++
	return &executionLease{shell: shell, reserved: true}, nil
}

func (lease *executionLease) retain(value process.Process) *ownedProcess {
	record := &ownedProcess{process: value}
	lease.shell.ownershipMu.Lock()
	if lease.reserved {
		lease.shell.reservations--
		lease.reserved = false
	}
	lease.shell.owned = append(lease.shell.owned, record)
	lease.shell.ownershipMu.Unlock()
	return record
}

func (lease *executionLease) finish() {
	lease.shell.ownershipMu.Lock()
	if lease.reserved {
		lease.shell.reservations--
		lease.reserved = false
	}
	lease.shell.active--
	if lease.shell.closing && lease.shell.active == 0 {
		lease.shell.drainedOnce.Do(func() { close(lease.shell.drained) })
	}
	lease.shell.ownershipMu.Unlock()
}

func (shell *shellTool) releaseOwned(record *ownedProcess) {
	if record == nil {
		return
	}
	shell.ownershipMu.Lock()
	defer shell.ownershipMu.Unlock()
	for index, candidate := range shell.owned {
		if candidate == record {
			copy(shell.owned[index:], shell.owned[index+1:])
			shell.owned[len(shell.owned)-1] = nil
			shell.owned = shell.owned[:len(shell.owned)-1]
			return
		}
	}
}

func (shell *shellTool) waitOwned(ctx context.Context, record *ownedProcess) error {
	if record == nil {
		return nil
	}
	shell.ownershipMu.Lock()
	terminal := record.terminal
	shell.ownershipMu.Unlock()
	if terminal != nil {
		return terminal
	}
	err := process.NewFailure(process.OperationWait, record.process.Wait(ctx))
	if err == nil {
		shell.releaseOwned(record)
		return nil
	}
	if waitFailureIsTerminal(err) {
		shell.ownershipMu.Lock()
		if record.terminal == nil {
			record.terminal = err
		}
		terminal = record.terminal
		shell.ownershipMu.Unlock()
		return terminal
	}
	return err
}

func waitFailureIsTerminal(err error) (terminal bool) {
	classified, present := errors.AsType[interface {
		error
		Retryable() bool
	}](err)
	if !present {
		return false
	}
	terminal = true
	defer func() {
		if recover() != nil {
			terminal = true
		}
	}()
	return !classified.Retryable()
}

func (shell *shellTool) cleanup(ctx context.Context) error {
	if ctx == nil {
		return errors.New("shell cleanup context must not be nil")
	}
	shell.ownershipMu.Lock()
	shell.closing = true
	if shell.active == 0 {
		shell.drainedOnce.Do(func() { close(shell.drained) })
	}
	drained := shell.drained
	shell.ownershipMu.Unlock()

	select {
	case shell.cleanupToken <- struct{}{}:
		defer func() { <-shell.cleanupToken }()
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-drained:
	case <-ctx.Done():
		return ctx.Err()
	}

	shell.ownershipMu.Lock()
	owned := append([]*ownedProcess(nil), shell.owned...)
	shell.ownershipMu.Unlock()
	failures := make([]error, 0, len(owned))
	for _, record := range owned {
		if err := shell.retryWaitOwned(ctx, record); err != nil {
			failures = append(failures, err)
		}
		if ctx.Err() != nil {
			failures = append(failures, ctx.Err())
			break
		}
	}
	return errors.Join(failures...)
}

func (shell *shellTool) retryWaitOwned(ctx context.Context, record *ownedProcess) error {
	for {
		err := shell.waitOwned(ctx, record)
		if err == nil || waitFailureIsTerminal(err) {
			return err
		}
		timer := time.NewTimer(processRetryWait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return errors.Join(err, ctx.Err())
		}
	}
}

func (shell *shellTool) runProcess(
	ctx context.Context,
	spec process.Spec,
	lease *executionLease,
) processOutcome {
	processContext, cancel := context.WithTimeout(ctx, shell.config.CommandTimeout)
	defer cancel()
	if err := processContext.Err(); err != nil {
		return interruptedOutcome(ctx, err, false)
	}
	launched, launchErr := shell.launcher.Start(processContext, spec)
	if launched == nil {
		if launchErr == nil {
			launchErr = errors.New("launcher returned no process")
		}
		outcome := interruptedOutcome(ctx, processContext.Err(), false)
		outcome.launchErr = process.NewFailure(process.OperationLaunch, launchErr)
		return outcome
	}
	// Start transfers ownership whenever it returns a non-nil process, including
	// when it also returns an error. Retention therefore precedes every error
	// branch and every other call on the process.
	record := lease.retain(launched)
	if launchErr != nil {
		outcome := interruptedOutcome(ctx, processContext.Err(), true)
		outcome.launchErr = process.NewFailure(process.OperationLaunch, launchErr)
		shell.stopAndJoin(record, &outcome)
		return outcome
	}

	select {
	case <-launched.Done():
		return shell.observeAndJoin(record)
	case <-processContext.Done():
		outcome := interruptedOutcome(ctx, processContext.Err(), true)
		shell.stopAndJoin(record, &outcome)
		return outcome
	}
}

func interruptedOutcome(parent context.Context, processErr error, started bool) processOutcome {
	outcome := processOutcome{started: started}
	switch {
	case parent != nil && parent.Err() != nil:
		outcome.contextErr = parent.Err()
	case errors.Is(processErr, context.DeadlineExceeded):
		outcome.timedOut = true
	case processErr != nil:
		outcome.contextErr = processErr
	}
	return outcome
}

func (shell *shellTool) observeAndJoin(record *ownedProcess) processOutcome {
	outcome := processOutcome{started: true}
	result, err := record.process.Result()
	if err == nil {
		err = result.Validate()
	}
	if err != nil {
		outcome.resultErr = process.NewFailure(process.OperationResult, err)
	} else {
		outcome.result = result
		outcome.hasResult = true
	}
	waitContext, cancel := context.WithTimeout(context.Background(), processForceWait)
	defer cancel()
	outcome.waitErr = shell.waitOwned(waitContext, record)
	return outcome
}

func (shell *shellTool) stopAndJoin(record *ownedProcess, outcome *processOutcome) {
	stopContext, cancelStop := context.WithTimeout(context.Background(), processStopGrace)
	stopErr := process.NewFailure(process.OperationRequestStop, record.process.RequestStop(stopContext))
	cancelStop()

	timer := time.NewTimer(processStopGrace)
	select {
	case <-record.process.Done():
		timer.Stop()
	case <-timer.C:
		killContext, cancelKill := context.WithTimeout(context.Background(), processStopGrace)
		killErr := process.NewFailure(process.OperationForceKill, record.process.ForceKill(killContext))
		cancelKill()
		stopErr = errors.Join(stopErr, killErr)
	}
	outcome.controlErr = stopErr

	if processIsDone(record.process) {
		result, err := record.process.Result()
		if err == nil {
			err = result.Validate()
		}
		if err != nil {
			outcome.resultErr = process.NewFailure(process.OperationResult, err)
		} else {
			outcome.result = result
			outcome.hasResult = true
		}
	}
	waitContext, cancelWait := context.WithTimeout(context.Background(), processForceWait)
	outcome.waitErr = shell.waitOwned(waitContext, record)
	cancelWait()
}

func processIsDone(value process.Process) bool {
	select {
	case <-value.Done():
		return true
	default:
		return false
	}
}

func (shell *shellTool) ownedCount() int {
	shell.ownershipMu.Lock()
	defer shell.ownershipMu.Unlock()
	return len(shell.owned)
}

func formatProcessProblem(outcome processOutcome) string {
	if outcome.hasResult {
		switch outcome.result.Kind() {
		case process.OutcomeExited:
			code, _ := outcome.result.ExitCode()
			return fmt.Sprintf("command exited with status %d", code)
		case process.OutcomeSignaled:
			return "command was terminated by the platform"
		case process.OutcomeUnknown:
			return "command terminated without a portable exit status"
		}
	}
	return "command outcome is unavailable"
}
