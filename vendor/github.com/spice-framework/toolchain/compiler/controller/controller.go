// Package controller validates generated net/http controller metadata from the
// shared typed compiler program.
package controller

import (
	"fmt"
	"go/token"
	"go/types"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"unicode"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/spice/annotation/sdk"
	spicesecurity "github.com/spice-framework/spice/security"
	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/compiler/modulith"
	"github.com/spice-framework/toolchain/compiler/provider"
	"github.com/spice-framework/toolchain/compiler/resolve"
)

// Location identifies one generated request DTO binding source.
type Location string

const (
	// Path binds an http.ServeMux path wildcard.
	Path Location = "path"
	// Query binds one URL query value.
	Query Location = "query"
	// Header binds one HTTP request header.
	Header Location = "header"
	// Body binds the strict JSON request body.
	Body Location = "body"
	// Form binds one URL-encoded form field.
	Form Location = "form"
)

// ScalarKind identifies one generated path, query, or header conversion.
type ScalarKind string

const (
	// ScalarString identifies string data.
	ScalarString ScalarKind = "string"
	// ScalarBoolean identifies strconv-compatible Boolean data.
	ScalarBoolean ScalarKind = "boolean"
	// ScalarInteger identifies a signed integer.
	ScalarInteger ScalarKind = "integer"
	// ScalarDuration identifies time.Duration data.
	ScalarDuration ScalarKind = "duration"
)

// Binding maps one request DTO field to an HTTP request location.
type Binding struct {
	Index            int
	Field            string
	Name             string
	Location         Location
	Required         bool
	Kind             ScalarKind
	Type             types.Type
	TypeID           string
	Position         token.Position
	PhysicalPosition token.Position
}

// Authorization is one validated deny-by-default policy attached to a
// generated HTTP route.
type Authorization struct {
	PolicyID         string
	Module           string
	Authenticated    bool
	Position         token.Position
	PhysicalPosition token.Position
	anyRoles         []string
	allRoles         []string
	allScopes        []string
	expression       string
}

// AnyRoles returns the sorted roles of which at least one is required.
func (authorization Authorization) AnyRoles() []string {
	return append([]string(nil), authorization.anyRoles...)
}

// AllRoles returns the sorted roles that are all required.
func (authorization Authorization) AllRoles() []string {
	return append([]string(nil), authorization.allRoles...)
}

// AllScopes returns the sorted scopes that are all required.
func (authorization Authorization) AllScopes() []string {
	return append([]string(nil), authorization.allScopes...)
}

// Expression returns the compiler-validated restricted policy expression.
func (authorization Authorization) Expression() string {
	return authorization.expression
}

// Route is one validated controller method and HTTP pattern.
type Route struct {
	Symbol            load.Symbol
	SymbolID          string
	Name              string
	HTTPMethod        string
	Path              string
	Module            string
	ProviderID        string
	Receiver          types.Type
	Request           types.Type
	RequestTypeID     string
	Response          types.Type
	ResponseTypeID    string
	Raw               bool
	NoContent         bool
	View              bool
	ExecutorParameter bool
	BindingResult     bool
	ViewRendererID    string
	ValidatorID       string
	Position          token.Position
	PhysicalPosition  token.Position
	bindings          []Binding
	authorization     *Authorization
}

// Bindings returns request fields in declaration order.
func (r Route) Bindings() []Binding {
	return append([]Binding(nil), r.bindings...)
}

// Authorization returns immutable generated policy metadata when the route is
// explicitly protected.
func (r Route) Authorization() (Authorization, bool) {
	if r.authorization == nil {
		return Authorization{}, false
	}
	return cloneAuthorization(*r.authorization), true
}

// Controller is one @Controller type and its stable routes.
type Controller struct {
	Symbol            load.Symbol
	SymbolID          string
	Name              string
	PackagePath       string
	Prefix            string
	Module            string
	ProviderID        string
	Position          token.Position
	PhysicalPosition  token.Position
	routes            []Route
	routeDeclarations int
}

// Routes returns routes sorted by HTTP method, path, and symbol ID.
func (c Controller) Routes() []Route {
	result := append([]Route(nil), c.routes...)
	for index := range result {
		result[index].bindings = append([]Binding(nil), c.routes[index].bindings...)
		if c.routes[index].authorization != nil {
			authorization := cloneAuthorization(
				*c.routes[index].authorization,
			)
			result[index].authorization = &authorization
		}
	}
	return result
}

// Diagnostic is one deterministic source-positioned controller failure.
type Diagnostic struct {
	Position         token.Position
	PhysicalPosition token.Position
	SymbolID         string
	Kind             string
	Message          string
}

// Error renders a compiler-style diagnostic.
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

// Catalog is the immutable-by-convention controller validation result.
type Catalog struct {
	controllers []Controller
	diagnostics []Diagnostic
}

// Controllers returns controller records in stable symbol order.
func (c Catalog) Controllers() []Controller {
	result := append([]Controller(nil), c.controllers...)
	for index := range result {
		result[index].routes = c.controllers[index].Routes()
	}
	return result
}

// Diagnostics returns deterministic controller diagnostics.
func (c Catalog) Diagnostics() []Diagnostic {
	return append([]Diagnostic(nil), c.diagnostics...)
}

