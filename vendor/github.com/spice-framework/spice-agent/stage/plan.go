package stage

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/spice-framework/spice-agent/tool"
)

const maximumPlanIDBytes = 256

// PlanID identifies one immutable tool-dispatch generation. A source must never
// reuse an ID for different definitions or executable behavior.
type PlanID string

// NewPlanID validates one tool-plan identity.
func NewPlanID(value string) (PlanID, error) {
	result := PlanID(value)
	if err := result.Validate(); err != nil {
		return "", err
	}
	return result, nil
}

// Validate rejects empty, unbounded, non-canonical, or control-bearing IDs.
func (id PlanID) Validate() error {
	value := string(id)
	if value == "" || value != strings.TrimSpace(value) {
		return errors.New("tool plan ID must be non-empty without surrounding whitespace")
	}
	if len(value) > maximumPlanIDBytes {
		return fmt.Errorf("tool plan ID exceeds %d bytes", maximumPlanIDBytes)
	}
	if !utf8.ValidString(value) {
		return errors.New("tool plan ID must be valid UTF-8")
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return errors.New("tool plan ID must not contain whitespace or control characters")
		}
	}
	return nil
}

// String returns the stable wire representation.
func (id PlanID) String() string { return string(id) }

// ToolPlanSource is a trusted owner of immutable dispatch generations.
// LeaseCurrent selects the generation for a new run; LeaseGeneration must
// resolve exactly the requested generation for snapshot recovery without
// silently substituting. A source must never reuse a PlanID for changed
// definitions or executable behavior and must keep a leased dispatcher stable.
type ToolPlanSource interface {
	LeaseCurrent(context.Context) (*ToolPlanLease, error)
	LeaseGeneration(context.Context, PlanID) (*ToolPlanLease, error)
}

// ToolPlanLease owns one source-guaranteed immutable dispatcher generation and
// a host-captured definition snapshot. Go cannot structurally freeze executable
// behavior; the trusted source is responsible for stability until Release.
type ToolPlanLease struct {
	id            PlanID
	dispatcher    ToolDispatcher
	definitions   []tool.Definition
	release       func() error
	releaseStart  sync.Once
	releaseFinish sync.Once
	releaseDone   chan struct{}
	releaseErr    error
}

// NewToolPlanLease captures definitions and ownership for one source-guaranteed
// immutable generation; it does not clone executable Go behavior.
func NewToolPlanLease(id PlanID, dispatcher ToolDispatcher, release func() error) (*ToolPlanLease, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	if dispatcher == nil {
		return nil, errors.New("tool plan lease requires a dispatcher")
	}
	if release == nil {
		return nil, errors.New("tool plan lease requires a release function")
	}
	definitions, err := snapshotDefinitions(dispatcher)
	if err != nil {
		return nil, fmt.Errorf("snapshot tool plan %q definitions: %w", id, err)
	}
	snapshot := &definitionSnapshotDispatcher{delegate: dispatcher, definitions: definitions}
	return &ToolPlanLease{
		id: id, dispatcher: snapshot, definitions: definitions,
		release: release, releaseDone: make(chan struct{}),
	}, nil
}

// Validate rejects nil or corrupted leases. A valid source returns a fresh
// lease for every successful acquisition.
func (lease *ToolPlanLease) Validate() error {
	if lease == nil {
		return errors.New("tool plan lease is nil")
	}
	if err := lease.id.Validate(); err != nil {
		return err
	}
	if lease.dispatcher == nil || lease.release == nil || lease.releaseDone == nil {
		return errors.New("tool plan lease is incomplete")
	}
	return validateDefinitions(lease.definitions)
}

// ToolPlanID returns the immutable generation identity.
func (lease *ToolPlanLease) ToolPlanID() PlanID {
	if lease == nil {
		return ""
	}
	return lease.id
}

// Dispatcher returns a dispatcher whose definitions are the leased snapshot.
func (lease *ToolPlanLease) Dispatcher() ToolDispatcher {
	if lease == nil {
		return nil
	}
	return lease.dispatcher
}

// Definitions returns canonical defensive copies of the leased definitions.
func (lease *ToolPlanLease) Definitions() []tool.Definition {
	if lease == nil {
		return []tool.Definition{}
	}
	return cloneDefinitions(lease.definitions)
}

