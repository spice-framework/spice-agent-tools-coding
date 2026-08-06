// Package async compiles provider-owned asynchronous method annotations into
// deterministic metadata. It validates source only and never invokes methods.
package async

import (
	"fmt"
	"go/token"
	"go/types"
	"sort"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/spice/annotation/sdk"
	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/compiler/provider"
	"github.com/spice-framework/toolchain/compiler/resolve"
	"github.com/spice-framework/toolchain/compiler/signature"
)

const (
	// Annotation identifies the built-in asynchronous execution annotation.
	Annotation = "async.Execute"

	acceptedMethodSignature = "func(receiver)(context.Context, arguments...) error"
)

var errorType = types.Universe.Lookup("error").Type()

// Parameter is one immutable explicit task argument after context.Context.
type Parameter struct {
	Index  int
	Name   string
	Type   types.Type
	TypeID string
}

// Task is one immutable provider-owned asynchronous method.
type Task struct {
	Method           load.Symbol
	MethodID         string
	ProviderID       string
	Receiver         types.Type
	ReceiverTypeID   string
	SubmitMethod     string
	Position         token.Position
	PhysicalPosition token.Position
	parameters       []Parameter
}

// Parameters returns task arguments in source-signature order.
func (task Task) Parameters() []Parameter {
	return append([]Parameter(nil), task.parameters...)
}

// Diagnostic is one source-positioned asynchronous contract failure.
type Diagnostic struct {
	Position         token.Position
	PhysicalPosition token.Position
	MethodID         string
	ProviderID       string
	Kind             string
	Message          string
}

// Error renders a compiler-style diagnostic.
func (diagnostic Diagnostic) Error() string {
	position := diagnostic.Position
	if position.Filename == "" {
		position.Filename = "<unknown>"
	}
	if position.Line <= 0 {
		position.Line = 1
	}
	if position.Column <= 0 {
		position.Column = 1
	}
	return fmt.Sprintf(
		"%s:%d:%d: %s",
		position.Filename,
		position.Line,
		position.Column,
		diagnostic.Message,
	)
}

// Catalog is immutable asynchronous metadata and deterministic diagnostics.
type Catalog struct {
	tasks       []Task
	diagnostics []Diagnostic
}

// Tasks returns asynchronous tasks in stable method identity order.
func (catalog Catalog) Tasks() []Task {
	result := append([]Task(nil), catalog.tasks...)
	for index := range result {
		result[index].parameters = append(
			[]Parameter(nil),
			catalog.tasks[index].parameters...,
		)
	}
	return result
}

// Diagnostics returns a defensive copy in stable source order.
func (catalog Catalog) Diagnostics() []Diagnostic {
	return append([]Diagnostic(nil), catalog.diagnostics...)
}

// Build validates every @async.Execute occurrence against the existing typed
// program and exact provider catalog.
func Build(
	program *load.Program,
	resolution resolve.Result,
	providers provider.Catalog,
) Catalog {
	catalog := Catalog{}
	if program == nil {
		catalog.diagnostics = []Diagnostic{{
			Kind:    "internal",
			Message: "async catalog requires a loaded program",
		}}
		return catalog
	}
	contextType := signature.ContextType(program)
	symbols := make(map[string]load.Symbol)
	for _, symbol := range program.Symbols() {
		symbols[symbol.ID] = symbol
	}
	occurrences := taskOccurrences(resolution)
	for _, methodID := range sortedMethodIDs(occurrences) {
		methodOccurrences := occurrences[methodID]
		if len(methodOccurrences) == 0 {
			continue
		}
		occurrence := methodOccurrences[0]
		if len(methodOccurrences) != 1 {
			catalog.diagnostics = append(
				catalog.diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"duplicate-annotation",
					fmt.Sprintf(
						"method %q carries @%s more than once; asynchronous annotations are not repeatable",
						occurrence.Name,
						Annotation,
					),
				),
			)
			continue
		}
		symbol, found := symbols[methodID]
		if !found {
			catalog.diagnostics = append(
				catalog.diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"missing-symbol",
					fmt.Sprintf(
						"@%s target %q has no stable typed method symbol",
						Annotation,
						occurrence.Name,
					),
				),
			)
			continue
		}
		task, diagnostic := analyzeTask(
			occurrence,
			symbol,
			contextType,
			providers.Providers(),
		)
		if diagnostic != nil {
			catalog.diagnostics = append(catalog.diagnostics, *diagnostic)
			continue
		}
		catalog.tasks = append(catalog.tasks, task)
	}
	sort.SliceStable(catalog.tasks, func(left, right int) bool {
		return catalog.tasks[left].MethodID < catalog.tasks[right].MethodID
	})
	catalog.diagnostics = append(
		catalog.diagnostics,
		submitMethodDiagnostics(catalog.tasks)...,
	)
	sortDiagnostics(catalog.diagnostics)
	return catalog
}

