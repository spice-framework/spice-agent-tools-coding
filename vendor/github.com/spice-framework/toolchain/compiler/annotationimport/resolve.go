// @import { NamedInterface } from "github.com/spice-framework/spice/annotation/modulith"

// Package annotationimport resolves explicit file-scoped annotation imports.
//
// @NamedInterface("annotationimport")
package annotationimport

import (
	"fmt"
	"go/token"
	"sort"
	"strings"

	"github.com/spice-framework/spice/annotation"
)

// Diagnostic is one deterministic import binding failure.
type Diagnostic struct {
	Position token.Position
	Message  string
}

// Error formats a compiler-style import diagnostic.
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

// Binding is one resolved file-local annotation name.
type Binding struct {
	Local     string
	Namespace bool
	Reference annotation.DefinitionReference
	Position  token.Position
}

// Table is an immutable-by-construction file-scoped binding table.
type Table struct {
	named      map[string]Binding
	namespaces map[string]Binding
}

// Resolve validates imports and creates a fail-closed symbol table.
func Resolve(directives []annotation.ImportDirective) (Table, []Diagnostic) {
	table := Table{
		named:      make(map[string]Binding),
		namespaces: make(map[string]Binding),
	}
	var diagnostics []Diagnostic
	for _, directive := range directives {
		switch directive.Kind {
		case annotation.ImportNamed:
			for _, item := range directive.Bindings {
				binding := Binding{
					Local: item.Local,
					Reference: annotation.DefinitionReference{
						Package: directive.Package,
						Symbol:  item.Imported,
					},
					Position: directive.Position,
				}
				if prior, collision := table.local(item.Local); collision {
					diagnostics = append(
						diagnostics,
						collisionDiagnostic(directive.Position, item.Local, prior),
					)
					continue
				}
				table.named[item.Local] = binding
			}
		case annotation.ImportNamespace:
			binding := Binding{
				Local:     directive.Namespace,
				Namespace: true,
				Reference: annotation.DefinitionReference{
					Package: directive.Package,
				},
				Position: directive.Position,
			}
			if prior, collision := table.local(directive.Namespace); collision {
				diagnostics = append(
					diagnostics,
					collisionDiagnostic(
						directive.Position,
						directive.Namespace,
						prior,
					),
				)
				continue
			}
			table.namespaces[directive.Namespace] = binding
		default:
			diagnostics = append(diagnostics, Diagnostic{
				Position: directive.Position,
				Message: fmt.Sprintf(
					"annotation import has unsupported binding kind %q",
					directive.Kind,
				),
			})
		}
	}
	sort.SliceStable(diagnostics, func(i, j int) bool {
		left, right := diagnostics[i], diagnostics[j]
		if left.Position.Filename != right.Position.Filename {
			return left.Position.Filename < right.Position.Filename
		}
		if left.Position.Offset != right.Position.Offset {
			return left.Position.Offset < right.Position.Offset
		}
		return left.Message < right.Message
	})
	return table, diagnostics
}

// Lookup resolves one invocation name to its descriptor source identity.
func (table Table) Lookup(name string) (annotation.DefinitionReference, bool) {
	qualifier, symbol, qualified := strings.Cut(name, ".")
	if !qualified {
		binding, found := table.named[name]
		return binding.Reference, found
	}
	if qualifier == "" || symbol == "" || strings.Contains(symbol, ".") {
		return annotation.DefinitionReference{}, false
	}
	binding, found := table.namespaces[qualifier]
	if !found {
		return annotation.DefinitionReference{}, false
	}
	reference := binding.Reference
	reference.Symbol = symbol
	return reference, true
}

// Bindings returns all direct and namespace bindings in stable local-name order.
func (table Table) Bindings() []Binding {
	result := make([]Binding, 0, len(table.named)+len(table.namespaces))
	for _, binding := range table.named {
		result = append(result, binding)
	}
	for _, binding := range table.namespaces {
		result = append(result, binding)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Local < result[j].Local
	})
	return result
}

func (table Table) local(name string) (Binding, bool) {
	if binding, found := table.named[name]; found {
		return binding, true
	}
	binding, found := table.namespaces[name]
	return binding, found
}

func collisionDiagnostic(
	position token.Position,
	local string,
	prior Binding,
) Diagnostic {
	return Diagnostic{
		Position: position,
		Message: fmt.Sprintf(
			"annotation import name %q conflicts with the binding at %s:%d",
			local,
			prior.Position.Filename,
			prior.Position.Line,
		),
	}
}
