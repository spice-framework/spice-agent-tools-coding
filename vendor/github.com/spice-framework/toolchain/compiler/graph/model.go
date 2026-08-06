// Package graph validates exact-type provider dependencies and builds a
// deterministic dependency-first provider construction order.
package graph

import (
	"fmt"
	"go/token"

	"github.com/spice-framework/toolchain/compiler/provider"
)

// Node represents one active bootstrap provider.
// Provider points at the exact record returned by the catalog used for Build.
type Node struct {
	ProviderID string
	provider   *provider.Provider
}

// Provider returns a defensive copy of the exact catalog record represented by
// the node. Live go/types values still belong to the catalog's compiler run.
func (n Node) Provider() provider.Provider {
	return cloneProvider(n.provider)
}

// Edge represents one provider parameter resolved to its exact provider.
type Edge struct {
	ConsumerID       string
	DependencyID     string
	RequiredTypeID   string
	ParameterIndex   int
	ParameterName    string
	DependencyKind   provider.DependencyKind
	CollectionIndex  int
	Position         token.Position
	PhysicalPosition token.Position
	consumer         *provider.Provider
	dependency       *provider.Provider
}

// Consumer returns a defensive copy of the consuming provider record.
func (e Edge) Consumer() provider.Provider {
	return cloneProvider(e.consumer)
}

// Dependency returns a defensive copy of the dependency provider record.
func (e Edge) Dependency() provider.Provider {
	return cloneProvider(e.dependency)
}

// Diagnostic is one deterministic source-positioned graph failure.
type Diagnostic struct {
	Position         token.Position
	PhysicalPosition token.Position
	ProviderID       string
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

// Result is the immutable-by-convention graph metadata for one provider catalog.
type Result struct {
	providers   []provider.Provider
	nodes       []Node
	edges       []Edge
	order       []*provider.Provider
	diagnostics []Diagnostic
}

// Nodes returns a defensive copy of nodes in stable provider-ID order.
func (r Result) Nodes() []Node {
	return append([]Node(nil), r.nodes...)
}

// Edges returns a defensive copy of parameter edges in stable consumer and
// parameter order.
func (r Result) Edges() []Edge {
	return append([]Edge(nil), r.edges...)
}

// ConstructionOrder returns providers in deterministic dependency-first order.
// It is empty whenever Diagnostics is non-empty.
func (r Result) ConstructionOrder() []provider.Provider {
	result := make([]provider.Provider, len(r.order))
	for index, item := range r.order {
		result[index] = cloneProvider(item)
	}
	return result
}

// Diagnostics returns a defensive copy of deterministic graph diagnostics.
func (r Result) Diagnostics() []Diagnostic {
	return append([]Diagnostic(nil), r.diagnostics...)
}

func cloneProvider(item *provider.Provider) provider.Provider {
	if item == nil {
		return provider.Provider{}
	}
	result := *item
	result.Dependencies = append([]provider.Dependency(nil), item.Dependencies...)
	for index := range result.Dependencies {
		result.Dependencies[index].Qualifiers = append(
			[]string(nil),
			item.Dependencies[index].Qualifiers...,
		)
	}
	result.Aliases = append([]string(nil), item.Aliases...)
	result.Qualifiers = append([]string(nil), item.Qualifiers...)
	result.Interfaces = append(
		[]provider.InterfaceBinding(nil),
		item.Interfaces...,
	)
	return result
}