// Build validates controller and route declarations without reloading source,
// inspecting bodies, reflecting on runtime values, or invoking methods.
func Build(
	program *load.Program,
	resolution resolve.Result,
	providers provider.Catalog,
	modules modulith.Model,
) Catalog {
	if program == nil {
		return Catalog{diagnostics: []Diagnostic{{
			Kind:    "internal",
			Message: "controller catalog requires a loaded program",
		}}}
	}
	symbols := symbolIndex(program.Symbols())
	objectSymbols := objectSymbolIndex(program.Symbols())
	fileSets := packageFileSets(program)
	catalog := Catalog{}
	controllerObjects := make(map[*types.TypeName]int)
	seenControllers := make(map[string]resolve.Occurrence)
	for _, occurrence := range resolution.Occurrences {
		if !occurrence.HasContribution(sdk.ContributionController) {
			continue
		}
		if previous, duplicate := seenControllers[occurrence.SymbolID]; duplicate {
			catalog.diagnostics = append(catalog.diagnostics, occurrenceDiagnostic(
				occurrence,
				"duplicate-controller",
				fmt.Sprintf("@Controller %q is repeated; first declaration is at %s", occurrence.Name, previous.DisplayPosition),
			))
			continue
		}
		seenControllers[occurrence.SymbolID] = occurrence
		symbol, ok := symbols[occurrence.SymbolID]
		if !ok {
			catalog.diagnostics = append(catalog.diagnostics, occurrenceDiagnostic(
				occurrence,
				"missing-symbol",
				fmt.Sprintf("@Controller target %q has no stable typed symbol", occurrence.Name),
			))
			continue
		}
		item, object, diagnostic := analyzeController(occurrence, symbol, modules)
		if diagnostic != nil {
			catalog.diagnostics = append(catalog.diagnostics, *diagnostic)
			continue
		}
		controllerObjects[object] = len(catalog.controllers)
		catalog.controllers = append(catalog.controllers, item)
	}

	for _, occurrence := range resolution.Occurrences {
		if !routeOccurrence(occurrence) {
			continue
		}
		symbol, ok := symbols[occurrence.SymbolID]
		if !ok {
			catalog.diagnostics = append(catalog.diagnostics, occurrenceDiagnostic(
				occurrence,
				"missing-symbol",
				fmt.Sprintf("@%s target %q has no stable typed symbol", occurrence.Annotation.Name, occurrence.Name),
			))
			continue
		}
		index, owned := controllerIndex(symbol, controllerObjects)
		if !owned {
			catalog.diagnostics = append(catalog.diagnostics, symbolDiagnostic(
				occurrence,
				symbol,
				"orphan-route",
				fmt.Sprintf("@%s method %s is not owned by an @Controller type", occurrence.Annotation.Name, symbol.DisplayLabel),
			))
			continue
		}
		controller := &catalog.controllers[index]
		controller.routeDeclarations++
		route, diagnostic := analyzeRoute(
			occurrence,
			symbol,
			*controller,
			providers.Providers(),
			fileSets[symbol.PackagePath],
			objectSymbols,
		)
		if diagnostic != nil {
			catalog.diagnostics = append(catalog.diagnostics, *diagnostic)
			continue
		}
		if controller.ProviderID == "" {
			controller.ProviderID = route.ProviderID
		} else if controller.ProviderID != route.ProviderID {
			catalog.diagnostics = append(catalog.diagnostics, symbolDiagnostic(
				occurrence,
				symbol,
				"receiver-provider",
				fmt.Sprintf("controller %s routes use different exact receiver provider types", controller.Name),
			))
			continue
		}
		controller.routes = append(controller.routes, route)
	}
	applyAuthorizations(
		&catalog,
		resolution,
		symbols,
		providers.Providers(),
	)
	finalize(&catalog)
	return catalog
}

type routeLocation struct {
	controller int
	route      int
}

func applyAuthorizations(
	catalog *Catalog,
	resolution resolve.Result,
	symbols map[string]load.Symbol,
	providers []provider.Provider,
) {
	routes := make(map[string][]routeLocation)
	for controllerIndex := range catalog.controllers {
		for routeIndex, route := range catalog.controllers[controllerIndex].routes {
			routes[route.SymbolID] = append(
				routes[route.SymbolID],
				routeLocation{controller: controllerIndex, route: routeIndex},
			)
		}
	}
	seen := make(map[string]resolve.Occurrence)
	for _, occurrence := range resolution.Occurrences {
		if !occurrence.HasContribution(
			sdk.ContributionAuthorization,
		) {
			continue
		}
		if serviceOwnedMethod(symbols[occurrence.SymbolID], providers) {
			continue
		}
		if previous, duplicate := seen[occurrence.SymbolID]; duplicate {
			catalog.diagnostics = append(
				catalog.diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"duplicate-authorization",
					fmt.Sprintf(
						"@security.Authorize %q is repeated; first declaration is at %s",
						occurrence.Name,
						previous.DisplayPosition,
					),
				),
			)
			continue
		}
		seen[occurrence.SymbolID] = occurrence
		locations := routes[occurrence.SymbolID]
		if len(locations) != 1 {
			catalog.diagnostics = append(
				catalog.diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"invalid-authorization-target",
					fmt.Sprintf(
						"@security.Authorize method %q must declare exactly one valid @Get or @Post route",
						occurrence.Name,
					),
				),
			)
			continue
		}
		location := locations[0]
		route := &catalog.controllers[location.controller].
			routes[location.route]
		authorization, problem := analyzeAuthorization(occurrence, *route)
		if problem != "" {
			catalog.diagnostics = append(
				catalog.diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"invalid-authorization",
					problem,
				),
			)
			continue
		}
		route.authorization = &authorization
	}
}

func serviceOwnedMethod(
	symbol load.Symbol,
	providers []provider.Provider,
) bool {
	if symbol.Signature == nil || symbol.Signature.Recv() == nil {
		return false
	}
	receiver := symbol.Signature.Recv().Type()
	for _, item := range providers {
		if item.Role == "service" && types.Identical(item.Output, receiver) {
			return true
		}
	}
	return false
}

