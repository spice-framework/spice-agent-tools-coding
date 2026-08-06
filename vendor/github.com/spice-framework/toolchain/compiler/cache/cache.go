// Package cache compiles explicit cacheable HTTP read boundaries into
// deterministic metadata. It validates source only and never invokes methods.
package cache

import (
	"fmt"
	"go/token"
	"go/types"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/spice/annotation/sdk"
	"github.com/spice-framework/toolchain/compiler/controller"
	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/compiler/resolve"
)

const (
	// Annotation identifies the built-in cacheable read annotation.
	Annotation = "cache.Cacheable"
)

var cacheNamePattern = regexp.MustCompile(
	`^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*$`,
)

// Boundary is one immutable generated cache boundary.
type Boundary struct {
	RouteID          string
	RouteName        string
	CacheName        string
	Module           string
	Key              types.Type
	KeyTypeID        string
	Value            types.Type
	ValueTypeID      string
	Position         token.Position
	PhysicalPosition token.Position
}

// Diagnostic is one source-positioned cache contract failure.
type Diagnostic struct {
	Position         token.Position
	PhysicalPosition token.Position
	RouteID          string
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

// Catalog is immutable cache metadata and deterministic diagnostics.
type Catalog struct {
	boundaries  []Boundary
	diagnostics []Diagnostic
}

// Boundaries returns cache boundaries in stable route identity order.
func (catalog Catalog) Boundaries() []Boundary {
	return append([]Boundary(nil), catalog.boundaries...)
}

// Diagnostics returns a defensive copy in stable source order.
func (catalog Catalog) Diagnostics() []Diagnostic {
	return append([]Diagnostic(nil), catalog.diagnostics...)
}

// Build validates @cache.Cacheable occurrences against generated typed GET
// routes. Authorization-aware and transactional cache keys remain explicit
// integrations rather than unsafe implicit behavior.
func Build(
	program *load.Program,
	resolution resolve.Result,
	controllers []controller.Controller,
) Catalog {
	catalog := Catalog{}
	if program == nil {
		catalog.diagnostics = []Diagnostic{{
			Kind:    "internal",
			Message: "cache catalog requires a loaded program",
		}}
		return catalog
	}
	routes := routeIndex(controllers)
	occurrences := cacheOccurrences(resolution)
	cacheNames := make(map[string]Boundary)
	environmentNames := make(map[string]Boundary)
	for _, routeID := range sortedOccurrenceIDs(occurrences) {
		routeOccurrences := occurrences[routeID]
		if len(routeOccurrences) == 0 {
			continue
		}
		occurrence := routeOccurrences[0]
		for _, repeated := range routeOccurrences[1:] {
			catalog.diagnostics = append(
				catalog.diagnostics,
				occurrenceDiagnostic(
					repeated,
					"duplicate-cacheable",
					fmt.Sprintf(
						"@%s %q is repeated; first declaration is at %s",
						Annotation,
						repeated.Name,
						occurrence.DisplayPosition,
					),
				),
			)
		}
		routeMatches := routes[occurrence.SymbolID]
		if len(routeMatches) != 1 {
			catalog.diagnostics = append(
				catalog.diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"invalid-cache-target",
					fmt.Sprintf(
						"@%s method %q must declare exactly one valid typed @Get route",
						Annotation,
						occurrence.Name,
					),
				),
			)
			continue
		}
		boundary, problem := compileBoundary(occurrence, routeMatches[0])
		if problem != "" {
			catalog.diagnostics = append(
				catalog.diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"invalid-cacheable",
					problem,
				),
			)
			continue
		}
		if first, duplicate := cacheNames[boundary.CacheName]; duplicate {
			catalog.diagnostics = append(
				catalog.diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"duplicate-cache-name",
					fmt.Sprintf(
						"@%s cache name %q is already owned by route %q at %s",
						Annotation,
						boundary.CacheName,
						first.RouteName,
						first.Position,
					),
				),
			)
			continue
		}
		environment := cacheEnvironmentIdentity(boundary.CacheName)
		if first, duplicate := environmentNames[environment]; duplicate {
			catalog.diagnostics = append(
				catalog.diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"duplicate-cache-environment",
					fmt.Sprintf(
						"@%s cache names %q and %q produce the same generated environment prefix %q",
						Annotation,
						first.CacheName,
						boundary.CacheName,
						environment,
					),
				),
			)
			continue
		}
		cacheNames[boundary.CacheName] = boundary
		environmentNames[environment] = boundary
		catalog.boundaries = append(catalog.boundaries, boundary)
	}
	sort.SliceStable(catalog.boundaries, func(left, right int) bool {
		return catalog.boundaries[left].RouteID <
			catalog.boundaries[right].RouteID
	})
	sortDiagnostics(catalog.diagnostics)
	return catalog
}

func cacheEnvironmentIdentity(name string) string {
	replacer := strings.NewReplacer(".", "_", "-", "_")
	return "SPICE_CACHE_" + strings.ToUpper(replacer.Replace(name))
}

