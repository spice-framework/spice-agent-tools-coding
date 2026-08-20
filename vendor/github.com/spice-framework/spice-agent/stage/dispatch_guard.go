package stage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/spice-framework/spice-agent/interaction"
	"github.com/spice-framework/spice-agent/tool"
)

const dispatchFingerprintPrefix = "sha256:"

// ToolDispatchScope is immutable authority and execution identity supplied by
// the engine for one tool dispatch. It contains no policy decision and grants
// no capability by itself.
type ToolDispatchScope struct {
	runID                string
	turn                 uint32
	toolPlanID           PlanID
	planFingerprint      string
	workspaceFingerprint string
	interactionAuthority interaction.Scope
	interactionRequester *toolInteractionCapability
}

type toolInteractionCapability struct{ requester interaction.Requester }

// NewToolDispatchScope constructs the immutable facts visible to terminal
// guards. An empty workspace fingerprint is allowed only for deliberately
// non-portable embedded engines whose snapshot compatibility identity is empty.
func NewToolDispatchScope(
	runID string,
	turn uint32,
	toolPlanID PlanID,
	planFingerprint string,
	workspaceFingerprint string,
	interactionAuthority interaction.Scope,
	interactionRequester interaction.Requester,
) (ToolDispatchScope, error) {
	result := ToolDispatchScope{
		runID: runID, turn: turn, toolPlanID: toolPlanID, planFingerprint: planFingerprint,
		workspaceFingerprint: workspaceFingerprint,
		interactionAuthority: interactionAuthority,
		interactionRequester: &toolInteractionCapability{requester: interactionRequester},
	}
	if interactionRequester == nil {
		return ToolDispatchScope{}, errors.New("tool dispatch interaction requester is required")
	}
	if err := result.Validate(); err != nil {
		return ToolDispatchScope{}, err
	}
	return result, nil
}

// Validate rejects incomplete or contradictory dispatch authority.
func (scope ToolDispatchScope) Validate() error {
	if scope.runID == "" || scope.runID != strings.TrimSpace(scope.runID) || len(scope.runID) > 96 {
		return errors.New("tool dispatch run ID is invalid")
	}
	if scope.turn == 0 {
		return errors.New("tool dispatch turn must be positive")
	}
	if err := scope.toolPlanID.Validate(); err != nil {
		return fmt.Errorf("tool dispatch plan ID: %w", err)
	}
	if err := validateDispatchFingerprint("plan", scope.planFingerprint, false); err != nil {
		return err
	}
	if err := validateDispatchFingerprint("workspace", scope.workspaceFingerprint, true); err != nil {
		return err
	}
	if err := scope.interactionAuthority.Validate(); err != nil {
		return fmt.Errorf("tool dispatch interaction authority: %w", err)
	}
	if scope.interactionAuthority.RunID() != scope.runID {
		return errors.New("tool dispatch interaction authority does not own the run")
	}
	if scope.interactionRequester == nil || scope.interactionRequester.requester == nil {
		return errors.New("tool dispatch interaction requester is required")
	}
	return nil
}

func (scope ToolDispatchScope) RunID() string                { return scope.runID }
func (scope ToolDispatchScope) Turn() uint32                 { return scope.turn }
func (scope ToolDispatchScope) ToolPlanID() PlanID           { return scope.toolPlanID }
func (scope ToolDispatchScope) PlanFingerprint() string      { return scope.planFingerprint }
func (scope ToolDispatchScope) WorkspaceFingerprint() string { return scope.workspaceFingerprint }
func (scope ToolDispatchScope) InteractionAuthority() interaction.Scope {
	return scope.interactionAuthority
}

// RequestInteraction invokes the run-owned interaction lifecycle without
// exposing broker Scope authority to a guard.
func (scope ToolDispatchScope) RequestInteraction(
	ctx context.Context,
	request interaction.Request,
) (interaction.Response, error) {
	if ctx == nil {
		return interaction.Response{}, errors.New("tool dispatch interaction context must not be nil")
	}
	if err := scope.Validate(); err != nil {
		return interaction.Response{}, err
	}
	if err := request.Validate(); err != nil {
		return interaction.Response{}, err
	}
	if err := ctx.Err(); err != nil {
		return interaction.Response{}, err
	}
	response, err := safeRequestInteraction(ctx, scope.interactionRequester.requester, request.Clone())
	if err != nil {
		return interaction.Response{}, err
	}
	if err = ctx.Err(); err != nil {
		return interaction.Response{}, err
	}
	if err = response.Validate(); err != nil {
		return interaction.Response{}, errors.New("tool dispatch interaction response is invalid")
	}
	if response.ID() != request.ID() {
		return interaction.Response{}, errors.New("tool dispatch interaction response does not match the request")
	}
	return response.Clone(), nil
}

func validateDispatchFingerprint(label, value string, allowEmpty bool) error {
	if value == "" && allowEmpty {
		return nil
	}
	if !strings.HasPrefix(value, dispatchFingerprintPrefix) {
		return fmt.Errorf("tool dispatch %s fingerprint must use sha256", label)
	}
	digest := strings.TrimPrefix(value, dispatchFingerprintPrefix)
	if len(digest) != sha256.Size*2 {
		return fmt.Errorf("tool dispatch %s fingerprint has an invalid SHA-256 length", label)
	}
	if _, err := hex.DecodeString(digest); err != nil || strings.ToLower(digest) != digest {
		return fmt.Errorf("tool dispatch %s fingerprint must be lowercase hexadecimal SHA-256", label)
	}
	return nil
}

