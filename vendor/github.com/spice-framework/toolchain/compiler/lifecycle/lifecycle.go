// Package lifecycle validates explicit lifecycle-hook metadata against the
// provider catalog. It records typed metadata only and never executes hooks.
package lifecycle

import (
	"fmt"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/spice/annotation/sdk"
	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/compiler/provider"
	"github.com/spice-framework/toolchain/compiler/resolve"
	"github.com/spice-framework/toolchain/compiler/signature"
)

const acceptedHookSignature = "func(receiver)(context.Context) error"

type Kind string

const (
	Start Kind = "start"
	Stop  Kind = "stop"
)

var errorType = types.Universe.Lookup("error").Type()

type Hook struct {
	Kind             Kind
	Method           load.Symbol
	MethodID         string
	ProviderID       string
	Receiver         types.Type
	ReceiverTypeID   string
	Position         token.Position
	PhysicalPosition token.Position
}

type Component struct {
	Provider provider.Provider
	Start    *Hook
	Stop     *Hook
}

type Diagnostic struct {
	Position         token.Position
	PhysicalPosition token.Position
	MethodID         string
	ProviderID       string
	Kind             string
	Message          string
}

func (d Diagnostic) Error() string {
	position := d.Position
	if position.Filename == "" {
		position.Filename = "<unknown>"
	}
	if position.Line <= 0 {
		position.Line = 1
	}
	if position.Column <= 0 {
		position.Column = 1
	}
	return fmt.Sprintf("%s:%d:%d: %s", position.Filename, position.Line, position.Column, d.Message)
}

type Catalog struct {
	components  []Component
	diagnostics []Diagnostic
}

func (c Catalog) Components() []Component {
	result := make([]Component, len(c.components))
	copy(result, c.components)
	for index := range result {
		result[index].Provider.Dependencies = append([]provider.Dependency(nil), result[index].Provider.Dependencies...)
		result[index].Provider.Interfaces = append(
			[]provider.InterfaceBinding(nil),
			result[index].Provider.Interfaces...,
		)
		if result[index].Start != nil {
			copyHook := *result[index].Start
			result[index].Start = &copyHook
		}
		if result[index].Stop != nil {
			copyHook := *result[index].Stop
			result[index].Stop = &copyHook
		}
	}
	return result
}

func (c Catalog) Diagnostics() []Diagnostic { return append([]Diagnostic(nil), c.diagnostics...) }

type buildStats struct{ identityChecks int }

func Build(program *load.Program, resolution resolve.Result, providers provider.Catalog) Catalog {
	catalog, _ := build(program, resolution, providers)
	return catalog
}

func build(program *load.Program, resolution resolve.Result, providers provider.Catalog) (Catalog, buildStats) {
	catalog := Catalog{}
	stats := buildStats{}
	if program == nil {
		catalog.diagnostics = []Diagnostic{{Kind: "internal", Message: "lifecycle catalog requires a loaded program"}}
		return catalog, stats
	}
	symbols := symbolIndex(program)
	providerIndex := lifecycleProviderIndex(providers)
	contextType := signature.ContextType(program)
	components := make(map[string]*Component)
	methodKinds := lifecycleOccurrences(resolution)
	for _, methodID := range sortedMethodIDs(methodKinds) {
		processLifecycleMethod(
			&catalog,
			&stats,
			methodID,
			methodKinds[methodID],
			symbols,
			providerIndex,
			contextType,
			components,
		)
	}
	finalizeComponents(&catalog, components)
	return catalog, stats
}

func symbolIndex(program *load.Program) map[string]load.Symbol {
	symbols := make(map[string]load.Symbol)
	for _, symbol := range program.Symbols() {
		symbols[symbol.ID] = symbol
	}
	return symbols
}

func lifecycleProviderIndex(providers provider.Catalog) map[string][]provider.Provider {
	providerIndex := make(map[string][]provider.Provider)
	for _, item := range providers.Providers() {
		key := semanticTypeKey(item.Output)
		providerIndex[key] = append(providerIndex[key], item)
	}
	return providerIndex
}

func lifecycleOccurrences(resolution resolve.Result) map[string]map[Kind][]resolve.Occurrence {
	methodKinds := make(map[string]map[Kind][]resolve.Occurrence)
	for _, occurrence := range resolution.Occurrences {
		kind, lifecycleOccurrence := occurrenceKind(occurrence)
		if !lifecycleOccurrence {
			continue
		}
		roles := methodKinds[occurrence.SymbolID]
		if roles == nil {
			roles = make(map[Kind][]resolve.Occurrence)
			methodKinds[occurrence.SymbolID] = roles
		}
		roles[kind] = append(roles[kind], occurrence)
	}
	for _, roles := range methodKinds {
		for _, occurrences := range roles {
			sort.SliceStable(occurrences, func(i, j int) bool {
				if occurrences[i].PhysicalFile != occurrences[j].PhysicalFile {
					return occurrences[i].PhysicalFile < occurrences[j].PhysicalFile
				}
				return occurrences[i].PhysicalOffset < occurrences[j].PhysicalOffset
			})
		}
	}
	return methodKinds
}

