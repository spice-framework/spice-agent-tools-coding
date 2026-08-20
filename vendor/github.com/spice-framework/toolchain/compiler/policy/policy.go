// @import { NamedInterface } from "github.com/spice-framework/spice/annotation/modulith"

// Package policy compiles service-method annotations into one deterministic,
// ordered decorator model. It validates typed source only and never invokes
// application methods or provider bodies.
//
// @NamedInterface("policy")
package policy

import (
	"fmt"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/spice/annotation/sdk"
	spicesecurity "github.com/spice-framework/spice/security"
	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/compiler/modulith"
	"github.com/spice-framework/toolchain/compiler/provider"
	"github.com/spice-framework/toolchain/compiler/resolve"
	"github.com/spice-framework/toolchain/compiler/signature"
)

const dataPackagePath = "github.com/spice-framework/spice/data"

var errorType = types.Universe.Lookup("error").Type()

// Method is one method of the interface exposed by a decorated service.
// Policy payloads are nil for an ordinary direct forwarding method.
type Method struct {
	Name             string
	MethodID         string
	Signature        *types.Signature
	Position         token.Position
	PhysicalPosition token.Position
	Transaction      *sdk.TransactionContribution
	Authorization    *sdk.AuthorizationContribution
	Cache            *sdk.CacheContribution
	Retry            *sdk.RetryContribution
	Observation      *sdk.ObservationContribution
}

// Decorated reports whether this method owns at least one generated policy.
func (method Method) Decorated() bool {
	return method.Transaction != nil || method.Authorization != nil ||
		method.Cache != nil || method.Retry != nil || method.Observation != nil
}

// Service is one singleton @Service exposed only through an explicit
// interface and decorated by generated direct calls.
type Service struct {
	Provider          provider.Provider
	Interface         provider.InterfaceBinding
	Module            string
	ManagerProviderID string
	methods           []Method
}

// Methods returns all exposed interface methods in stable name order.
func (service Service) Methods() []Method {
	return cloneMethods(service.methods)
}

// Clone returns a defensive copy suitable for crossing compiler package
// boundaries while retaining live go/types identities from the same Program.
func (service Service) Clone() Service {
	service.Provider.Dependencies = append(
		[]provider.Dependency(nil),
		service.Provider.Dependencies...,
	)
	for index := range service.Provider.Dependencies {
		service.Provider.Dependencies[index].Qualifiers = append(
			[]string(nil),
			service.Provider.Dependencies[index].Qualifiers...,
		)
	}
	service.Provider.Interfaces = append(
		[]provider.InterfaceBinding(nil),
		service.Provider.Interfaces...,
	)
	service.Provider.Aliases = append([]string(nil), service.Provider.Aliases...)
	service.Provider.Qualifiers = append([]string(nil), service.Provider.Qualifiers...)
	service.methods = cloneMethods(service.methods)
	return service
}

// Diagnostic is one source-positioned service policy contract failure.
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
	return fmt.Sprintf("%s:%d:%d: %s", position.Filename, position.Line, position.Column, diagnostic.Message)
}

// Catalog is immutable service policy metadata and stable diagnostics.
type Catalog struct {
	services    []Service
	diagnostics []Diagnostic
}

// Services returns a defensive copy in provider identity order.
func (catalog Catalog) Services() []Service {
	result := append([]Service(nil), catalog.services...)
	for index := range result {
		result[index] = catalog.services[index].Clone()
	}
	return result
}

// Diagnostics returns a defensive copy in source order.
func (catalog Catalog) Diagnostics() []Diagnostic {
	return append([]Diagnostic(nil), catalog.diagnostics...)
}

