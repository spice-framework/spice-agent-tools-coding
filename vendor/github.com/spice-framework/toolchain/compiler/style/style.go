// @import { NamedInterface } from "github.com/spice-framework/spice/annotation/modulith"

// Package style enforces optional source-organization profiles over the shared
// typed compiler program. Profiles constrain handwritten application shape;
// they never rewrite source or affect ordinary Go type checking.
//
// @NamedInterface("style")
package style

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/spice/annotation/sdk"
	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/compiler/provider"
	"github.com/spice-framework/toolchain/compiler/resolve"
)

// Profile identifies one opt-in source-organization contract.
type Profile string

const (
	// ProfileNone preserves ordinary valid Go source organization.
	ProfileNone Profile = ""
	// ProfileJavaStructured makes named types and receiver methods the center of
	// handwritten application source.
	ProfileJavaStructured Profile = "java-structured"
)

// ValidateProfile rejects unknown profile names.
func ValidateProfile(profile Profile) error {
	switch profile {
	case ProfileNone, ProfileJavaStructured:
		return nil
	default:
		return fmt.Errorf(
			"unsupported Spice profile %q; expected %q",
			profile,
			ProfileJavaStructured,
		)
	}
}

// Diagnostic is one deterministic style-profile violation.
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

// Catalog is the immutable style validation result.
type Catalog struct {
	diagnostics []Diagnostic
}

// Diagnostics returns deterministic profile diagnostics.
func (catalog Catalog) Diagnostics() []Diagnostic {
	return append([]Diagnostic(nil), catalog.diagnostics...)
}

// Build validates one optional profile against handwritten production source.
func Build(
	program *load.Program,
	resolution resolve.Result,
	providers provider.Catalog,
	profile Profile,
) Catalog {
	if profile == ProfileNone {
		return Catalog{}
	}
	if err := ValidateProfile(profile); err != nil {
		return Catalog{diagnostics: []Diagnostic{{
			Kind:    "profile",
			Message: err.Error(),
		}}}
	}
	if program == nil {
		return Catalog{diagnostics: []Diagnostic{{
			Kind:    "internal",
			Message: "style profile requires a loaded program",
		}}}
	}

	files := handwrittenFiles(program.PrimaryPackages())
	typesByFile, typeFiles := declaredTypes(program.PrimarySymbols(), files)
	annotatedFunctions := functionAnnotationKinds(resolution)
	packageNames := primaryPackageNames(program.PrimaryPackages())
	catalog := Catalog{}
	catalog.diagnostics = append(
		catalog.diagnostics,
		fileStructureDiagnostics(files)...,
	)
	catalog.diagnostics = append(
		catalog.diagnostics,
		receiverLocationDiagnostics(
			program.PrimarySymbols(),
			files,
			typeFiles,
		)...,
	)
	catalog.diagnostics = append(
		catalog.diagnostics,
		freeFunctionDiagnostics(
			program.PrimarySymbols(),
			files,
			typesByFile,
			annotatedFunctions,
			packageNames,
		)...,
	)
	catalog.diagnostics = append(
		catalog.diagnostics,
		constructorDiagnostics(providers.Providers())...,
	)
	catalog.diagnostics = append(
		catalog.diagnostics,
		implicitInterfaceDiagnostics(
			program.PrimarySymbols(),
			providers.Providers(),
		)...,
	)
	sortDiagnostics(catalog.diagnostics)
	return catalog
}

type sourceFile struct {
	packagePath string
	packageName string
	physical    string
	fileSet     *token.FileSet
	syntax      *ast.File
}