// Release relinquishes the generation at most once and waits for completion.
// Engine code uses ReleaseContext to impose its finalization bound.
func (lease *ToolPlanLease) Release() error {
	return lease.ReleaseContext(context.Background())
}

// ReleaseContext relinquishes the generation at most once. The first callback
// completion or context expiry becomes the same bounded observable result for
// every caller; a callback that violates the non-blocking contract is left in
// its isolated goroutine and cannot suppress run finalization.
func (lease *ToolPlanLease) ReleaseContext(ctx context.Context) error {
	if lease == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("tool plan release context must not be nil")
	}
	lease.releaseStart.Do(func() { go lease.invokeRelease() })
	select {
	case <-lease.releaseDone:
	case <-ctx.Done():
		lease.finishRelease(boundedPlanError("tool plan release did not complete", ctx.Err()))
		<-lease.releaseDone
	}
	return lease.releaseErr
}

func (lease *ToolPlanLease) invokeRelease() {
	var result error
	defer func() {
		if recover() != nil {
			result = errors.New("tool plan release panicked")
		}
		lease.finishRelease(result)
	}()
	if err := lease.release(); err != nil {
		result = boundedPlanError("tool plan release failed", err)
	}
}

func (lease *ToolPlanLease) finishRelease(err error) {
	lease.releaseFinish.Do(func() {
		lease.releaseErr = err
		close(lease.releaseDone)
	})
}

// StaticToolPlanSource adapts an already-constructed dispatcher for embedded
// applications and preserves the existing Engine constructors.
type StaticToolPlanSource struct {
	id         PlanID
	dispatcher ToolDispatcher
}

// NewStaticToolPlanSource creates a stable no-op leased source.
func NewStaticToolPlanSource(dispatcher ToolDispatcher) (*StaticToolPlanSource, error) {
	if dispatcher == nil {
		return nil, errors.New("static tool plan source requires a dispatcher")
	}
	definitions, err := snapshotDefinitions(dispatcher)
	if err != nil {
		return nil, fmt.Errorf("snapshot static tool plan: %w", err)
	}
	id, err := staticPlanID(definitions)
	if err != nil {
		return nil, err
	}
	return &StaticToolPlanSource{
		id:         id,
		dispatcher: &definitionSnapshotDispatcher{delegate: dispatcher, definitions: definitions},
	}, nil
}