func analyzeTask(
	occurrence resolve.Occurrence,
	method load.Symbol,
	contextType types.Type,
	providers []provider.Provider,
) (Task, *Diagnostic) {
	receiver, parameters, taskProblem := validateMethod(
		occurrence,
		method,
		contextType,
	)
	if taskProblem != nil {
		return taskFailure(occurrence, method, taskProblem)
	}
	owner, taskProblem := providerOwner(receiver, providers)
	if taskProblem != nil {
		return taskFailure(occurrence, method, taskProblem)
	}
	receiverName, found := namedReceiver(receiver)
	if !found {
		return taskFailure(occurrence, method, &problem{
			kind:   "receiver-type",
			reason: "receiver must be a named type or pointer to a named type",
		})
	}
	return Task{
		Method:         method,
		MethodID:       method.ID,
		ProviderID:     owner.SymbolID,
		Receiver:       receiver,
		ReceiverTypeID: provider.TypeID(receiver),
		SubmitMethod:   "Submit" + receiverName + method.Name,
		Position:       occurrence.DisplayPosition,
		PhysicalPosition: token.Position{
			Filename: occurrence.PhysicalFile,
			Offset:   occurrence.PhysicalOffset,
		},
		parameters: parameters,
	}, nil
}

type problem struct {
	kind   string
	reason string
}

func validateMethod(
	occurrence resolve.Occurrence,
	method load.Symbol,
	contextType types.Type,
) (types.Type, []Parameter, *problem) {
	if occurrence.Target != annotation.TargetMethod ||
		method.Kind != load.SymbolMethod ||
		method.Signature == nil ||
		method.Signature.Recv() == nil {
		return nil, nil, &problem{
			kind:   "invalid-target",
			reason: "asynchronous execution must target an ordinary method",
		}
	}
	if len(occurrence.Annotation.Arguments) != 0 {
		return nil, nil, &problem{
			kind:   "arguments",
			reason: "asynchronous execution does not accept annotation arguments",
		}
	}
	if !token.IsExported(method.Name) {
		return nil, nil, &problem{
			kind:   "unexported-method",
			reason: "method must be exported so target-scoped generated code can invoke it",
		}
	}
	methodSignature := method.Signature
	if hasTypeParameters(methodSignature) {
		return nil, nil, &problem{
			kind:   "generic-receiver",
			reason: "receiver-generic asynchronous methods are not supported",
		}
	}
	if methodSignature.Variadic() {
		return nil, nil, &problem{
			kind:   "variadic",
			reason: "asynchronous methods must be non-variadic",
		}
	}
	if methodSignature.Params().Len() == 0 {
		return nil, nil, &problem{
			kind:   "parameter-count",
			reason: "asynchronous methods require context.Context as parameter 0",
		}
	}
	if contextType == nil ||
		!types.Identical(methodSignature.Params().At(0).Type(), contextType) {
		return nil, nil, &problem{
			kind: "context-type",
			reason: fmt.Sprintf(
				"parameter 0 must be the exact loaded context.Context type, got %s",
				provider.TypeID(methodSignature.Params().At(0).Type()),
			),
		}
	}
	if methodSignature.Results().Len() != 1 ||
		!types.Identical(methodSignature.Results().At(0).Type(), errorType) {
		return nil, nil, &problem{
			kind:   "result-type",
			reason: "asynchronous methods require exactly one predeclared error result",
		}
	}
	parameters, parameterProblem := taskParameters(methodSignature.Params())
	if parameterProblem != nil {
		return nil, nil, parameterProblem
	}
	return methodSignature.Recv().Type(), parameters, nil
}

