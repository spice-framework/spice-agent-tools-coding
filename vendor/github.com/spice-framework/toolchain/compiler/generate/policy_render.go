package generate

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"go/types"
	"sort"
	"strconv"
	"strings"
	"time"

	compilercache "github.com/spice-framework/toolchain/compiler/cache"
	compilerpolicy "github.com/spice-framework/toolchain/compiler/policy"
	"github.com/spice-framework/toolchain/compiler/provider"
)

func servicePolicyIndex(values []compilerpolicy.Service) map[string]compilerpolicy.Service {
	result := make(map[string]compilerpolicy.Service, len(values))
	for _, service := range values {
		result[service.Provider.SymbolID] = service
	}
	return result
}

func policiesUseAuthorization(values []compilerpolicy.Service) bool {
	return policiesUse(values, func(method compilerpolicy.Method) bool { return method.Authorization != nil })
}

func policiesUseTransactions(values []compilerpolicy.Service) bool {
	return policiesUse(values, func(method compilerpolicy.Method) bool { return method.Transaction != nil })
}

func policiesUseCache(values []compilerpolicy.Service) bool {
	return policiesUse(values, func(method compilerpolicy.Method) bool { return method.Cache != nil })
}

func policiesUseRetry(values []compilerpolicy.Service) bool {
	return policiesUse(values, func(method compilerpolicy.Method) bool { return method.Retry != nil })
}

func policiesUseObservation(values []compilerpolicy.Service) bool {
	return policiesUse(values, func(method compilerpolicy.Method) bool { return method.Observation != nil })
}

func policiesUse(
	values []compilerpolicy.Service,
	matches func(compilerpolicy.Method) bool,
) bool {
	for _, service := range values {
		for _, method := range service.Methods() {
			if matches(method) {
				return true
			}
		}
	}
	return false
}

func serviceCacheBoundaries(values []compilerpolicy.Service) []compilercache.Boundary {
	var result []compilercache.Boundary
	for _, service := range values {
		for _, method := range service.Methods() {
			if method.Cache == nil {
				continue
			}
			result = append(result, compilercache.Boundary{
				RouteID: method.MethodID, RouteName: method.Name,
				CacheName: method.Cache.Name, Module: service.Module,
				Position: method.Position, PhysicalPosition: method.PhysicalPosition,
			})
		}
	}
	sort.SliceStable(result, func(left, right int) bool {
		return result[left].RouteID < result[right].RouteID
	})
	return result
}

func policyExposureVariables(
	providers []provider.Provider,
	policies []compilerpolicy.Service,
	localVariables map[string]string,
	dependencyVariables map[string]string,
) (map[string]string, map[string]string) {
	local := make(map[string]string, len(localVariables))
	dependencies := make(map[string]string, len(dependencyVariables))
	used := make(map[string]struct{}, len(providers)+len(policies))
	for providerID, variable := range localVariables {
		local[providerID] = variable
		used[variable] = struct{}{}
	}
	for providerID, variable := range dependencyVariables {
		dependencies[providerID] = variable
	}
	for _, service := range policies {
		base := localVariables[service.Provider.SymbolID] + "Policy"
		candidate := base
		for suffix := 2; ; suffix++ {
			if _, found := used[candidate]; !found {
				break
			}
			candidate = base + strconv.Itoa(suffix)
		}
		used[candidate] = struct{}{}
		local[service.Provider.SymbolID] = candidate
		dependencies[service.Provider.SymbolID] = "dependencies." + candidate
	}
	return local, dependencies
}

func writeServicePolicyDeclarations(
	source *bytes.Buffer,
	services []compilerpolicy.Service,
	aliases map[string]string,
) {
	for _, service := range services {
		writeServicePolicyDeclaration(source, service, aliases)
	}
}