func compileBoundary(
	occurrence resolve.Occurrence,
	route controller.Route,
) (Boundary, string) {
	name, problem := cacheOccurrenceName(occurrence)
	if problem != "" {
		return Boundary{}, problem
	}
	switch {
	case route.Raw:
		return Boundary{}, fmt.Sprintf(
			"@%s route %q must use a typed request and response",
			Annotation,
			route.Name,
		)
	case route.HTTPMethod != http.MethodGet:
		return Boundary{}, fmt.Sprintf(
			"@%s route %q must use @Get; mutating requests are never cached implicitly",
			Annotation,
			route.Name,
		)
	case route.NoContent || route.Response == nil:
		return Boundary{}, fmt.Sprintf(
			"@%s route %q must return a response value",
			Annotation,
			route.Name,
		)
	case route.Request == nil:
		return Boundary{}, fmt.Sprintf(
			"@%s route %q must accept a request DTO as its cache key",
			Annotation,
			route.Name,
		)
	case !namedStruct(route.Request):
		return Boundary{}, fmt.Sprintf(
			"@%s route %q request must be an exported named struct value",
			Annotation,
			route.Name,
		)
	case !types.Comparable(route.Request):
		return Boundary{}, fmt.Sprintf(
			"@%s route %q request type %s is not comparable",
			Annotation,
			route.Name,
			route.RequestTypeID,
		)
	case route.ExecutorParameter:
		return Boundary{}, fmt.Sprintf(
			"@%s route %q cannot also own a transaction boundary",
			Annotation,
			route.Name,
		)
	default:
		if _, protected := route.Authorization(); protected {
			return Boundary{}, fmt.Sprintf(
				"@%s route %q cannot use authorization until its cache key explicitly includes principal identity",
				Annotation,
				route.Name,
			)
		}
	}
	module := route.Module
	if module == "" {
		module = route.Symbol.PackagePath
	}
	return Boundary{
		RouteID:          route.SymbolID,
		RouteName:        route.Name,
		CacheName:        name,
		Module:           module,
		Key:              route.Request,
		KeyTypeID:        route.RequestTypeID,
		Value:            route.Response,
		ValueTypeID:      route.ResponseTypeID,
		Position:         occurrence.DisplayPosition,
		PhysicalPosition: physicalPosition(occurrence),
	}, ""
}

func cacheOccurrenceName(
	occurrence resolve.Occurrence,
) (string, string) {
	if contribution, found := occurrence.DescriptorContribution(
		sdk.ContributionCache,
	); found {
		return validatedCacheName(contribution.Cache.Name)
	}
	return cacheName(occurrence.Annotation)
}

func cacheName(value annotation.Annotation) (string, string) {
	if len(value.Arguments) != 1 ||
		value.Arguments[0].Name != "name" ||
		value.Arguments[0].Value.Kind != annotation.KindString {
		return "", "@cache.Cacheable requires exactly one named string argument \"name\""
	}
	return validatedCacheName(value.Arguments[0].Value.String)
}

func validatedCacheName(name string) (string, string) {
	if !cacheNamePattern.MatchString(name) {
		return "", fmt.Sprintf(
			"@cache.Cacheable argument \"name\" %q must match %s",
			name,
			cacheNamePattern,
		)
	}
	return name, ""
}

func namedStruct(value types.Type) bool {
	named, ok := types.Unalias(value).(*types.Named)
	if !ok || named.Obj() == nil || !named.Obj().Exported() {
		return false
	}
	_, ok = named.Underlying().(*types.Struct)
	return ok
}

func routeIndex(
	controllers []controller.Controller,
) map[string][]controller.Route {
	result := make(map[string][]controller.Route)
	for _, item := range controllers {
		for _, route := range item.Routes() {
			result[route.SymbolID] = append(result[route.SymbolID], route)
		}
	}
	return result
}

func cacheOccurrences(
	resolution resolve.Result,
) map[string][]resolve.Occurrence {
	result := make(map[string][]resolve.Occurrence)
	for _, occurrence := range resolution.Occurrences {
		if occurrence.HasContribution(sdk.ContributionCache) {
			result[occurrence.SymbolID] = append(
				result[occurrence.SymbolID],
				occurrence,
			)
		}
	}
	for _, occurrences := range result {
		sort.SliceStable(occurrences, func(left, right int) bool {
			if occurrences[left].PhysicalFile !=
				occurrences[right].PhysicalFile {
				return occurrences[left].PhysicalFile <
					occurrences[right].PhysicalFile
			}
			return occurrences[left].PhysicalOffset <
				occurrences[right].PhysicalOffset
		})
	}
	return result
}

func sortedOccurrenceIDs(
	occurrences map[string][]resolve.Occurrence,
) []string {
	result := make([]string, 0, len(occurrences))
	for routeID := range occurrences {
		result = append(result, routeID)
	}
	sort.Strings(result)
	return result
}

func occurrenceDiagnostic(
	occurrence resolve.Occurrence,
	kind string,
	message string,
) Diagnostic {
	return Diagnostic{
		Position:         occurrence.DisplayPosition,
		PhysicalPosition: physicalPosition(occurrence),
		RouteID:          occurrence.SymbolID,
		Kind:             kind,
		Message:          message,
	}
}

func physicalPosition(occurrence resolve.Occurrence) token.Position {
	return token.Position{
		Filename: occurrence.PhysicalFile,
		Offset:   occurrence.PhysicalOffset,
	}
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
		if leftDiagnostic.RouteID != rightDiagnostic.RouteID {
			return leftDiagnostic.RouteID < rightDiagnostic.RouteID
		}
		return leftDiagnostic.Message < rightDiagnostic.Message
	})
}