func taskParameters(values *types.Tuple) ([]Parameter, *problem) {
	result := make([]Parameter, 0, values.Len()-1)
	for index := 1; index < values.Len(); index++ {
		parameter := values.At(index)
		if !generatedNameable(parameter.Type()) {
			return nil, &problem{
				kind: "parameter-type",
				reason: fmt.Sprintf(
					"parameter %d type %s cannot be named safely by target-scoped generated code",
					index,
					provider.TypeID(parameter.Type()),
				),
			}
		}
		result = append(result, Parameter{
			Index:  index,
			Name:   parameter.Name(),
			Type:   parameter.Type(),
			TypeID: provider.TypeID(parameter.Type()),
		})
	}
	return result, nil
}

func generatedNameable(value types.Type) bool {
	switch typed := value.(type) {
	case *types.Alias:
		return exportedTypeObject(typed.Obj()) &&
			typeArgumentsNameable(typed.TypeArgs())
	case *types.Named:
		return exportedTypeObject(typed.Obj()) &&
			typeArgumentsNameable(typed.TypeArgs())
	case *types.Basic:
		return true
	default:
		return generatedCompositeNameable(value)
	}
}

func generatedCompositeNameable(value types.Type) bool {
	switch typed := value.(type) {
	case *types.Pointer:
		return generatedNameable(typed.Elem())
	case *types.Slice:
		return generatedNameable(typed.Elem())
	case *types.Array:
		return generatedNameable(typed.Elem())
	case *types.Map:
		return generatedNameable(typed.Key()) &&
			generatedNameable(typed.Elem())
	case *types.Chan:
		return generatedNameable(typed.Elem())
	default:
		return false
	}
}

func exportedTypeObject(object *types.TypeName) bool {
	return object != nil && (object.Pkg() == nil || object.Exported())
}

func typeArgumentsNameable(arguments *types.TypeList) bool {
	if arguments == nil {
		return true
	}
	for argument := range arguments.Types() {
		if !generatedNameable(argument) {
			return false
		}
	}
	return true
}

func namedReceiver(value types.Type) (string, bool) {
	value = types.Unalias(value)
	if pointer, ok := value.(*types.Pointer); ok {
		value = types.Unalias(pointer.Elem())
	}
	named, ok := value.(*types.Named)
	return namedObjectName(named, ok)
}

func namedObjectName(named *types.Named, ok bool) (string, bool) {
	if !ok || named.Obj() == nil || !token.IsExported(named.Obj().Name()) {
		return "", false
	}
	return named.Obj().Name(), true
}

func hasTypeParameters(methodSignature *types.Signature) bool {
	receiverParameters := methodSignature.RecvTypeParams()
	methodParameters := methodSignature.TypeParams()
	return (receiverParameters != nil && receiverParameters.Len() > 0) ||
		(methodParameters != nil && methodParameters.Len() > 0)
}