func handwrittenFiles(packages []load.Package) map[string]sourceFile {
	result := make(map[string]sourceFile)
	for _, pkg := range packages {
		if pkg.Raw == nil || pkg.Raw.Fset == nil {
			continue
		}
		for _, file := range pkg.Files {
			physical := cleanFile(file.PhysicalPath)
			if file.Syntax == nil ||
				strings.HasSuffix(physical, "_test.go") ||
				ast.IsGenerated(file.Syntax) {
				continue
			}
			result[physical] = sourceFile{
				packagePath: pkg.Path,
				packageName: pkg.Name,
				physical:    physical,
				fileSet:     pkg.Raw.Fset,
				syntax:      file.Syntax,
			}
		}
	}
	return result
}

func declaredTypes(
	symbols []load.Symbol,
	files map[string]sourceFile,
) (map[string][]string, map[string]string) {
	byFile := make(map[string][]string)
	byType := make(map[string]string)
	for _, symbol := range symbols {
		if symbol.Kind != load.SymbolType {
			continue
		}
		file := cleanFile(symbol.PhysicalPosition.Filename)
		if _, handwritten := files[file]; !handwritten {
			continue
		}
		byFile[file] = append(byFile[file], symbol.Name)
		byType[symbol.PackagePath+"\x00"+symbol.Name] = file
	}
	for file := range byFile {
		sort.Strings(byFile[file])
	}
	return byFile, byType
}

func fileStructureDiagnostics(files map[string]sourceFile) []Diagnostic {
	paths := sortedFilePaths(files)
	var diagnostics []Diagnostic
	for _, path := range paths {
		file := files[path]
		var typeSpecs []*ast.TypeSpec
		for _, declaration := range file.syntax.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.TYPE {
				continue
			}
			for _, specification := range general.Specs {
				if typed, ok := specification.(*ast.TypeSpec); ok {
					typeSpecs = append(typeSpecs, typed)
				}
			}
		}
		if len(typeSpecs) > 1 {
			position, physical := positions(file, typeSpecs[1].Name.Pos())
			diagnostics = append(diagnostics, Diagnostic{
				Position:         position,
				PhysicalPosition: physical,
				Kind:             "one-type-per-file",
				Message: fmt.Sprintf(
					"profile %s requires one named type per handwritten production file; %s declares %d",
					ProfileJavaStructured,
					filepath.Base(path),
					len(typeSpecs),
				),
			})
		}
		if strings.EqualFold(filepath.Base(path), "package.go") {
			for _, declaration := range file.syntax.Decls {
				if general, ok := declaration.(*ast.GenDecl); ok &&
					general.Tok == token.IMPORT {
					continue
				}
				position, physical := positions(file, declaration.Pos())
				diagnostics = append(diagnostics, Diagnostic{
					Position:         position,
					PhysicalPosition: physical,
					Kind:             "package-file-declaration",
					Message: fmt.Sprintf(
						"profile %s reserves package.go for package documentation, annotation imports, and the package clause",
						ProfileJavaStructured,
					),
				})
			}
		}
		for _, declaration := range file.syntax.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.VAR {
				continue
			}
			for _, specification := range general.Specs {
				value, ok := specification.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, name := range value.Names {
					position, physical := positions(file, name.Pos())
					diagnostics = append(diagnostics, Diagnostic{
						Position:         position,
						PhysicalPosition: physical,
						Kind:             "mutable-package-global",
						Message: fmt.Sprintf(
							"profile %s forbids mutable package variable %s; move state onto a managed type",
							ProfileJavaStructured,
							name.Name,
						),
					})
				}
			}
		}
		for _, declaration := range file.syntax.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil || function.Name.Name != "init" {
				continue
			}
			position, physical := positions(file, function.Name.Pos())
			diagnostics = append(diagnostics, Diagnostic{
				Position:         position,
				PhysicalPosition: physical,
				Kind:             "init-function",
				Message: fmt.Sprintf(
					"profile %s forbids init functions; make initialization an explicit constructor or lifecycle method",
					ProfileJavaStructured,
				),
			})
		}
	}
	return diagnostics
}