func writeServicePolicyDeclaration(
	source *bytes.Buffer,
	service compilerpolicy.Service,
	aliases map[string]string,
) {
	typeName := servicePolicyTypeName(service)
	methods := service.Methods()
	for _, method := range methods {
		if method.Cache != nil && method.Signature.Params().Len() > 2 {
			writeServiceCacheKeyType(source, typeName, method, aliases)
		}
	}
	fmt.Fprintf(source, "type %s struct {\n", typeName)
	fmt.Fprintf(source, "\ttarget %s\n", renderedType(service.Provider.Output, aliases))
	if service.ManagerProviderID != "" {
		source.WriteString("\tmanager *spicedata.Manager\n")
	}
	if serviceUsesAuthorization(service) {
		source.WriteString("\tauthorizer *spicesecurity.Authorizer\n")
	}
	if serviceUsesRetry(service) {
		source.WriteString("\tretryObservers []spiceretry.Observer\n")
	}
	if serviceUsesObservation(service) {
		source.WriteString("\tmethodObservers []spiceobservability.MethodObserver\n")
	}
	for _, method := range methods {
		field := policyMethodField(method)
		if method.Authorization != nil {
			fmt.Fprintf(source, "\t%sAuthorization spicesecurity.Policy\n", field)
		}
		if method.Cache != nil {
			fmt.Fprintf(
				source,
				"\t%sCache *spicecache.Memory[%s, %s]\n",
				field,
				serviceCacheKeyType(typeName, method, aliases),
				renderedType(method.Signature.Results().At(0).Type(), aliases),
			)
			fmt.Fprintf(source, "\t%sCacheTTL time.Duration\n", field)
		}
	}
	source.WriteString("}\n\n")
	writeServicePolicyConstructor(source, service, typeName, aliases)
	if serviceUsesRetry(service) {
		fmt.Fprintf(source, "func (decorator *%s) observeRetry(ctx context.Context, observation spiceretry.Observation) {\n", typeName)
		source.WriteString("\tfor _, observer := range decorator.retryObservers {\n")
		source.WriteString("\t\tobserver(ctx, observation)\n")
		source.WriteString("\t}\n")
		source.WriteString("}\n\n")
	}
	for _, method := range methods {
		writeServicePolicyMethod(source, service, typeName, method, aliases)
	}
}

func writeServiceCacheKeyType(
	source *bytes.Buffer,
	typeName string,
	method compilerpolicy.Method,
	aliases map[string]string,
) {
	fmt.Fprintf(source, "type %s struct {\n", serviceCacheKeyType(typeName, method, aliases))
	for index := 1; index < method.Signature.Params().Len(); index++ {
		fmt.Fprintf(
			source,
			"\tP%d %s\n",
			index,
			renderedType(method.Signature.Params().At(index).Type(), aliases),
		)
	}
	source.WriteString("}\n\n")
}

func writeServicePolicyConstructor(
	source *bytes.Buffer,
	service compilerpolicy.Service,
	typeName string,
	aliases map[string]string,
) {
	constructor := "new" + exportedGeneratedIdentifier(typeName, "ServicePolicies")
	fmt.Fprintf(source, "func %s(\n", constructor)
	fmt.Fprintf(source, "\ttarget %s,\n", renderedType(service.Provider.Output, aliases))
	if service.ManagerProviderID != "" {
		source.WriteString("\tmanager *spicedata.Manager,\n")
	}
	if serviceUsesAuthorization(service) {
		source.WriteString("\tauthorizer *spicesecurity.Authorizer,\n")
	}
	source.WriteString("\toptions ApplicationOptions,\n")
	if serviceUsesCache(service) {
		source.WriteString("\tconfigurationSnapshot spiceconfig.Snapshot,\n")
	}
	fmt.Fprintf(source, ") (%s, error) {\n", renderedType(service.Interface.Type, aliases))
	fmt.Fprintf(source, "\tdecorator := &%s{target: target", typeName)
	if service.ManagerProviderID != "" {
		source.WriteString(", manager: manager")
	}
	if serviceUsesAuthorization(service) {
		source.WriteString(", authorizer: authorizer")
	}
	if serviceUsesRetry(service) {
		source.WriteString(", retryObservers: append([]spiceretry.Observer(nil), options.RetryObservers...)")
	}
	if serviceUsesObservation(service) {
		source.WriteString(", methodObservers: append([]spiceobservability.MethodObserver(nil), options.MethodObservers...)")
	}
	source.WriteString("}\n")
	if serviceUsesRetry(service) {
		source.WriteString("\tfor index, observer := range decorator.retryObservers {\n")
		source.WriteString("\t\tif observer == nil {\n")
		source.WriteString("\t\t\treturn nil, fmt.Errorf(\"construct service policies: retry observer %d is nil\", index)\n")
		source.WriteString("\t\t}\n\t}\n")
	}
	if serviceUsesObservation(service) {
		source.WriteString("\tfor index, observer := range decorator.methodObservers {\n")
		source.WriteString("\t\tif observer == nil {\n")
		source.WriteString("\t\t\treturn nil, fmt.Errorf(\"construct service policies: method observer %d is nil\", index)\n")
		source.WriteString("\t\t}\n\t}\n")
	}
	for _, method := range service.Methods() {
		field := policyMethodField(method)
		if method.Authorization != nil {
			writeServiceAuthorizationConstruction(source, service, method, field)
		}
		if method.Cache != nil {
			writeServiceCacheConstruction(source, service, typeName, method, field, aliases)
		}
	}
	source.WriteString("\treturn decorator, nil\n")
	source.WriteString("}\n\n")
}