func analyzeAuthorization(
	occurrence resolve.Occurrence,
	route Route,
) (Authorization, string) {
	if occurrence.Target != annotation.TargetMethod {
		return Authorization{}, fmt.Sprintf(
			"@security.Authorize %q must target an HTTP route method",
			occurrence.Name,
		)
	}
	owner := route.Module
	if owner == "" {
		owner = route.Symbol.PackagePath
	}
	if owner == "" {
		return Authorization{}, fmt.Sprintf(
			"@security.Authorize method %q has no stable package owner",
			occurrence.Name,
		)
	}
	authorization := Authorization{
		PolicyID:         route.SymbolID,
		Module:           owner,
		Position:         occurrence.DisplayPosition,
		PhysicalPosition: physicalPosition(occurrence),
	}
	if contribution, found := occurrence.DescriptorContribution(
		sdk.ContributionAuthorization,
	); found {
		authorization.Authenticated = contribution.Authorization.Authenticated
		authorization.anyRoles = append(
			[]string(nil),
			contribution.Authorization.AnyRoles...,
		)
		authorization.allRoles = append(
			[]string(nil),
			contribution.Authorization.AllRoles...,
		)
		authorization.allScopes = append(
			[]string(nil),
			contribution.Authorization.AllScopes...,
		)
		authorization.expression = contribution.Authorization.Expression
		sort.Strings(authorization.anyRoles)
		sort.Strings(authorization.allRoles)
		sort.Strings(authorization.allScopes)
	} else {
		seenArguments := make(map[string]struct{})
		for _, argument := range occurrence.Annotation.Arguments {
			if problem := applyAuthorizationArgument(
				&authorization,
				argument,
				seenArguments,
			); problem != "" {
				return Authorization{}, problem
			}
		}
	}
	if !authorization.Authenticated &&
		len(authorization.anyRoles) == 0 &&
		len(authorization.allRoles) == 0 &&
		len(authorization.allScopes) == 0 &&
		authorization.expression == "" {
		return Authorization{}, fmt.Sprintf(
			"@security.Authorize method %q must require authentication, a role, or a scope",
			occurrence.Name,
		)
	}
	if authorization.expression != "" {
		if err := spicesecurity.ValidateExpression(authorization.expression); err != nil {
			return Authorization{}, fmt.Sprintf(
				"@security.Authorize method %q expression is invalid: %v",
				occurrence.Name,
				err,
			)
		}
	}
	return authorization, ""
}

func applyAuthorizationArgument(
	authorization *Authorization,
	argument annotation.Argument,
	seen map[string]struct{},
) string {
	if argument.Name == "" {
		return "@security.Authorize accepts only named arguments"
	}
	if _, duplicate := seen[argument.Name]; duplicate {
		return fmt.Sprintf(
			"@security.Authorize assigns argument %q more than once",
			argument.Name,
		)
	}
	seen[argument.Name] = struct{}{}
	if argument.Name == "authenticated" {
		if argument.Value.Kind != annotation.KindBoolean {
			return fmt.Sprintf(
				"@security.Authorize argument %q requires boolean",
				argument.Name,
			)
		}
		authorization.Authenticated = argument.Value.Boolean
		return ""
	}
	if argument.Name == "expression" {
		if argument.Value.Kind != annotation.KindString {
			return "@security.Authorize argument \"expression\" requires string"
		}
		authorization.expression = argument.Value.String
		return ""
	}
	values, problem := authorizationNames(argument.Name, argument.Value)
	if problem != "" {
		return problem
	}
	switch argument.Name {
	case "anyRoles":
		authorization.anyRoles = values
	case "allRoles":
		authorization.allRoles = values
	case "allScopes":
		authorization.allScopes = values
	default:
		return fmt.Sprintf(
			"@security.Authorize does not define argument %q",
			argument.Name,
		)
	}
	return ""
}

func authorizationNames(
	name string,
	value annotation.Value,
) ([]string, string) {
	if name != "anyRoles" && name != "allRoles" && name != "allScopes" {
		return nil, fmt.Sprintf(
			"@security.Authorize does not define argument %q",
			name,
		)
	}
	if value.Kind != annotation.KindList {
		return nil, fmt.Sprintf(
			"@security.Authorize argument %q requires a string list",
			name,
		)
	}
	result := make([]string, 0, len(value.List))
	seen := make(map[string]struct{}, len(value.List))
	for index, item := range value.List {
		if item.Kind != annotation.KindString {
			return nil, fmt.Sprintf(
				"@security.Authorize argument %q item %d requires string",
				name,
				index,
			)
		}
		if item.String == "" || strings.TrimSpace(item.String) != item.String {
			return nil, fmt.Sprintf(
				"@security.Authorize argument %q item %d must be non-empty and have no surrounding space",
				name,
				index,
			)
		}
		if _, duplicate := seen[item.String]; duplicate {
			return nil, fmt.Sprintf(
				"@security.Authorize argument %q repeats %q",
				name,
				item.String,
			)
		}
		seen[item.String] = struct{}{}
		result = append(result, item.String)
	}
	sort.Strings(result)
	return result, ""
}

func cloneAuthorization(value Authorization) Authorization {
	value.anyRoles = append([]string(nil), value.anyRoles...)
	value.allRoles = append([]string(nil), value.allRoles...)
	value.allScopes = append([]string(nil), value.allScopes...)
	return value
}