func providerOwner(
	receiver types.Type,
	providers []provider.Provider,
) (provider.Provider, *problem) {
	var matches []provider.Provider
	for _, candidate := range providers {
		if types.Identical(receiver, candidate.Output) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 0 {
		return provider.Provider{}, &problem{
			kind: "receiver-provider",
			reason: fmt.Sprintf(
				"no @Bean provider produces exact receiver type %s",
				provider.TypeID(receiver),
			),
		}
	}
	if len(matches) != 1 {
		return provider.Provider{}, &problem{
			kind: "ambiguous-provider",
			reason: fmt.Sprintf(
				"receiver type %s is produced by %d providers; asynchronous ownership requires exactly one",
				provider.TypeID(receiver),
				len(matches),
			),
		}
	}
	return matches[0], nil
}

func submitMethodDiagnostics(tasks []Task) []Diagnostic {
	byMethod := make(map[string][]Task)
	for _, task := range tasks {
		byMethod[task.SubmitMethod] = append(
			byMethod[task.SubmitMethod],
			task,
		)
	}
	var diagnostics []Diagnostic
	for submitMethod, matches := range byMethod {
		if len(matches) < 2 {
			continue
		}
		for _, task := range matches {
			diagnostics = append(diagnostics, Diagnostic{
				Position:         task.Position,
				PhysicalPosition: task.PhysicalPosition,
				MethodID:         task.MethodID,
				ProviderID:       task.ProviderID,
				Kind:             "submit-method-collision",
				Message: fmt.Sprintf(
					"@%s method %s generates duplicate Application.%s; rename a receiver or method",
					Annotation,
					methodLabel(task.Method),
					submitMethod,
				),
			})
		}
	}
	return diagnostics
}

func taskOccurrences(
	resolution resolve.Result,
) map[string][]resolve.Occurrence {
	result := make(map[string][]resolve.Occurrence)
	for _, occurrence := range resolution.Occurrences {
		if occurrence.HasContribution(sdk.ContributionAsync) {
			result[occurrence.SymbolID] = append(
				result[occurrence.SymbolID],
				occurrence,
			)
		}
	}
	for _, occurrences := range result {
		sort.SliceStable(occurrences, func(left, right int) bool {
			if occurrences[left].PhysicalFile != occurrences[right].PhysicalFile {
				return occurrences[left].PhysicalFile <
					occurrences[right].PhysicalFile
			}
			return occurrences[left].PhysicalOffset <
				occurrences[right].PhysicalOffset
		})
	}
	return result
}

func sortedMethodIDs(
	occurrences map[string][]resolve.Occurrence,
) []string {
	result := make([]string, 0, len(occurrences))
	for methodID := range occurrences {
		result = append(result, methodID)
	}
	sort.Strings(result)
	return result
}

func taskFailure(
	occurrence resolve.Occurrence,
	method load.Symbol,
	taskProblem *problem,
) (Task, *Diagnostic) {
	diagnostic := symbolDiagnostic(
		occurrence,
		method,
		taskProblem.kind,
		fmt.Sprintf(
			"@%s method %s is invalid: %s; accepted form is %s",
			Annotation,
			methodLabel(method),
			taskProblem.reason,
			acceptedMethodSignature,
		),
	)
	return Task{}, &diagnostic
}

func methodLabel(method load.Symbol) string {
	if method.DisplayLabel != "" {
		return method.DisplayLabel
	}
	if method.ID != "" {
		return method.ID
	}
	return method.Name
}

func occurrenceDiagnostic(
	occurrence resolve.Occurrence,
	kind string,
	message string,
) Diagnostic {
	return Diagnostic{
		Position: occurrence.DisplayPosition,
		PhysicalPosition: token.Position{
			Filename: occurrence.PhysicalFile,
			Offset:   occurrence.PhysicalOffset,
		},
		MethodID: occurrence.SymbolID,
		Kind:     kind,
		Message:  message,
	}
}

func symbolDiagnostic(
	occurrence resolve.Occurrence,
	method load.Symbol,
	kind string,
	message string,
) Diagnostic {
	diagnostic := occurrenceDiagnostic(occurrence, kind, message)
	if diagnostic.Position.Filename == "" {
		diagnostic.Position = method.Position
	}
	if diagnostic.PhysicalPosition.Filename == "" {
		diagnostic.PhysicalPosition = method.PhysicalPosition
	}
	return diagnostic
}

func sortDiagnostics(diagnostics []Diagnostic) {
	sort.SliceStable(diagnostics, func(left, right int) bool {
		leftDiagnostic := diagnostics[left]
		rightDiagnostic := diagnostics[right]
		if leftDiagnostic.PhysicalPosition.Filename !=
			rightDiagnostic.PhysicalPosition.Filename {
			return leftDiagnostic.PhysicalPosition.Filename <
				rightDiagnostic.PhysicalPosition.Filename
		}
		if leftDiagnostic.PhysicalPosition.Offset !=
			rightDiagnostic.PhysicalPosition.Offset {
			return leftDiagnostic.PhysicalPosition.Offset <
				rightDiagnostic.PhysicalPosition.Offset
		}
		if leftDiagnostic.Kind != rightDiagnostic.Kind {
			return leftDiagnostic.Kind < rightDiagnostic.Kind
		}
		if leftDiagnostic.MethodID != rightDiagnostic.MethodID {
			return leftDiagnostic.MethodID < rightDiagnostic.MethodID
		}
		return leftDiagnostic.Message < rightDiagnostic.Message
	})
}
