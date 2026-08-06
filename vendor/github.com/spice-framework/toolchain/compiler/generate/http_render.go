package generate

import (
	"bytes"
	"database/sql"
	"fmt"
	"go/types"
	"strconv"
	"strings"

	compilercache "github.com/spice-framework/toolchain/compiler/cache"
	"github.com/spice-framework/toolchain/compiler/controller"
	compilertransaction "github.com/spice-framework/toolchain/compiler/transaction"
)

type cacheRuntime struct {
	variable string
	ttl      string
}

func writeCacheSetup(
	source *bytes.Buffer,
	caches []compilercache.Boundary,
	aliases map[string]string,
) map[string]cacheRuntime {
	result := make(map[string]cacheRuntime, len(caches))
	for index, boundary := range caches {
		variable := "generatedCache" + strconv.Itoa(index)
		capacity := variable + "Capacity"
		ttl := variable + "TTL"
		fmt.Fprintf(
			source,
			"\t%s, err := configurationSnapshot.Integer(%s)\n",
			capacity,
			strconv.Quote(cacheCapacityKey(boundary.CacheName)),
		)
		source.WriteString("\tif err != nil {\n")
		fmt.Fprintf(
			source,
			"\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(%s, err))\n",
			strconv.Quote(
				"decode capacity for cache "+boundary.CacheName+": %w",
			),
		)
		source.WriteString("\t}\n")
		fmt.Fprintf(
			source,
			"\tif %s < 1 || uint64(%s) > uint64(^uint(0)>>1) {\n",
			capacity,
			capacity,
		)
		fmt.Fprintf(
			source,
			"\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(%s))\n",
			strconv.Quote(
				"decode capacity for cache "+boundary.CacheName+
					": value must fit a positive int",
			),
		)
		source.WriteString("\t}\n")
		fmt.Fprintf(
			source,
			"\t%s, err := configurationSnapshot.Duration(%s)\n",
			ttl,
			strconv.Quote(cacheTTLKey(boundary.CacheName)),
		)
		source.WriteString("\tif err != nil {\n")
		fmt.Fprintf(
			source,
			"\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(%s, err))\n",
			strconv.Quote(
				"decode TTL for cache "+boundary.CacheName+": %w",
			),
		)
		source.WriteString("\t}\n")
		fmt.Fprintf(source, "\tif %s < 0 {\n", ttl)
		fmt.Fprintf(
			source,
			"\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(%s))\n",
			strconv.Quote(
				"decode TTL for cache "+boundary.CacheName+
					": duration must not be negative",
			),
		)
		source.WriteString("\t}\n")
		fmt.Fprintf(
			source,
			"\t%s, err := spicecache.NewMemory[%s, %s](\n",
			variable,
			renderedType(boundary.Key, aliases),
			renderedType(boundary.Value, aliases),
		)
		source.WriteString("\t\tspicecache.Definition{\n")
		fmt.Fprintf(
			source,
			"\t\t\tID: %s,\n",
			strconv.Quote(boundary.CacheName),
		)
		fmt.Fprintf(
			source,
			"\t\t\tModule: %s,\n",
			strconv.Quote(boundary.Module),
		)
		source.WriteString("\t\t},\n")
		fmt.Fprintf(source, "\t\tint(%s),\n", capacity)
		source.WriteString("\t\toptions.CacheClock,\n")
		source.WriteString("\t\toptions.CacheObservers...,\n")
		source.WriteString("\t)\n")
		source.WriteString("\tif err != nil {\n")
		fmt.Fprintf(
			source,
			"\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(%s, err))\n",
			strconv.Quote(
				"construct generated cache "+boundary.CacheName+": %w",
			),
		)
		source.WriteString("\t}\n")
		result[boundary.RouteID] = cacheRuntime{
			variable: variable,
			ttl:      ttl,
		}
	}
	return result
}

