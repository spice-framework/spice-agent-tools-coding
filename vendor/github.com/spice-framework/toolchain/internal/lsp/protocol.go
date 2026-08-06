package lsp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

const (
	maximumHeaderLine  = 8 << 10
	maximumHeaderCount = 64
	maximumMessageSize = 16 << 20
)

const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
)

type rpcClientError struct {
	id      json.RawMessage
	code    int
	message string
}

func (clientError *rpcClientError) Error() string {
	return clientError.message
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (message rpcMessage) request() bool {
	return len(message.ID) != 0 && !bytes.Equal(message.ID, []byte("null"))
}

type rpcReader struct {
	reader *bufio.Reader
}

func newRPCReader(reader io.Reader) *rpcReader {
	return &rpcReader{
		reader: bufio.NewReaderSize(reader, maximumHeaderLine),
	}
}

func (reader *rpcReader) read() (rpcMessage, error) {
	length, err := reader.readHeaders()
	if err != nil {
		return rpcMessage{}, err
	}
	content := make([]byte, length)
	if _, err := io.ReadFull(reader.reader, content); err != nil {
		return rpcMessage{}, fmt.Errorf("read LSP message body: %w", err)
	}
	if !json.Valid(content) {
		return rpcMessage{}, &rpcClientError{
			code:    rpcParseError,
			message: "invalid JSON-RPC message",
		}
	}
	var message rpcMessage
	if err := json.Unmarshal(content, &message); err != nil {
		return rpcMessage{}, &rpcClientError{
			code:    rpcInvalidRequest,
			message: "JSON-RPC message must be an object",
		}
	}
	if message.JSONRPC != "2.0" {
		return rpcMessage{}, invalidRPCRequest(
			message.ID,
			fmt.Sprintf(
				"unsupported JSON-RPC version %q",
				message.JSONRPC,
			),
		)
	}
	if message.Method == "" {
		return rpcMessage{}, invalidRPCRequest(
			message.ID,
			"LSP client message has no method",
		)
	}
	return message, nil
}

func invalidRPCRequest(
	id json.RawMessage,
	message string,
) *rpcClientError {
	return &rpcClientError{
		id:      bytes.Clone(id),
		code:    rpcInvalidRequest,
		message: message,
	}
}

func (reader *rpcReader) readHeaders() (int, error) {
	length := -1
	for count := 0; ; count++ {
		if count >= maximumHeaderCount {
			return 0, errors.New("LSP message has too many headers")
		}
		line, prefix, err := reader.reader.ReadLine()
		if err != nil {
			return 0, err
		}
		if prefix {
			return 0, errors.New("LSP header line exceeds limit")
		}
		if len(line) == 0 {
			break
		}
		name, value, found := bytes.Cut(line, []byte(":"))
		if !found {
			return 0, fmt.Errorf(
				"invalid LSP header %q",
				boundedHeader(line),
			)
		}
		if !strings.EqualFold(
			strings.TrimSpace(string(name)),
			"Content-Length",
		) {
			continue
		}
		if length >= 0 {
			return 0, errors.New(
				"LSP message has duplicate Content-Length headers",
			)
		}
		parsed, err := parseContentLength(value)
		if err != nil {
			return 0, err
		}
		length = parsed
	}
	if length < 0 {
		return 0, errors.New("LSP message is missing Content-Length")
	}
	return length, nil
}

func parseContentLength(value []byte) (int, error) {
	parsed, err := strconv.Atoi(strings.TrimSpace(string(value)))
	if err != nil || parsed <= 0 || parsed > maximumMessageSize {
		return 0, fmt.Errorf(
			"invalid LSP Content-Length %q",
			boundedHeader(value),
		)
	}
	return parsed, nil
}

func boundedHeader(value []byte) string {
	const maximum = 128
	if len(value) <= maximum {
		return string(value)
	}
	return string(value[:maximum]) + "..."
}

type rpcWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func newRPCWriter(writer io.Writer) *rpcWriter {
	return &rpcWriter{writer: writer}
}

func (writer *rpcWriter) response(id json.RawMessage, result any) error {
	return writer.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(bytes.Clone(id)),
		"result":  result,
	})
}

func (writer *rpcWriter) failure(
	id json.RawMessage,
	code int,
	message string,
) error {
	return writer.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(bytes.Clone(id)),
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

func (writer *rpcWriter) notification(method string, params any) error {
	return writer.write(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

func (writer *rpcWriter) write(message any) error {
	content, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode LSP message: %w", err)
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if _, err := fmt.Fprintf(
		writer.writer,
		"Content-Length: %d\r\n\r\n",
		len(content),
	); err != nil {
		return fmt.Errorf("write LSP message header: %w", err)
	}
	if _, err := writer.writer.Write(content); err != nil {
		return fmt.Errorf("write LSP message body: %w", err)
	}
	return nil
}