func receiverLocationDiagnostics(
	symbols []load.Symbol,
	files map[string]sourceFile,
	typeFiles map[string]string,
) []Diagnostic {
	var diagnostics []Diagnostic
	for _, symbol := range symbols {
		if symbol.Kind != load.SymbolMethod {
			continue
		}
		methodFile := cleanFile(symbol.PhysicalPosition.Filename)
		if _, handwritten := files[methodFile]; !handwritten {
			continue
		}
		typeFile := typeFiles[symbol.PackagePath+"\x00"+symbol.Receiver]
		if typeFile == "" || typeFile == methodFile {
			continue
		}
		diagnostics = append(diagnostics, symbolDiagnostic(
			symbol,
			"receiver-method-outside-type-file",
			fmt.Sprintf(
				"profile %s requires method %s to live with receiver type %s in %s",
				ProfileJavaStructured,
				symbol.DisplayLabel,
				symbol.Receiver,
				filepath.Base(typeFile),
			),
		))
	}
	return diagnostics
}

type functionAnnotationKind uint8

const (
	functionAnnotationNone functionAnnotationKind = iota
	functionAnnotationBean
	functionAnnotationTopic
)

func functionAnnotationKinds(
	resolution resolve.Result,
) map[string]functionAnnotationKind {
	result := make(map[string]functionAnnotationKind)
	for _, occurrence := range resolution.Occurrences {
		if occurrence.Target != annotation.TargetFunction {
			continue
		}
		switch {
		case occurrence.HasContribution(sdk.ContributionProvider):
			result[occurrence.SymbolID] = functionAnnotationBean
		case occurrence.HasContribution(sdk.ContributionEventTopic):
			result[occurrence.SymbolID] = functionAnnotationTopic
		}
	}
	return result
}

func freeFunctionDiagnostics(
	symbols []load.Symbol,
	files map[string]sourceFile,
	typesByFile map[string][]string,
	annotations map[string]functionAnnotationKind,
	packageNames map[string]string,
) []Diagnostic {
	var diagnostics []Diagnostic
	for _, symbol := range symbols {
		if symbol.Kind != load.SymbolFunction || symbol.Receiver != "" ||
			symbol.Name == "init" {
			continue
		}
		file := cleanFile(symbol.PhysicalPosition.Filename)
		if _, handwritten := files[file]; !handwritten {
			continue
		}
		if symbol.Name == "main" && packageNames[symbol.PackagePath] == "main" {
			continue
		}
		switch annotations[symbol.ID] {
		case functionAnnotationNone:
		case functionAnnotationBean:
			diagnostics = append(diagnostics, symbolDiagnostic(
				symbol,
				"package-bean",
				fmt.Sprintf(
					"package-level @Bean provider %s is forbidden by profile %s; move it onto an @Configuration type",
					symbol.DisplayLabel,
					ProfileJavaStructured,
				),
			))
			continue
		case functionAnnotationTopic:
			diagnostics = append(diagnostics, symbolDiagnostic(
				symbol,
				"function-topic",
				fmt.Sprintf(
					"function-owned event topic %s is forbidden by profile %s; annotate the payload type",
					symbol.DisplayLabel,
					ProfileJavaStructured,
				),
			))
			continue
		}
		if associatedFunction(symbol.Name, typesByFile[file]) {
			continue
		}
		diagnostics = append(diagnostics, symbolDiagnostic(
			symbol,
			"free-function",
			fmt.Sprintf(
				"package function %s is not type-associated under profile %s; use a receiver method or managed component",
				symbol.DisplayLabel,
				ProfileJavaStructured,
			),
		))
	}
	return diagnostics
}

func associatedFunction(name string, typeNames []string) bool {
	for _, typeName := range typeNames {
		if name == "New"+typeName ||
			name == "Parse"+typeName ||
			name == "Must"+typeName ||
			strings.HasPrefix(name, typeName+"From") &&
				len(name) > len(typeName)+len("From") {
			return true
		}
	}
	return false
}