func writeControllerRoute(
	source *bytes.Buffer,
	route controller.Route,
	transactionIndex map[string]compilertransaction.Boundary,
	caches map[string]cacheRuntime,
	providerVariables map[string]string,
	receiver string,
	middleware string,
	aliases map[string]string,
	routeIndex int,
	interceptorField generatedRouteInterceptorField,
) error {
	pattern := route.HTTPMethod + " " + route.Path
	observation := writeRouteObservation(source, route, pattern, routeIndex)
	if route.Raw {
		if _, transactional := transactionIndex[route.SymbolID]; transactional {
			return fmt.Errorf(
				"raw route %s cannot own a transaction boundary",
				route.SymbolID,
			)
		}
		fmt.Fprintf(
			source,
			"\tif routeErr := spiceweb.RegisterObserved(routeMux, %s, http.HandlerFunc(%s.%s), %s, %s...); routeErr != nil {\n",
			strconv.Quote(pattern),
			receiver,
			route.Name,
			observation,
			middleware,
		)
		writeRouteRegistrationError(source, pattern)
		return nil
	}
	boundary, transactional := transactionIndex[route.SymbolID]
	if route.ExecutorParameter != transactional {
		return fmt.Errorf(
			"typed route %s transaction metadata does not match its explicit executor parameter",
			route.SymbolID,
		)
	}
	if transactional &&
		providerVariables[boundary.ManagerProviderID] == "" {
		return fmt.Errorf(
			"transaction boundary %s has no manager provider variable",
			route.SymbolID,
		)
	}
	if route.View &&
		providerVariables[route.ViewRendererID] == "" {
		return fmt.Errorf(
			"view route %s has no renderer provider variable",
			route.SymbolID,
		)
	}
	if route.BindingResult {
		if _, cacheable := caches[route.SymbolID]; cacheable {
			return fmt.Errorf(
				"form route %s cannot be cacheable",
				route.SymbolID,
			)
		}
	}
	invocation := ""
	if interceptorField.routeID != "" {
		invocation = writeGeneratedRouteInvocation(
			source,
			route,
			transactionIndex,
			caches,
			providerVariables,
			receiver,
			aliases,
			interceptorField,
		)
	}
	writeTypedRoute(
		source,
		route,
		transactionIndex,
		caches,
		providerVariables,
		receiver,
		pattern,
		observation,
		middleware,
		aliases,
		invocation,
	)
	return nil
}

func writeCacheableRouteCall(
	source *bytes.Buffer,
	route controller.Route,
	cache cacheRuntime,
	receiver string,
) {
	fmt.Fprintf(
		source,
		"\t\tresponseValue, cacheHit, routeErr := %s.Get(httpRequest.Context(), requestValue)\n",
		cache.variable,
	)
	source.WriteString("\t\tif routeErr == nil && !cacheHit {\n")
	fmt.Fprintf(
		source,
		"\t\t\tresponseValue, routeErr = %s.%s(httpRequest.Context(), requestValue)\n",
		receiver,
		route.Name,
	)
	source.WriteString("\t\t\tif routeErr == nil {\n")
	fmt.Fprintf(
		source,
		"\t\t\t\trouteErr = %s.Put(httpRequest.Context(), requestValue, responseValue, %s)\n",
		cache.variable,
		cache.ttl,
	)
	source.WriteString("\t\t\t}\n")
	source.WriteString("\t\t}\n")
}

func hasAuthorization(controllers []controller.Controller) bool {
	for _, item := range controllers {
		for _, route := range item.Routes() {
			if _, protected := route.Authorization(); protected {
				return true
			}
		}
	}
	return false
}

func writeRouteAuthorization(
	source *bytes.Buffer,
	authorization controller.Authorization,
	pattern string,
	index int,
) string {
	policy := "authorizationPolicy" + strconv.Itoa(index)
	policyErr := policy + "Err"
	fmt.Fprintf(
		source,
		"\t%s, %s := spicesecurity.NewPolicy(spicesecurity.PolicySpec{\n",
		policy,
		policyErr,
	)
	source.WriteString("\t\tDefinition: spicesecurity.Definition{\n")
	fmt.Fprintf(
		source,
		"\t\t\tID: %s,\n\t\t\tModule: %s,\n",
		strconv.Quote(authorization.PolicyID),
		strconv.Quote(authorization.Module),
	)
	source.WriteString("\t\t},\n")
	if authorization.Authenticated {
		source.WriteString("\t\tAuthenticated: true,\n")
	}
	writeAuthorizationNames(
		source,
		"AnyRoles",
		authorization.AnyRoles(),
	)
	writeAuthorizationNames(
		source,
		"AllRoles",
		authorization.AllRoles(),
	)
	writeAuthorizationNames(
		source,
		"AllScopes",
		authorization.AllScopes(),
	)
	if authorization.Expression() != "" {
		fmt.Fprintf(
			source,
			"\t\tExpression: %s,\n",
			strconv.Quote(authorization.Expression()),
		)
	}
	source.WriteString("\t})\n")
	fmt.Fprintf(source, "\tif %s != nil {\n", policyErr)
	fmt.Fprintf(
		source,
		"\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(%s, %s))\n",
		strconv.Quote(
			"construct generated authorization policy for route "+
				pattern+": %w",
		),
		policyErr,
	)
	source.WriteString("\t}\n")
	guard := "authorizationGuard" + strconv.Itoa(index)
	guardErr := guard + "Err"
	fmt.Fprintf(
		source,
		"\t%s, %s := spicesecurity.Guard(authorizer, %s, options.AuthorizationWriteFailure)\n",
		guard,
		guardErr,
		policy,
	)
	fmt.Fprintf(source, "\tif %s != nil {\n", guardErr)
	fmt.Fprintf(
		source,
		"\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(%s, %s))\n",
		strconv.Quote(
			"construct generated authorization guard for route "+
				pattern+": %w",
		),
		guardErr,
	)
	source.WriteString("\t}\n")
	middleware := "routeMiddleware" + strconv.Itoa(index)
	fmt.Fprintf(
		source,
		"\t%s := append(append([]spiceweb.Middleware(nil), options.Middleware...), %s)\n",
		middleware,
		guard,
	)
	return middleware
}