func writeServiceAuthorizationConstruction(
	source *bytes.Buffer,
	service compilerpolicy.Service,
	method compilerpolicy.Method,
	field string,
) {
	authorization := method.Authorization
	fmt.Fprintf(source, "\t%sAuthorization, err := spicesecurity.NewPolicy(spicesecurity.PolicySpec{\n", field)
	source.WriteString("\t\tDefinition: spicesecurity.Definition{\n")
	fmt.Fprintf(source, "\t\t\tID: %s,\n", strconv.Quote(method.MethodID))
	fmt.Fprintf(source, "\t\t\tModule: %s,\n", strconv.Quote(service.Module))
	source.WriteString("\t\t},\n")
	if authorization.Authenticated {
		source.WriteString("\t\tAuthenticated: true,\n")
	}
	writeStringSliceField(source, "AnyRoles", authorization.AnyRoles, 2)
	writeStringSliceField(source, "AllRoles", authorization.AllRoles, 2)
	writeStringSliceField(source, "AllScopes", authorization.AllScopes, 2)
	if authorization.Expression != "" {
		fmt.Fprintf(source, "\t\tExpression: %s,\n", strconv.Quote(authorization.Expression))
	}
	source.WriteString("\t})\n")
	source.WriteString("\tif err != nil {\n")
	fmt.Fprintf(source, "\t\treturn nil, fmt.Errorf(%s, err)\n", strconv.Quote("construct authorization policy "+method.MethodID+": %w"))
	source.WriteString("\t}\n")
	fmt.Fprintf(source, "\tdecorator.%sAuthorization = %sAuthorization\n", field, field)
}

func writeStringSliceField(source *bytes.Buffer, name string, values []string, indent int) {
	if len(values) == 0 {
		return
	}
	prefix := strings.Repeat("\t", indent)
	fmt.Fprintf(source, "%s%s: []string{", prefix, name)
	for index, value := range values {
		if index != 0 {
			source.WriteString(", ")
		}
		source.WriteString(strconv.Quote(value))
	}
	source.WriteString("},\n")
}