// Build validates generic service policies against the existing provider and
// module catalogs. Existing controller-only transaction, cache, and
// authorization annotations remain owned by their HTTP compiler stages.
func Build(
	program *load.Program,
	resolution resolve.Result,
	providers provider.Catalog,
	modules modulith.Model,
) Catalog {
	catalog := Catalog{}
	if program == nil {
		catalog.diagnostics = []Diagnostic{{Kind: "internal", Message: "method policy catalog requires a loaded program"}}
		return catalog
	}
	symbols := make(map[string]load.Symbol)
	functions := make(map[string]load.Symbol)
	for _, symbol := range program.Symbols() {
		symbols[symbol.ID] = symbol
		if symbol.Kind == load.SymbolFunction && symbol.Receiver == "" {
			functions[symbol.PackagePath+"\x00"+symbol.Name] = symbol
		}
	}
	providerItems := providers.Providers()
	grouped := groupOccurrences(resolution)
	byProvider := make(map[string][]compiledMethod)
	for _, methodID := range sortedKeys(grouped) {
		occurrences := grouped[methodID]
		if len(occurrences) == 0 {
			continue
		}
		symbol, found := symbols[methodID]
		if !found || symbol.Kind != load.SymbolMethod || symbol.Signature == nil || symbol.Signature.Recv() == nil {
			catalog.diagnostics = append(catalog.diagnostics, occurrenceDiagnostic(occurrences[0], "invalid-target", "method policies must target an ordinary typed method"))
			continue
		}
		owner, found := providerOwner(symbol.Signature.Recv().Type(), providerItems)
		if !found {
			if hasNewPolicy(occurrences) {
				catalog.diagnostics = append(catalog.diagnostics, occurrenceDiagnostic(occurrences[0], "receiver-provider", fmt.Sprintf("method policy %q requires exactly one managed provider for receiver %s", symbol.Name, provider.TypeID(symbol.Signature.Recv().Type()))))
			}
			continue
		}
		if owner.Role != "service" {
			if hasNewPolicy(occurrences) {
				catalog.diagnostics = append(catalog.diagnostics, occurrenceDiagnostic(occurrences[0], "service-only", fmt.Sprintf("@retry.Retryable and @observability.Observed are legal only on managed @Service methods; %q belongs to %s %q", symbol.Name, owner.Role, owner.Name)))
			}
			continue
		}
		method, diagnostics := compileMethod(symbol, owner, occurrences, signature.ContextType(program), functions)
		catalog.diagnostics = append(catalog.diagnostics, diagnostics...)
		if len(diagnostics) == 0 {
			byProvider[owner.SymbolID] = append(byProvider[owner.SymbolID], method)
		}
	}
	for _, item := range providerItems {
		methods := byProvider[item.SymbolID]
		if len(methods) == 0 {
			continue
		}
		service, diagnostics := compileService(item, methods, providerItems, modules)
		catalog.diagnostics = append(catalog.diagnostics, diagnostics...)
		if len(diagnostics) == 0 {
			catalog.services = append(catalog.services, service)
		}
	}
	if len(catalog.diagnostics) == 0 {
		catalog.diagnostics = rawConcreteInjectionDiagnostics(catalog.services, providerItems)
	}
	sort.SliceStable(catalog.services, func(left, right int) bool {
		return catalog.services[left].Provider.SymbolID < catalog.services[right].Provider.SymbolID
	})
	sortDiagnostics(catalog.diagnostics)
	return catalog
}

type compiledMethod struct {
	symbol load.Symbol
	method Method
}