func writeAuthorizationNames(
	source *bytes.Buffer,
	field string,
	values []string,
) {
	if len(values) == 0 {
		return
	}
	fmt.Fprintf(source, "\t\t%s: []string{", field)
	for index, value := range values {
		if index != 0 {
			source.WriteString(", ")
		}
		source.WriteString(strconv.Quote(value))
	}
	source.WriteString("},\n")
}

func writeRouteObservation(
	source *bytes.Buffer,
	route controller.Route,
	pattern string,
	index int,
) string {
	observation := "routeObservation" + strconv.Itoa(index)
	observationErr := "routeObservationErr" + strconv.Itoa(index)
	fmt.Fprintf(
		source,
		"\t%s, %s := spiceweb.ObservationMiddleware(spiceweb.RouteMetadata{ID: %s, Module: %s, Method: %s, Pattern: %s}, httpObservers...)\n",
		observation,
		observationErr,
		strconv.Quote(route.SymbolID),
		strconv.Quote(route.Module),
		strconv.Quote(route.HTTPMethod),
		strconv.Quote(route.Path),
	)
	fmt.Fprintf(source, "\tif %s != nil {\n", observationErr)
	fmt.Fprintf(
		source,
		"\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(%s, %s))\n",
		strconv.Quote("configure generated route "+pattern+" observation: %w"),
		observationErr,
	)
	source.WriteString("\t}\n")
	return observation
}

func writeTypedRoute(
	source *bytes.Buffer,
	route controller.Route,
	transactions map[string]compilertransaction.Boundary,
	caches map[string]cacheRuntime,
	providerVariables map[string]string,
	receiver string,
	pattern string,
	observation string,
	middleware string,
	aliases map[string]string,
	invocation string,
) {
	fmt.Fprintf(
		source,
		"\tif routeErr := spiceweb.RegisterObserved(routeMux, %s, http.HandlerFunc(func(writer http.ResponseWriter, httpRequest *http.Request) {\n",
		strconv.Quote(pattern),
	)
	writeTypedRouteErrorWriter(source, route, providerVariables)
	writeTypedRouteRequest(source, route, aliases)
	writeTypedRouteValidation(source, route)
	writeTypedRouteInvocation(
		source,
		route,
		transactions,
		caches,
		providerVariables,
		receiver,
		aliases,
		invocation,
	)
	source.WriteString("\t\tif routeErr != nil {\n")
	writeGeneratedError(source, "routeErr", 3)
	source.WriteString("\t\t\treturn\n")
	source.WriteString("\t\t}\n")
	writeTypedRouteResponse(source, route, providerVariables)
	fmt.Fprintf(
		source,
		"\t}), %s, %s...); routeErr != nil {\n",
		observation,
		middleware,
	)
	writeRouteRegistrationError(source, pattern)
}

func writeTypedRouteErrorWriter(
	source *bytes.Buffer,
	route controller.Route,
	providerVariables map[string]string,
) {
	if route.View {
		fmt.Fprintf(
			source,
			"\t\twriteRouteError := func(routeError error) error { return %s.WriteError(writer, httpRequest, routeError, options.ErrorMapper) }\n",
			providerVariables[route.ViewRendererID],
		)
	} else {
		source.WriteString(
			"\t\twriteRouteError := func(routeError error) error { return spiceweb.WriteError(writer, httpRequest, routeError, options.ErrorMapper) }\n",
		)
	}
}

