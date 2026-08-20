// Package transaction compiles explicit transactional HTTP boundaries into
// deterministic metadata. It validates source only and never invokes methods.
package transaction

import (
	"database/sql"
	"fmt"
	"go/token"
	"go/types"
	"sort"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/spice/annotation/sdk"
	"github.com/spice-framework/toolchain/compiler/controller"
	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/compiler/provider"
	"github.com/spice-framework/toolchain/compiler/resolve"
)

const (
	// Annotation identifies the built-in transaction-boundary annotation.
	Annotation = "data.Transactional"

	dataPackagePath = "github.com/spice-framework/spice/data"
)

// Boundary is one immutable generated transaction boundary.
type Boundary struct {
	RouteID           string
	RouteName         string
	ManagerProviderID string
	Module            string
	Isolation         sql.IsolationLevel
	ReadOnly          bool
	Position          token.Position
	PhysicalPosition  token.Position
}

// Diagnostic is one source-positioned transactional contract failure.
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

// Catalog is immutable transaction metadata and deterministic diagnostics.
type Catalog struct {
	boundaries  []Boundary
	diagnostics []Diagnostic
}

// Boundaries returns transaction boundaries in stable route identity order.
func (catalog Catalog) Boundaries() []Boundary {
	return append([]Boundary(nil), catalog.boundaries...)
}

// Diagnostics returns a defensive copy in stable source order.
func (catalog Catalog) Diagnostics() []Diagnostic {
	return append([]Diagnostic(nil), catalog.diagnostics...)
}

// Build validates @data.Transactional occurrences against generated typed
// routes and the exact provider catalog.
func Build(
	program *load.Program,
	resolution resolve.Result,
	providers provider.Catalog,
	controllers []controller.Controller,
) Catalog {
	catalog := Catalog{}
	if program == nil {
		catalog.diagnostics = []Diagnostic{{
			Kind:    "internal",
			Message: "transaction catalog requires a loaded program",
		}}
		return catalog
	}
	routes := routeIndex(controllers)
	managerProviderID := transactionManagerProvider(providers.Providers())
	annotated := make(map[string]struct{})
	occurrences := transactionOccurrences(
		program,
		resolution,
		providers.Providers(),
	)
	for _, routeID := range sortedOccurrenceIDs(occurrences) {
		routeOccurrences := occurrences[routeID]
		if len(routeOccurrences) == 0 {
			continue
		}
		occurrence := routeOccurrences[0]
		annotated[occurrence.SymbolID] = struct{}{}
		for _, repeated := range routeOccurrences[1:] {
			catalog.diagnostics = append(
				catalog.diagnostics,
				occurrenceDiagnostic(
					repeated,
					"duplicate-transaction",
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
					"invalid-transaction-target",
					fmt.Sprintf(
						"@%s method %q must declare exactly one valid typed @Get or @Post route",
						Annotation,
						occurrence.Name,
					),
				),
			)
			continue
		}
		route := routeMatches[0]
		if !route.ExecutorParameter {
			catalog.diagnostics = append(
				catalog.diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"missing-executor",
					fmt.Sprintf(
						"@%s route %q must accept exact data.Executor as parameter 1",
						Annotation,
						occurrence.Name,
					),
				),
			)
			continue
		}
		if managerProviderID == "" {
			catalog.diagnostics = append(
				catalog.diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"missing-manager",
					fmt.Sprintf(
						"@%s route %q requires exactly one @Bean provider for *data.Manager",
						Annotation,
						occurrence.Name,
					),
				),
			)
			continue
		}
		options, problem := parseOptions(occurrence)
		if problem != "" {
			catalog.diagnostics = append(
				catalog.diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"invalid-transaction",
					problem,
				),
			)
			continue
		}
		module := route.Module
		if module == "" {
			module = route.Symbol.PackagePath
		}
		catalog.boundaries = append(catalog.boundaries, Boundary{
			RouteID:           route.SymbolID,
			RouteName:         route.Name,
			ManagerProviderID: managerProviderID,
			Module:            module,
			Isolation:         options.isolation,
			ReadOnly:          options.readOnly,
			Position:          occurrence.DisplayPosition,
			PhysicalPosition:  physicalPosition(occurrence),
		})
	}
	for routeID, routeMatches := range routes {
		if _, found := annotated[routeID]; found {
			continue
		}
		for _, route := range routeMatches {
			if !route.ExecutorParameter {
				continue
			}
			catalog.diagnostics = append(catalog.diagnostics, Diagnostic{
				Position:         route.Position,
				PhysicalPosition: route.PhysicalPosition,
				RouteID:          route.SymbolID,
				Kind:             "missing-transaction",
				Message: fmt.Sprintf(
					"typed route %q accepts data.Executor but does not declare @%s",
					route.Name,
					Annotation,
				),
			})
		}
	}
	sort.SliceStable(catalog.boundaries, func(left, right int) bool {
		return catalog.boundaries[left].RouteID <
			catalog.boundaries[right].RouteID
	})
	sortDiagnostics(catalog.diagnostics)
	return catalog
}