func compileMethod(
	symbol load.Symbol,
	owner provider.Provider,
	occurrences []resolve.Occurrence,
	contextType types.Type,
	functions map[string]load.Symbol,
) (compiledMethod, []Diagnostic) {
	var diagnostics []Diagnostic
	method := Method{Name: symbol.Name, MethodID: symbol.ID, Signature: stripReceiver(symbol.Signature)}
	seen := make(map[sdk.ContributionKind]resolve.Occurrence)
	for _, occurrence := range occurrences {
		for _, kind := range policyKinds() {
			contribution, found := occurrence.Contribution(kind)
			if !found {
				continue
			}
			if first, duplicate := seen[kind]; duplicate {
				diagnostics = append(diagnostics, occurrenceDiagnostic(occurrence, "duplicate-policy", fmt.Sprintf("method %q repeats %s policy; first declaration is at %s", symbol.Name, kind, first.DisplayPosition)))
				continue
			}
			seen[kind] = occurrence
			method.Position = occurrence.DisplayPosition
			method.PhysicalPosition = physicalPosition(occurrence)
			setContribution(&method, contribution)
		}
	}
	first := occurrences[0]
	signatureValue := symbol.Signature
	switch {
	case owner.Scope != sdk.BeanScopeSingleton:
		diagnostics = append(diagnostics, occurrenceDiagnostic(first, "scoped-service", fmt.Sprintf("service method policy %q requires singleton scope; %q uses %s", symbol.Name, owner.Name, owner.Scope)))
	case !token.IsExported(symbol.Name):
		diagnostics = append(diagnostics, occurrenceDiagnostic(first, "unexported-method", fmt.Sprintf("service method policy %q must target an exported method", symbol.Name)))
	case signatureValue.Variadic():
		diagnostics = append(diagnostics, occurrenceDiagnostic(first, "variadic-method", fmt.Sprintf("service method policy %q must be non-variadic", symbol.Name)))
	case hasTypeParameters(signatureValue):
		diagnostics = append(diagnostics, occurrenceDiagnostic(first, "generic-method", fmt.Sprintf("service method policy %q cannot use receiver or method type parameters", symbol.Name)))
	case signatureValue.Params().Len() == 0 || contextType == nil || !types.Identical(signatureValue.Params().At(0).Type(), contextType):
		diagnostics = append(diagnostics, occurrenceDiagnostic(first, "context-parameter", fmt.Sprintf("service method policy %q requires exact context.Context as parameter 0", symbol.Name)))
	case signatureValue.Results().Len() == 0 || !types.Identical(signatureValue.Results().At(signatureValue.Results().Len()-1).Type(), errorType):
		diagnostics = append(diagnostics, occurrenceDiagnostic(first, "error-result", fmt.Sprintf("service method policy %q requires predeclared error as its final result", symbol.Name)))
	case !signatureNameable(stripReceiver(signatureValue)):
		diagnostics = append(diagnostics, occurrenceDiagnostic(first, "unnameable-signature", fmt.Sprintf("service method policy %q uses a type that target-scoped generated Go cannot name", symbol.Name)))
	}
	if method.Cache != nil {
		if signatureValue.Results().Len() != 2 {
			diagnostics = append(diagnostics, occurrenceDiagnostic(seen[sdk.ContributionCache], "cache-result", fmt.Sprintf("@cache.Cacheable service method %q must return exactly (value, error)", symbol.Name)))
		}
		for index := 1; index < signatureValue.Params().Len(); index++ {
			if !types.Comparable(signatureValue.Params().At(index).Type()) {
				diagnostics = append(diagnostics, occurrenceDiagnostic(seen[sdk.ContributionCache], "cache-key", fmt.Sprintf("@cache.Cacheable service method %q parameter %d type %s is not comparable", symbol.Name, index, provider.TypeID(signatureValue.Params().At(index).Type()))))
			}
		}
	}
	if method.Authorization != nil {
		authorization := method.Authorization
		if !authorization.Authenticated && len(authorization.AnyRoles) == 0 && len(authorization.AllRoles) == 0 && len(authorization.AllScopes) == 0 && authorization.Expression == "" {
			diagnostics = append(diagnostics, occurrenceDiagnostic(seen[sdk.ContributionAuthorization], "empty-authorization", fmt.Sprintf("@security.Authorize service method %q must require authentication, a role, a scope, or an expression", symbol.Name)))
		} else if authorization.Expression != "" {
			if err := spicesecurity.ValidateExpression(authorization.Expression); err != nil {
				diagnostics = append(diagnostics, occurrenceDiagnostic(seen[sdk.ContributionAuthorization], "authorization-expression", fmt.Sprintf("@security.Authorize service method %q expression is invalid: %v", symbol.Name, err)))
			}
		}
	}
	if method.Retry != nil && method.Retry.Classifier != "" {
		classifier := method.Retry.Classifier
		candidate, found := functions[symbol.PackagePath+"\x00"+classifier]
		if !found || !token.IsExported(classifier) || !retryClassifierSignature(candidate.Signature) {
			diagnostics = append(diagnostics, occurrenceDiagnostic(seen[sdk.ContributionRetry], "retry-classifier", fmt.Sprintf("@retry.Retryable service method %q classifier %q must name an exported same-package func(error) bool", symbol.Name, classifier)))
		}
	}
	return compiledMethod{symbol: symbol, method: method}, diagnostics
}