func writeTypedRouteRequest(
	source *bytes.Buffer,
	route controller.Route,
	aliases map[string]string,
) {
	writeRouteNegotiation(source, route)
	fmt.Fprintf(source, "\t\trequestValue := %s{}\n", renderedType(route.Request, aliases))
	if route.BindingResult {
		writeFormSetup(source, route)
	}
	for _, binding := range route.Bindings() {
		if binding.Location == controller.Form {
			writeFormBinding(source, binding, aliases)
			continue
		}
		writeRequestBinding(source, binding, aliases)
	}
}

func writeTypedRouteValidation(source *bytes.Buffer, route controller.Route) {
	if route.ValidatorID != "" {
		if route.BindingResult {
			source.WriteString("\t\tif bindingResult.Valid() {\n")
			source.WriteString("\t\t\tif validationErr := spiceweb.Validate(httpRequest.Context(), requestValue.Validate); validationErr != nil {\n")
			writeBindingRejection(source, "validationErr", 4)
			source.WriteString("\t\t\t}\n")
			source.WriteString("\t\t}\n")
		} else {
			source.WriteString("\t\tif validationErr := spiceweb.Validate(httpRequest.Context(), requestValue.Validate); validationErr != nil {\n")
			writeGeneratedError(source, "validationErr", 3)
			source.WriteString("\t\t\treturn\n")
			source.WriteString("\t\t}\n")
		}
	}
}

func writeTypedRouteInvocation(
	source *bytes.Buffer,
	route controller.Route,
	transactions map[string]compilertransaction.Boundary,
	caches map[string]cacheRuntime,
	providerVariables map[string]string,
	receiver string,
	aliases map[string]string,
	invocation string,
) {
	boundary, transactional := transactions[route.SymbolID]
	cache, cacheable := caches[route.SymbolID]
	switch {
	case invocation != "" && route.NoContent:
		fmt.Fprintf(
			source,
			"\t\t_, routeErr := %s(httpRequest.Context(), requestValue)\n",
			invocation,
		)
	case invocation != "":
		fmt.Fprintf(
			source,
			"\t\tresponseValue, routeErr := %s(httpRequest.Context(), requestValue)\n",
			invocation,
		)
	case transactional:
		writeTransactionalRouteCall(
			source,
			route,
			boundary,
			providerVariables[boundary.ManagerProviderID],
			receiver,
			aliases,
		)
	case cacheable:
		writeCacheableRouteCall(
			source,
			route,
			cache,
			receiver,
		)
	case route.NoContent:
		fmt.Fprintf(
			source,
			"\t\t_, routeErr := %s.%s(httpRequest.Context(), requestValue)\n",
			receiver,
			route.Name,
		)
	case route.BindingResult:
		fmt.Fprintf(
			source,
			"\t\tresponseValue, routeErr := %s.%s(httpRequest.Context(), requestValue, bindingResult)\n",
			receiver,
			route.Name,
		)
	default:
		fmt.Fprintf(
			source,
			"\t\tresponseValue, routeErr := %s.%s(httpRequest.Context(), requestValue)\n",
			receiver,
			route.Name,
		)
	}
}

func writeTypedRouteResponse(
	source *bytes.Buffer,
	route controller.Route,
	providerVariables map[string]string,
) {
	switch {
	case route.NoContent:
		source.WriteString("\t\t_ = spiceweb.WriteNoContent(writer)\n")
	case route.View:
		fmt.Fprintf(
			source,
			"\t\t_ = %s.Respond(httpRequest.Context(), writer, responseValue)\n",
			providerVariables[route.ViewRendererID],
		)
	default:
		source.WriteString("\t\t_ = spiceweb.WriteJSON(writer, http.StatusOK, responseValue)\n")
	}
}