func analyzeController(
	occurrence resolve.Occurrence,
	symbol load.Symbol,
	modules modulith.Model,
) (Controller, *types.TypeName, *Diagnostic) {
	label := symbolLabel(symbol)
	typeName, ok := symbol.Object.(*types.TypeName)
	if occurrence.Target != annotation.TargetType || symbol.Kind != load.SymbolType || !ok || typeName.IsAlias() {
		diagnostic := symbolDiagnostic(occurrence, symbol, "invalid-controller", fmt.Sprintf("@Controller %s must target a defined named struct", label))
		return Controller{}, nil, &diagnostic
	}
	named, ok := typeName.Type().(*types.Named)
	if !ok {
		diagnostic := symbolDiagnostic(occurrence, symbol, "invalid-controller", fmt.Sprintf("@Controller %s must target a defined named struct", label))
		return Controller{}, nil, &diagnostic
	}
	if !token.IsExported(symbol.Name) {
		diagnostic := symbolDiagnostic(occurrence, symbol, "unexported-controller", fmt.Sprintf("@Controller %s must be exported", label))
		return Controller{}, nil, &diagnostic
	}
	if named.TypeParams() != nil && named.TypeParams().Len() != 0 {
		diagnostic := symbolDiagnostic(occurrence, symbol, "generic-controller", fmt.Sprintf("@Controller %s must not declare type parameters", label))
		return Controller{}, nil, &diagnostic
	}
	if _, ok := named.Underlying().(*types.Struct); !ok {
		diagnostic := symbolDiagnostic(occurrence, symbol, "invalid-controller", fmt.Sprintf("@Controller %s must have a struct underlying type", label))
		return Controller{}, nil, &diagnostic
	}
	prefix, valid := controllerPrefix(occurrence)
	if !valid || !validPrefix(prefix) {
		diagnostic := symbolDiagnostic(occurrence, symbol, "invalid-prefix", fmt.Sprintf("@Controller %s prefix %q must be empty or an absolute path without wildcards, query, fragment, or trailing slash", label, prefix))
		return Controller{}, nil, &diagnostic
	}
	module := ""
	if owner, found := modules.Owner(symbol.PackagePath); found {
		module = owner.ID
	}
	return Controller{
		Symbol:           symbol,
		SymbolID:         symbol.ID,
		Name:             symbol.Name,
		PackagePath:      symbol.PackagePath,
		Prefix:           prefix,
		Module:           module,
		Position:         occurrence.DisplayPosition,
		PhysicalPosition: physicalPosition(occurrence),
	}, typeName, nil
}

func analyzeRoute(
	occurrence resolve.Occurrence,
	symbol load.Symbol,
	controller Controller,
	providers []provider.Provider,
	fileSet *token.FileSet,
	objectSymbols map[types.Object]load.Symbol,
) (Route, *Diagnostic) {
	signature := symbol.Signature
	if diagnostic := validateRouteMethod(occurrence, symbol, signature); diagnostic != nil {
		return Route{}, diagnostic
	}
	method, routePath, valid := routeContribution(occurrence)
	fullPath, wildcards, pathProblem := routePattern(controller.Prefix, routePath)
	if !valid || pathProblem != "" {
		diagnostic := symbolDiagnostic(occurrence, symbol, "invalid-path", fmt.Sprintf("@%s method %s path %q is invalid: %s", occurrence.Annotation.Name, symbolLabel(symbol), routePath, pathProblem))
		return Route{}, &diagnostic
	}
	receiverProvider, found := exactProvider(signature.Recv().Type(), providers)
	if !found {
		diagnostic := symbolDiagnostic(occurrence, symbol, "missing-controller-provider", fmt.Sprintf("@%s method %s receiver exact type %s has no provider", occurrence.Annotation.Name, symbolLabel(symbol), provider.TypeID(signature.Recv().Type())))
		return Route{}, &diagnostic
	}
	route := Route{
		Symbol:           symbol,
		SymbolID:         symbol.ID,
		Name:             symbol.Name,
		HTTPMethod:       method,
		Path:             fullPath,
		Module:           controller.Module,
		ProviderID:       receiverProvider.SymbolID,
		Receiver:         signature.Recv().Type(),
		Position:         occurrence.DisplayPosition,
		PhysicalPosition: physicalPosition(occurrence),
	}
	if rawRoute(signature) {
		route.Raw = true
		return route, nil
	}
	diagnostic := typedRoute(
		&route,
		occurrence,
		symbol,
		signature,
		wildcards,
		providers,
		fileSet,
		objectSymbols,
	)
	if diagnostic != nil {
		return Route{}, diagnostic
	}
	if method == http.MethodGet && hasBody(route.bindings) {
		diagnostic := symbolDiagnostic(occurrence, symbol, "get-body", fmt.Sprintf("@Get method %s must not bind a request body", symbolLabel(symbol)))
		return Route{}, &diagnostic
	}
	if method == http.MethodGet && hasLocation(route.bindings, Form) {
		diagnostic := symbolDiagnostic(occurrence, symbol, "get-form", fmt.Sprintf("@Get method %s must not bind a request form", symbolLabel(symbol)))
		return Route{}, &diagnostic
	}
	return route, nil
}

func validateRouteMethod(
	occurrence resolve.Occurrence,
	symbol load.Symbol,
	signature *types.Signature,
) *Diagnostic {
	if occurrence.Target != annotation.TargetMethod || symbol.Kind != load.SymbolMethod ||
		signature == nil || signature.Recv() == nil {
		diagnostic := symbolDiagnostic(occurrence, symbol, "invalid-route", fmt.Sprintf("@%s %s must target an ordinary method", occurrence.Annotation.Name, symbolLabel(symbol)))
		return &diagnostic
	}
	if !token.IsExported(symbol.Name) || signature.Variadic() ||
		(signature.TypeParams() != nil && signature.TypeParams().Len() != 0) {
		diagnostic := symbolDiagnostic(occurrence, symbol, "invalid-route", fmt.Sprintf("@%s method %s must be exported, non-generic, and non-variadic", occurrence.Annotation.Name, symbolLabel(symbol)))
		return &diagnostic
	}
	return nil
}

