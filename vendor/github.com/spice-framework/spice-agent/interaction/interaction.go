package interaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const MaximumPayloadBytes = 512 << 10

// Scope identifies the immutable run authority that owns an interaction.
// Brokers use it to route pending requests without depending on agent internals.
type Scope struct {
	runID string
}

// NewScope constructs a validated interaction scope.
func NewScope(runID string) (Scope, error) {
	if err := token("interaction run ID", runID); err != nil {
		return Scope{}, err
	}
	return Scope{runID: runID}, nil
}

// Validate rejects a zero or malformed scope.
func (scope Scope) Validate() error {
	_, err := NewScope(scope.runID)
	return err
}

// RunID returns the stable run that owns the interaction.
func (scope Scope) RunID() string { return scope.runID }

// ID identifies one interaction lifecycle.
type ID string

// Request asks an injected broker for typed user input.
type Request struct {
	id     ID
	kind   string
	prompt string
	schema json.RawMessage
}

// NewRequest validates and defensively copies one interaction request.
func NewRequest(id ID, kind, prompt string, schema json.RawMessage) (Request, error) {
	if err := token("interaction ID", string(id)); err != nil {
		return Request{}, err
	}
	if err := token("interaction kind", kind); err != nil {
		return Request{}, err
	}
	if prompt == "" || prompt != strings.TrimSpace(prompt) {
		return Request{}, errors.New("interaction prompt must be non-empty without surrounding whitespace")
	}
	if len(prompt) > MaximumPayloadBytes {
		return Request{}, fmt.Errorf("interaction prompt exceeds %d bytes", MaximumPayloadBytes)
	}
	if err := validateJSON("interaction schema", schema); err != nil {
		return Request{}, err
	}
	return Request{id: id, kind: kind, prompt: prompt, schema: cloneJSON(schema)}, nil
}

func (request Request) Validate() error {
	_, err := NewRequest(request.id, request.kind, request.prompt, request.schema)
	return err
}

func (request Request) ID() ID                  { return request.id }
func (request Request) Kind() string            { return request.kind }
func (request Request) Prompt() string          { return request.prompt }
func (request Request) Schema() json.RawMessage { return cloneJSON(request.schema) }
func (request Request) Clone() Request {
	return Request{id: request.id, kind: request.kind, prompt: request.prompt, schema: request.Schema()}
}

// Response completes one interaction with structured user input.
type Response struct {
	id    ID
	value json.RawMessage
}

// NewResponse validates and defensively copies one response.
func NewResponse(id ID, value json.RawMessage) (Response, error) {
	if err := token("interaction ID", string(id)); err != nil {
		return Response{}, err
	}
	if err := validateJSON("interaction response", value); err != nil {
		return Response{}, err
	}
	return Response{id: id, value: cloneJSON(value)}, nil
}

func (response Response) Validate() error {
	_, err := NewResponse(response.id, response.value)
	return err
}

func (response Response) ID() ID                 { return response.id }
func (response Response) Value() json.RawMessage { return cloneJSON(response.value) }
func (response Response) Clone() Response        { return Response{id: response.id, value: response.Value()} }

// Broker is the UI-neutral user-interaction port injected into the engine.
// Implementations must be concurrent-safe and cooperatively honor context.
type Broker interface {
	Request(context.Context, Scope, Request) (Response, error)
}

// Requester is a run-bound interaction lifecycle capability. Unlike Broker it
// accepts no caller-supplied Scope; the owner fixes run authority at binding.
type Requester interface {
	Request(context.Context, Request) (Response, error)
}

// UnavailableRequester is a fail-closed capability for direct dispatcher tests
// and embeddings that deliberately provide no run interaction lifecycle.
type UnavailableRequester struct{}

// Request always fails without observing request payload data.
func (UnavailableRequester) Request(context.Context, Request) (Response, error) {
	return Response{}, errors.New("interaction requester is unavailable")
}

// UnavailableBroker is the fail-closed fallback when no client owns prompts.
type UnavailableBroker struct{}

func (UnavailableBroker) Request(context.Context, Scope, Request) (Response, error) {
	return Response{}, errors.New("interaction broker is unavailable")
}

func validateJSON(label string, value json.RawMessage) error {
	if len(value) == 0 || !json.Valid(value) {
		return fmt.Errorf("%s must contain valid JSON", label)
	}
	if len(value) > MaximumPayloadBytes {
		return fmt.Errorf("%s exceeds %d bytes", label, MaximumPayloadBytes)
	}
	return nil
}

func token(label, value string) error {
	if value == "" || value != strings.TrimSpace(value) {
		return fmt.Errorf("%s must be non-empty without surrounding whitespace", label)
	}
	if len(value) > 128 {
		return fmt.Errorf("%s exceeds 128 bytes", label)
	}
	return nil
}

func cloneJSON(value json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), value...)
}