func writeServiceCacheConstruction(
	source *bytes.Buffer,
	service compilerpolicy.Service,
	typeName string,
	method compilerpolicy.Method,
	field string,
	aliases map[string]string,
) {
	capacity := field + "CacheCapacity"
	ttl := field + "CacheTTL"
	fmt.Fprintf(source, "\t%s, err := configurationSnapshot.Integer(%s)\n", capacity, strconv.Quote(cacheCapacityKey(method.Cache.Name)))
	source.WriteString("\tif err != nil {\n")
	fmt.Fprintf(source, "\t\treturn nil, fmt.Errorf(%s, err)\n", strconv.Quote("decode capacity for cache "+method.Cache.Name+": %w"))
	source.WriteString("\t}\n")
	fmt.Fprintf(source, "\tif %s < 1 || uint64(%s) > uint64(^uint(0)>>1) {\n", capacity, capacity)
	fmt.Fprintf(source, "\t\treturn nil, fmt.Errorf(%s)\n", strconv.Quote("decode capacity for cache "+method.Cache.Name+": value must fit a positive int"))
	source.WriteString("\t}\n")
	fmt.Fprintf(source, "\t%s, err := configurationSnapshot.Duration(%s)\n", ttl, strconv.Quote(cacheTTLKey(method.Cache.Name)))
	source.WriteString("\tif err != nil {\n")
	fmt.Fprintf(source, "\t\treturn nil, fmt.Errorf(%s, err)\n", strconv.Quote("decode TTL for cache "+method.Cache.Name+": %w"))
	source.WriteString("\t}\n")
	fmt.Fprintf(source, "\tif %s < 0 {\n", ttl)
	fmt.Fprintf(source, "\t\treturn nil, fmt.Errorf(%s)\n", strconv.Quote("decode TTL for cache "+method.Cache.Name+": duration must not be negative"))
	source.WriteString("\t}\n")
	fmt.Fprintf(
		source,
		"\t%sCache, err := spicecache.NewMemory[%s, %s](\n",
		field,
		serviceCacheKeyType(typeName, method, aliases),
		renderedType(method.Signature.Results().At(0).Type(), aliases),
	)
	source.WriteString("\t\tspicecache.Definition{\n")
	fmt.Fprintf(source, "\t\t\tID: %s,\n", strconv.Quote(method.Cache.Name))
	fmt.Fprintf(source, "\t\t\tModule: %s,\n", strconv.Quote(service.Module))
	source.WriteString("\t\t},\n")
	fmt.Fprintf(source, "\t\tint(%s),\n", capacity)
	source.WriteString("\t\toptions.CacheClock,\n")
	source.WriteString("\t\toptions.CacheObservers...,\n")
	source.WriteString("\t)\n")
	source.WriteString("\tif err != nil {\n")
	fmt.Fprintf(source, "\t\treturn nil, fmt.Errorf(%s, err)\n", strconv.Quote("construct generated cache "+method.Cache.Name+": %w"))
	source.WriteString("\t}\n")
	fmt.Fprintf(source, "\tdecorator.%sCache = %sCache\n", field, field)
	fmt.Fprintf(source, "\tdecorator.%sCacheTTL = %s\n", field, ttl)
}

func writeServicePolicyConstruction(
	source *bytes.Buffer,
	service compilerpolicy.Service,
	targetVariable string,
	exposedVariable string,
	providerVariables map[string]string,
	features commandFeatures,
) {
	constructor := "new" + exportedGeneratedIdentifier(servicePolicyTypeName(service), "ServicePolicies")
	fmt.Fprintf(source, "\t%s, err := %s(\n", exposedVariable, constructor)
	fmt.Fprintf(source, "\t\t%s,\n", targetVariable)
	if service.ManagerProviderID != "" {
		fmt.Fprintf(source, "\t\t%s,\n", providerVariables[service.ManagerProviderID])
	}
	if serviceUsesAuthorization(service) {
		source.WriteString("\t\tauthorizer,\n")
	}
	source.WriteString("\t\toptions,\n")
	if serviceUsesCache(service) {
		source.WriteString("\t\tconfigurationSnapshot,\n")
	}
	source.WriteString("\t)\n")
	source.WriteString("\tif err != nil {\n")
	fmt.Fprintf(
		source,
		"\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(%s, err))\n",
		strconv.Quote("construct generated method policies for service "+service.Provider.Name+": %w"),
	)
	source.WriteString("\t}\n")
	fmt.Fprintf(source, "\t_ = %s\n", exposedVariable)
	_ = features
}

func writeServicePolicyMethod(
	source *bytes.Buffer,
	service compilerpolicy.Service,
	typeName string,
	method compilerpolicy.Method,
	aliases map[string]string,
) {
	writePolicyMethodSignature(source, typeName, method, aliases)
	if !method.Decorated() {
		writeDirectForward(source, method)
		source.WriteString("}\n\n")
		return
	}
	writeDecoratedMethodBody(source, service, typeName, method, aliases)
	source.WriteString("}\n\n")
}