func typedRoute(
	route *Route,
	occurrence resolve.Occurrence,
	symbol load.Symbol,
	signature *types.Signature,
	wildcards []string,
	providers []provider.Provider,
	fileSet *token.FileSet,
	objectSymbols map[types.Object]load.Symbol,
) *Diagnostic {
	requestIndex, executorParameter, bindingResult := typedRouteParameters(
		signature,
	)
	if requestIndex < 0 || signature.Results().Len() != 2 ||
		!namedType(signature.Params().At(0).Type(), "context", "Context") ||
		!types.Identical(signature.Results().At(1).Type(), types.Universe.Lookup("error").Type()) {
		diagnostic := symbolDiagnostic(
			occurrence,
			symbol,
			"typed-signature",
			fmt.Sprintf(
				"typed route %s must have signature func(context.Context, RequestDTO) (Response, error), optionally adding data.Executor before the request or web.BindingResult after it",
				symbolLabel(symbol),
			),
		)
		return &diagnostic
	}
	requestType := signature.Params().At(requestIndex).Type()
	requestNamed, ok := types.Unalias(requestType).(*types.Named)
	if !ok || !token.IsExported(requestNamed.Obj().Name()) {
		diagnostic := symbolDiagnostic(occurrence, symbol, "request-type", fmt.Sprintf("typed route %s request must be an exported named struct value", symbolLabel(symbol)))
		return &diagnostic
	}
	structure, ok := requestNamed.Underlying().(*types.Struct)
	if !ok {
		diagnostic := symbolDiagnostic(occurrence, symbol, "request-type", fmt.Sprintf("typed route %s request must be an exported named struct value", symbolLabel(symbol)))
		return &diagnostic
	}
	bindings, problem := requestBindings(structure, fileSet)
	if problem != nil {
		diagnostic := symbolDiagnostic(occurrence, symbol, problem.kind, fmt.Sprintf("typed route %s: %s", symbolLabel(symbol), problem.message))
		diagnostic.Position = problem.position
		diagnostic.PhysicalPosition = problem.physical
		return &diagnostic
	}
	if message := validatePathBindings(bindings, wildcards); message != "" {
		diagnostic := symbolDiagnostic(occurrence, symbol, "path-bindings", fmt.Sprintf("typed route %s: %s", symbolLabel(symbol), message))
		return &diagnostic
	}
	hasForm := hasLocation(bindings, Form)
	if diagnostic := validateFormContract(
		occurrence,
		symbol,
		bindings,
		bindingResult,
	); diagnostic != nil {
		return diagnostic
	}
	route.Request = requestType
	route.RequestTypeID = provider.TypeID(requestType)
	route.Response = signature.Results().At(0).Type()
	route.ResponseTypeID = provider.TypeID(route.Response)
	route.NoContent = namedType(route.Response, "github.com/spice-framework/spice/web", "NoContent")
	route.View = namedType(
		route.Response,
		"github.com/spice-framework/spice/view",
		"Result",
	)
	route.ExecutorParameter = executorParameter
	route.BindingResult = bindingResult
	if diagnostic := configureViewRoute(
		route,
		occurrence,
		symbol,
		hasForm,
		providers,
	); diagnostic != nil {
		return diagnostic
	}
	validatorID, validatorProblem := requestValidator(requestNamed, objectSymbols)
	if validatorProblem != nil {
		diagnostic := symbolDiagnostic(occurrence, symbol, validatorProblem.kind, fmt.Sprintf("typed route %s: %s", symbolLabel(symbol), validatorProblem.message))
		if validatorProblem.position.Filename != "" {
			diagnostic.Position = validatorProblem.position
			diagnostic.PhysicalPosition = validatorProblem.physical
		}
		return &diagnostic
	}
	route.ValidatorID = validatorID
	route.bindings = bindings
	return nil
}

func validateFormContract(
	occurrence resolve.Occurrence,
	symbol load.Symbol,
	bindings []Binding,
	bindingResult bool,
) *Diagnostic {
	hasForm := hasLocation(bindings, Form)
	if hasForm != bindingResult {
		message := "a request containing form bindings must receive web.BindingResult after the request DTO"
		if bindingResult {
			message = "web.BindingResult is only valid for a request DTO containing form bindings"
		}
		diagnostic := symbolDiagnostic(
			occurrence,
			symbol,
			"form-signature",
			fmt.Sprintf("typed route %s: %s", symbolLabel(symbol), message),
		)
		return &diagnostic
	}
	if hasForm && hasLocation(bindings, Body) {
		diagnostic := symbolDiagnostic(
			occurrence,
			symbol,
			"mixed-body-form",
			fmt.Sprintf("typed route %s must not combine body and form bindings", symbolLabel(symbol)),
		)
		return &diagnostic
	}
	return nil
}

func configureViewRoute(
	route *Route,
	occurrence resolve.Occurrence,
	symbol load.Symbol,
	hasForm bool,
	providers []provider.Provider,
) *Diagnostic {
	if hasForm && !route.View {
		diagnostic := symbolDiagnostic(
			occurrence,
			symbol,
			"form-response",
			fmt.Sprintf("typed route %s with form bindings must return view.Result", symbolLabel(symbol)),
		)
		return &diagnostic
	}
	if !route.View {
		return nil
	}
	renderer, found := providerByTypeID(
		"*github.com/spice-framework/spice/view.Renderer",
		providers,
	)
	if !found {
		diagnostic := symbolDiagnostic(
			occurrence,
			symbol,
			"missing-view-renderer",
			fmt.Sprintf("typed route %s returning view.Result requires exactly one *view.Renderer provider", symbolLabel(symbol)),
		)
		return &diagnostic
	}
	route.ViewRendererID = renderer.SymbolID
	return nil
}

func typedRouteParameters(signature *types.Signature) (int, bool, bool) {
	if signature == nil {
		return -1, false, false
	}
	switch signature.Params().Len() {
	case 2:
		return 1, false, false
	case 3:
		if namedType(
			signature.Params().At(1).Type(),
			"github.com/spice-framework/spice/data",
			"Executor",
		) {
			return 2, true, false
		}
		if namedType(
			signature.Params().At(2).Type(),
			"github.com/spice-framework/spice/web",
			"BindingResult",
		) {
			return 1, false, true
		}
	case 4:
		if namedType(
			signature.Params().At(1).Type(),
			"github.com/spice-framework/spice/data",
			"Executor",
		) && namedType(
			signature.Params().At(3).Type(),
			"github.com/spice-framework/spice/web",
			"BindingResult",
		) {
			return 2, true, true
		}
	}
	return -1, false, false
}

