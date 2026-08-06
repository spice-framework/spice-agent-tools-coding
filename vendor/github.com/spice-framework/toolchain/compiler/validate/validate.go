// Package validate checks scanned annotations against typed definitions.
package validate

import (
	"fmt"
	"go/token"
	"slices"
	"sort"
	"strings"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/toolchain/compiler/scan"
)

type diagnosticKind uint8

const (
	diagnosticUnknownAnnotation diagnosticKind = iota
	diagnosticInvalidTarget
	diagnosticArgumentsNotAccepted
	diagnosticUnknownArgument
	diagnosticPositionalNotAccepted
	diagnosticTooManyPositional
	diagnosticDuplicateAssignment
	diagnosticWrongKind
	diagnosticWrongListElementKind
	diagnosticMissingRequired
)

// Diagnostic is one source-positioned annotation validation failure.
type Diagnostic struct {
	Position         token.Position
	PhysicalPosition token.Position
	Annotation       string
	Target           annotation.Target
	Name             string
	Allowed          []annotation.Target
	Unknown          bool

	Argument      string
	Available     []string
	ExpectedKinds []annotation.Kind
	ActualKind    annotation.Kind
	ListIndex     int

	kind diagnosticKind
}

// Code returns the stable validation failure kind.
func (diagnostic Diagnostic) Code() string {
	if diagnostic.Unknown {
		return "unknown-annotation"
	}
	switch diagnostic.kind {
	case diagnosticUnknownAnnotation:
		return "unknown-annotation"
	case diagnosticInvalidTarget:
		return "invalid-target"
	case diagnosticArgumentsNotAccepted:
		return "arguments-not-accepted"
	case diagnosticUnknownArgument:
		return "unknown-argument"
	case diagnosticPositionalNotAccepted:
		return "positional-not-accepted"
	case diagnosticTooManyPositional:
		return "too-many-positional"
	case diagnosticDuplicateAssignment:
		return "duplicate-assignment"
	case diagnosticWrongKind:
		return "wrong-kind"
	case diagnosticWrongListElementKind:
		return "wrong-list-element-kind"
	case diagnosticMissingRequired:
		return "missing-required"
	}
	return "unknown"
}

// Message returns the actionable diagnostic text without its source prefix.
func (diagnostic Diagnostic) Message() string {
	position := normalizedPosition(diagnostic.Position)
	prefix := fmt.Sprintf(
		"%s:%d:%d: ",
		position.Filename,
		position.Line,
		position.Column,
	)
	return strings.TrimPrefix(diagnostic.Error(), prefix)
}

// Error formats an actionable, stable compiler-style diagnostic.
func (diagnostic Diagnostic) Error() string {
	position := normalizedPosition(diagnostic.Position)

	if diagnostic.Unknown {
		return fmt.Sprintf(
			"%s:%d:%d: unknown annotation @%s; add a registered annotation definition before using it",
			position.Filename,
			position.Line,
			position.Column,
			diagnostic.Annotation,
		)
	}

	switch diagnostic.kind {
	case diagnosticUnknownAnnotation:
		return fmt.Sprintf(
			"%s:%d:%d: unknown annotation @%s; add a registered annotation definition before using it",
			position.Filename,
			position.Line,
			position.Column,
			diagnostic.Annotation,
		)
	case diagnosticInvalidTarget:
		allowed := targetsToStrings(diagnostic.Allowed)
		label := "targets"
		if len(allowed) == 1 {
			label = "target"
		}
		return fmt.Sprintf(
			"%s:%d:%d: annotation @%s cannot target %s %q; allowed %s: %s",
			position.Filename,
			position.Line,
			position.Column,
			diagnostic.Annotation,
			diagnostic.Target,
			diagnostic.Name,
			label,
			strings.Join(allowed, ", "),
		)
	case diagnosticArgumentsNotAccepted:
		return fmt.Sprintf(
			"%s:%d:%d: annotation @%s does not accept arguments",
			position.Filename,
			position.Line,
			position.Column,
			diagnostic.Annotation,
		)
	case diagnosticUnknownArgument:
		return fmt.Sprintf(
			"%s:%d:%d: annotation @%s does not define argument %q; %s",
			position.Filename,
			position.Line,
			position.Column,
			diagnostic.Annotation,
			diagnostic.Argument,
			availableArgumentsText(diagnostic.Available),
		)
	case diagnosticPositionalNotAccepted:
		if len(diagnostic.Available) == 1 {
			return fmt.Sprintf(
				"%s:%d:%d: annotation @%s does not accept a positional argument; use named argument %q",
				position.Filename,
				position.Line,
				position.Column,
				diagnostic.Annotation,
				diagnostic.Available[0],
			)
		}
		return fmt.Sprintf(
			"%s:%d:%d: annotation @%s does not accept positional arguments; %s",
			position.Filename,
			position.Line,
			position.Column,
			diagnostic.Annotation,
			availableArgumentsText(diagnostic.Available),
		)
	case diagnosticTooManyPositional:
		return fmt.Sprintf(
			"%s:%d:%d: annotation @%s accepts at most one positional argument",
			position.Filename,
			position.Line,
			position.Column,
			diagnostic.Annotation,
		)
	case diagnosticDuplicateAssignment:
		return fmt.Sprintf(
			"%s:%d:%d: annotation @%s assigns argument %q more than once",
			position.Filename,
			position.Line,
			position.Column,
			diagnostic.Annotation,
			diagnostic.Argument,
		)
	case diagnosticWrongKind:
		return fmt.Sprintf(
			"%s:%d:%d: annotation @%s argument %q requires %s, got %s",
			position.Filename,
			position.Line,
			position.Column,
			diagnostic.Annotation,
			diagnostic.Argument,
			kindsText(diagnostic.ExpectedKinds),
			diagnostic.ActualKind,
		)
	case diagnosticWrongListElementKind:
		return fmt.Sprintf(
			"%s:%d:%d: annotation @%s argument %q list item %d requires %s, got %s",
			position.Filename,
			position.Line,
			position.Column,
			diagnostic.Annotation,
			diagnostic.Argument,
			diagnostic.ListIndex,
			kindsText(diagnostic.ExpectedKinds),
			diagnostic.ActualKind,
		)
	case diagnosticMissingRequired:
		return fmt.Sprintf(
			"%s:%d:%d: annotation @%s requires argument %q",
			position.Filename,
			position.Line,
			position.Column,
			diagnostic.Annotation,
			diagnostic.Argument,
		)
	default:
		return fmt.Sprintf(
			"%s:%d:%d: annotation @%s is invalid",
			position.Filename,
			position.Line,
			position.Column,
			diagnostic.Annotation,
		)
	}
}