func compileService(
	item provider.Provider,
	compiled []compiledMethod,
	providers []provider.Provider,
	modules modulith.Model,
) (Service, []Diagnostic) {
	first := compiled[0].method
	var candidates []provider.InterfaceBinding
	for _, binding := range item.Interfaces {
		if interfaceContains(binding.Type, compiled) {
			candidates = append(candidates, binding)
		}
	}
	if len(item.Interfaces) != 1 || len(candidates) != 1 {
		return Service{}, []Diagnostic{{
			Position: first.Position, PhysicalPosition: first.PhysicalPosition,
			MethodID: first.MethodID, ProviderID: item.SymbolID, Kind: "interface-binding",
			Message: fmt.Sprintf("managed @Service %q has method policies and requires exactly one explicit @Implements interface containing every decorated method; found %d", item.Name, len(item.Interfaces)),
		}}
	}
	methods, problem := interfaceMethods(candidates[0].Type, compiled)
	if problem != "" {
		return Service{}, []Diagnostic{{
			Position: first.Position, PhysicalPosition: first.PhysicalPosition,
			MethodID: first.MethodID, ProviderID: item.SymbolID, Kind: "interface-method",
			Message: problem,
		}}
	}
	managerID := ""
	if containsTransaction(methods) {
		var managers []provider.Provider
		for _, candidate := range providers {
			if pointerNamedType(candidate.Output, dataPackagePath, "Manager") {
				managers = append(managers, candidate)
			}
		}
		if len(managers) != 1 {
			return Service{}, []Diagnostic{{
				Position: first.Position, PhysicalPosition: first.PhysicalPosition,
				MethodID: first.MethodID, ProviderID: item.SymbolID, Kind: "transaction-manager",
				Message: fmt.Sprintf("transactional service %q requires exactly one provider for *data.Manager; found %d", item.Name, len(managers)),
			}}
		}
		managerID = managers[0].SymbolID
	}
	module := item.PackagePath
	if owner, found := modules.Owner(item.PackagePath); found {
		module = owner.ID
	}
	return Service{Provider: item, Interface: candidates[0], Module: module, ManagerProviderID: managerID, methods: methods}, nil
}

func groupOccurrences(resolution resolve.Result) map[string][]resolve.Occurrence {
	result := make(map[string][]resolve.Occurrence)
	for _, occurrence := range resolution.Occurrences {
		if occurrence.Target != annotation.TargetMethod {
			continue
		}
		for _, kind := range policyKinds() {
			if occurrence.HasContribution(kind) {
				result[occurrence.SymbolID] = append(result[occurrence.SymbolID], occurrence)
				break
			}
		}
	}
	for _, values := range result {
		sort.SliceStable(values, func(left, right int) bool {
			if values[left].PhysicalFile != values[right].PhysicalFile {
				return values[left].PhysicalFile < values[right].PhysicalFile
			}
			return values[left].PhysicalOffset < values[right].PhysicalOffset
		})
	}
	return result
}

func policyKinds() []sdk.ContributionKind {
	return []sdk.ContributionKind{sdk.ContributionObservation, sdk.ContributionAuthorization, sdk.ContributionCache, sdk.ContributionRetry, sdk.ContributionTransaction}
}

func hasNewPolicy(occurrences []resolve.Occurrence) bool {
	for _, occurrence := range occurrences {
		if occurrence.HasContribution(sdk.ContributionRetry) || occurrence.HasContribution(sdk.ContributionObservation) {
			return true
		}
	}
	return false
}

func setContribution(method *Method, contribution sdk.Contribution) {
	switch contribution.Kind {
	case sdk.ContributionTransaction:
		value := *contribution.Transaction
		method.Transaction = &value
	case sdk.ContributionAuthorization:
		value := *contribution.Authorization
		value.AnyRoles = append([]string(nil), value.AnyRoles...)
		value.AllRoles = append([]string(nil), value.AllRoles...)
		value.AllScopes = append([]string(nil), value.AllScopes...)
		method.Authorization = &value
	case sdk.ContributionCache:
		value := *contribution.Cache
		method.Cache = &value
	case sdk.ContributionRetry:
		value := *contribution.Retry
		method.Retry = &value
	case sdk.ContributionObservation:
		value := *contribution.Observation
		method.Observation = &value
	case sdk.ContributionApplication,
		sdk.ContributionStereotype,
		sdk.ContributionInterface,
		sdk.ContributionProvider,
		sdk.ContributionBeanMetadata,
		sdk.ContributionConfiguration,
		sdk.ContributionEnum,
		sdk.ContributionController,
		sdk.ContributionRoute,
		sdk.ContributionModule,
		sdk.ContributionNamedInterface,
		sdk.ContributionLifecycle,
		sdk.ContributionBootstrap,
		sdk.ContributionSchedule,
		sdk.ContributionAsync,
		sdk.ContributionEventTopic,
		sdk.ContributionEventListener,
		sdk.ContributionGeneratedFile:
		// Non-policy contributions are deliberately ignored by this projection.
	}
}

