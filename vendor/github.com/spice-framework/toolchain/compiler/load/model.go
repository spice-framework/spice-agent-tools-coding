// @import { NamedInterface } from "github.com/spice-framework/spice/annotation/modulith"

// Package load provides Spice's single type-aware Go package loading boundary.
//
// @NamedInterface("load")
package load

import (
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// SymbolKind identifies a declaration kind in a loaded program.
type SymbolKind string

const (
	SymbolPackage  SymbolKind = "package"
	SymbolType     SymbolKind = "type"
	SymbolFunction SymbolKind = "function"
	SymbolMethod   SymbolKind = "method"
	SymbolVariable SymbolKind = "variable"
	SymbolConstant SymbolKind = "constant"
)

// Diagnostic is a normalized package-list, parse, or type-checking diagnostic.
// Position remains the familiar rendered Go position. Filename, Line, and
// Column retain its structured components so deterministic ordering is numeric.
type Diagnostic struct {
	PackagePath string
	Position    string
	Filename    string
	Line        int
	Column      int
	Kind        string
	Message     string
}

// String returns a deterministic, human-readable diagnostic.
func (d Diagnostic) String() string {
	location := d.Position
	if location == "" {
		location = d.PackagePath
	}
	if location == "" {
		location = "go/packages"
	}
	return location + ": " + d.Message
}

// SourceFile pairs one deterministic compiled-file identity with its syntax
// tree. Syntax may be nil when the package driver reports a compiled file for
// which go/packages could not retain an AST.
type SourceFile struct {
	PhysicalPath string
	Syntax       *ast.File
}

// Symbol is a stable logical declaration record backed by live go/types and AST
// references from one Program instance. ID is the canonical machine key;
// DisplayLabel is a concise human label and must not be used as an identity key.
// Position is developer-facing and //line-adjusted; PhysicalPosition identifies
// the loaded Go file.
type Symbol struct {
	ID               string
	DisplayLabel     string
	Kind             SymbolKind
	Name             string
	PackagePath      string
	Receiver         string
	Position         token.Position
	PhysicalPosition token.Position
	Object           types.Object
	Node             ast.Node
	Signature        *types.Signature
}

// Package is one root package selected by the standard Go package driver.
// Auxiliary packages share the Program's type universe but are excluded from
// primary annotation and application-module discovery.
type Package struct {
	ID              string
	Path            string
	Name            string
	Dir             string
	ModulePath      string
	Auxiliary       bool
	Files           []SourceFile
	CompiledGoFiles []string
	IllTyped        bool
	Types           *types.Package
	TypesInfo       *types.Info
	Syntax          []*ast.File
	Raw             *packages.Package
}

// Program is the immutable-by-convention result of one Load call. Its methods
// return copies of ordered record slices. Live type and syntax references must
// never be combined with values from another Program.
type Program struct {
	packages    []Package
	symbols     []Symbol
	diagnostics []Diagnostic
}

// Packages returns deterministically ordered root package records.
func (p *Program) Packages() []Package {
	if p == nil {
		return nil
	}
	return clonePackages(p.packages)
}

// PrimaryPackages returns roots selected for application annotation and module
// analysis, excluding auxiliary compiler-extension packages.
func (p *Program) PrimaryPackages() []Package {
	if p == nil {
		return nil
	}
	result := make([]Package, 0, len(p.packages))
	for _, pkg := range p.packages {
		if !pkg.Auxiliary {
			result = append(result, pkg)
		}
	}
	return clonePackages(result)
}

func clonePackages(packages []Package) []Package {
	result := make([]Package, len(packages))
	copy(result, packages)
	for i := range result {
		result[i].Files = append([]SourceFile(nil), result[i].Files...)
		result[i].CompiledGoFiles = append([]string(nil), result[i].CompiledGoFiles...)
		result[i].Syntax = append([]*ast.File(nil), result[i].Syntax...)
	}
	return result
}

// Symbols returns deterministically ordered declaration records.
func (p *Program) Symbols() []Symbol {
	if p == nil {
		return nil
	}
	result := make([]Symbol, len(p.symbols))
	copy(result, p.symbols)
	return result
}

// PrimarySymbols returns declarations owned by primary application roots.
func (p *Program) PrimarySymbols() []Symbol {
	if p == nil {
		return nil
	}
	auxiliary := make(map[string]struct{})
	for _, pkg := range p.packages {
		if pkg.Auxiliary {
			auxiliary[pkg.Path] = struct{}{}
		}
	}
	result := make([]Symbol, 0, len(p.symbols))
	for _, symbol := range p.symbols {
		if _, excluded := auxiliary[symbol.PackagePath]; !excluded {
			result = append(result, symbol)
		}
	}
	return result
}

// Diagnostics returns deterministically ordered load diagnostics.
func (p *Program) Diagnostics() []Diagnostic {
	if p == nil {
		return nil
	}
	result := make([]Diagnostic, len(p.diagnostics))
	copy(result, p.diagnostics)
	return result
}

// LoadError reports that one or more requested root packages could not be
// safely used for semantic generation.
type LoadError struct {
	Diagnostics []Diagnostic
}

func (e *LoadError) Error() string {
	if e == nil || len(e.Diagnostics) == 0 {
		return "Spice package loading failed"
	}
	var builder strings.Builder
	builder.WriteString("Spice package loading failed")
	for _, diagnostic := range e.Diagnostics {
		builder.WriteString("\n- ")
		builder.WriteString(diagnostic.String())
	}
	return builder.String()
}

func sortDiagnostics(diagnostics []Diagnostic) {
	sort.SliceStable(diagnostics, func(i, j int) bool {
		left, right := diagnostics[i], diagnostics[j]
		if left.PackagePath != right.PackagePath {
			return left.PackagePath < right.PackagePath
		}
		if left.Filename != right.Filename {
			return left.Filename < right.Filename
		}
		if left.Line != right.Line {
			return left.Line < right.Line
		}
		if left.Column != right.Column {
			return left.Column < right.Column
		}
		if left.Position != right.Position {
			return left.Position < right.Position
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		return left.Message < right.Message
	})
}
