package protocol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	errorInvalidRequest = -32600
	errorMethodNotFound = -32601
	errorInvalidParams  = -32602
	errorInternal       = -32603
)

// Tool implements the public annotation process protocol.
type Tool interface {
	Initialize(context.Context, InitializeParams) (InitializeResult, error)
	Describe(context.Context, DescribeParams) (DescribeResult, error)
	Analyze(context.Context, AnalyzeParams) (AnalyzeResult, error)
	Shutdown(context.Context, ShutdownParams) error
}

// Serve runs a synchronous deterministic protocol loop until shutdown, EOF,
// cancellation, or a framing failure. It never closes caller-owned streams.
func Serve(
	ctx context.Context,
	reader io.Reader,
	writer io.Writer,
	tool Tool,
) error {
	if ctx == nil {
		return errors.New("annotation protocol server context must not be nil")
	}
	if reader == nil || writer == nil || tool == nil {
		return errors.New(
			"annotation protocol server requires reader, writer, and tool",
		)
	}
	buffered := bufio.NewReader(reader)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var request Request
		if err := ReadMessage(buffered, &request); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		response, shutdown := dispatch(ctx, tool, request)
		if err := WriteMessage(writer, response); err != nil {
			return err
		}
		if shutdown {
			return nil
		}
	}
}

func dispatch(
	ctx context.Context,
	tool Tool,
	request Request,
) (response Response, shutdown bool) {
	response = Response{JSONRPC: "2.0", ID: request.ID}
	if request.JSONRPC != "2.0" || request.ID == 0 {
		response.Error = rpcError(
			errorInvalidRequest,
			"request must use JSON-RPC 2.0 with a positive ID",
		)
		return response, false
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			response.Result = nil
			response.Error = rpcError(
				errorInternal,
				fmt.Sprintf("annotation tool panic: %v", recovered),
			)
			shutdown = false
		}
	}()
	switch request.Method {
	case "initialize":
		var params InitializeParams
		if err := decodeParams(request.Params, &params); err != nil {
			response.Error = invalidParams(err)
			return response, false
		}
		result, err := tool.Initialize(ctx, params)
		response.Result, response.Error = encodeResult(result, err)
	case "describe":
		var params DescribeParams
		if err := decodeParams(request.Params, &params); err != nil {
			response.Error = invalidParams(err)
			return response, false
		}
		result, err := tool.Describe(ctx, params)
		response.Result, response.Error = encodeResult(result, err)
	case "analyze":
		var params AnalyzeParams
		if err := decodeParams(request.Params, &params); err != nil {
			response.Error = invalidParams(err)
			return response, false
		}
		result, err := tool.Analyze(ctx, params)
		response.Result, response.Error = encodeResult(result, err)
	case "shutdown":
		var params ShutdownParams
		if err := decodeParams(request.Params, &params); err != nil {
			response.Error = invalidParams(err)
			return response, false
		}
		err := tool.Shutdown(ctx, params)
		response.Result, response.Error = encodeResult(struct{}{}, err)
		shutdown = err == nil
	default:
		response.Error = rpcError(
			errorMethodNotFound,
			fmt.Sprintf("method %q was not found", request.Method),
		)
	}
	return response, shutdown
}

func decodeParams(content json.RawMessage, destination any) error {
	if len(content) == 0 {
		content = json.RawMessage("{}")
	}
	if err := json.Unmarshal(content, destination); err != nil {
		return fmt.Errorf("decode parameters: %w", err)
	}
	return nil
}

func encodeResult(value any, err error) (json.RawMessage, *ResponseError) {
	if err != nil {
		return nil, rpcError(errorInternal, err.Error())
	}
	content, marshalErr := json.Marshal(value)
	if marshalErr != nil {
		return nil, rpcError(
			errorInternal,
			fmt.Sprintf("encode result: %v", marshalErr),
		)
	}
	return content, nil
}

func invalidParams(err error) *ResponseError {
	return rpcError(errorInvalidParams, err.Error())
}

func rpcError(code int, message string) *ResponseError {
	return &ResponseError{Code: code, Message: message}
}