func interfaceContains(value types.Type, methods []compiledMethod) bool {
	underlying, ok := types.Unalias(value).Underlying().(*types.Interface)
	if !ok {
		return false
	}
	underlying.Complete()
	set := types.NewMethodSet(value)
	for _, method := range methods {
		selection := set.Lookup(nil, method.symbol.Name)
		if selection == nil {
			return false
		}
		selectionSignature, ok := selection.Obj().Type().(*types.Signature)
		if !ok || !types.Identical(stripReceiver(method.symbol.Signature), stripReceiver(selectionSignature)) {
			return false
		}
	}
	return true
}

func interfaceMethods(value types.Type, decorated []compiledMethod) ([]Method, string) {
	byName := make(map[string]Method, len(decorated))
	for _, item := range decorated {
		byName[item.method.Name] = item.method
	}
	set := types.NewMethodSet(value)
	result := make([]Method, 0, set.Len())
	for index := 0; index < set.Len(); index++ {
		selection := set.At(index)
		function, ok := selection.Obj().(*types.Func)
		if !ok || !function.Exported() {
			return nil, fmt.Sprintf("generated service interface %s contains a method that cannot be called from target-scoped generated Go", provider.TypeID(value))
		}
		signatureValue, ok := function.Type().(*types.Signature)
		if !ok || !signatureNameable(stripReceiver(signatureValue)) {
			return nil, fmt.Sprintf("generated service interface method %s.%s uses a type that target-scoped generated Go cannot name", provider.TypeID(value), function.Name())
		}
		method, decoratedMethod := byName[function.Name()]
		if !decoratedMethod {
			method = Method{Name: function.Name(), Signature: stripReceiver(signatureValue)}
		} else {
			method.Signature = stripReceiver(signatureValue)
		}
		result = append(result, method)
	}
	sort.SliceStable(result, func(left, right int) bool { return result[left].Name < result[right].Name })
	return result, ""
}

func rawConcreteInjectionDiagnostics(services []Service, providers []provider.Provider) []Diagnostic {
	byType := make(map[string]Service, len(services))
	for _, service := range services {
		byType[service.Provider.OutputTypeID] = service
	}
	var diagnostics []Diagnostic
	for _, consumer := range providers {
		for _, dependency := range consumer.Dependencies {
			service, found := byType[provider.TypeID(dependency.MatchType())]
			if !found {
				continue
			}
			diagnostics = append(diagnostics, Diagnostic{
				Position: dependency.Position, PhysicalPosition: dependency.PhysicalPosition,
				ProviderID: consumer.SymbolID, Kind: "raw-concrete-injection",
				Message: fmt.Sprintf("provider %q injects raw concrete service %s and would bypass generated method policies; inject explicit interface %s instead", consumer.Name, service.Provider.OutputTypeID, service.Interface.TypeID),
			})
		}
	}
	return diagnostics
}

func providerOwner(receiver types.Type, providers []provider.Provider) (provider.Provider, bool) {
	var result provider.Provider
	count := 0
	for _, candidate := range providers {
		if types.Identical(receiver, candidate.Output) {
			result = candidate
			count++
		}
	}
	return result, count == 1
}

func stripReceiver(value *types.Signature) *types.Signature {
	if value == nil {
		return nil
	}
	return types.NewSignatureType(nil, nil, nil, value.Params(), value.Results(), value.Variadic())
}

func hasTypeParameters(value *types.Signature) bool {
	return (value.RecvTypeParams() != nil && value.RecvTypeParams().Len() != 0) ||
		(value.TypeParams() != nil && value.TypeParams().Len() != 0)
}