// Occurrences validates declarations and invocations against registry and
// returns all independent diagnostics in deterministic source and argument order.
func Occurrences(occurrences []scan.Occurrence, registry annotation.Registry) []Diagnostic {
	diagnostics := make([]Diagnostic, 0)
	for _, occurrence := range occurrences {
		definition, ok := registry.Lookup(occurrence.Annotation.Name)
		if !ok {
			diagnostics = append(diagnostics, Diagnostic{
				Position:   occurrence.Annotation.Position,
				Annotation: occurrence.Annotation.Name,
				Target:     occurrence.Target,
				Name:       occurrence.Name,
				Unknown:    true,
				kind:       diagnosticUnknownAnnotation,
			})
			continue
		}

		if !definition.Targets.Contains(occurrence.Target) {
			diagnostics = append(diagnostics, Diagnostic{
				Position:   occurrence.Annotation.Position,
				Annotation: occurrence.Annotation.Name,
				Target:     occurrence.Target,
				Name:       occurrence.Name,
				Allowed:    definition.Targets.Values(),
				kind:       diagnosticInvalidTarget,
			})
		}

		diagnostics = append(diagnostics, annotationArguments(occurrence, definition)...)
	}

	sort.SliceStable(diagnostics, func(i, j int) bool {
		left := diagnostics[i]
		right := diagnostics[j]
		if left.Position.Filename != right.Position.Filename {
			return left.Position.Filename < right.Position.Filename
		}
		if left.Position.Line != right.Position.Line {
			return left.Position.Line < right.Position.Line
		}
		if left.Position.Column != right.Position.Column {
			return left.Position.Column < right.Position.Column
		}
		if left.Annotation != right.Annotation {
			return left.Annotation < right.Annotation
		}
		if left.Argument != right.Argument {
			return left.Argument < right.Argument
		}
		if left.kind != right.kind {
			return left.kind < right.kind
		}
		if left.Target != right.Target {
			return left.Target < right.Target
		}
		return left.Name < right.Name
	})
	return diagnostics
}

