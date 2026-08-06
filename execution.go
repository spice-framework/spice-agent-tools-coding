package coding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spice-framework/spice-agent/tool"
)

type operationError struct {
	code    string
	problem string
}

func (failure operationError) Error() string { return failure.problem }

func invalidArguments(problem string) error {
	return operationError{code: "invalid_arguments", problem: problem}
}

func executionFailure(code, problem string) error {
	return operationError{code: code, problem: problem}
}

type boundedCause struct {
	message string
	unwrap  error
}

func (cause boundedCause) Error() string { return cause.message }

func (cause boundedCause) Unwrap() error {
	return cause.unwrap
}

func decodeArguments(arguments json.RawMessage, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return invalidArguments("tool arguments do not match the declared schema")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return invalidArguments("tool arguments contain trailing JSON values")
	}
	return nil
}

func validateCall(call tool.Call, expectedName string) error {
	if err := call.Validate(); err != nil {
		return invalidArguments("tool call is invalid")
	}
	if call.Name() != expectedName {
		return invalidArguments(fmt.Sprintf("tool call name must be %q", expectedName))
	}
	return nil
}

func reportProgress(
	ctx context.Context,
	reporter tool.Reporter,
	callID tool.CallID,
	message string,
	retry tool.RetryDisposition,
) error {
	if reporter == nil {
		return nil
	}
	progress, err := tool.NewProgress(callID, message)
	if err != nil {
		return infrastructureFailure(
			callID,
			tool.ExecutionDefinitive,
			retry,
			"tool progress could not be encoded",
		)
	}
	if err := reporter.Report(ctx, progress); err != nil {
		if ctx.Err() != nil {
			return cancellationFailure(callID, retry, ctx.Err())
		}
		return infrastructureFailure(
			callID,
			tool.ExecutionDefinitive,
			retry,
			"tool progress receiver rejected the operation",
		)
	}
	return nil
}

func successResult(callID tool.CallID, content any) (tool.Result, error) {
	result, err := newSuccessResult(callID, content)
	if err == nil {
		return result, nil
	}
	return tool.Result{}, err
}

func newSuccessResult(callID tool.CallID, content any) (tool.Result, error) {
	encoded, err := json.Marshal(content)
	if err != nil {
		return tool.Result{}, fmt.Errorf("marshal tool result: %w", err)
	}
	result, err := tool.NewResult(callID, encoded)
	if err != nil {
		return tool.Result{}, fmt.Errorf("construct tool result: %w", err)
	}
	return result, nil
}

func failureResult(callID tool.CallID, err error) tool.Result {
	failure := operationError{code: "operation_failed", problem: "tool operation failed"}
	if !errors.As(err, &failure) {
		failure = operationError{code: "operation_failed", problem: "tool operation failed"}
	}
	content, marshalErr := json.Marshal(struct {
		OK   bool   `json:"ok"`
		Code string `json:"code"`
	}{OK: false, Code: failure.code})
	if marshalErr != nil {
		return tool.Result{}
	}
	result, resultErr := tool.NewErrorResult(callID, content, failure.problem)
	if resultErr != nil {
		return tool.Result{}
	}
	return result
}

func contextFailure(ctx context.Context) error {
	if ctx == nil {
		return invalidArguments("tool context must not be nil")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func infrastructureFailure(
	callID tool.CallID,
	state tool.ExecutionState,
	retry tool.RetryDisposition,
	message string,
) *tool.ExecutionError {
	failure, err := tool.NewExecutionError(callID, state, retry, boundedCause{message: message})
	if err != nil {
		panic(fmt.Sprintf("construct coding-tool execution failure: %v", err))
	}
	return failure
}

func cancellationFailure(
	callID tool.CallID,
	retry tool.RetryDisposition,
	cause error,
) *tool.ExecutionError {
	message := "tool operation was cancelled"
	if errors.Is(cause, context.DeadlineExceeded) {
		message = "tool operation exceeded the caller deadline"
	}
	return contextInfrastructureFailure(callID, tool.ExecutionDefinitive, retry, message, cause)
}

func initialContextOutcome(
	ctx context.Context,
	callID tool.CallID,
	retry tool.RetryDisposition,
) (tool.Result, error, bool) {
	if err := contextFailure(ctx); err != nil {
		if ctx != nil && ctx.Err() != nil {
			return tool.Result{}, cancellationFailure(callID, retry, ctx.Err()), true
		}
		result, resultErr := modelFailure(callID, err)
		return result, resultErr, true
	}
	return tool.Result{}, nil, false
}

func contextInfrastructureFailure(
	callID tool.CallID,
	state tool.ExecutionState,
	retry tool.RetryDisposition,
	message string,
	cause error,
) *tool.ExecutionError {
	var canonical error
	switch {
	case errors.Is(cause, context.Canceled):
		canonical = context.Canceled
	case errors.Is(cause, context.DeadlineExceeded):
		canonical = context.DeadlineExceeded
	}
	failure, err := tool.NewExecutionError(
		callID,
		state,
		retry,
		boundedCause{message: message, unwrap: canonical},
	)
	if err != nil {
		panic(fmt.Sprintf("construct coding-tool cancellation failure: %v", err))
	}
	return failure
}

func modelFailure(callID tool.CallID, err error) (tool.Result, error) {
	return failureResult(callID, err), nil
}

func closeBestEffort(closer io.Closer) {
	if err := closer.Close(); err != nil {
		return
	}
}

func removeBestEffort(workspace *os.Root, name string) {
	if err := workspace.Remove(name); err != nil {
		return
	}
}