func signatureNameable(value *types.Signature) bool {
	if value == nil {
		return false
	}
	for _, tuple := range []*types.Tuple{value.Params(), value.Results()} {
		for index := 0; index < tuple.Len(); index++ {
			if !generatedNameable(tuple.At(index).Type()) {
				return false
			}
		}
	}
	return true
}

func generatedNameable(value types.Type) bool {
	switch typed := value.(type) {
	case *types.Alias:
		return typed.Obj() != nil && (typed.Obj().Pkg() == nil || typed.Obj().Exported()) && typeArgumentsNameable(typed.TypeArgs())
	case *types.Named:
		return typed.Obj() != nil && (typed.Obj().Pkg() == nil || typed.Obj().Exported()) && typeArgumentsNameable(typed.TypeArgs())
	case *types.Basic:
		return true
	case *types.Pointer:
		return generatedNameable(typed.Elem())
	case *types.Slice:
		return generatedNameable(typed.Elem())
	case *types.Array:
		return generatedNameable(typed.Elem())
	case *types.Map:
		return generatedNameable(typed.Key()) && generatedNameable(typed.Elem())
	case *types.Chan:
		return generatedNameable(typed.Elem())
	case *types.Interface:
		return typed.Empty()
	default:
		return false
	}
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

func retryClassifierSignature(value *types.Signature) bool {
	return value != nil && value.Recv() == nil && !value.Variadic() &&
		value.Params().Len() == 1 && types.Identical(value.Params().At(0).Type(), errorType) &&
		value.Results().Len() == 1 && types.Identical(value.Results().At(0).Type(), types.Typ[types.Bool])
}

func containsTransaction(methods []Method) bool {
	for _, method := range methods {
		if method.Transaction != nil {
			return true
		}
	}
	return false
}

func pointerNamedType(value types.Type, packagePath, name string) bool {
	pointer, ok := types.Unalias(value).(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := types.Unalias(pointer.Elem()).(*types.Named)
	return ok && named.Obj() != nil && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == packagePath && named.Obj().Name() == name
}

func cloneMethods(values []Method) []Method {
	result := append([]Method(nil), values...)
	for index := range result {
		if values[index].Transaction != nil {
			value := *values[index].Transaction
			result[index].Transaction = &value
		}
		if values[index].Authorization != nil {
			value := *values[index].Authorization
			value.AnyRoles = append([]string(nil), value.AnyRoles...)
			value.AllRoles = append([]string(nil), value.AllRoles...)
			value.AllScopes = append([]string(nil), value.AllScopes...)
			result[index].Authorization = &value
		}
		if values[index].Cache != nil {
			value := *values[index].Cache
			result[index].Cache = &value
		}
		if values[index].Retry != nil {
			value := *values[index].Retry
			result[index].Retry = &value
		}
		if values[index].Observation != nil {
			value := *values[index].Observation
			result[index].Observation = &value
		}
	}
	return result
}

func sortedKeys[T any](values map[string]T) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func occurrenceDiagnostic(occurrence resolve.Occurrence, kind, message string) Diagnostic {
	return Diagnostic{Position: occurrence.DisplayPosition, PhysicalPosition: physicalPosition(occurrence), MethodID: occurrence.SymbolID, Kind: kind, Message: message}
}

func physicalPosition(occurrence resolve.Occurrence) token.Position {
	return token.Position{Filename: occurrence.PhysicalFile, Offset: occurrence.PhysicalOffset}
}

func sortDiagnostics(values []Diagnostic) {
	sort.SliceStable(values, func(left, right int) bool {
		if values[left].PhysicalPosition.Filename != values[right].PhysicalPosition.Filename {
			return values[left].PhysicalPosition.Filename < values[right].PhysicalPosition.Filename
		}
		if values[left].PhysicalPosition.Offset != values[right].PhysicalPosition.Offset {
			return values[left].PhysicalPosition.Offset < values[right].PhysicalPosition.Offset
		}
		if values[left].Kind != values[right].Kind {
			return values[left].Kind < values[right].Kind
		}
		if values[left].ProviderID != values[right].ProviderID {
			return values[left].ProviderID < values[right].ProviderID
		}
		return strings.Compare(values[left].Message, values[right].Message) < 0
	})
}