func sortedMethodIDs(methodKinds map[string]map[Kind][]resolve.Occurrence) []string {
	methodIDs := make([]string, 0, len(methodKinds))
	for methodID := range methodKinds {
		methodIDs = append(methodIDs, methodID)
	}
	sort.Strings(methodIDs)
	return methodIDs
}

func processLifecycleMethod(
	catalog *Catalog,
	stats *buildStats,
	methodID string,
	roles map[Kind][]resolve.Occurrence,
	symbols map[string]load.Symbol,
	providerIndex map[string][]provider.Provider,
	contextType types.Type,
	components map[string]*Component,
) {
	if len(roles) > 1 {
		occurrences := roles[Start]
		if len(occurrences) == 0 {
			occurrences = roles[Stop]
		}
		if len(occurrences) == 0 {
			return
		}
		occurrence := occurrences[0]
		catalog.diagnostics = append(catalog.diagnostics, occurrenceDiagnostic(
			occurrence,
			methodID,
			"conflicting-annotations",
			fmt.Sprintf(
				"method %q may carry only one lifecycle annotation; @OnStart and @OnStop cannot be combined",
				occurrence.Name,
			),
		))
		return
	}
	for _, kind := range []Kind{Start, Stop} {
		processLifecycleRole(
			catalog,
			stats,
			methodID,
			kind,
			roles[kind],
			symbols,
			providerIndex,
			contextType,
			components,
		)
	}
}

func processLifecycleRole(
	catalog *Catalog,
	stats *buildStats,
	methodID string,
	kind Kind,
	occurrences []resolve.Occurrence,
	symbols map[string]load.Symbol,
	providerIndex map[string][]provider.Provider,
	contextType types.Type,
	components map[string]*Component,
) {
	if len(occurrences) == 0 {
		return
	}
	occurrence := occurrences[0]
	if len(occurrences) > 1 {
		catalog.diagnostics = append(catalog.diagnostics, occurrenceDiagnostic(
			occurrence,
			methodID,
			"duplicate-annotation",
			fmt.Sprintf(
				"method %q carries @On%s more than once; lifecycle annotations are not repeatable",
				occurrence.Name,
				kindTitle(kind),
			),
		))
		return
	}
	symbol, ok := symbols[methodID]
	if !ok {
		catalog.diagnostics = append(catalog.diagnostics, occurrenceDiagnostic(
			occurrence,
			methodID,
			"missing-symbol",
			fmt.Sprintf("@On%s target %q has no stable typed method symbol", kindTitle(kind), occurrence.Name),
		))
		return
	}
	hook, owner, diagnostic := analyzeHook(occurrence, kind, symbol, contextType, providerIndex, stats)
	if diagnostic != nil {
		catalog.diagnostics = append(catalog.diagnostics, *diagnostic)
		return
	}
	component := componentFor(components, owner)
	existing := component.Start
	if kind == Stop {
		existing = component.Stop
	}
	if existing != nil {
		catalog.diagnostics = append(catalog.diagnostics, hookDiagnostic(
			hook,
			"duplicate-"+string(kind),
			fmt.Sprintf(
				"provider %s has multiple @On%s hooks: %s and %s",
				owner.Symbol.DisplayLabel,
				kindTitle(kind),
				existing.Method.DisplayLabel,
				hook.Method.DisplayLabel,
			),
		))
		return
	}
	copyHook := hook
	if kind == Start {
		component.Start = &copyHook
	} else {
		component.Stop = &copyHook
	}
}

func componentFor(components map[string]*Component, owner provider.Provider) *Component {
	component := components[owner.SymbolID]
	if component == nil {
		copyProvider := owner
		copyProvider.Dependencies = append([]provider.Dependency(nil), owner.Dependencies...)
		copyProvider.Interfaces = append(
			[]provider.InterfaceBinding(nil),
			owner.Interfaces...,
		)
		component = &Component{Provider: copyProvider}
		components[owner.SymbolID] = component
	}
	return component
}