// LeaseCurrent leases the one static generation.
func (source *StaticToolPlanSource) LeaseCurrent(ctx context.Context) (*ToolPlanLease, error) {
	if ctx == nil {
		return nil, errors.New("lease current context must not be nil")
	}
	if source == nil {
		return nil, errors.New("static tool plan source is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return NewToolPlanLease(source.id, source.dispatcher, func() error { return nil })
}

// LeaseGeneration leases the static generation only when the ID matches.
func (source *StaticToolPlanSource) LeaseGeneration(ctx context.Context, id PlanID) (*ToolPlanLease, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	if source == nil {
		return nil, errors.New("static tool plan source is nil")
	}
	if id != source.id {
		return nil, fmt.Errorf("static tool plan %q is unavailable", id)
	}
	return source.LeaseCurrent(ctx)
}

// ApplyToolDispatchDecorators applies an already ordered collection only after
// the merged base dispatcher exists. The first decorator is the outermost and
// therefore observes a call first and its result last.
func ApplyToolDispatchDecorators(
	base ToolDispatcher,
	decorators []ToolDispatchDecorator,
) (ToolDispatcher, error) {
	if base == nil {
		return nil, errors.New("tool dispatch decorators require a base dispatcher")
	}
	definitions, err := snapshotDefinitions(base)
	if err != nil {
		return nil, fmt.Errorf("snapshot base tool dispatcher: %w", err)
	}
	var current ToolDispatcher = &definitionSnapshotDispatcher{delegate: base, definitions: definitions}
	for index, decorator := range slices.Backward(decorators) {
		if decorator == nil {
			return nil, fmt.Errorf("tool dispatch decorator %d is nil", index)
		}
		wrapped, wrapErr := safeWrap(decorator, current)
		if wrapErr != nil {
			return nil, fmt.Errorf("tool dispatch decorator %d: %w", index, wrapErr)
		}
		if wrapped == nil {
			return nil, fmt.Errorf("tool dispatch decorator %d returned nil", index)
		}
		wrappedDefinitions, definitionsErr := snapshotDefinitions(wrapped)
		if definitionsErr != nil {
			return nil, fmt.Errorf("tool dispatch decorator %d definitions: %w", index, definitionsErr)
		}
		if !sameDefinitions(definitions, wrappedDefinitions) {
			return nil, fmt.Errorf("tool dispatch decorator %d changed the merged definition set", index)
		}
		current = &definitionSnapshotDispatcher{delegate: wrapped, definitions: definitions}
	}
	return current, nil
}

type definitionSnapshotDispatcher struct {
	delegate    ToolDispatcher
	definitions []tool.Definition
}

func (dispatcher *definitionSnapshotDispatcher) Definitions() []tool.Definition {
	if dispatcher == nil {
		return []tool.Definition{}
	}
	return cloneDefinitions(dispatcher.definitions)
}

func (dispatcher *definitionSnapshotDispatcher) Definition(name string) (tool.Definition, bool) {
	if dispatcher == nil {
		return tool.Definition{}, false
	}
	index, found := slices.BinarySearchFunc(dispatcher.definitions, name, func(definition tool.Definition, target string) int {
		return strings.Compare(definition.Name(), target)
	})
	if !found {
		return tool.Definition{}, false
	}
	return dispatcher.definitions[index].Clone(), true
}

func (dispatcher *definitionSnapshotDispatcher) Dispatch(
	ctx context.Context,
	call tool.Call,
	reporter tool.Reporter,
) (tool.Result, error) {
	if dispatcher == nil || dispatcher.delegate == nil {
		return tool.Result{}, errors.New("leased tool dispatcher is nil")
	}
	if err := call.Validate(); err != nil {
		return tool.Result{}, err
	}
	if _, declared := dispatcher.Definition(call.Name()); !declared {
		return tool.Result{}, fmt.Errorf("tool %q is not declared by the leased plan", call.Name())
	}
	return dispatcher.delegate.Dispatch(ctx, call, reporter)
}

func snapshotDefinitions(dispatcher ToolDispatcher) (definitions []tool.Definition, err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("tool dispatcher definitions panicked")
		}
	}()
	definitions = dispatcher.Definitions()
	definitions = cloneDefinitions(definitions)
	slices.SortFunc(definitions, func(left, right tool.Definition) int {
		return strings.Compare(left.Name(), right.Name())
	})
	if err = validateDefinitions(definitions); err != nil {
		return nil, err
	}
	return definitions, nil
}

func validateDefinitions(definitions []tool.Definition) error {
	for index, definition := range definitions {
		if err := definition.Validate(); err != nil {
			return fmt.Errorf("tool definition %d: %w", index, err)
		}
		if index > 0 && definitions[index-1].Name() >= definition.Name() {
			return fmt.Errorf("tool definitions must be sorted and unique at %q", definition.Name())
		}
	}
	return nil
}

func cloneDefinitions(definitions []tool.Definition) []tool.Definition {
	result := make([]tool.Definition, len(definitions))
	for index, definition := range definitions {
		result[index] = definition.Clone()
	}
	return result
}

func sameDefinitions(left, right []tool.Definition) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Name() != right[index].Name() ||
			left[index].Fingerprint() != right[index].Fingerprint() {
			return false
		}
	}
	return true
}

func safeWrap(decorator ToolDispatchDecorator, next ToolDispatcher) (wrapped ToolDispatcher, err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("wrap panicked")
		}
	}()
	return decorator.Wrap(next), nil
}

func staticPlanID(definitions []tool.Definition) (PlanID, error) {
	hash := sha256.New()
	for _, definition := range definitions {
		writePlanField(hash, definition.Name())
		writePlanField(hash, definition.Fingerprint())
	}
	return NewPlanID(fmt.Sprintf("static:%x", hash.Sum(nil)))
}

type planHashWriter interface {
	Write([]byte) (int, error)
}

func writePlanField(destination planHashWriter, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = destination.Write(size[:])
	_, _ = destination.Write([]byte(value))
}

func boundedPlanError(label string, err error) error {
	message := strings.TrimSpace(strings.ToValidUTF8(err.Error(), "\uFFFD"))
	if message == "" {
		return errors.New(label)
	}
	maximum := tool.MaximumExecutionErrorBytes - len(label) - 2
	if len(message) > maximum {
		message = message[:maximum]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	return fmt.Errorf("%s: %s", label, message)
}