func requestValidator(
	request *types.Named,
	objectSymbols map[types.Object]load.Symbol,
) (string, *bindingProblem) {
	methodSet := types.NewMethodSet(types.NewPointer(request))
	for selection := range methodSet.Methods() {
		method, ok := selection.Obj().(*types.Func)
		if !ok {
			continue
		}
		if method.Name() != "Validate" {
			continue
		}
		signature, ok := method.Type().(*types.Signature)
		if !ok || !validRequestValidatorSignature(signature, request) {
			problem := fieldProblem(
				"validator-signature",
				fmt.Sprintf("request validator %s.Validate must have exact signature func(context.Context) error with a value receiver", request.Obj().Name()),
			)
			if symbol, found := objectSymbols[method]; found {
				problem.position = symbol.Position
				problem.physical = symbol.PhysicalPosition
			}
			return "", problem
		}
		if symbol, found := objectSymbols[method]; found {
			return symbol.ID, nil
		}
		return provider.TypeID(request) + ".Validate", nil
	}
	return "", nil
}

func validRequestValidatorSignature(signature *types.Signature, request *types.Named) bool {
	return signature.Recv() != nil &&
		types.Identical(signature.Recv().Type(), request) &&
		!signature.Variadic() &&
		(signature.TypeParams() == nil || signature.TypeParams().Len() == 0) &&
		signature.Params().Len() == 1 &&
		namedType(signature.Params().At(0).Type(), "context", "Context") &&
		signature.Results().Len() == 1 &&
		types.Identical(signature.Results().At(0).Type(), types.Universe.Lookup("error").Type())
}

type bindingProblem struct {
	kind     string
	message  string
	position token.Position
	physical token.Position
}

func requestBindings(structure *types.Struct, fileSet *token.FileSet) ([]Binding, *bindingProblem) {
	var result []Binding
	seen := make(map[string]struct{})
	bodyCount := 0
	for index := 0; index < structure.NumFields(); index++ {
		field := structure.Field(index)
		position, physical := fieldPositions(field, fileSet)
		binding, included, problem := fieldBinding(field, structure.Tag(index), index)
		if problem != nil {
			problem.position, problem.physical = position, physical
			return nil, problem
		}
		if !included {
			continue
		}
		binding.Position, binding.PhysicalPosition = position, physical
		if binding.Location == Body {
			bodyCount++
			if bodyCount > 1 {
				return nil, &bindingProblem{kind: "duplicate-body", message: "request DTO may contain only one body field", position: position, physical: physical}
			}
			result = append(result, binding)
			continue
		}
		key := string(binding.Location) + "\x00" + binding.Name
		if _, duplicate := seen[key]; duplicate {
			return nil, &bindingProblem{kind: "duplicate-binding", message: fmt.Sprintf("%s binding %q is declared more than once", binding.Location, binding.Name), position: position, physical: physical}
		}
		seen[key] = struct{}{}
		result = append(result, binding)
	}
	return result, nil
}

func fieldBinding(field *types.Var, tag string, index int) (Binding, bool, *bindingProblem) {
	bindingTags := []struct {
		name     string
		location Location
	}{
		{"path", Path},
		{"query", Query},
		{"header", Header},
		{"body", Body},
		{"form", Form},
	}
	var selected []struct {
		location Location
		value    string
	}
	structTag := reflect.StructTag(tag)
	for _, candidate := range bindingTags {
		if value, ok := structTag.Lookup(candidate.name); ok {
			selected = append(selected, struct {
				location Location
				value    string
			}{candidate.location, value})
		}
	}
	ignored := structTag.Get("web") == "-"
	if !field.Exported() {
		if len(selected) != 0 {
			return Binding{}, false, fieldProblem("unexported-field", fmt.Sprintf("field %s is unexported and cannot be bound", field.Name()))
		}
		return Binding{}, false, nil
	}
	if field.Embedded() {
		return Binding{}, false, fieldProblem("embedded-field", fmt.Sprintf("field %s is embedded; request bindings must be explicit", field.Name()))
	}
	if ignored {
		if len(selected) != 0 {
			return Binding{}, false, fieldProblem("conflicting-tags", fmt.Sprintf("field %s combines web:\"-\" with a binding tag", field.Name()))
		}
		return Binding{}, false, nil
	}
	if len(selected) != 1 {
		return Binding{}, false, fieldProblem("binding-tags", fmt.Sprintf("field %s requires exactly one path, query, header, body, or form tag, or web:\"-\"", field.Name()))
	}
	location, raw := selected[0].location, selected[0].value
	if location == Body {
		if raw != "" {
			return Binding{}, false, fieldProblem("body-tag", fmt.Sprintf("field %s body tag must be empty", field.Name()))
		}
		return Binding{Index: index, Field: field.Name(), Location: Body, Required: true, Type: field.Type(), TypeID: provider.TypeID(field.Type())}, true, nil
	}
	name, required, valid := bindingName(raw, location)
	if !valid {
		return Binding{}, false, fieldProblem("binding-tag", fmt.Sprintf("field %s has invalid %s tag %q", field.Name(), location, raw))
	}
	kind, supported := scalarKind(field.Type())
	if !supported {
		return Binding{}, false, fieldProblem("binding-type", fmt.Sprintf("field %s type %s is not a supported request scalar", field.Name(), provider.TypeID(field.Type())))
	}
	if !accessibleType(field.Type()) {
		return Binding{}, false, fieldProblem("binding-type", fmt.Sprintf("field %s type %s is unexported and cannot be named by generated code", field.Name(), provider.TypeID(field.Type())))
	}
	return Binding{
		Index:    index,
		Field:    field.Name(),
		Name:     name,
		Location: location,
		Required: required || location == Path,
		Kind:     kind,
		Type:     field.Type(),
		TypeID:   provider.TypeID(field.Type()),
	}, true, nil
}

