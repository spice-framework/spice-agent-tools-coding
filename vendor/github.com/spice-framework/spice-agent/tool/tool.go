package tool

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	// MaximumPayloadBytes bounds one tool schema, call, or result JSON value.
	MaximumPayloadBytes = 1 << 20
	// MaximumProgressBytes bounds one progress message.
	MaximumProgressBytes = 4096
	// MaximumExecutionErrorBytes bounds one tool execution failure message.
	MaximumExecutionErrorBytes = 4096
	maxMessageBytes            = MaximumProgressBytes
	maxIdentityBytes           = 128
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

// Effect classifies whether a successful invocation can mutate external state.
// It is mandatory policy metadata, not inferred from capabilities or arguments.
type Effect string

const (
	EffectReadOnly Effect = "read_only"
	EffectMutating Effect = "mutating"
)

// ReplaySafety declares whether an invocation may be deliberately executed
// again after a definitive infrastructure failure. It never authorizes retry
// after an uncertain mutation outcome.
type ReplaySafety string

const (
	ReplaySafe       ReplaySafety = "safe"
	ReplayIdempotent ReplaySafety = "idempotent"
	ReplayUnsafe     ReplaySafety = "unsafe"
)

// Definition is an immutable model-visible tool description. Capabilities are
// an unordered set exposed in canonical lexical order.
type Definition struct {
	name         string
	description  string
	inputSchema  json.RawMessage
	effect       Effect
	replaySafety ReplaySafety
	capabilities []Capability
}

// NewDefinition validates and defensively copies a tool definition.
func NewDefinition(
	name,
	description string,
	inputSchema json.RawMessage,
	effect Effect,
	replaySafety ReplaySafety,
	capabilities ...Capability,
) (Definition, error) {
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
	if !validEffect(effect) {
		return Definition{}, fmt.Errorf("tool effect %q is unsupported", effect)
	}
	if !validReplaySafety(replaySafety) {
		return Definition{}, fmt.Errorf("tool replay safety %q is unsupported", replaySafety)
	}
	if effect == EffectReadOnly && replaySafety != ReplaySafe {
		return Definition{}, errors.New("read-only tools must declare safe replay")
	}
	if effect == EffectMutating && replaySafety == ReplaySafe {
		return Definition{}, errors.New("mutating tools must declare idempotent or unsafe replay")
	}
	seen := make(map[Capability]struct{}, len(capabilities))
	for _, capability := range capabilities {
		if !validCapability(capability) {
			return Definition{}, fmt.Errorf("tool capability %q is unsupported", capability)
		}
		if _, duplicate := seen[capability]; duplicate {
			return Definition{}, fmt.Errorf("tool capability %q is duplicated", capability)
		}
		if effect == EffectReadOnly && mutationCapable(capability) {
			return Definition{}, fmt.Errorf(
				"read-only tool cannot declare mutation-capable capability %q",
				capability,
			)
		}
		seen[capability] = struct{}{}
	}
	normalizedCapabilities := append([]Capability(nil), capabilities...)
	slices.Sort(normalizedCapabilities)
	return Definition{
		name:         name,
		description:  description,
		inputSchema:  cloneJSON(inputSchema),
		effect:       effect,
		replaySafety: replaySafety,
		capabilities: normalizedCapabilities,
	}, nil
}

// Validate rejects a zero or corrupted definition.
func (definition Definition) Validate() error {
	_, err := NewDefinition(
		definition.name,
		definition.description,
		definition.inputSchema,
		definition.effect,
		definition.replaySafety,
		definition.capabilities...,
	)
	return err
}

// Name returns the canonical Spice bean and model-visible name.
func (definition Definition) Name() string { return definition.name }

// Description returns human-facing model guidance.
func (definition Definition) Description() string { return definition.description }

// InputSchema returns a defensive copy.
func (definition Definition) InputSchema() json.RawMessage { return cloneJSON(definition.inputSchema) }

// Effect returns the declared external-state effect.
func (definition Definition) Effect() Effect { return definition.effect }

// ReplaySafety returns the declared deliberate replay contract.
func (definition Definition) ReplaySafety() ReplaySafety { return definition.replaySafety }

// Capabilities returns the unordered set in canonical lexical order.
func (definition Definition) Capabilities() []Capability {
	return append([]Capability(nil), definition.capabilities...)
}