func writeGeneratedRouteInvocation(
	source *bytes.Buffer,
	route controller.Route,
	transactions map[string]compilertransaction.Boundary,
	caches map[string]cacheRuntime,
	providerVariables map[string]string,
	receiver string,
	aliases map[string]string,
	field generatedRouteInterceptorField,
) string {
	requestType := renderedType(route.Request, aliases)
	responseType := renderedType(route.Response, aliases)
	source.WriteString("\trouteTerminal := func(invocationContext context.Context, invocationRequest ")
	source.WriteString(requestType)
	source.WriteString(") (")
	source.WriteString(responseType)
	source.WriteString(", error) {\n")
	boundary, transactional := transactions[route.SymbolID]
	cache, cacheable := caches[route.SymbolID]
	switch {
	case transactional:
		writeTransactionalInvocation(
			source,
			route,
			boundary,
			providerVariables[boundary.ManagerProviderID],
			receiver,
			responseType,
		)
	case cacheable:
		writeCacheInvocation(source, route, cache, receiver)
	default:
		fmt.Fprintf(
			source,
			"\t\treturn %s.%s(invocationContext, invocationRequest)\n",
			receiver,
			route.Name,
		)
	}
	source.WriteString("\t}\n")
	source.WriteString("\trouteInvocation, routeInterceptorErr := spiceintercept.Chain(\n")
	source.WriteString("\t\trouteTerminal,\n")
	fmt.Fprintf(
		source,
		"\t\toptions.Interceptors.%s...,\n",
		field.fieldName,
	)
	source.WriteString("\t)\n")
	source.WriteString("\tif routeInterceptorErr != nil {\n")
	fmt.Fprintf(
		source,
		"\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(%s, routeInterceptorErr))\n",
		strconv.Quote(
			"construct generated typed interceptors for route "+
				route.HTTPMethod+" "+route.Path+": %w",
		),
	)
	source.WriteString("\t}\n")
	return "routeInvocation"
}

func writeTransactionalInvocation(
	source *bytes.Buffer,
	route controller.Route,
	boundary compilertransaction.Boundary,
	manager string,
	receiver string,
	responseType string,
) {
	fmt.Fprintf(source, "\t\tvar responseValue %s\n", responseType)
	fmt.Fprintf(
		source,
		"\t\trouteErr := %s.Within(invocationContext, spicedata.Definition{\n",
		manager,
	)
	fmt.Fprintf(source, "\t\t\tID: %s,\n", strconv.Quote(boundary.RouteID))
	fmt.Fprintf(source, "\t\t\tModule: %s,\n", strconv.Quote(boundary.Module))
	fmt.Fprintf(
		source,
		"\t\t\tIsolation: %s,\n",
		isolationLevelName(boundary.Isolation),
	)
	if boundary.ReadOnly {
		source.WriteString("\t\t\tReadOnly: true,\n")
	}
	source.WriteString("\t\t}, func(transactionContext context.Context, executor spicedata.Executor) error {\n")
	source.WriteString("\t\t\tvar transactionErr error\n")
	fmt.Fprintf(
		source,
		"\t\t\tresponseValue, transactionErr = %s.%s(transactionContext, executor, invocationRequest)\n",
		receiver,
		route.Name,
	)
	source.WriteString("\t\t\treturn transactionErr\n")
	source.WriteString("\t\t})\n")
	source.WriteString("\t\treturn responseValue, routeErr\n")
}

func writeCacheInvocation(
	source *bytes.Buffer,
	route controller.Route,
	cache cacheRuntime,
	receiver string,
) {
	fmt.Fprintf(
		source,
		"\t\tresponseValue, cacheHit, routeErr := %s.Get(invocationContext, invocationRequest)\n",
		cache.variable,
	)
	source.WriteString("\t\tif routeErr != nil || cacheHit {\n")
	source.WriteString("\t\t\treturn responseValue, routeErr\n")
	source.WriteString("\t\t}\n")
	fmt.Fprintf(
		source,
		"\t\tresponseValue, routeErr = %s.%s(invocationContext, invocationRequest)\n",
		receiver,
		route.Name,
	)
	source.WriteString("\t\tif routeErr == nil {\n")
	fmt.Fprintf(
		source,
		"\t\t\trouteErr = %s.Put(invocationContext, invocationRequest, responseValue, %s)\n",
		cache.variable,
		cache.ttl,
	)
	source.WriteString("\t\t}\n")
	source.WriteString("\t\treturn responseValue, routeErr\n")
}