func annotationArguments(occurrence scan.Occurrence, definition annotation.Definition) []Diagnostic {
	supplied := occurrence.Annotation.Arguments
	if len(supplied) == 0 {
		return missingRequired(occurrence, definition, nil)
	}

	if len(definition.Arguments) == 0 {
		return []Diagnostic{argumentDiagnostic(occurrence, diagnosticArgumentsNotAccepted, "")}
	}

	definitionsByName := make(map[string]annotation.ArgumentDefinition, len(definition.Arguments))
	available := make([]string, 0, len(definition.Arguments))
	var positional *annotation.ArgumentDefinition
	for _, argument := range definition.Arguments {
		definitionsByName[argument.Name] = argument
		available = append(available, argument.Name)
		if argument.Positional {
			positionalDefinition := argument
			positional = &positionalDefinition
		}
	}
	sort.Strings(available)

	diagnostics := make([]Diagnostic, 0)
	assigned := make(map[string]int, len(definition.Arguments))
	positionalCount := 0

	for _, suppliedArgument := range supplied {
		argumentName := suppliedArgument.Name
		var argumentDefinition annotation.ArgumentDefinition

		if argumentName == "" {
			positionalCount++
			if positionalCount > 1 &&
				(positional == nil || !positional.Variadic) {
				diagnostics = append(diagnostics, argumentDiagnostic(occurrence, diagnosticTooManyPositional, ""))
				continue
			}
			if positional == nil {
				diagnostic := argumentDiagnostic(occurrence, diagnosticPositionalNotAccepted, "")
				diagnostic.Available = append([]string(nil), available...)
				diagnostics = append(diagnostics, diagnostic)
				continue
			}
			argumentName = positional.Name
			argumentDefinition = *positional
		} else {
			definition, found := definitionsByName[argumentName]
			if !found {
				diagnostic := argumentDiagnostic(occurrence, diagnosticUnknownArgument, argumentName)
				diagnostic.Available = append([]string(nil), available...)
				diagnostics = append(diagnostics, diagnostic)
				continue
			}
			argumentDefinition = definition
		}

		assigned[argumentName]++
		if assigned[argumentName] > 1 && !argumentDefinition.Variadic {
			diagnostics = append(diagnostics, argumentDiagnostic(occurrence, diagnosticDuplicateAssignment, argumentName))
		}
		if !kindAccepted(suppliedArgument.Value.Kind, argumentDefinition.Kinds) {
			diagnostic := argumentDiagnostic(occurrence, diagnosticWrongKind, argumentName)
			diagnostic.ExpectedKinds = append([]annotation.Kind(nil), argumentDefinition.Kinds...)
			diagnostic.ActualKind = suppliedArgument.Value.Kind
			diagnostics = append(diagnostics, diagnostic)
			continue
		}
		diagnostics = append(
			diagnostics,
			listElementDiagnostics(occurrence, argumentName, suppliedArgument.Value, argumentDefinition)...,
		)
	}

	return append(diagnostics, missingRequired(occurrence, definition, assigned)...)
}

func listElementDiagnostics(
	occurrence scan.Occurrence,
	argumentName string,
	value annotation.Value,
	definition annotation.ArgumentDefinition,
) []Diagnostic {
	if value.Kind != annotation.KindList || len(definition.ListElementKinds) == 0 {
		return nil
	}
	var diagnostics []Diagnostic
	for index, item := range value.List {
		if kindAccepted(item.Kind, definition.ListElementKinds) {
			continue
		}
		diagnostic := argumentDiagnostic(
			occurrence,
			diagnosticWrongListElementKind,
			argumentName,
		)
		diagnostic.ListIndex = index
		diagnostic.ExpectedKinds = append(
			[]annotation.Kind(nil),
			definition.ListElementKinds...,
		)
		diagnostic.ActualKind = item.Kind
		diagnostics = append(diagnostics, diagnostic)
	}
	return diagnostics
}

func missingRequired(occurrence scan.Occurrence, definition annotation.Definition, assigned map[string]int) []Diagnostic {
	diagnostics := make([]Diagnostic, 0)
	for _, argument := range definition.Arguments {
		if argument.Required && assigned[argument.Name] == 0 {
			diagnostics = append(diagnostics, argumentDiagnostic(occurrence, diagnosticMissingRequired, argument.Name))
		}
	}
	return diagnostics
}

func argumentDiagnostic(occurrence scan.Occurrence, kind diagnosticKind, argument string) Diagnostic {
	return Diagnostic{
		Position:   occurrence.Annotation.Position,
		Annotation: occurrence.Annotation.Name,
		Target:     occurrence.Target,
		Name:       occurrence.Name,
		Argument:   argument,
		kind:       kind,
	}
}

func kindAccepted(actual annotation.Kind, expected []annotation.Kind) bool {
	return slices.Contains(expected, actual)
}

func normalizedPosition(position token.Position) token.Position {
	if position.Filename == "" {
		position.Filename = "<unknown>"
	}
	if position.Line <= 0 {
		position.Line = 1
	}
	if position.Column <= 0 {
		position.Column = 1
	}
	return position
}

func targetsToStrings(targets []annotation.Target) []string {
	result := make([]string, len(targets))
	for index, target := range targets {
		result[index] = string(target)
	}
	return result
}

func availableArgumentsText(arguments []string) string {
	if len(arguments) == 1 {
		return fmt.Sprintf("available argument: %s", arguments[0])
	}
	return fmt.Sprintf("available arguments: %s", strings.Join(arguments, ", "))
}

func kindsText(kinds []annotation.Kind) string {
	values := make([]string, len(kinds))
	for index, kind := range kinds {
		values[index] = string(kind)
	}
	if len(values) == 1 {
		return values[0]
	}
	return "one of " + strings.Join(values, ", ")
}