// Fingerprint returns a deterministic hexadecimal SHA-256 identity for the
// complete model-visible and security-relevant contract. Capability order is
// normalized because it does not change the contract's meaning.
func (definition Definition) Fingerprint() string {
	hash := sha256.New()
	writeFingerprintField(hash, []byte(definition.name))
	writeFingerprintField(hash, []byte(definition.description))
	writeFingerprintField(hash, definition.inputSchema)
	writeFingerprintField(hash, []byte(definition.effect))
	writeFingerprintField(hash, []byte(definition.replaySafety))
	capabilities := append([]Capability(nil), definition.capabilities...)
	slices.Sort(capabilities)
	for _, capability := range capabilities {
		writeFingerprintField(hash, []byte(capability))
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

type fingerprintWriter interface {
	Write([]byte) (int, error)
}

func writeFingerprintField(destination fingerprintWriter, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = destination.Write(size[:]) // hash.Hash writes cannot fail.
	_, _ = destination.Write(value)   // hash.Hash writes cannot fail.
}

// SizeBytes returns deterministic request-budget accounting.
func (definition Definition) SizeBytes() int {
	total := len(definition.name) + len(definition.description) + len(definition.inputSchema) +
		len(definition.effect) + len(definition.replaySafety)
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

// IsZero reports whether no terminal result was returned. It is used to reject
// ambiguous implementations that return both output and an execution failure.
func (result Result) IsZero() bool {
	return result.callID == "" && result.content == nil && result.problem == ""
}

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

// ExecutionState states whether the tool host knows that an attempted
// invocation did not commit an external mutation.
type ExecutionState string

const (
	ExecutionDefinitive ExecutionState = "definitive"
	ExecutionUncertain  ExecutionState = "uncertain"
)

// RetryDisposition states whether policy may deliberately invoke the call
// again. Permission to retry is still bounded by Definition.ReplaySafety.
type RetryDisposition string

const (
	RetryNever   RetryDisposition = "never"
	RetryAllowed RetryDisposition = "allowed"
)

// ExecutionError is a bounded correlated infrastructure failure. It is never
// model-visible tool output. Unwrap preserves cancellation and deadline checks.
type ExecutionError struct {
	callID CallID
	state  ExecutionState
	retry  RetryDisposition
	cause  error
}

// NewExecutionError constructs a validated infrastructure failure.
func NewExecutionError(
	callID CallID,
	state ExecutionState,
	retry RetryDisposition,
	cause error,
) (*ExecutionError, error) {
	result := &ExecutionError{callID: callID, state: state, retry: retry, cause: cause}
	if err := result.Validate(); err != nil {
		return nil, err
	}
	return result, nil
}

// Error returns the bounded underlying failure text.
func (failure *ExecutionError) Error() string {
	if failure == nil || failure.cause == nil {
		return "tool execution failure is unavailable"
	}
	return failure.cause.Error()
}

// Unwrap preserves errors.Is and errors.As for cancellation and typed causes.
func (failure *ExecutionError) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.cause
}

// CallID returns the invocation correlation identity.
func (failure *ExecutionError) CallID() CallID {
	if failure == nil {
		return ""
	}
	return failure.callID
}

// State returns whether the execution outcome is definitive or uncertain.
func (failure *ExecutionError) State() ExecutionState {
	if failure == nil {
		return ""
	}
	return failure.state
}

// RetryDisposition returns the validated retry advice.
func (failure *ExecutionError) RetryDisposition() RetryDisposition {
	if failure == nil {
		return ""
	}
	return failure.retry
}

// Validate rejects uncorrelated, unbounded, or unsafe outcome combinations.
func (failure *ExecutionError) Validate() error {
	if failure == nil {
		return errors.New("tool execution error is nil")
	}
	if err := validateIdentity("tool execution call ID", string(failure.callID)); err != nil {
		return err
	}
	if failure.state != ExecutionDefinitive && failure.state != ExecutionUncertain {
		return fmt.Errorf("tool execution state %q is unsupported", failure.state)
	}
	if failure.retry != RetryNever && failure.retry != RetryAllowed {
		return fmt.Errorf("tool retry disposition %q is unsupported", failure.retry)
	}
	if failure.state == ExecutionUncertain && failure.retry != RetryNever {
		return errors.New("uncertain execution outcomes must never permit automatic retry")
	}
	if failure.cause == nil {
		return errors.New("tool execution error requires a cause")
	}
	message := failure.cause.Error()
	if message == "" || message != strings.TrimSpace(message) {
		return errors.New("tool execution error cause must be non-empty without surrounding whitespace")
	}
	if len(message) > MaximumExecutionErrorBytes {
		return fmt.Errorf("tool execution error cause exceeds %d bytes", MaximumExecutionErrorBytes)
	}
	if _, nested := errors.AsType[*ExecutionError](failure.cause); nested {
		return errors.New("tool execution errors must not be nested")
	}
	return nil
}

// Tool is one constructor-injected executable contribution. Implementations
// are singleton beans by default and must be safe for concurrent Execute calls.
// Context cancellation is cooperative; Spice cannot forcibly stop trusted
// in-process code that ignores the context. Execute must not retain or invoke
// Reporter after it returns. A model-visible tool problem is a Result with a
// problem and a nil error. Infrastructure failure is a zero Result with exactly
// one direct, correlated *ExecutionError; wrappers, joins, and returning both
// result and error are invalid.
type Tool interface {
	Definition() Definition
	Execute(context.Context, Call, Reporter) (Result, error)
}

func validEffect(effect Effect) bool {
	return effect == EffectReadOnly || effect == EffectMutating
}

func validReplaySafety(replaySafety ReplaySafety) bool {
	switch replaySafety {
	case ReplaySafe, ReplayIdempotent, ReplayUnsafe:
		return true
	default:
		return false
	}
}

func mutationCapable(capability Capability) bool {
	switch capability {
	case CapabilityFilesystemWrite, CapabilityProcessExecute,
		CapabilityNetworkAccess, CapabilityEnvironmentWrite:
		return true
	default:
		return false
	}
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
	if !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n\t") {
		return fmt.Errorf("%s contains invalid text", label)
	}
	if len(value) > maxIdentityBytes {
		return fmt.Errorf("%s exceeds %d bytes", label, maxIdentityBytes)
	}
	return nil
}

func validateJSON(label string, value json.RawMessage) error {
	if len(value) == 0 || !utf8.Valid(value) || !json.Valid(value) {
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