func writeRouteNegotiation(
	source *bytes.Buffer,
	route controller.Route,
) {
	if route.View {
		source.WriteString("\t\tif !spiceview.AcceptsHTML(httpRequest.Header.Get(\"Accept\")) {\n")
		source.WriteString("\t\t\tproblem := spiceweb.Problem{\n")
		source.WriteString("\t\t\t\tType: \"about:blank\",\n")
		source.WriteString("\t\t\t\tTitle: \"Not Acceptable\",\n")
		source.WriteString("\t\t\t\tStatus: http.StatusNotAcceptable,\n")
		source.WriteString("\t\t\t\tDetail: \"the endpoint produces text/html\",\n")
		source.WriteString("\t\t\t}\n")
		source.WriteString("\t\t\t_ = spiceweb.WriteProblem(writer, problem)\n")
		source.WriteString("\t\t\treturn\n")
		source.WriteString("\t\t}\n")
	} else if !route.NoContent {
		source.WriteString("\t\tif !spiceweb.AcceptsJSON(httpRequest.Header.Get(\"Accept\")) {\n")
		source.WriteString("\t\t\tproblem := spiceweb.Problem{\n")
		source.WriteString("\t\t\t\tType: \"about:blank\",\n")
		source.WriteString("\t\t\t\tTitle: \"Not Acceptable\",\n")
		source.WriteString("\t\t\t\tStatus: http.StatusNotAcceptable,\n")
		source.WriteString("\t\t\t\tDetail: \"the endpoint produces application/json\",\n")
		source.WriteString("\t\t\t}\n")
		source.WriteString("\t\t\t_ = spiceweb.WriteProblem(writer, problem)\n")
		source.WriteString("\t\t\treturn\n")
		source.WriteString("\t\t}\n")
	}
}

func writeTransactionalRouteCall(
	source *bytes.Buffer,
	route controller.Route,
	boundary compilertransaction.Boundary,
	manager string,
	receiver string,
	aliases map[string]string,
) {
	if !route.NoContent {
		fmt.Fprintf(
			source,
			"\t\tvar responseValue %s\n",
			renderedType(route.Response, aliases),
		)
	}
	fmt.Fprintf(
		source,
		"\t\trouteErr := %s.Within(httpRequest.Context(), spicedata.Definition{\n",
		manager,
	)
	fmt.Fprintf(source, "\t\t\tID: %s,\n", strconv.Quote(boundary.RouteID))
	fmt.Fprintf(source, "\t\t\tModule: %s,\n", strconv.Quote(boundary.Module))
	fmt.Fprintf(
		source,
		"\t\t\tIsolation: %s,\n",
		isolationLevelName(boundary.Isolation),
	)
	if boundary.ReadOnly {
		source.WriteString("\t\t\tReadOnly: true,\n")
	}
	source.WriteString("\t\t}, func(transactionContext context.Context, executor spicedata.Executor) error {\n")
	switch {
	case route.NoContent:
		fmt.Fprintf(
			source,
			"\t\t\t_, transactionErr := %s.%s(transactionContext, executor, requestValue)\n",
			receiver,
			route.Name,
		)
	case route.BindingResult:
		fmt.Fprintf(
			source,
			"\t\t\tvar transactionErr error\n\t\t\tresponseValue, transactionErr = %s.%s(transactionContext, executor, requestValue, bindingResult)\n",
			receiver,
			route.Name,
		)
	default:
		fmt.Fprintf(
			source,
			"\t\t\tvar transactionErr error\n\t\t\tresponseValue, transactionErr = %s.%s(transactionContext, executor, requestValue)\n",
			receiver,
			route.Name,
		)
	}
	source.WriteString("\t\t\treturn transactionErr\n")
	source.WriteString("\t\t})\n")
}

func writeFormSetup(source *bytes.Buffer, route controller.Route) {
	source.WriteString("\t\tbindingResult := spiceweb.BindingResult{}\n")
	source.WriteString("\t\tformValues, formErr := spiceweb.DecodeForm(httpRequest, options.MaxRequestBodyBytes)\n")
	source.WriteString("\t\tif formErr != nil {\n")
	writeBindingRejection(source, "formErr", 3)
	source.WriteString("\t\t} else if unknownFormErr := spiceweb.RejectUnknownForm(formValues, []string{")
	first := true
	for _, binding := range route.Bindings() {
		if binding.Location != controller.Form {
			continue
		}
		if !first {
			source.WriteString(", ")
		}
		first = false
		source.WriteString(strconv.Quote(binding.Name))
	}
	source.WriteString("}); unknownFormErr != nil {\n")
	writeBindingRejection(source, "unknownFormErr", 3)
	source.WriteString("\t\t}\n")
}

