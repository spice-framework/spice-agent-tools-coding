// Package enum validates closed, type-safe enum declarations from the shared
// typed compiler program. It never evaluates application code or emits runtime
// registries.
package enum

import (
	"fmt"
	"go/constant"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/spice/annotation/sdk"
	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/compiler/provider"
	"github.com/spice-framework/toolchain/compiler/resolve"
)

// Member is one declared member of a closed enum.
type Member struct {
	Name             string
	Value            string
	Position         token.Position
	PhysicalPosition token.Position
}

// Type is one validated @Enum declaration.
type Type struct {
	Symbol           load.Symbol
	SymbolID         string
	Name             string
	PackagePath      string
	Type             types.Type
	TypeID           string
	Underlying       string
	Position         token.Position
	PhysicalPosition token.Position
	members          []Member
}

// Members returns enum members in source declaration order.
func (value Type) Members() []Member {
	return append([]Member(nil), value.members...)
}

// Diagnostic is one deterministic enum validation failure.
type Diagnostic struct {
	Position         token.Position
	PhysicalPosition token.Position
	SymbolID         string
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

// Catalog is the immutable enum validation result.
type Catalog struct {
	types       []Type
	diagnostics []Diagnostic
}

// Types returns validated enums in stable symbol order.
func (catalog Catalog) Types() []Type {
	result := append([]Type(nil), catalog.types...)
	for index := range result {
		result[index].members = append(
			[]Member(nil),
			catalog.types[index].members...,
		)
	}
	return result
}

// Diagnostics returns deterministic enum diagnostics.
func (catalog Catalog) Diagnostics() []Diagnostic {
	return append([]Diagnostic(nil), catalog.diagnostics...)
}

// Build validates every resolved @Enum declaration without reloading source.
func Build(program *load.Program, resolution resolve.Result) Catalog {
	if program == nil {
		return Catalog{diagnostics: []Diagnostic{{
			Kind:    "internal",
			Message: "enum catalog requires a loaded program",
		}}}
	}

	symbols := symbolIndex(program.Symbols())
	constants := constantSymbols(program.PrimarySymbols())
	seenSymbols := make(map[string]resolve.Occurrence)
	seenFiles := make(map[string]resolve.Occurrence)
	catalog := Catalog{}
	for _, occurrence := range resolution.Occurrences {
		if !occurrence.HasContribution(sdk.ContributionEnum) {
			continue
		}
		if previous, duplicate := seenSymbols[occurrence.SymbolID]; duplicate {
			catalog.diagnostics = append(catalog.diagnostics, occurrenceDiagnostic(
				occurrence,
				"duplicate-annotation",
				fmt.Sprintf(
					"@Enum type %q is declared more than once; first declaration is at %s",
					occurrence.Name,
					renderPosition(previous.DisplayPosition),
				),
			))
			continue
		}
		seenSymbols[occurrence.SymbolID] = occurrence
		symbol, found := symbols[occurrence.SymbolID]
		if !found {
			catalog.diagnostics = append(catalog.diagnostics, occurrenceDiagnostic(
				occurrence,
				"missing-symbol",
				fmt.Sprintf(
					"@Enum target %q has no stable typed symbol in the loaded program",
					occurrence.Name,
				),
			))
			continue
		}
		file := cleanFile(symbol.PhysicalPosition.Filename)
		if previous, duplicate := seenFiles[file]; duplicate {
			catalog.diagnostics = append(catalog.diagnostics, symbolDiagnostic(
				occurrence,
				symbol,
				"multiple-enums-in-file",
				fmt.Sprintf(
					"@Enum type %s shares %s with enum %q at %s; enum files declare exactly one enum type",
					symbol.DisplayLabel,
					filepath.Base(file),
					previous.Name,
					renderPosition(previous.DisplayPosition),
				),
			))
			continue
		}
		seenFiles[file] = occurrence
		value, diagnostics := analyzeType(occurrence, symbol, constants)
		catalog.diagnostics = append(catalog.diagnostics, diagnostics...)
		if len(diagnostics) == 0 {
			catalog.types = append(catalog.types, value)
		}
	}
	sort.SliceStable(catalog.types, func(i, j int) bool {
		return catalog.types[i].SymbolID < catalog.types[j].SymbolID
	})
	sortDiagnostics(catalog.diagnostics)
	return catalog
}

func analyzeType(
	occurrence resolve.Occurrence,
	symbol load.Symbol,
	constants []load.Symbol,
) (Type, []Diagnostic) {
	named, problem := enumType(occurrence, symbol)
	if problem != nil {
		return Type{}, []Diagnostic{symbolDiagnostic(
			occurrence,
			symbol,
			problem.kind,
			problem.message,
		)}
	}
	basic, ok := named.Underlying().(*types.Basic)
	if !ok {
		return Type{}, []Diagnostic{symbolDiagnostic(
			occurrence,
			symbol,
			"internal",
			fmt.Sprintf("@Enum %s lost its validated scalar type", symbol.DisplayLabel),
		)}
	}
	result := Type{
		Symbol:           symbol,
		SymbolID:         symbol.ID,
		Name:             symbol.Name,
		PackagePath:      symbol.PackagePath,
		Type:             named,
		TypeID:           provider.TypeID(named),
		Underlying:       basic.Name(),
		Position:         occurrence.DisplayPosition,
		PhysicalPosition: physicalPosition(occurrence),
	}
	file := cleanFile(symbol.PhysicalPosition.Filename)
	values := make(map[string]Member)
	var diagnostics []Diagnostic
	for _, candidate := range constants {
		if candidate.PackagePath != symbol.PackagePath {
			continue
		}
		constantObject, ok := candidate.Object.(*types.Const)
		if !ok {
			continue
		}
		sameFile := cleanFile(candidate.PhysicalPosition.Filename) == file
		exactType := types.Identical(constantObject.Type(), named)
		switch {
		case sameFile && !exactType:
			diagnostics = append(diagnostics, constantDiagnostic(
				candidate,
				"non-enum-constant",
				fmt.Sprintf(
					"enum file %s contains constant %s with type %s; every constant in an enum file must use exact enum type %s",
					filepath.Base(file),
					candidate.Name,
					provider.TypeID(constantObject.Type()),
					result.TypeID,
				),
			))
		case !sameFile && exactType:
			diagnostics = append(diagnostics, constantDiagnostic(
				candidate,
				"member-outside-enum-file",
				fmt.Sprintf(
					"enum member %s has type %s but is declared outside %s",
					candidate.Name,
					result.TypeID,
					filepath.Base(file),
				),
			))
		case sameFile && exactType:
			if candidate.Name == "_" {
				diagnostics = append(diagnostics, constantDiagnostic(
					candidate,
					"blank-member",
					fmt.Sprintf("enum %s cannot declare a blank member", result.TypeID),
				))
				continue
			}
			member := Member{
				Name:             candidate.Name,
				Value:            exactValue(constantObject.Val()),
				Position:         candidate.Position,
				PhysicalPosition: candidate.PhysicalPosition,
			}
			if previous, duplicate := values[member.Value]; duplicate {
				diagnostics = append(diagnostics, constantDiagnostic(
					candidate,
					"duplicate-value",
					fmt.Sprintf(
						"enum member %s duplicates underlying value %s from member %s at %s",
						member.Name,
						member.Value,
						previous.Name,
						renderPosition(previous.Position),
					),
				))
				continue
			}
			values[member.Value] = member
			result.members = append(result.members, member)
		}
	}
	if len(result.members) == 0 {
		diagnostics = append(diagnostics, symbolDiagnostic(
			occurrence,
			symbol,
			"missing-members",
			fmt.Sprintf(
				"@Enum type %s requires at least one same-file constant of exact type %s",
				symbol.DisplayLabel,
				result.TypeID,
			),
		))
	}
	sort.SliceStable(result.members, func(i, j int) bool {
		left := result.members[i].PhysicalPosition
		right := result.members[j].PhysicalPosition
		return left.Offset < right.Offset
	})
	return result, diagnostics
}

type enumProblem struct {
	kind    string
	message string
}

func enumType(
	occurrence resolve.Occurrence,
	symbol load.Symbol,
) (*types.Named, *enumProblem) {
	label := symbol.DisplayLabel
	if label == "" {
		label = symbol.ID
	}
	if occurrence.Target != annotation.TargetType || symbol.Kind != load.SymbolType {
		return nil, &enumProblem{
			kind:    "invalid-target",
			message: fmt.Sprintf("@Enum %s must target a named type", label),
		}
	}
	typeName, ok := symbol.Object.(*types.TypeName)
	if !ok || typeName.IsAlias() {
		return nil, &enumProblem{
			kind:    "alias",
			message: fmt.Sprintf("@Enum %s must be a defined named type; aliases are not supported", label),
		}
	}
	if !token.IsExported(symbol.Name) {
		return nil, &enumProblem{
			kind:    "unexported-type",
			message: fmt.Sprintf("@Enum %s must be exported so source helpers can name it", label),
		}
	}
	named, ok := types.Unalias(typeName.Type()).(*types.Named)
	if !ok {
		return nil, &enumProblem{
			kind:    "unnamed-type",
			message: fmt.Sprintf("@Enum %s must be a defined named type", label),
		}
	}
	if named.TypeParams() != nil && named.TypeParams().Len() != 0 {
		return nil, &enumProblem{
			kind:    "generic-type",
			message: fmt.Sprintf("@Enum %s must not declare type parameters", label),
		}
	}
	basic, ok := named.Underlying().(*types.Basic)
	if !ok || basic.Info()&(types.IsString|types.IsInteger) == 0 {
		return nil, &enumProblem{
			kind: "unsupported-underlying-type",
			message: fmt.Sprintf(
				"@Enum %s must have a string or integer underlying type",
				label,
			),
		}
	}
	if len(occurrence.Annotation.Arguments) != 0 {
		return nil, &enumProblem{
			kind:    "arguments",
			message: fmt.Sprintf("@Enum %s does not accept arguments", label),
		}
	}
	return named, nil
}

func symbolIndex(symbols []load.Symbol) map[string]load.Symbol {
	result := make(map[string]load.Symbol, len(symbols))
	for _, symbol := range symbols {
		result[symbol.ID] = symbol
	}
	return result
}

func constantSymbols(symbols []load.Symbol) []load.Symbol {
	var result []load.Symbol
	for _, symbol := range symbols {
		if symbol.Kind == load.SymbolConstant {
			result = append(result, symbol)
		}
	}
	return result
}

func exactValue(value constant.Value) string {
	if value == nil {
		return "<invalid>"
	}
	return value.ExactString()
}

func cleanFile(value string) string {
	if value == "" {
		return ""
	}
	return filepath.Clean(value)
}

func occurrenceDiagnostic(
	occurrence resolve.Occurrence,
	kind string,
	message string,
) Diagnostic {
	return Diagnostic{
		Position:         occurrence.DisplayPosition,
		PhysicalPosition: physicalPosition(occurrence),
		SymbolID:         occurrence.SymbolID,
		Kind:             kind,
		Message:          message,
	}
}

func symbolDiagnostic(
	occurrence resolve.Occurrence,
	symbol load.Symbol,
	kind string,
	message string,
) Diagnostic {
	result := occurrenceDiagnostic(occurrence, kind, message)
	result.SymbolID = symbol.ID
	if symbol.Position.IsValid() {
		result.Position = symbol.Position
	}
	if symbol.PhysicalPosition.IsValid() {
		result.PhysicalPosition = symbol.PhysicalPosition
	}
	return result
}

func constantDiagnostic(
	symbol load.Symbol,
	kind string,
	message string,
) Diagnostic {
	return Diagnostic{
		Position:         symbol.Position,
		PhysicalPosition: symbol.PhysicalPosition,
		SymbolID:         symbol.ID,
		Kind:             kind,
		Message:          message,
	}
}

func physicalPosition(occurrence resolve.Occurrence) token.Position {
	if occurrence.PhysicalPosition.IsValid() {
		return occurrence.PhysicalPosition
	}
	return token.Position{
		Filename: occurrence.PhysicalFile,
		Offset:   occurrence.PhysicalOffset,
	}
}

func renderPosition(position token.Position) string {
	if position.Filename == "" {
		return "<unknown>"
	}
	return fmt.Sprintf(
		"%s:%d:%d",
		position.Filename,
		position.Line,
		position.Column,
	)
}

func sortDiagnostics(diagnostics []Diagnostic) {
	sort.SliceStable(diagnostics, func(i, j int) bool {
		left := diagnostics[i].PhysicalPosition
		right := diagnostics[j].PhysicalPosition
		if left.Filename != right.Filename {
			return left.Filename < right.Filename
		}
		if left.Offset != right.Offset {
			return left.Offset < right.Offset
		}
		if diagnostics[i].Kind != diagnostics[j].Kind {
			return diagnostics[i].Kind < diagnostics[j].Kind
		}
		return diagnostics[i].Message < diagnostics[j].Message
	})
}
