// Package tool defines provider-neutral immutable executable tool contracts.
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	// MaximumPayloadBytes bounds one tool schema, call, or result JSON value.
	MaximumPayloadBytes = 1 << 20
	// MaximumProgressBytes bounds one progress message.
	MaximumProgressBytes = 4096
	maxMessageBytes      = MaximumProgressBytes
	maxIdentityBytes     = 128
)

// CallID identifies one tool operation.
type CallID string

// Capability declares a security-relevant effect. It is descriptive metadata,
// not a sandbox or permission boundary.
type Capability string

const (
	CapabilityFilesystemRead   Capability = "filesystem.read"
	CapabilityFilesystemWrite  Capability = "filesystem.write"
	CapabilityProcessExecute   Capability = "process.execute"
	CapabilityNetworkAccess    Capability = "network.access"
	CapabilitySecretsRead      Capability = "secrets.read"
	CapabilityEnvironmentRead  Capability = "environment.read"
	CapabilityEnvironmentWrite Capability = "environment.write"
)

// Definition is an immutable model-visible tool description. Capability order
// is declaration order and is significant.
type Definition struct {
	name         string
	description  string
	inputSchema  json.RawMessage
	capabilities []Capability
}

// NewDefinition validates and defensively copies a tool definition.
func NewDefinition(name, description string, inputSchema json.RawMessage, capabilities ...Capability) (Definition, error) {
	if err := validateIdentity("tool name", name); err != nil {
		return Definition{}, err
	}
	if description == "" || description != strings.TrimSpace(description) {
		return Definition{}, errors.New("tool description must be non-empty without surrounding whitespace")
	}
	if len(description) > maxMessageBytes {
		return Definition{}, fmt.Errorf("tool description exceeds %d bytes", maxMessageBytes)
	}
	if err := validateJSON("tool input schema", inputSchema); err != nil {
		return Definition{}, err
	}
	seen := make(map[Capability]struct{}, len(capabilities))
	for _, capability := range capabilities {
		if !validCapability(capability) {
			return Definition{}, fmt.Errorf("tool capability %q is unsupported", capability)
		}
		if _, duplicate := seen[capability]; duplicate {
			return Definition{}, fmt.Errorf("tool capability %q is duplicated", capability)
		}
		seen[capability] = struct{}{}
	}
	return Definition{
		name:         name,
		description:  description,
		inputSchema:  cloneJSON(inputSchema),
		capabilities: append([]Capability(nil), capabilities...),
	}, nil
}

// Validate rejects a zero or corrupted definition.
func (definition Definition) Validate() error {
	_, err := NewDefinition(definition.name, definition.description, definition.inputSchema, definition.capabilities...)
	return err
}

// Name returns the canonical Spice bean and model-visible name.
func (definition Definition) Name() string { return definition.name }

// Description returns human-facing model guidance.
func (definition Definition) Description() string { return definition.description }

// InputSchema returns a defensive copy.
func (definition Definition) InputSchema() json.RawMessage { return cloneJSON(definition.inputSchema) }

// Capabilities returns an ordered defensive copy.
func (definition Definition) Capabilities() []Capability {
	return append([]Capability(nil), definition.capabilities...)
}

// SizeBytes returns deterministic request-budget accounting.
func (definition Definition) SizeBytes() int {
	total := len(definition.name) + len(definition.description) + len(definition.inputSchema)
	for _, capability := range definition.capabilities {
		total += len(capability)
	}
	return total
}

// Clone returns a deep defensive copy.
func (definition Definition) Clone() Definition {
	definition.inputSchema = cloneJSON(definition.inputSchema)
	definition.capabilities = append([]Capability(nil), definition.capabilities...)
	return definition
}

// Call is one immutable invocation requested by a model.
type Call struct {
	id        CallID
	name      string
	arguments json.RawMessage
}

// NewCall validates and copies one call.
func NewCall(id CallID, name string, arguments json.RawMessage) (Call, error) {
	if err := validateIdentity("tool call ID", string(id)); err != nil {
		return Call{}, err
	}
	if err := validateIdentity("tool name", name); err != nil {
		return Call{}, err
	}
	if err := validateJSON("tool arguments", arguments); err != nil {
		return Call{}, err
	}
	return Call{id: id, name: name, arguments: cloneJSON(arguments)}, nil
}

// Validate rejects a zero or corrupted call.
func (call Call) Validate() error {
	_, err := NewCall(call.id, call.name, call.arguments)
	return err
}

// ID returns the correlation identity.
func (call Call) ID() CallID { return call.id }

// Name returns the canonical tool name.
func (call Call) Name() string { return call.name }

// Arguments returns a defensive JSON copy.
func (call Call) Arguments() json.RawMessage { return cloneJSON(call.arguments) }

