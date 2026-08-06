// Package protocol defines the versioned framed stdio contract shared by
// Spice annotation tools and the compiler host.
package protocol

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spice-framework/spice/annotation/sdk"
)

const (
	// VersionV1Alpha2 is the typed-handler JSON-RPC contribution protocol.
	VersionV1Alpha2 = sdk.ProtocolV1Alpha2
	// MaximumMessageBytes bounds one decoded plugin message.
	MaximumMessageBytes = 16 << 20
)

// Request is one JSON-RPC 2.0 request.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is one JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *ResponseError  `json:"error,omitempty"`
}

// ResponseError is a deterministic JSON-RPC failure.
type ResponseError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// InitializeParams starts protocol and identity negotiation.
type InitializeParams struct {
	Protocol      sdk.ProtocolVersion `json:"protocol"`
	SpiceVersion  string              `json:"spice_version"`
	WorkspaceRoot string              `json:"workspace_root"`
	ToolPath      string              `json:"tool_path"`
}

// InitializeResult confirms the executable's build identity.
type InitializeResult struct {
	Protocol      sdk.ProtocolVersion `json:"protocol"`
	ToolPath      string              `json:"tool_path"`
	ModulePath    string              `json:"module_path"`
	ModuleVersion string              `json:"module_version,omitempty"`
}

// DescribeParams requests inspectable handler metadata.
type DescribeParams struct{}

// Handler describes one descriptor-to-implementation registration.
type Handler struct {
	Descriptor   sdk.Symbol `json:"descriptor"`
	Capabilities []string   `json:"capabilities"`
}

// DescribeResult reports every public descriptor package and handler owned by
// the process.
type DescribeResult struct {
	DescriptorPackages []string  `json:"descriptor_packages"`
	Handlers           []Handler `json:"handlers"`
}

// Declaration is the normalized SDK declaration payload.
type Declaration = sdk.Declaration

// Invocation is the normalized SDK invocation payload.
type Invocation = sdk.Invocation

// Argument is the normalized SDK invocation argument payload.
type Argument = sdk.InvocationArgument

// AnalyzeParams dispatches one invocation by its descriptor identity. The
// process owns the descriptor-to-typed-handler registration.
type AnalyzeParams struct {
	Descriptor sdk.Symbol `json:"descriptor"`
	Invocation Invocation `json:"invocation"`
}

// Contribution is a versioned generic IR input. Kind is an SDK-defined
// capability and Value is decoded into the corresponding typed contribution by
// the host before it can enter the compiler IR.
type Contribution struct {
	Kind  sdk.ContributionKind `json:"kind"`
	Value json.RawMessage      `json:"value"`
}

// Diagnostic is one plugin-owned source diagnostic.
type Diagnostic = sdk.HandlerDiagnostic

// AnalyzeResult returns validated IR inputs and diagnostics.
type AnalyzeResult struct {
	Contributions []Contribution `json:"contributions,omitempty"`
	Diagnostics   []Diagnostic   `json:"diagnostics,omitempty"`
}

// ShutdownParams requests graceful process termination.
type ShutdownParams struct{}

// ReadMessage reads one Content-Length framed JSON message.
func ReadMessage(reader *bufio.Reader, destination any) error {
	if reader == nil {
		return errors.New("annotation protocol reader must not be nil")
	}
	length, err := readHeaders(reader)
	if err != nil {
		return err
	}
	content := make([]byte, length)
	if _, err := io.ReadFull(reader, content); err != nil {
		return fmt.Errorf("read annotation protocol body: %w", err)
	}
	if err := json.Unmarshal(content, destination); err != nil {
		return fmt.Errorf("decode annotation protocol JSON: %w", err)
	}
	return nil
}

// WriteMessage writes one deterministic Content-Length framed JSON message.
func WriteMessage(writer io.Writer, value any) error {
	if writer == nil {
		return errors.New("annotation protocol writer must not be nil")
	}
	content, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode annotation protocol JSON: %w", err)
	}
	if len(content) > MaximumMessageBytes {
		return fmt.Errorf(
			"annotation protocol message has %d bytes; maximum is %d",
			len(content),
			MaximumMessageBytes,
		)
	}
	header := []byte(fmt.Sprintf(
		"Content-Length: %d\r\n\r\n",
		len(content),
	))
	if err := writeBytes(writer, header); err != nil {
		return fmt.Errorf("write annotation protocol header: %w", err)
	}
	if err := writeBytes(writer, content); err != nil {
		return fmt.Errorf("write annotation protocol body: %w", err)
	}
	return nil
}

func writeBytes(writer io.Writer, content []byte) error {
	for len(content) > 0 {
		written, err := writer.Write(content)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(content) {
			return io.ErrShortWrite
		}
		content = content[written:]
	}
	return nil
}

func readHeaders(reader *bufio.Reader) (int, error) {
	contentLength := -1
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return 0, fmt.Errorf("read annotation protocol header: %w", err)
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			break
		}
		name, value, found := strings.Cut(line, ":")
		if !found || !strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			return 0, fmt.Errorf(
				"annotation protocol contains unsupported header %q",
				line,
			)
		}
		if contentLength >= 0 {
			return 0, errors.New(
				"annotation protocol contains duplicate Content-Length header",
			)
		}
		contentLength, err = strconv.Atoi(strings.TrimSpace(value))
		if err != nil || contentLength < 0 ||
			contentLength > MaximumMessageBytes {
			return 0, fmt.Errorf(
				"annotation protocol Content-Length %q is invalid",
				strings.TrimSpace(value),
			)
		}
	}
	if contentLength < 0 {
		return 0, errors.New(
			"annotation protocol requires a Content-Length header",
		)
	}
	return contentLength, nil
}