func constructorDiagnostics(providers []provider.Provider) []Diagnostic {
	var diagnostics []Diagnostic
	for _, item := range providers {
		if item.Source != provider.SourceStereotype ||
			item.Construction != provider.ConstructionFactory {
			continue
		}
		expected := "New" + item.Symbol.Name
		if item.Constructor.Name != expected {
			diagnostics = append(diagnostics, symbolDiagnostic(
				item.Constructor,
				"constructor-name",
				fmt.Sprintf(
					"profile %s requires constructor %s for managed type %s; generic or custom constructor names are forbidden",
					ProfileJavaStructured,
					expected,
					item.Symbol.DisplayLabel,
				),
			))
		}
		if cleanFile(item.Constructor.PhysicalPosition.Filename) !=
			cleanFile(item.Symbol.PhysicalPosition.Filename) {
			diagnostics = append(diagnostics, symbolDiagnostic(
				item.Constructor,
				"constructor-outside-type-file",
				fmt.Sprintf(
					"profile %s requires constructor %s to live with managed type %s in %s",
					ProfileJavaStructured,
					item.Constructor.Name,
					item.Symbol.DisplayLabel,
					filepath.Base(item.Symbol.PhysicalPosition.Filename),
				),
			))
		}
	}
	return diagnostics
}

func implicitInterfaceDiagnostics(
	symbols []load.Symbol,
	providers []provider.Provider,
) []Diagnostic {
	interfaces := namedInterfaces(symbols)
	var diagnostics []Diagnostic
	for _, item := range providers {
		if item.Source != provider.SourceStereotype || item.Output == nil {
			continue
		}
		explicit := make(map[string]struct{}, len(item.Interfaces))
		for _, binding := range item.Interfaces {
			explicit[binding.TypeID] = struct{}{}
		}
		for _, candidate := range interfaces {
			if _, declared := explicit[candidate.typeID]; declared ||
				!types.Implements(item.Output, candidate.interfaceType) {
				continue
			}
			diagnostics = append(diagnostics, symbolDiagnostic(
				item.Symbol,
				"implicit-managed-interface",
				fmt.Sprintf(
					"managed type %s satisfies application interface %s; profile %s requires an explicit @Implements(%s) relationship",
					item.OutputTypeID,
					candidate.typeID,
					ProfileJavaStructured,
					candidate.typeID,
				),
			))
		}
	}
	return diagnostics
}

type namedInterface struct {
	typeID        string
	interfaceType *types.Interface
}

func namedInterfaces(symbols []load.Symbol) []namedInterface {
	var result []namedInterface
	for _, symbol := range symbols {
		if symbol.Kind != load.SymbolType {
			continue
		}
		typeName, ok := symbol.Object.(*types.TypeName)
		if !ok || typeName.IsAlias() {
			continue
		}
		named, ok := types.Unalias(typeName.Type()).(*types.Named)
		if !ok || named.TypeParams() != nil && named.TypeParams().Len() != 0 {
			continue
		}
		interfaceType, ok := named.Underlying().(*types.Interface)
		if !ok {
			continue
		}
		interfaceType.Complete()
		result = append(result, namedInterface{
			typeID:        provider.TypeID(named),
			interfaceType: interfaceType,
		})
	}
	sort.SliceStable(result, func(i, j int) bool {
		return result[i].typeID < result[j].typeID
	})
	return result
}

func primaryPackageNames(packages []load.Package) map[string]string {
	result := make(map[string]string, len(packages))
	for _, pkg := range packages {
		result[pkg.Path] = pkg.Name
	}
	return result
}

func positions(
	file sourceFile,
	position token.Pos,
) (token.Position, token.Position) {
	return file.fileSet.PositionFor(position, true),
		file.fileSet.PositionFor(position, false)
}

func symbolDiagnostic(
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

func sortedFilePaths(files map[string]sourceFile) []string {
	result := make([]string, 0, len(files))
	for path := range files {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func cleanFile(value string) string {
	if value == "" {
		return ""
	}
	return filepath.Clean(value)
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