// Clone returns a deep defensive copy.
func (call Call) Clone() Call {
	call.arguments = cloneJSON(call.arguments)
	return call
}

// Result is one immutable terminal tool outcome. A problem is model-visible
// terminal data, not a Go transport error.
type Result struct {
	callID  CallID
	content json.RawMessage
	problem string
}

// NewResult constructs a successful terminal result.
func NewResult(callID CallID, content json.RawMessage) (Result, error) {
	return newResult(callID, content, "")
}

// NewErrorResult constructs a normalized error result.
func NewErrorResult(callID CallID, content json.RawMessage, problem string) (Result, error) {
	if problem == "" || problem != strings.TrimSpace(problem) {
		return Result{}, errors.New("tool result problem must be non-empty without surrounding whitespace")
	}
	if len(problem) > maxMessageBytes {
		return Result{}, fmt.Errorf("tool result problem exceeds %d bytes", maxMessageBytes)
	}
	return newResult(callID, content, problem)
}

func newResult(callID CallID, content json.RawMessage, problem string) (Result, error) {
	if err := validateIdentity("tool result call ID", string(callID)); err != nil {
		return Result{}, err
	}
	if err := validateJSON("tool result content", content); err != nil {
		return Result{}, err
	}
	return Result{callID: callID, content: cloneJSON(content), problem: problem}, nil
}

// Validate rejects a zero or corrupted result.
func (result Result) Validate() error {
	if result.problem == "" {
		_, err := NewResult(result.callID, result.content)
		return err
	}
	_, err := NewErrorResult(result.callID, result.content, result.problem)
	return err
}

// CallID returns the active call identity.
func (result Result) CallID() CallID { return result.callID }

// Content returns a defensive JSON copy.
func (result Result) Content() json.RawMessage { return cloneJSON(result.content) }

// Problem returns normalized error text and whether the result is an error.
func (result Result) Problem() (string, bool) { return result.problem, result.problem != "" }

// Clone returns a deep defensive copy.
func (result Result) Clone() Result {
	result.content = cloneJSON(result.content)
	return result
}

// Progress is immutable bounded observable progress for a running call.
type Progress struct {
	callID  CallID
	message string
}

// NewProgress validates one progress observation.
func NewProgress(callID CallID, message string) (Progress, error) {
	if err := validateIdentity("tool progress call ID", string(callID)); err != nil {
		return Progress{}, err
	}
	if message == "" || message != strings.TrimSpace(message) {
		return Progress{}, errors.New("tool progress message must be non-empty without surrounding whitespace")
	}
	if len(message) > maxMessageBytes {
		return Progress{}, fmt.Errorf("tool progress message exceeds %d bytes", maxMessageBytes)
	}
	return Progress{callID: callID, message: message}, nil
}

// CallID returns the active call identity.
func (progress Progress) CallID() CallID { return progress.callID }

// Message returns the progress text.
func (progress Progress) Message() string { return progress.message }

// Validate rejects a zero or corrupted progress value.
func (progress Progress) Validate() error {
	_, err := NewProgress(progress.callID, progress.message)
	return err
}

// Reporter receives progress synchronously.
type Reporter interface {
	Report(context.Context, Progress) error
}

// Tool is one constructor-injected executable contribution. Implementations
// are singleton beans by default and must be safe for concurrent Execute calls.
// Context cancellation is cooperative; Spice cannot forcibly stop trusted
// in-process code that ignores the context. Execute must not retain or invoke
// Reporter after it returns.
type Tool interface {
	Definition() Definition
	Execute(context.Context, Call, Reporter) Result
}

func validCapability(capability Capability) bool {
	switch capability {
	case CapabilityFilesystemRead, CapabilityFilesystemWrite,
		CapabilityProcessExecute, CapabilityNetworkAccess, CapabilitySecretsRead,
		CapabilityEnvironmentRead, CapabilityEnvironmentWrite:
		return true
	default:
		return false
	}
}

func validateIdentity(label, value string) error {
	if value == "" || value != strings.TrimSpace(value) {
		return fmt.Errorf("%s must be non-empty without surrounding whitespace", label)
	}
	if len(value) > maxIdentityBytes {
		return fmt.Errorf("%s exceeds %d bytes", label, maxIdentityBytes)
	}
	return nil
}

func validateJSON(label string, value json.RawMessage) error {
	if len(value) == 0 || !json.Valid(value) {
		return fmt.Errorf("%s must be valid JSON", label)
	}
	if len(value) > MaximumPayloadBytes {
		return fmt.Errorf("%s exceeds %d bytes", label, MaximumPayloadBytes)
	}
	return nil
}

func cloneJSON(value json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), value...)
}
