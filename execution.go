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

func reportProgress(ctx context.Context, reporter tool.Reporter, callID tool.CallID, message string) error {
	if reporter == nil {
		return nil
	}
	progress, err := tool.NewProgress(callID, message)
	if err != nil {
		return executionFailure("internal_contract", "tool progress could not be encoded")
	}
	if err := reporter.Report(ctx, progress); err != nil {
		if ctx.Err() != nil {
			return executionFailure("cancelled", "tool operation was cancelled")
		}
		return executionFailure("progress_rejected", "tool progress receiver rejected the operation")
	}
	return nil
}

func successResult(callID tool.CallID, content any) tool.Result {
	result, err := newSuccessResult(callID, content)
	if err == nil {
		return result
	}
	return failureResult(callID, executionFailure("result_too_large", "tool result exceeds the supported payload limit"))
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
		return executionFailure("cancelled", "tool operation was cancelled")
	}
	return nil
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