func finalizeComponents(catalog *Catalog, components map[string]*Component) {
	for _, component := range components {
		if component.Start == nil && component.Stop != nil {
			catalog.diagnostics = append(catalog.diagnostics, hookDiagnostic(
				*component.Stop,
				"stop-without-start",
				fmt.Sprintf(
					"@OnStop method %s has no corresponding @OnStart hook for provider %s",
					component.Stop.Method.DisplayLabel,
					component.Provider.Symbol.DisplayLabel,
				),
			))
		}
		catalog.components = append(catalog.components, *component)
	}
	sort.Slice(catalog.components, func(i, j int) bool {
		return catalog.components[i].Provider.SymbolID < catalog.components[j].Provider.SymbolID
	})
	sortDiagnostics(catalog.diagnostics)
}

type hookProblem struct {
	kind   string
	reason string
}

func analyzeHook(
	occurrence resolve.Occurrence,
	kind Kind,
	symbol load.Symbol,
	contextType types.Type,
	providerIndex map[string][]provider.Provider,
	stats *buildStats,
) (Hook, provider.Provider, *Diagnostic) {
	receiver, problem := validateHookSignature(occurrence, symbol, contextType)
	if problem != nil {
		return hookFailure(occurrence, kind, symbol, problem)
	}
	owner, problem := lifecycleOwner(receiver, providerIndex, stats)
	if problem != nil {
		return hookFailure(occurrence, kind, symbol, problem)
	}
	return Hook{
		Kind:             kind,
		Method:           symbol,
		MethodID:         symbol.ID,
		ProviderID:       owner.SymbolID,
		Receiver:         receiver,
		ReceiverTypeID:   provider.TypeID(receiver),
		Position:         occurrence.DisplayPosition,
		PhysicalPosition: token.Position{Filename: occurrence.PhysicalFile, Offset: occurrence.PhysicalOffset},
	}, owner, nil
}

func validateHookSignature(
	occurrence resolve.Occurrence,
	symbol load.Symbol,
	contextType types.Type,
) (types.Type, *hookProblem) {
	if occurrence.Target != annotation.TargetMethod || symbol.Kind != load.SymbolMethod || symbol.Signature == nil || symbol.Signature.Recv() == nil {
		return nil, &hookProblem{"invalid-target", "lifecycle hooks must target ordinary methods"}
	}
	if len(occurrence.Annotation.Arguments) != 0 {
		return nil, &hookProblem{"arguments", "lifecycle annotations do not accept arguments"}
	}
	signature := symbol.Signature
	if hookHasTypeParameters(signature) {
		return nil, &hookProblem{"generic-receiver", "receiver-generic lifecycle methods are not supported"}
	}
	if signature.Variadic() {
		return nil, &hookProblem{"variadic", "lifecycle methods must be non-variadic"}
	}
	if signature.Params().Len() != 1 {
		return nil, &hookProblem{
			"parameter-count",
			fmt.Sprintf("lifecycle methods require exactly one explicit context parameter, got %d", signature.Params().Len()),
		}
	}
	if contextType == nil {
		return nil, &hookProblem{
			"context-type",
			"canonical context.Context identity could not be established safely from the loaded Go 1.26 type universe",
		}
	}
	if !types.Identical(signature.Params().At(0).Type(), contextType) {
		return nil, &hookProblem{
			"context-type",
			fmt.Sprintf(
				"parameter 0 must be the exact loaded context.Context type, got %s",
				provider.TypeID(signature.Params().At(0).Type()),
			),
		}
	}
	if signature.Results().Len() != 1 {
		return nil, &hookProblem{
			"result-count",
			fmt.Sprintf("lifecycle methods require exactly one error result, got %d", signature.Results().Len()),
		}
	}
	if !types.Identical(signature.Results().At(0).Type(), errorType) {
		return nil, &hookProblem{
			"result-type",
			fmt.Sprintf(
				"result 0 must be the exact predeclared error type, got %s",
				provider.TypeID(signature.Results().At(0).Type()),
			),
		}
	}
	return signature.Recv().Type(), nil
}

func hookHasTypeParameters(signature *types.Signature) bool {
	receiverParameters := signature.RecvTypeParams()
	methodParameters := signature.TypeParams()
	return (receiverParameters != nil && receiverParameters.Len() > 0) ||
		(methodParameters != nil && methodParameters.Len() > 0)
}