func writePolicyMethodSignature(
	source *bytes.Buffer,
	typeName string,
	method compilerpolicy.Method,
	aliases map[string]string,
) {
	fmt.Fprintf(source, "func (decorator *%s) %s(", typeName, method.Name)
	for index := 0; index < method.Signature.Params().Len(); index++ {
		if index != 0 {
			source.WriteString(", ")
		}
		name := "parameter" + strconv.Itoa(index)
		if method.Decorated() && index == 0 {
			name = "ctx"
		}
		fmt.Fprintf(source, "%s %s", name, renderedType(method.Signature.Params().At(index).Type(), aliases))
	}
	source.WriteString(")")
	if method.Decorated() {
		source.WriteString(" (")
		for index := 0; index < method.Signature.Results().Len(); index++ {
			if index != 0 {
				source.WriteString(", ")
			}
			name := "result" + strconv.Itoa(index)
			if index == method.Signature.Results().Len()-1 {
				name = "resultErr"
			}
			fmt.Fprintf(source, "%s %s", name, renderedType(method.Signature.Results().At(index).Type(), aliases))
		}
		source.WriteString(")")
	} else {
		writeUnnamedResults(source, method.Signature.Results(), aliases)
	}
	source.WriteString(" {\n")
}

func writeUnnamedResults(source *bytes.Buffer, results *types.Tuple, aliases map[string]string) {
	if results.Len() == 0 {
		return
	}
	if results.Len() == 1 {
		source.WriteString(" ")
		source.WriteString(renderedType(results.At(0).Type(), aliases))
		return
	}
	source.WriteString(" (")
	for index := 0; index < results.Len(); index++ {
		if index != 0 {
			source.WriteString(", ")
		}
		source.WriteString(renderedType(results.At(index).Type(), aliases))
	}
	source.WriteString(")")
}

func writeDirectForward(source *bytes.Buffer, method compilerpolicy.Method) {
	if method.Signature.Results().Len() != 0 {
		source.WriteString("\treturn ")
	} else {
		source.WriteString("\t")
	}
	fmt.Fprintf(source, "decorator.target.%s(", method.Name)
	writePolicyArguments(source, method, "", false)
	source.WriteString(")\n")
}