func writeFormBinding(
	source *bytes.Buffer,
	binding controller.Binding,
	aliases map[string]string,
) {
	index := strconv.Itoa(binding.Index)
	fmt.Fprintf(
		source,
		"\t\traw%s, present%s, bindErr%s := spiceweb.FormValue(formValues, %s, %t)\n",
		index,
		index,
		index,
		strconv.Quote(binding.Name),
		binding.Required,
	)
	fmt.Fprintf(source, "\t\tif bindErr%s != nil {\n", index)
	writeBindingRejection(source, "bindErr"+index, 3)
	source.WriteString("\t\t} else if present" + index + " {\n")
	writeFormScalarAssignment(source, binding, index, aliases)
	source.WriteString("\t\t}\n")
}

func writeFormScalarAssignment(
	source *bytes.Buffer,
	binding controller.Binding,
	index string,
	aliases map[string]string,
) {
	typeName := renderedType(binding.Type, aliases)
	if binding.Kind == controller.ScalarString {
		fmt.Fprintf(
			source,
			"\t\t\trequestValue.%s = %s(raw%s)\n",
			binding.Field,
			typeName,
			index,
		)
		return
	}
	accessor := "Boolean"
	extra := ""
	if binding.Kind == controller.ScalarInteger {
		accessor = "Integer"
		extra = ", " + strconv.Itoa(integerBitSize(binding.Type))
	}
	if binding.Kind == controller.ScalarDuration {
		accessor = "Duration"
	}
	fmt.Fprintf(
		source,
		"\t\t\tparsed%s, parseErr%s := spiceweb.%s(spiceweb.LocationForm, %s, raw%s%s)\n",
		index,
		index,
		accessor,
		strconv.Quote(binding.Name),
		index,
		extra,
	)
	fmt.Fprintf(source, "\t\t\tif parseErr%s != nil {\n", index)
	writeBindingRejection(source, "parseErr"+index, 4)
	source.WriteString("\t\t\t} else {\n")
	fmt.Fprintf(
		source,
		"\t\t\t\trequestValue.%s = %s(parsed%s)\n",
		binding.Field,
		typeName,
		index,
	)
	source.WriteString("\t\t\t}\n")
}

func writeBindingRejection(
	source *bytes.Buffer,
	variable string,
	tabs int,
) {
	indent := strings.Repeat("\t", tabs)
	fmt.Fprintf(
		source,
		"%supdatedBindingResult, rejectErr := bindingResult.RejectBinding(%s)\n",
		indent,
		variable,
	)
	fmt.Fprintf(source, "%sif rejectErr != nil {\n", indent)
	writeGeneratedError(source, "rejectErr", tabs+1)
	fmt.Fprintf(source, "%s\treturn\n", indent)
	fmt.Fprintf(source, "%s}\n", indent)
	fmt.Fprintf(source, "%sbindingResult = updatedBindingResult\n", indent)
}

func isolationLevelName(level sql.IsolationLevel) string {
	switch level {
	case sql.LevelDefault:
		return "sql.LevelDefault"
	case sql.LevelReadUncommitted:
		return "sql.LevelReadUncommitted"
	case sql.LevelReadCommitted:
		return "sql.LevelReadCommitted"
	case sql.LevelWriteCommitted:
		return "sql.LevelWriteCommitted"
	case sql.LevelRepeatableRead:
		return "sql.LevelRepeatableRead"
	case sql.LevelSnapshot:
		return "sql.LevelSnapshot"
	case sql.LevelSerializable:
		return "sql.LevelSerializable"
	case sql.LevelLinearizable:
		return "sql.LevelLinearizable"
	default:
		return strconv.Itoa(int(level))
	}
}

func writeRouteRegistrationError(source *bytes.Buffer, pattern string) {
	fmt.Fprintf(
		source,
		"\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(%s, routeErr))\n",
		strconv.Quote("register generated route "+pattern+": %w"),
	)
	source.WriteString("\t}\n")
}