func lifecycleOwner(
	receiver types.Type,
	providerIndex map[string][]provider.Provider,
	stats *buildStats,
) (provider.Provider, *hookProblem) {
	candidates := providerIndex[semanticTypeKey(receiver)]
	var matches []provider.Provider
	for _, candidate := range candidates {
		stats.identityChecks++
		if types.Identical(receiver, candidate.Output) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 0 {
		return provider.Provider{}, &hookProblem{
			"receiver-provider",
			fmt.Sprintf(
				"no @Bean provider produces exact receiver type %s; pointer/value convenience, assignability, interface implementation, and method promotion do not establish lifecycle ownership",
				provider.TypeID(receiver),
			),
		}
	}
	if len(matches) != 1 {
		return provider.Provider{}, &hookProblem{
			"ambiguous-provider",
			fmt.Sprintf(
				"receiver type %s is produced by %d providers; lifecycle ownership requires exactly one",
				provider.TypeID(receiver),
				len(matches),
			),
		}
	}
	return matches[0], nil
}

func hookFailure(
	occurrence resolve.Occurrence,
	kind Kind,
	symbol load.Symbol,
	problem *hookProblem,
) (Hook, provider.Provider, *Diagnostic) {
	diagnostic := symbolDiagnostic(
		occurrence,
		symbol,
		problem.kind,
		fmt.Sprintf(
			"@On%s method %s is invalid: %s; accepted form is %s",
			kindTitle(kind),
			methodLabel(symbol),
			problem.reason,
			acceptedHookSignature,
		),
	)
	return Hook{}, provider.Provider{}, &diagnostic
}

func occurrenceKind(occurrence resolve.Occurrence) (Kind, bool) {
	if contribution, found := occurrence.Contribution(
		sdk.ContributionLifecycle,
	); found {
		switch contribution.Lifecycle.Phase {
		case sdk.LifecycleStart:
			return Start, true
		case sdk.LifecycleStop:
			return Stop, true
		default:
			return "", false
		}
	}
	return "", false
}

func semanticTypeKey(value types.Type) string {
	if value == nil {
		return "<invalid>"
	}
	value = types.Unalias(value)
	switch typed := value.(type) {
	case *types.Pointer:
		return "*" + semanticTypeKey(typed.Elem())
	case *types.Named:
		origin := typed.Origin()
		object := origin.Obj()
		var key strings.Builder
		key.WriteString("named:")
		if object.Pkg() != nil {
			key.WriteString(object.Pkg().Path() + ".")
		}
		key.WriteString(object.Name())
		if arguments := typed.TypeArgs(); arguments != nil && arguments.Len() > 0 {
			key.WriteString("[")
			for index := 0; index < arguments.Len(); index++ {
				if index > 0 {
					key.WriteString(",")
				}
				key.WriteString(semanticTypeKey(arguments.At(index)))
			}
			key.WriteString("]")
		}
		return key.String()
	default:
		return provider.TypeID(value)
	}
}

func methodLabel(symbol load.Symbol) string {
	if symbol.DisplayLabel != "" {
		return symbol.DisplayLabel
	}
	if symbol.ID != "" {
		return symbol.ID
	}
	return symbol.Name
}

func kindTitle(kind Kind) string {
	if kind == Start {
		return "Start"
	}
	return "Stop"
}

func occurrenceDiagnostic(occurrence resolve.Occurrence, methodID, kind, message string) Diagnostic {
	return Diagnostic{
		Position:         occurrence.DisplayPosition,
		PhysicalPosition: token.Position{Filename: occurrence.PhysicalFile, Offset: occurrence.PhysicalOffset},
		MethodID:         methodID,
		Kind:             kind,
		Message:          message,
	}
}

func symbolDiagnostic(occurrence resolve.Occurrence, symbol load.Symbol, kind, message string) Diagnostic {
	diagnostic := occurrenceDiagnostic(occurrence, symbol.ID, kind, message)
	if diagnostic.Position.Filename == "" {
		diagnostic.Position = symbol.Position
	}
	if diagnostic.PhysicalPosition.Filename == "" {
		diagnostic.PhysicalPosition = symbol.PhysicalPosition
	}
	return diagnostic
}

func hookDiagnostic(hook Hook, kind, message string) Diagnostic {
	return Diagnostic{
		Position:         hook.Position,
		PhysicalPosition: hook.PhysicalPosition,
		MethodID:         hook.MethodID,
		ProviderID:       hook.ProviderID,
		Kind:             kind,
		Message:          message,
	}
}

func sortDiagnostics(diagnostics []Diagnostic) {
	sort.SliceStable(diagnostics, func(i, j int) bool {
		left, right := diagnostics[i], diagnostics[j]
		if left.PhysicalPosition.Filename != right.PhysicalPosition.Filename {
			return left.PhysicalPosition.Filename < right.PhysicalPosition.Filename
		}
		if left.PhysicalPosition.Offset != right.PhysicalPosition.Offset {
			return left.PhysicalPosition.Offset < right.PhysicalPosition.Offset
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.MethodID != right.MethodID {
			return left.MethodID < right.MethodID
		}
		return left.Message < right.Message
	})
}