func writeDecoratedMethodBody(
	source *bytes.Buffer,
	service compilerpolicy.Service,
	typeName string,
	method compilerpolicy.Method,
	aliases map[string]string,
) {
	field := policyMethodField(method)
	source.WriteString("\tinvoke := func(current context.Context) error {\n")
	if method.Signature.Results().Len() > 1 {
		for index := 0; index < method.Signature.Results().Len()-1; index++ {
			if index != 0 {
				source.WriteString(", ")
			}
			fmt.Fprintf(source, "result%d", index)
		}
		source.WriteString(", ")
	}
	source.WriteString("resultErr = decorator.target.")
	source.WriteString(method.Name)
	source.WriteString("(")
	writePolicyArguments(source, method, "current", true)
	source.WriteString(")\n\t\treturn resultErr\n\t}\n")
	if method.Transaction != nil {
		source.WriteString("\ttransactionNext := invoke\n")
		source.WriteString("\tinvoke = func(current context.Context) error {\n")
		source.WriteString("\t\treturn decorator.manager.Within(current, spicedata.Definition{\n")
		fmt.Fprintf(source, "\t\t\tID: %s,\n", strconv.Quote(method.MethodID))
		fmt.Fprintf(source, "\t\t\tModule: %s,\n", strconv.Quote(service.Module))
		fmt.Fprintf(source, "\t\t\tIsolation: %s,\n", serviceIsolationLevel(method.Transaction.Isolation))
		if method.Transaction.ReadOnly {
			source.WriteString("\t\t\tReadOnly: true,\n")
		}
		source.WriteString("\t\t}, func(transactionContext context.Context, _ spicedata.Executor) error {\n")
		source.WriteString("\t\t\treturn transactionNext(transactionContext)\n")
		source.WriteString("\t\t})\n\t}\n")
	}
	if method.Retry != nil {
		retry := method.Retry
		source.WriteString("\tretryNext := invoke\n")
		source.WriteString("\tinvoke = func(current context.Context) error {\n")
		source.WriteString("\t\treturn spiceretry.Run(current, spiceretry.Policy{\n")
		fmt.Fprintf(source, "\t\t\tID: %s,\n", strconv.Quote(method.MethodID))
		fmt.Fprintf(source, "\t\t\tModule: %s,\n", strconv.Quote(service.Module))
		fmt.Fprintf(source, "\t\t\tMaxAttempts: %d,\n", retry.MaxAttempts)
		fmt.Fprintf(source, "\t\t\tInitialBackoff: time.Duration(%d),\n", mustDuration(retry.InitialBackoff))
		fmt.Fprintf(source, "\t\t\tMaxBackoff: time.Duration(%d),\n", mustDuration(retry.MaxBackoff))
		fmt.Fprintf(source, "\t\t\tMultiplier: uint32(%d),\n", retry.Multiplier)
		classifier := "spiceretry.Transient"
		if retry.Classifier != "" {
			classifier = aliases[service.Provider.PackagePath] + "." + retry.Classifier
		}
		fmt.Fprintf(source, "\t\t\tRetryable: %s,\n", classifier)
		source.WriteString("\t\t\tObserver: decorator.observeRetry,\n")
		source.WriteString("\t\t}, func(retryContext context.Context, _ spiceretry.Attempt) error {\n")
		source.WriteString("\t\t\treturn retryNext(retryContext)\n")
		source.WriteString("\t\t})\n\t}\n")
	}
	if method.Cache != nil {
		source.WriteString("\tcacheNext := invoke\n")
		source.WriteString("\tinvoke = func(current context.Context) error {\n")
		fmt.Fprintf(source, "\t\tcacheKey := %s\n", serviceCacheKeyExpression(typeName, method))
		fmt.Fprintf(source, "\t\tcached, found, cacheErr := decorator.%sCache.Get(current, cacheKey)\n", field)
		source.WriteString("\t\tif cacheErr != nil { return fmt.Errorf(\"read generated cache: %w\", cacheErr) }\n")
		source.WriteString("\t\tif found { result0 = cached; return nil }\n")
		source.WriteString("\t\tif callErr := cacheNext(current); callErr != nil { return callErr }\n")
		fmt.Fprintf(source, "\t\tif cacheErr := decorator.%sCache.Put(current, cacheKey, result0, decorator.%sCacheTTL); cacheErr != nil {\n", field, field)
		source.WriteString("\t\t\treturn fmt.Errorf(\"write generated cache: %w\", cacheErr)\n\t\t}\n")
		source.WriteString("\t\treturn nil\n\t}\n")
	}
	if method.Authorization != nil {
		source.WriteString("\tauthorizationNext := invoke\n")
		source.WriteString("\tinvoke = func(current context.Context) error {\n")
		fmt.Fprintf(source, "\t\tif authorizeErr := decorator.authorizer.Authorize(current, decorator.%sAuthorization); authorizeErr != nil { return authorizeErr }\n", field)
		source.WriteString("\t\treturn authorizationNext(current)\n\t}\n")
	}
	if method.Observation != nil {
		name := method.Observation.Name
		if name == "" {
			name = method.MethodID
		}
		source.WriteString("\tobservationNext := invoke\n")
		source.WriteString("\tinvoke = func(current context.Context) error {\n")
		source.WriteString("\t\treturn spiceobservability.Observe(current, spiceobservability.MethodDefinition{\n")
		fmt.Fprintf(source, "\t\t\tID: %s,\n", strconv.Quote(name))
		fmt.Fprintf(source, "\t\t\tModule: %s,\n", strconv.Quote(service.Module))
		fmt.Fprintf(source, "\t\t\tService: %s,\n", strconv.Quote(service.Interface.TypeID))
		fmt.Fprintf(source, "\t\t\tMethod: %s,\n", strconv.Quote(method.Name))
		source.WriteString("\t\t}, decorator.methodObservers, observationNext)\n\t}\n")
	}
	source.WriteString("\tresultErr = invoke(ctx)\n\treturn\n")
}

