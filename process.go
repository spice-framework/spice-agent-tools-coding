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
	waitErr   error
	cancelled bool
	timedOut  bool
	stopErr   error
}

func runProcess(ctx context.Context, command *exec.Cmd, timeout time.Duration) processOutcome {
	processContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tree, err := prepareProcessTree(command)
	if err != nil {
		return processOutcome{waitErr: err}
	}
	if err := command.Start(); err != nil {
		return processOutcome{waitErr: errors.Join(err, tree.close())}
	}
	if err := tree.attach(command.Process); err != nil {
		forceErr := errors.Join(tree.forceStop(), command.Process.Kill())
		waitErr := boundedWait(command)
		return processOutcome{waitErr: waitErr, stopErr: errors.Join(err, forceErr, tree.close())}
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	select {
	case err := <-waited:
		return processOutcome{waitErr: err, stopErr: tree.close()}
	case <-processContext.Done():
		outcome := processOutcome{
			cancelled: errors.Is(processContext.Err(), context.Canceled),
			timedOut:  errors.Is(processContext.Err(), context.DeadlineExceeded),
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
		return errors.New("process wait did not complete after force termination")
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
		return errors.New("process wait did not complete after force termination"),
			errors.Join(stopErr, forceErr, errors.New("process-tree termination could not be confirmed"))
	}
}