func (scope ToolDispatchScope) equal(other ToolDispatchScope) bool {
	return scope.runID == other.runID && scope.turn == other.turn && scope.toolPlanID == other.toolPlanID &&
		scope.planFingerprint == other.planFingerprint && scope.workspaceFingerprint == other.workspaceFingerprint &&
		scope.interactionAuthority.RunID() == other.interactionAuthority.RunID() &&
		scope.interactionRequester == other.interactionRequester
}

func safeRequestInteraction(
	ctx context.Context,
	requester interaction.Requester,
	request interaction.Request,
) (response interaction.Response, err error) {
	defer func() {
		if recover() != nil {
			response = interaction.Response{}
			err = errors.New("tool dispatch interaction requester panicked")
		}
	}()
	return requester.Request(ctx, request)
}

// ToolDispatchNext is a single-use continuation bound to the immutable context,
// scope, definition, call, and reporter received by a guard.
type ToolDispatchNext func() (tool.Result, error)

// ToolDispatchGuard is the terminal, innermost interception seam for policy.
// Guards may deny or invoke next exactly once. They do not own engine events.
type ToolDispatchGuard interface {
	Guard(context.Context, ToolDispatchScope, tool.Definition, tool.Call, ToolDispatchNext) (tool.Result, error)
}

type guardedToolDispatcher struct {
	delegate    ToolDispatcher
	definitions []tool.Definition
	guards      []ToolDispatchGuard
}

type guardDispatchContextKey struct{ dispatcher *guardedToolDispatcher }

func (dispatcher *guardedToolDispatcher) Definitions() []tool.Definition {
	return cloneDefinitions(dispatcher.definitions)
}

func (dispatcher *guardedToolDispatcher) Definition(name string) (tool.Definition, bool) {
	return definitionFromSnapshot(dispatcher.definitions, name)
}

func (dispatcher *guardedToolDispatcher) Dispatch(
	ctx context.Context,
	scope ToolDispatchScope,
	call tool.Call,
	reporter tool.Reporter,
) (tool.Result, error) {
	if ctx == nil {
		return tool.Result{}, errors.New("tool dispatch context must not be nil")
	}
	if dispatcher == nil || dispatcher.delegate == nil {
		return tool.Result{}, errors.New("guarded tool dispatcher is nil")
	}
	if err := scope.Validate(); err != nil {
		return tool.Result{}, err
	}
	bound, ok := ctx.Value(toolDispatchAuthorityContextKey{}).(ToolDispatchScope)
	if !ok || !bound.equal(scope) {
		return tool.Result{}, errors.New("tool dispatch scope was substituted after authority binding")
	}
	if err := call.Validate(); err != nil {
		return tool.Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return tool.Result{}, err
	}
	definition, declared := dispatcher.Definition(call.Name())
	if !declared {
		return tool.Result{}, fmt.Errorf("tool %q is not declared by the guarded plan", call.Name())
	}
	key := guardDispatchContextKey{dispatcher: dispatcher}
	if ctx.Value(key) != nil {
		return tool.Result{}, errors.New("tool dispatch guard re-entry is forbidden")
	}
	ctx = context.WithValue(ctx, key, struct{}{})
	var invoke func(context.Context, int) (tool.Result, error)
	invoke = func(guardContext context.Context, index int) (tool.Result, error) {
		if err := guardContext.Err(); err != nil {
			return tool.Result{}, err
		}
		if index == len(dispatcher.guards) {
			return dispatcher.delegate.Dispatch(guardContext, scope, call, reporter)
		}
		continuation, closeContinuation := newGuardContinuation(func() (tool.Result, error) {
			return invoke(guardContext, index+1)
		})
		result, guardErr := safeGuard(
			dispatcher.guards[index], guardContext, scope, definition, call, continuation,
		)
		closeContinuation()
		return validateGuardOutcome(call, result, guardErr)
	}
	return invoke(ctx, 0)
}

func newGuardContinuation(delegate ToolDispatchNext) (ToolDispatchNext, func()) {
	var state atomic.Uint32
	done := make(chan struct{})
	return func() (tool.Result, error) {
			if !state.CompareAndSwap(0, 1) {
				return tool.Result{}, errors.New("tool dispatch continuation is closed or was already invoked")
			}
			defer func() {
				state.Store(2)
				close(done)
			}()
			return delegate()
		}, func() {
			if state.CompareAndSwap(0, 2) {
				return
			}
			if state.Load() == 1 {
				<-done
			}
		}
}

func validateGuardOutcome(call tool.Call, result tool.Result, guardErr error) (tool.Result, error) {
	if guardErr != nil {
		if !result.IsZero() {
			return tool.Result{}, errors.New("tool dispatch guard returned both a result and an error")
		}
		return tool.Result{}, guardErr
	}
	if err := result.Validate(); err != nil {
		return tool.Result{}, fmt.Errorf("validate tool dispatch guard result: %w", err)
	}
	if result.CallID() != call.ID() {
		return tool.Result{}, errors.New("tool dispatch guard result does not match the active call")
	}
	return result.Clone(), nil
}

func safeGuard(
	guard ToolDispatchGuard,
	ctx context.Context,
	scope ToolDispatchScope,
	definition tool.Definition,
	call tool.Call,
	next ToolDispatchNext,
) (result tool.Result, err error) {
	defer func() {
		if recover() != nil {
			result = tool.Result{}
			err = errors.New("tool dispatch guard panicked")
		}
	}()
	return guard.Guard(ctx, scope, definition.Clone(), call.Clone(), next)
}
