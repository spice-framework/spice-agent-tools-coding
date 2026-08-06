package coding

import (
	"context"
	"errors"
	"os/exec"
	"time"
)

const (
	processStopGrace = 500 * time.Millisecond
	processForceWait = 5 * time.Second
)

type processOutcome struct {
	waitErr    error
	contextErr error
	timedOut   bool
	stopErr    error
	started    bool
}

func runProcess(ctx context.Context, command *exec.Cmd, timeout time.Duration) processOutcome {
	return runProcessWithHooks(ctx, command, timeout, processHooks{})
}

type processHooks struct {
	beforeStart            func()
	afterStartBeforeAttach func()
}

func runProcessWithHooks(
	ctx context.Context,
	command *exec.Cmd,
	timeout time.Duration,
	hooks processHooks,
) processOutcome {
	processContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if ctx.Err() != nil {
		return processOutcome{contextErr: ctx.Err()}
	}
	tree, err := prepareProcessTree(command)
	if err != nil {
		if ctx.Err() != nil {
			return processOutcome{contextErr: ctx.Err()}
		}
		return processOutcome{waitErr: err}
	}
	if hooks.beforeStart != nil {
		hooks.beforeStart()
	}
	if ctx.Err() != nil {
		return processOutcome{contextErr: ctx.Err(), waitErr: tree.close()}
	}
	if processContext.Err() != nil {
		return processOutcome{timedOut: true, waitErr: tree.close()}
	}
	if err := command.Start(); err != nil {
		closeErr := tree.close()
		if ctx.Err() != nil {
			return processOutcome{contextErr: ctx.Err(), waitErr: closeErr}
		}
		return processOutcome{waitErr: errors.Join(err, closeErr)}
	}
	if hooks.afterStartBeforeAttach != nil {
		hooks.afterStartBeforeAttach()
	}
	if err := tree.attach(command.Process); err != nil {
		forceErr := errors.Join(tree.forceStop(), command.Process.Kill())
		waitErr := boundedWait(command)
		outcome := processOutcome{
			waitErr: waitErr, stopErr: errors.Join(err, forceErr, tree.close()), started: true,
		}
		if ctx.Err() != nil {
			outcome.contextErr = ctx.Err()
		}
		return outcome
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	select {
	case err := <-waited:
		return processOutcome{waitErr: err, stopErr: tree.close(), started: true}
	case <-processContext.Done():
		outcome := processOutcome{started: true}
		if ctx.Err() != nil {
			outcome.contextErr = ctx.Err()
		} else {
			outcome.timedOut = errors.Is(processContext.Err(), context.DeadlineExceeded)
		}
		outcome.waitErr, outcome.stopErr = stopAndWait(tree, waited)
		outcome.stopErr = errors.Join(outcome.stopErr, tree.close())
		return outcome
	}
}

func boundedWait(command *exec.Cmd) error {
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	timer := time.NewTimer(processForceWait)
	defer timer.Stop()
	select {
	case err := <-waited:
		return err
	case <-timer.C:
		return errors.New("command wait did not complete after forced cleanup")
	}
}

func stopAndWait(tree *processTree, waited <-chan error) (error, error) {
	stopErr := tree.requestStop()
	timer := time.NewTimer(processStopGrace)
	defer timer.Stop()
	select {
	case waitErr := <-waited:
		return waitErr, stopErr
	case <-timer.C:
	}
	forceErr := tree.forceStop()
	forceTimer := time.NewTimer(processForceWait)
	defer forceTimer.Stop()
	select {
	case waitErr := <-waited:
		return waitErr, errors.Join(stopErr, forceErr)
	case <-forceTimer.C:
		return errors.New("command wait did not complete after forced cleanup"),
			errors.Join(stopErr, forceErr, errors.New("managed launcher cleanup did not complete"))
	}
}