func bindingName(raw string, location Location) (string, bool, bool) {
	parts := strings.Split(raw, ",")
	if len(parts) == 0 || len(parts) > 2 || parts[0] == "" {
		return "", false, false
	}
	required := false
	if len(parts) == 2 {
		if parts[1] != "required" || location == Path {
			return "", false, false
		}
		required = true
	}
	name := parts[0]
	switch location {
	case Path:
		return name, required, token.IsIdentifier(name)
	case Query, Form:
		return name, required, validParameterName(name)
	case Header:
		return http.CanonicalHeaderKey(name), required, validHeaderName(name)
	case Body:
		return "", false, false
	}
	return "", false, false
}

func validParameterName(name string) bool {
	for _, character := range name {
		if unicode.IsControl(character) || unicode.IsSpace(character) ||
			strings.ContainsRune("=?#&", character) {
			return false
		}
	}
	return name != ""
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, character := range name {
		if character <= 32 || character >= 127 ||
			strings.ContainsRune("()<>@,;:\\\"/[]?={} \t", character) {
			return false
		}
	}
	return true
}

func accessibleType(value types.Type) bool {
	switch typed := value.(type) {
	case *types.Named:
		return typed.Obj() == nil || token.IsExported(typed.Obj().Name())
	case *types.Alias:
		return typed.Obj() == nil || token.IsExported(typed.Obj().Name())
	default:
		return true
	}
}

func scalarKind(value types.Type) (ScalarKind, bool) {
	unalias := types.Unalias(value)
	if named, ok := unalias.(*types.Named); ok && namedType(named, "time", "Duration") {
		return ScalarDuration, true
	}
	basic, ok := unalias.Underlying().(*types.Basic)
	if !ok {
		return "", false
	}
	kind := basic.Kind()
	if kind == types.String {
		return ScalarString, true
	}
	if kind == types.Bool {
		return ScalarBoolean, true
	}
	if kind == types.Int || kind == types.Int8 || kind == types.Int16 ||
		kind == types.Int32 || kind == types.Int64 {
		return ScalarInteger, true
	}
	return "", false
}

func routePattern(prefix, path string) (string, []string, string) {
	if path == "" || !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?# \t\r\n") {
		return "", nil, "route paths must be absolute and contain no query, fragment, or whitespace"
	}
	full := prefix + path
	if prefix == "/" {
		full = path
	}
	if strings.Contains(full, "//") {
		return "", nil, "route path contains an empty segment"
	}
	var wildcards []string
	seen := make(map[string]struct{})
	for segment := range strings.SplitSeq(strings.Trim(full, "/"), "/") {
		if !strings.ContainsAny(segment, "{}") {
			continue
		}
		if len(segment) < 3 || segment[0] != '{' || segment[len(segment)-1] != '}' {
			return "", nil, "wildcards must occupy a complete path segment"
		}
		name := segment[1 : len(segment)-1]
		if !token.IsIdentifier(name) || name == "$" || strings.HasSuffix(name, "...") {
			return "", nil, "only simple {name} wildcards are supported"
		}
		if _, duplicate := seen[name]; duplicate {
			return "", nil, fmt.Sprintf("wildcard %q is repeated", name)
		}
		seen[name] = struct{}{}
		wildcards = append(wildcards, name)
	}
	return full, wildcards, ""
}

func validPrefix(prefix string) bool {
	if prefix == "" || prefix == "/" {
		return true
	}
	return strings.HasPrefix(prefix, "/") && !strings.HasSuffix(prefix, "/") &&
		!strings.Contains(prefix, "//") && !strings.ContainsAny(prefix, "{}?# \t\r\n")
}

func validatePathBindings(bindings []Binding, wildcards []string) string {
	bound := make(map[string]struct{})
	for _, binding := range bindings {
		if binding.Location == Path {
			bound[binding.Name] = struct{}{}
		}
	}
	for _, wildcard := range wildcards {
		if _, ok := bound[wildcard]; !ok {
			return fmt.Sprintf("path wildcard %q has no matching path-tagged field", wildcard)
		}
		delete(bound, wildcard)
	}
	for name := range bound {
		return fmt.Sprintf("path-tagged field %q has no matching route wildcard", name)
	}
	return ""
}

func rawRoute(signature *types.Signature) bool {
	return signature.Params().Len() == 2 && signature.Results().Len() == 0 &&
		namedType(signature.Params().At(0).Type(), "net/http", "ResponseWriter") &&
		pointerNamedType(signature.Params().At(1).Type(), "net/http", "Request")
}

func controllerIndex(symbol load.Symbol, controllers map[*types.TypeName]int) (int, bool) {
	if symbol.Signature == nil || symbol.Signature.Recv() == nil {
		return 0, false
	}
	value := types.Unalias(symbol.Signature.Recv().Type())
	if pointer, ok := value.(*types.Pointer); ok {
		value = types.Unalias(pointer.Elem())
	}
	named, ok := value.(*types.Named)
	if !ok || named.Obj() == nil {
		return 0, false
	}
	index, found := controllers[named.Obj()]
	return index, found
}

func exactProvider(required types.Type, providers []provider.Provider) (provider.Provider, bool) {
	for _, candidate := range providers {
		if types.Identical(required, candidate.Output) {
			return candidate, true
		}
	}
	return provider.Provider{}, false
}

func providerByTypeID(
	typeID string,
	providers []provider.Provider,
) (provider.Provider, bool) {
	var match provider.Provider
	count := 0
	for _, candidate := range providers {
		if candidate.OutputTypeID == typeID {
			match = candidate
			count++
		}
	}
	return match, count == 1
}

func stringArgument(value annotation.Annotation, name string, positional bool) (string, bool) {
	if len(value.Arguments) == 0 {
		return "", name == "prefix"
	}
	if len(value.Arguments) != 1 {
		return "", false
	}
	argument := value.Arguments[0]
	if argument.Value.Kind != annotation.KindString ||
		(argument.Name != name && (!positional || argument.Name != "")) {
		return "", false
	}
	return argument.Value.String, true
}

func controllerPrefix(occurrence resolve.Occurrence) (string, bool) {
	if contribution, found := occurrence.DescriptorContribution(
		sdk.ContributionController,
	); found {
		return contribution.Controller.Prefix, true
	}
	return stringArgument(occurrence.Annotation, "prefix", false)
}