func writePolicyArguments(source *bytes.Buffer, method compilerpolicy.Method, first string, decorated bool) {
	for index := 0; index < method.Signature.Params().Len(); index++ {
		if index != 0 {
			source.WriteString(", ")
		}
		if decorated && index == 0 {
			source.WriteString(first)
		} else {
			fmt.Fprintf(source, "parameter%d", index)
		}
		if method.Signature.Variadic() && index == method.Signature.Params().Len()-1 {
			source.WriteString("...")
		}
	}
}

func serviceCacheKeyType(typeName string, method compilerpolicy.Method, aliases map[string]string) string {
	count := method.Signature.Params().Len() - 1
	switch count {
	case 0:
		return "struct{}"
	case 1:
		return renderedType(method.Signature.Params().At(1).Type(), aliases)
	default:
		return typeName + exportedGeneratedIdentifier(method.Name, "Method") + "CacheKey"
	}
}

func serviceCacheKeyExpression(typeName string, method compilerpolicy.Method) string {
	count := method.Signature.Params().Len() - 1
	switch count {
	case 0:
		return "struct{}{}"
	case 1:
		return "parameter1"
	default:
		var fields []string
		for index := 1; index < method.Signature.Params().Len(); index++ {
			fields = append(fields, fmt.Sprintf("P%d: parameter%d", index, index))
		}
		return typeName + exportedGeneratedIdentifier(method.Name, "Method") + "CacheKey{" + strings.Join(fields, ", ") + "}"
	}
}

func servicePolicyTypeName(service compilerpolicy.Service) string {
	digest := sha256.Sum256([]byte(service.Provider.SymbolID))
	return localGeneratedIdentifier(service.Provider.Name, "service") + "MethodPolicies" + hex.EncodeToString(digest[:3])
}

func policyMethodField(method compilerpolicy.Method) string {
	return localGeneratedIdentifier(method.Name, "method")
}

func serviceUsesAuthorization(service compilerpolicy.Service) bool {
	return serviceUses(service, func(method compilerpolicy.Method) bool { return method.Authorization != nil })
}

func serviceUsesRetry(service compilerpolicy.Service) bool {
	return serviceUses(service, func(method compilerpolicy.Method) bool { return method.Retry != nil })
}

func serviceUsesObservation(service compilerpolicy.Service) bool {
	return serviceUses(service, func(method compilerpolicy.Method) bool { return method.Observation != nil })
}

func serviceUsesCache(service compilerpolicy.Service) bool {
	return serviceUses(service, func(method compilerpolicy.Method) bool { return method.Cache != nil })
}

func serviceUses(service compilerpolicy.Service, matches func(compilerpolicy.Method) bool) bool {
	for _, method := range service.Methods() {
		if matches(method) {
			return true
		}
	}
	return false
}

func serviceIsolationLevel(value string) string {
	switch value {
	case "", "default":
		return isolationLevelName(sql.LevelDefault)
	case "read-uncommitted":
		return isolationLevelName(sql.LevelReadUncommitted)
	case "read-committed":
		return isolationLevelName(sql.LevelReadCommitted)
	case "write-committed":
		return isolationLevelName(sql.LevelWriteCommitted)
	case "repeatable-read":
		return isolationLevelName(sql.LevelRepeatableRead)
	case "snapshot":
		return isolationLevelName(sql.LevelSnapshot)
	case "serializable":
		return isolationLevelName(sql.LevelSerializable)
	case "linearizable":
		return isolationLevelName(sql.LevelLinearizable)
	default:
		return isolationLevelName(sql.LevelDefault)
	}
}

func mustDuration(value string) time.Duration {
	parsed, err := time.ParseDuration(value)
	if err != nil {
		panic("validated retry duration became invalid: " + value)
	}
	return parsed
}
