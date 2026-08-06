package service

import (
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"sort"

	"github.com/spice-framework/toolchain/compiler/diagnostic"
	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/compiler/provider"
	"golang.org/x/tools/go/packages"
)

type typedPackage struct {
	path  string
	name  string
	files []string
	types *types.Package
	raw   *packages.Package
}

// summarizeGoInterfaces creates the editor-facing type catalog from the same
// go/packages type universe used by provider validation and generation. It
// intentionally walks typed imports as well as primary roots so interface
// semantics never depend on an IDE index.
func summarizeGoInterfaces(
	workspaceRoot string,
	program *load.Program,
) GoInterfaceCatalog {
	packagesByPath := loadedTypedPackages(program)
	paths := make([]string, 0, len(packagesByPath))
	for packagePath := range packagesByPath {
		paths = append(paths, packagePath)
	}
	slices.Sort(paths)

	result := GoInterfaceCatalog{
		Packages: make([]GoInterfacePackage, 0, len(paths)),
	}
	for _, packagePath := range paths {
		pkg := packagesByPath[packagePath]
		interfaces := packageInterfaces(workspaceRoot, pkg)
		if len(interfaces) == 0 {
			continue
		}
		result.Packages = append(
			result.Packages,
			GoInterfacePackage{
				Name:       pkg.name,
				Path:       pkg.path,
				Files:      slices.Clone(pkg.files),
				Interfaces: interfaces,
			},
		)
	}
	return result
}

func loadedTypedPackages(program *load.Program) map[string]typedPackage {
	result := make(map[string]typedPackage)
	for _, pkg := range program.Packages() {
		collectTypedPackage(result, pkg.Raw)
		if pkg.Path == "" || pkg.Types == nil {
			continue
		}
		item := result[pkg.Path]
		item.path = pkg.Path
		item.name = pkg.Name
		item.files = cleanInterfaceFiles(pkg.CompiledGoFiles)
		item.types = pkg.Types
		item.raw = pkg.Raw
		result[pkg.Path] = item
	}
	return result
}

func collectTypedPackage(
	result map[string]typedPackage,
	raw *packages.Package,
) {
	if raw == nil {
		return
	}
	if raw.PkgPath != "" && raw.Types != nil {
		if _, found := result[raw.PkgPath]; found {
			return
		}
		result[raw.PkgPath] = typedPackage{
			path:  raw.PkgPath,
			name:  raw.Name,
			files: cleanInterfaceFiles(raw.CompiledGoFiles),
			types: raw.Types,
			raw:   raw,
		}
	}
	importPaths := make([]string, 0, len(raw.Imports))
	for packagePath := range raw.Imports {
		importPaths = append(importPaths, packagePath)
	}
	slices.Sort(importPaths)
	for _, packagePath := range importPaths {
		collectTypedPackage(result, raw.Imports[packagePath])
	}
}

func cleanInterfaceFiles(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		result = append(result, filepath.Clean(value))
	}
	slices.Sort(result)
	return slices.Compact(result)
}

func packageInterfaces(
	workspaceRoot string,
	pkg typedPackage,
) []GoInterface {
	if pkg.types == nil || pkg.types.Scope() == nil {
		return nil
	}
	names := pkg.types.Scope().Names()
	slices.Sort(names)
	result := make([]GoInterface, 0)
	for _, name := range names {
		object, ok := pkg.types.Scope().Lookup(name).(*types.TypeName)
		if !ok {
			continue
		}
		named, ok := types.Unalias(object.Type()).(*types.Named)
		if !ok {
			continue
		}
		contract, ok := named.Underlying().(*types.Interface)
		if !ok {
			continue
		}
		contract.Complete()
		if !contract.IsMethodSet() {
			continue
		}
		item := GoInterface{
			Name:           object.Name(),
			PackageName:    pkg.name,
			PackagePath:    pkg.path,
			TypeID:         provider.TypeID(object.Type()),
			TypeParameters: interfaceTypeParameters(named),
			Methods:        interfaceMethods(pkg.types, object.Type()),
			Exported:       object.Exported(),
		}
		display, physical := interfaceObjectPositions(pkg.raw, object)
		if display.Filename != "" || physical.Filename != "" {
			item.Location = diagnostic.SourceMappedLocation(
				workspaceRoot,
				display.Filename,
				physical.Filename,
				display.Line,
				display.Column,
				display.Offset,
				physical.Line,
				physical.Column,
				physical.Offset,
			)
			item.HasLocation = item.Location.Path != ""
		}
		result = append(result, item)
	}
	sort.SliceStable(result, func(left, right int) bool {
		return result[left].Name < result[right].Name
	})
	return result
}

func interfaceTypeParameters(named *types.Named) []string {
	parameters := named.TypeParams()
	if parameters == nil || parameters.Len() == 0 {
		return nil
	}
	result := make([]string, parameters.Len())
	for index := range parameters.Len() {
		result[index] = parameters.At(index).Obj().Name()
	}
	return result
}

func interfaceMethods(
	owner *types.Package,
	interfaceType types.Type,
) []GoInterfaceMethod {
	methodSet := types.NewMethodSet(interfaceType)
	result := make([]GoInterfaceMethod, 0, methodSet.Len())
	for selection := range methodSet.Methods() {
		method := selection.Obj()
		result = append(result, GoInterfaceMethod{
			Name: method.Name(),
			Signature: types.TypeString(
				method.Type(),
				types.RelativeTo(owner),
			),
		})
	}
	sort.SliceStable(result, func(left, right int) bool {
		if result[left].Name != result[right].Name {
			return result[left].Name < result[right].Name
		}
		return result[left].Signature < result[right].Signature
	})
	return result
}

func interfaceObjectPositions(
	raw *packages.Package,
	object types.Object,
) (token.Position, token.Position) {
	if raw == nil || raw.Fset == nil || object == nil ||
		!object.Pos().IsValid() {
		return token.Position{}, token.Position{}
	}
	return raw.Fset.PositionFor(object.Pos(), true),
		raw.Fset.PositionFor(object.Pos(), false)
}