func routeOccurrence(occurrence resolve.Occurrence) bool {
	return occurrence.HasContribution(sdk.ContributionRoute)
}

func routeContribution(
	occurrence resolve.Occurrence,
) (method string, routePath string, valid bool) {
	if contribution, found := occurrence.DescriptorContribution(
		sdk.ContributionRoute,
	); found {
		return contribution.Route.Method, contribution.Route.Path, true
	}
	contribution, found := occurrence.Contribution(
		sdk.ContributionRoute,
	)
	if !found {
		return "", "", false
	}
	routePath, valid = stringArgument(
		occurrence.Annotation,
		"path",
		true,
	)
	return contribution.Route.Method, routePath, valid
}

func namedType(value types.Type, packagePath, name string) bool {
	named, ok := types.Unalias(value).(*types.Named)
	return ok && named.Obj() != nil && named.Obj().Pkg() != nil &&
		named.Obj().Pkg().Path() == packagePath && named.Obj().Name() == name
}

func pointerNamedType(value types.Type, packagePath, name string) bool {
	pointer, ok := types.Unalias(value).(*types.Pointer)
	return ok && namedType(pointer.Elem(), packagePath, name)
}

func hasBody(bindings []Binding) bool {
	return hasLocation(bindings, Body)
}

func hasLocation(bindings []Binding, location Location) bool {
	for _, binding := range bindings {
		if binding.Location == location {
			return true
		}
	}
	return false
}

func finalize(catalog *Catalog) {
	seenRoutes := make(map[string]Route)
	for index := range catalog.controllers {
		controller := &catalog.controllers[index]
		if controller.routeDeclarations == 0 {
			catalog.diagnostics = append(catalog.diagnostics, Diagnostic{
				Position:         controller.Position,
				PhysicalPosition: controller.PhysicalPosition,
				SymbolID:         controller.SymbolID,
				Kind:             "empty-controller",
				Message:          fmt.Sprintf("@Controller %s declares no valid @Get or @Post routes", controller.Name),
			})
		}
		sort.SliceStable(controller.routes, func(i, j int) bool {
			left, right := controller.routes[i], controller.routes[j]
			if left.HTTPMethod != right.HTTPMethod {
				return left.HTTPMethod < right.HTTPMethod
			}
			if left.Path != right.Path {
				return left.Path < right.Path
			}
			return left.SymbolID < right.SymbolID
		})
		for _, route := range controller.routes {
			key := route.HTTPMethod + " " + route.Path
			if previous, duplicate := seenRoutes[key]; duplicate {
				catalog.diagnostics = append(catalog.diagnostics, Diagnostic{
					Position:         route.Position,
					PhysicalPosition: route.PhysicalPosition,
					SymbolID:         route.SymbolID,
					Kind:             "duplicate-route",
					Message:          fmt.Sprintf("route %s conflicts with %s", key, previous.SymbolID),
				})
			} else {
				seenRoutes[key] = route
			}
		}
	}
	sort.SliceStable(catalog.controllers, func(i, j int) bool {
		return catalog.controllers[i].SymbolID < catalog.controllers[j].SymbolID
	})
	sortDiagnostics(catalog.diagnostics)
}

func symbolIndex(symbols []load.Symbol) map[string]load.Symbol {
	result := make(map[string]load.Symbol, len(symbols))
	for _, symbol := range symbols {
		result[symbol.ID] = symbol
	}
	return result
}

func objectSymbolIndex(symbols []load.Symbol) map[types.Object]load.Symbol {
	result := make(map[types.Object]load.Symbol, len(symbols))
	for _, symbol := range symbols {
		if symbol.Object != nil {
			result[symbol.Object] = symbol
		}
	}
	return result
}

func packageFileSets(program *load.Program) map[string]*token.FileSet {
	result := make(map[string]*token.FileSet)
	for _, pkg := range program.Packages() {
		if pkg.Raw != nil && pkg.Raw.Fset != nil {
			result[pkg.Path] = pkg.Raw.Fset
		}
	}
	return result
}

func fieldPositions(field *types.Var, fileSet *token.FileSet) (token.Position, token.Position) {
	if field == nil || fileSet == nil || !field.Pos().IsValid() {
		return token.Position{}, token.Position{}
	}
	return fileSet.PositionFor(field.Pos(), true), fileSet.PositionFor(field.Pos(), false)
}

func fieldProblem(kind, message string) *bindingProblem {
	return &bindingProblem{kind: kind, message: message}
}

func occurrenceDiagnostic(occurrence resolve.Occurrence, kind, message string) Diagnostic {
	return Diagnostic{
		Position:         occurrence.DisplayPosition,
		PhysicalPosition: physicalPosition(occurrence),
		SymbolID:         occurrence.SymbolID,
		Kind:             kind,
		Message:          message,
	}
}

func symbolDiagnostic(occurrence resolve.Occurrence, symbol load.Symbol, kind, message string) Diagnostic {
	diagnostic := occurrenceDiagnostic(occurrence, kind, message)
	if symbol.ID != "" {
		diagnostic.SymbolID = symbol.ID
	}
	if diagnostic.Position.Filename == "" {
		diagnostic.Position = symbol.Position
	}
	if diagnostic.PhysicalPosition.Filename == "" {
		diagnostic.PhysicalPosition = symbol.PhysicalPosition
	}
	return diagnostic
}

func physicalPosition(occurrence resolve.Occurrence) token.Position {
	return token.Position{Filename: occurrence.PhysicalFile, Offset: occurrence.PhysicalOffset}
}

func symbolLabel(symbol load.Symbol) string {
	if symbol.DisplayLabel != "" {
		return symbol.DisplayLabel
	}
	return symbol.ID
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
		if left.SymbolID != right.SymbolID {
			return left.SymbolID < right.SymbolID
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		return left.Message < right.Message
	})
}