func transactionOccurrences(
	program *load.Program,
	resolution resolve.Result,
	providers []provider.Provider,
) map[string][]resolve.Occurrence {
	result := make(map[string][]resolve.Occurrence)
	serviceMethods := serviceMethodIDs(program, providers)
	for _, occurrence := range resolution.Occurrences {
		if occurrence.HasContribution(
			sdk.ContributionTransaction,
		) && !serviceMethods[occurrence.SymbolID] {
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

func serviceMethodIDs(
	program *load.Program,
	providers []provider.Provider,
) map[string]bool {
	result := make(map[string]bool)
	if program == nil {
		return result
	}
	for _, symbol := range program.Symbols() {
		if symbol.Signature == nil || symbol.Signature.Recv() == nil {
			continue
		}
		for _, item := range providers {
			if item.Role == "service" && types.Identical(
				item.Output,
				symbol.Signature.Recv().Type(),
			) {
				result[symbol.ID] = true
				break
			}
		}
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

type options struct {
	isolation sql.IsolationLevel
	readOnly  bool
}

func parseOptions(occurrence resolve.Occurrence) (options, string) {
	if contribution, found := occurrence.DescriptorContribution(
		sdk.ContributionTransaction,
	); found {
		isolation, valid := isolationLevelString(
			contribution.Transaction.Isolation,
		)
		if !valid {
			return options{}, "@data.Transactional contribution contains an unsupported isolation"
		}
		return options{
			isolation: isolation,
			readOnly:  contribution.Transaction.ReadOnly,
		}, ""
	}
	value := occurrence.Annotation
	result := options{isolation: sql.LevelDefault}
	seen := make(map[string]struct{}, len(value.Arguments))
	for _, argument := range value.Arguments {
		if argument.Name == "" {
			return options{}, "@data.Transactional accepts only named arguments"
		}
		if _, duplicate := seen[argument.Name]; duplicate {
			return options{}, fmt.Sprintf(
				"@data.Transactional assigns argument %q more than once",
				argument.Name,
			)
		}
		seen[argument.Name] = struct{}{}
		switch argument.Name {
		case "isolation":
			isolation, found := isolationLevel(argument.Value)
			if !found {
				return options{}, "@data.Transactional argument \"isolation\" requires one of default, read-uncommitted, read-committed, write-committed, repeatable-read, snapshot, serializable, or linearizable"
			}
			result.isolation = isolation
		case "readOnly":
			if argument.Value.Kind != annotation.KindBoolean {
				return options{}, "@data.Transactional argument \"readOnly\" requires boolean"
			}
			result.readOnly = argument.Value.Boolean
		default:
			return options{}, fmt.Sprintf(
				"@data.Transactional does not define argument %q",
				argument.Name,
			)
		}
	}
	return result, ""
}

func isolationLevel(value annotation.Value) (sql.IsolationLevel, bool) {
	if value.Kind != annotation.KindString {
		return sql.LevelDefault, false
	}
	return isolationLevelString(value.String)
}

func isolationLevelString(value string) (sql.IsolationLevel, bool) {
	switch value {
	case "", "default":
		return sql.LevelDefault, true
	case "read-uncommitted":
		return sql.LevelReadUncommitted, true
	case "read-committed":
		return sql.LevelReadCommitted, true
	case "write-committed":
		return sql.LevelWriteCommitted, true
	case "repeatable-read":
		return sql.LevelRepeatableRead, true
	case "snapshot":
		return sql.LevelSnapshot, true
	case "serializable":
		return sql.LevelSerializable, true
	case "linearizable":
		return sql.LevelLinearizable, true
	default:
		return sql.LevelDefault, false
	}
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

func transactionManagerProvider(providers []provider.Provider) string {
	for _, candidate := range providers {
		if pointerNamedType(candidate.Output, dataPackagePath, "Manager") {
			return candidate.SymbolID
		}
	}
	return ""
}

func pointerNamedType(value types.Type, packagePath, name string) bool {
	pointer, ok := types.Unalias(value).(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := types.Unalias(pointer.Elem()).(*types.Named)
	return ok &&
		named.Obj() != nil &&
		named.Obj().Pkg() != nil &&
		named.Obj().Pkg().Path() == packagePath &&
		named.Obj().Name() == name
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