func writeRequestBinding(source *bytes.Buffer, binding controller.Binding, aliases map[string]string) {
	if binding.Location == controller.Body {
		fmt.Fprintf(
			source,
			"\t\tif bindErr := spiceweb.DecodeJSON(httpRequest, &requestValue.%s, options.MaxRequestBodyBytes); bindErr != nil {\n",
			binding.Field,
		)
		writeGeneratedError(source, "bindErr", 3)
		source.WriteString("\t\t\treturn\n")
		source.WriteString("\t\t}\n")
		return
	}
	index := strconv.Itoa(binding.Index)
	values := bindingValues(binding)
	fmt.Fprintf(
		source,
		"\t\traw%s, present%s, bindErr%s := spiceweb.Parameter(%s, %s, %s, %t)\n",
		index,
		index,
		index,
		bindingLocation(binding.Location),
		strconv.Quote(binding.Name),
		values,
		binding.Required,
	)
	fmt.Fprintf(source, "\t\tif bindErr%s != nil {\n", index)
	writeGeneratedError(source, "bindErr"+index, 3)
	source.WriteString("\t\t\treturn\n")
	source.WriteString("\t\t}\n")
	fmt.Fprintf(source, "\t\tif present%s {\n", index)
	writeScalarAssignment(source, binding, index, aliases)
	source.WriteString("\t\t}\n")
}

func writeScalarAssignment(
	source *bytes.Buffer,
	binding controller.Binding,
	index string,
	aliases map[string]string,
) {
	typeName := renderedType(binding.Type, aliases)
	if binding.Kind == controller.ScalarString {
		fmt.Fprintf(source, "\t\t\trequestValue.%s = %s(raw%s)\n", binding.Field, typeName, index)
		return
	}
	accessor := "Boolean"
	extra := ""
	if binding.Kind == controller.ScalarInteger {
		accessor = "Integer"
		extra = ", " + strconv.Itoa(integerBitSize(binding.Type))
	}
	if binding.Kind == controller.ScalarDuration {
		accessor = "Duration"
	}
	fmt.Fprintf(
		source,
		"\t\t\tparsed%s, parseErr%s := spiceweb.%s(%s, %s, raw%s%s)\n",
		index,
		index,
		accessor,
		bindingLocation(binding.Location),
		strconv.Quote(binding.Name),
		index,
		extra,
	)
	fmt.Fprintf(source, "\t\t\tif parseErr%s != nil {\n", index)
	writeGeneratedError(source, "parseErr"+index, 4)
	source.WriteString("\t\t\t\treturn\n")
	source.WriteString("\t\t\t}\n")
	fmt.Fprintf(source, "\t\t\trequestValue.%s = %s(parsed%s)\n", binding.Field, typeName, index)
}

func writeGeneratedError(source *bytes.Buffer, variable string, tabs int) {
	indent := strings.Repeat("\t", tabs)
	fmt.Fprintf(
		source,
		"%s_ = writeRouteError(%s)\n",
		indent,
		variable,
	)
}

func bindingValues(binding controller.Binding) string {
	switch binding.Location {
	case controller.Path:
		return "[]string{httpRequest.PathValue(" + strconv.Quote(binding.Name) + ")}"
	case controller.Query:
		return "httpRequest.URL.Query()[" + strconv.Quote(binding.Name) + "]"
	case controller.Header:
		return "httpRequest.Header.Values(" + strconv.Quote(binding.Name) + ")"
	case controller.Body:
		return "nil"
	case controller.Form:
		return "formValues[" + strconv.Quote(binding.Name) + "]"
	}
	return "nil"
}

func bindingLocation(location controller.Location) string {
	switch location {
	case controller.Path:
		return "spiceweb.LocationPath"
	case controller.Query:
		return "spiceweb.LocationQuery"
	case controller.Header:
		return "spiceweb.LocationHeader"
	case controller.Body:
		return "spiceweb.LocationBody"
	case controller.Form:
		return "spiceweb.LocationForm"
	}
	return "spiceweb.LocationQuery"
}

func integerBitSize(value types.Type) int {
	basic, ok := types.Unalias(value).Underlying().(*types.Basic)
	if !ok {
		return 0
	}
	if basic.Kind() == types.Int8 {
		return 8
	}
	if basic.Kind() == types.Int16 {
		return 16
	}
	if basic.Kind() == types.Int32 {
		return 32
	}
	if basic.Kind() == types.Int64 {
		return 64
	}
	return 0
}

func pointerNamedType(value types.Type, packagePath, name string) bool {
	pointer, ok := types.Unalias(value).(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := types.Unalias(pointer.Elem()).(*types.Named)
	return ok && named.Obj() != nil && named.Obj().Pkg() != nil &&
		named.Obj().Pkg().Path() == packagePath && named.Obj().Name() == name
}
