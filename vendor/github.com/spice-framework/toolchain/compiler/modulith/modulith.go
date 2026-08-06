// @import { NamedInterface } from "github.com/spice-framework/spice/annotation/modulith"

// Package modulith discovers and validates Spice application-module metadata
// from the shared typed compiler program.
//
// @NamedInterface("modulith")
package modulith

import (
	"fmt"
	"go/token"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/spice/annotation/sdk"
	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/compiler/resolve"
)

const dependencySeparator = "::"

var interfaceNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// Package identifies one selected Go package and its module assignment.
type Package struct {
	Path         string
	Name         string
	GoModulePath string
	Root         bool
}

// NamedInterface exposes one otherwise-internal module package under a stable
// interface name.
type NamedInterface struct {
	Name             string
	PackagePath      string
	Position         token.Position
	PhysicalPosition token.Position
}

// Dependency identifies one explicitly allowed target module API. An empty
// Interface selects the target module's root-package default API.
type Dependency struct {
	ModuleID         string
	Interface        string
	Position         token.Position
	PhysicalPosition token.Position
}

// Edge is one distinct cross-module Go package import. API is empty for a
// root-package default API, a named-interface name for an exposed descendant,
// and empty with Exported false for an internal target.
type Edge struct {
	FromModule       string
	ToModule         string
	FromPackage      string
	ToPackage        string
	API              string
	Exported         bool
	Allowed          bool
	Position         token.Position
	PhysicalPosition token.Position
}

// Cycle describes one strongly connected module component and a deterministic
// representative closed path through it.
type Cycle struct {
	Members []string
	Path    []string
}

// String renders the portable dependency identity accepted by @Module.
func (d Dependency) String() string {
	if d.Interface == "" {
		return d.ModuleID
	}
	return d.ModuleID + dependencySeparator + d.Interface
}

// Module is one package-rooted application module. ID is always the root
// package's full Go import path.
type Module struct {
	ID               string
	RootPackage      string
	GoModulePath     string
	Position         token.Position
	PhysicalPosition token.Position
	packages         []Package
	namedInterfaces  []NamedInterface
	allowed          []Dependency
}

// Packages returns owned packages sorted by import path. The root package is
// marked Root and forms the module's default API.
func (m Module) Packages() []Package {
	return append([]Package(nil), m.packages...)
}

// NamedInterfaces returns explicit APIs sorted by name and package path.
func (m Module) NamedInterfaces() []NamedInterface {
	return append([]NamedInterface(nil), m.namedInterfaces...)
}

// AllowedDependencies returns exact allowed module APIs in stable order.
func (m Module) AllowedDependencies() []Dependency {
	return append([]Dependency(nil), m.allowed...)
}

// Diagnostic is one deterministic, source-positioned module-model failure.
type Diagnostic struct {
	Position         token.Position
	PhysicalPosition token.Position
	ModuleID         string
	PackagePath      string
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

// Model is the immutable-by-convention module discovery result.
type Model struct {
	modules         []Module
	edges           []Edge
	cycles          []Cycle
	unassigned      []Package
	diagnostics     []Diagnostic
	packageOwner    map[string]int
	focusID         string
	dependencyOrder []string
}

// FocusID returns the selected module for a focused test graph, or an empty
// string for the complete architecture model.
func (m Model) FocusID() string {
	return m.focusID
}

// DependencyOrder returns focused modules in dependency-first order.
func (m Model) DependencyOrder() []string {
	return append([]string(nil), m.dependencyOrder...)
}

// Edges returns distinct cross-module package imports in stable order.
func (m Model) Edges() []Edge {
	return append([]Edge(nil), m.edges...)
}

// Cycles returns module strongly connected components in stable member order.
func (m Model) Cycles() []Cycle {
	result := make([]Cycle, len(m.cycles))
	for index, cycle := range m.cycles {
		result[index] = Cycle{
			Members: append([]string(nil), cycle.Members...),
			Path:    append([]string(nil), cycle.Path...),
		}
	}
	return result
}

// Modules returns modules sorted by full import-path identity.
func (m Model) Modules() []Module {
	result := make([]Module, len(m.modules))
	for index := range m.modules {
		result[index] = cloneModule(m.modules[index])
	}
	return result
}

// UnassignedPackages returns selected packages in a Go module containing at
// least one @Module root but not owned by any root.
func (m Model) UnassignedPackages() []Package {
	return append([]Package(nil), m.unassigned...)
}

// Diagnostics returns deterministic semantic discovery failures.
func (m Model) Diagnostics() []Diagnostic {
	return append([]Diagnostic(nil), m.diagnostics...)
}

// Owner returns the module owning packagePath. Descendants belong to the
// longest matching root, so nested module roots are deterministic.
func (m Model) Owner(packagePath string) (Module, bool) {
	index, ok := m.packageOwner[packagePath]
	if !ok || index < 0 || index >= len(m.modules) {
		return Module{}, false
	}
	return cloneModule(m.modules[index]), true
}

// Focus retains moduleID plus only its transitively observed dependencies.
// The returned dependency order is suitable for composing a module test graph.
func (m Model) Focus(moduleID string) (Model, error) {
	found := false
	for _, module := range m.modules {
		if module.ID == moduleID {
			found = true
			break
		}
	}
	if !found {
		return Model{}, fmt.Errorf("focus module %q was not found", moduleID)
	}

	adjacency := moduleAdjacency(m.modules, m.edges)
	order, err := focusedDependencyOrder(moduleID, adjacency)
	if err != nil {
		return Model{}, err
	}
	included := make(map[string]struct{}, len(order))
	for _, id := range order {
		included[id] = struct{}{}
	}
	result := Model{
		packageOwner:    make(map[string]int),
		focusID:         moduleID,
		dependencyOrder: order,
	}
	for _, module := range m.modules {
		if _, ok := included[module.ID]; ok {
			result.modules = append(result.modules, cloneModule(module))
		}
	}
	for _, edge := range m.edges {
		_, fromIncluded := included[edge.FromModule]
		_, toIncluded := included[edge.ToModule]
		if fromIncluded && toIncluded {
			result.edges = append(result.edges, edge)
		}
	}
	for _, cycle := range m.cycles {
		if cycleIncluded(cycle, included) {
			result.cycles = append(result.cycles, Cycle{
				Members: append([]string(nil), cycle.Members...),
				Path:    append([]string(nil), cycle.Path...),
			})
		}
	}
	sortModel(&result)
	rebuildPackageOwners(&result)
	return result, nil
}

// Build discovers module roots, package ownership, named interfaces, allowed
// dependency identities, and unassigned packages without reloading source.
func Build(program *load.Program, resolution resolve.Result) Model {
	if program == nil {
		return Model{diagnostics: []Diagnostic{{
			Kind:    "internal",
			Message: "module discovery requires a loaded program",
		}}}
	}
	if len(resolution.Diagnostics) != 0 {
		return Model{diagnostics: []Diagnostic{{
			Kind:    "resolution",
			Message: "module discovery requires annotation resolution without diagnostics",
		}}}
	}

	primaryPackages := program.PrimaryPackages()
	packages := packageIndex(primaryPackages)
	model := Model{packageOwner: make(map[string]int)}
	model.modules, model.diagnostics = discoverModules(resolution.Occurrences, packages)
	assignPackages(&model, packages)
	discoverNamedInterfaces(&model, resolution.Occurrences, packages)
	validateDependencies(&model)
	discoverImportEdges(&model, primaryPackages)
	discoverCycles(&model)
	sortModel(&model)
	rebuildPackageOwners(&model)
	return model
}

func packageIndex(packages []load.Package) map[string]load.Package {
	result := make(map[string]load.Package, len(packages))
	for _, pkg := range packages {
		result[pkg.Path] = pkg
	}
	return result
}

func discoverModules(
	occurrences []resolve.Occurrence,
	packages map[string]load.Package,
) ([]Module, []Diagnostic) {
	seen := make(map[string]resolve.Occurrence)
	var modules []Module
	var diagnostics []Diagnostic
	for _, occurrence := range occurrences {
		if !occurrence.HasContribution(sdk.ContributionModule) {
			continue
		}
		if occurrence.Target != annotation.TargetPackage {
			diagnostics = append(diagnostics, occurrenceDiagnostic(
				occurrence,
				"invalid-target",
				"@Module must appear in package documentation",
			))
			continue
		}
		if previous, duplicate := seen[occurrence.PackagePath]; duplicate {
			diagnostics = append(diagnostics, occurrenceDiagnostic(
				occurrence,
				"duplicate-module",
				fmt.Sprintf(
					"package %s declares @Module more than once; first declaration is at %s",
					occurrence.PackagePath,
					renderPosition(previous.DisplayPosition),
				),
			))
			continue
		}
		seen[occurrence.PackagePath] = occurrence
		pkg, ok := packages[occurrence.PackagePath]
		if !ok {
			diagnostics = append(diagnostics, occurrenceDiagnostic(
				occurrence,
				"missing-package",
				fmt.Sprintf("@Module package %s is not in the loaded program", occurrence.PackagePath),
			))
			continue
		}
		allowed, allowedDiagnostics := parseDependencies(occurrence)
		diagnostics = append(diagnostics, allowedDiagnostics...)
		modules = append(modules, Module{
			ID:               pkg.Path,
			RootPackage:      pkg.Path,
			GoModulePath:     pkg.ModulePath,
			Position:         occurrence.DisplayPosition,
			PhysicalPosition: physicalPosition(occurrence),
			allowed:          allowed,
		})
	}
	sort.SliceStable(modules, func(i, j int) bool {
		return modules[i].ID < modules[j].ID
	})
	return modules, diagnostics
}

func parseDependencies(occurrence resolve.Occurrence) ([]Dependency, []Diagnostic) {
	if contribution, found := occurrence.DescriptorContribution(
		sdk.ContributionModule,
	); found {
		return parseDependencyValues(
			occurrence,
			contribution.Module.AllowedDependencies,
		)
	}
	var value annotation.Value
	found := false
	for _, argument := range occurrence.Annotation.Arguments {
		if argument.Name != "allowedDependencies" {
			continue
		}
		if found {
			return nil, []Diagnostic{occurrenceDiagnostic(
				occurrence,
				"duplicate-argument",
				"@Module assigns allowedDependencies more than once",
			)}
		}
		found = true
		value = argument.Value
	}
	if !found {
		return nil, nil
	}
	if value.Kind != annotation.KindList {
		return nil, []Diagnostic{occurrenceDiagnostic(
			occurrence,
			"dependency-kind",
			"@Module allowedDependencies must be a list of strings",
		)}
	}

	var diagnostics []Diagnostic
	var values []string
	for index, item := range value.List {
		if item.Kind != annotation.KindString {
			diagnostics = append(diagnostics, occurrenceDiagnostic(
				occurrence,
				"dependency-kind",
				fmt.Sprintf("@Module allowedDependencies item %d must be a string", index),
			))
			continue
		}
		values = append(values, item.String)
	}
	dependencies, parsedDiagnostics := parseDependencyValues(
		occurrence,
		values,
	)
	diagnostics = append(diagnostics, parsedDiagnostics...)
	return dependencies, diagnostics
}

func parseDependencyValues(
	occurrence resolve.Occurrence,
	values []string,
) ([]Dependency, []Diagnostic) {
	seen := make(map[string]struct{})
	var dependencies []Dependency
	var diagnostics []Diagnostic
	for _, value := range values {
		dependency, err := parseDependency(value, occurrence)
		if err != nil {
			diagnostics = append(diagnostics, occurrenceDiagnostic(
				occurrence,
				"dependency-identity",
				fmt.Sprintf("@Module allowed dependency %q is invalid: %v", value, err),
			))
			continue
		}
		if _, duplicate := seen[dependency.String()]; duplicate {
			diagnostics = append(diagnostics, occurrenceDiagnostic(
				occurrence,
				"duplicate-dependency",
				fmt.Sprintf("@Module allowed dependency %q is declared more than once", dependency.String()),
			))
			continue
		}
		seen[dependency.String()] = struct{}{}
		dependencies = append(dependencies, dependency)
	}
	sortDependencies(dependencies)
	return dependencies, diagnostics
}

func parseDependency(value string, occurrence resolve.Occurrence) (Dependency, error) {
	if value == "" || strings.TrimSpace(value) != value {
		return Dependency{}, fmt.Errorf("identity must be non-empty and contain no surrounding whitespace")
	}
	if strings.Count(value, dependencySeparator) > 1 {
		return Dependency{}, fmt.Errorf("identity may contain at most one %q separator", dependencySeparator)
	}
	moduleID, interfaceName, hasInterface := strings.Cut(value, dependencySeparator)
	if moduleID == "" {
		return Dependency{}, fmt.Errorf("module import path is required")
	}
	if hasInterface && !interfaceNamePattern.MatchString(interfaceName) {
		return Dependency{}, fmt.Errorf(
			"named interface must match %s",
			interfaceNamePattern,
		)
	}
	return Dependency{
		ModuleID:         moduleID,
		Interface:        interfaceName,
		Position:         occurrence.DisplayPosition,
		PhysicalPosition: physicalPosition(occurrence),
	}, nil
}

func assignPackages(model *Model, packages map[string]load.Package) {
	modulePaths := make(map[string]struct{}, len(model.modules))
	paths := make([]string, 0, len(packages))
	for _, module := range model.modules {
		modulePaths[module.GoModulePath] = struct{}{}
	}
	for packagePath := range packages {
		paths = append(paths, packagePath)
	}
	sort.Strings(paths)
	for _, packagePath := range paths {
		pkg := packages[packagePath]
		owner := ownerIndex(model.modules, pkg)
		item := Package{
			Path:         pkg.Path,
			Name:         pkg.Name,
			GoModulePath: pkg.ModulePath,
			Root:         owner >= 0 && model.modules[owner].RootPackage == pkg.Path,
		}
		if owner >= 0 {
			model.modules[owner].packages = append(model.modules[owner].packages, item)
			model.packageOwner[pkg.Path] = owner
		} else if _, sameGoModule := modulePaths[pkg.ModulePath]; sameGoModule {
			model.unassigned = append(model.unassigned, item)
		}
	}
}

func ownerIndex(modules []Module, pkg load.Package) int {
	owner := -1
	for index, module := range modules {
		if module.GoModulePath != pkg.ModulePath ||
			(pkg.Path != module.RootPackage && !strings.HasPrefix(pkg.Path, module.RootPackage+"/")) {
			continue
		}
		if owner == -1 || len(module.RootPackage) > len(modules[owner].RootPackage) {
			owner = index
		}
	}
	return owner
}

func discoverNamedInterfaces(
	model *Model,
	occurrences []resolve.Occurrence,
	packages map[string]load.Package,
) {
	seen := make(map[string]resolve.Occurrence)
	for _, occurrence := range occurrences {
		if !occurrence.HasContribution(
			sdk.ContributionNamedInterface,
		) {
			continue
		}
		name, ok := namedInterfaceName(occurrence, &model.diagnostics)
		if !ok {
			continue
		}
		pkg, exists := packages[occurrence.PackagePath]
		if occurrence.Target != annotation.TargetPackage || !exists {
			model.diagnostics = append(model.diagnostics, occurrenceDiagnostic(
				occurrence,
				"invalid-target",
				"@NamedInterface must appear in package documentation for a loaded package",
			))
			continue
		}
		owner := ownerIndex(model.modules, pkg)
		if owner < 0 {
			model.diagnostics = append(model.diagnostics, occurrenceDiagnostic(
				occurrence,
				"unassigned-interface",
				fmt.Sprintf(
					"@NamedInterface %q package %s is not inside an @Module root",
					name,
					pkg.Path,
				),
			))
			continue
		}
		key := model.modules[owner].ID + "\x00" + name
		if previous, duplicate := seen[key]; duplicate {
			model.diagnostics = append(model.diagnostics, occurrenceDiagnostic(
				occurrence,
				"duplicate-interface",
				fmt.Sprintf(
					"module %s declares named interface %q more than once; first declaration is at %s",
					model.modules[owner].ID,
					name,
					renderPosition(previous.DisplayPosition),
				),
			))
			continue
		}
		seen[key] = occurrence
		model.modules[owner].namedInterfaces = append(
			model.modules[owner].namedInterfaces,
			NamedInterface{
				Name:             name,
				PackagePath:      pkg.Path,
				Position:         occurrence.DisplayPosition,
				PhysicalPosition: physicalPosition(occurrence),
			},
		)
	}
}

func namedInterfaceName(occurrence resolve.Occurrence, diagnostics *[]Diagnostic) (string, bool) {
	if contribution, found := occurrence.DescriptorContribution(
		sdk.ContributionNamedInterface,
	); found {
		return validateNamedInterfaceName(
			occurrence,
			contribution.NamedInterface.Name,
			diagnostics,
		)
	}
	var names []string
	for _, argument := range occurrence.Annotation.Arguments {
		if argument.Name != "" && argument.Name != "name" {
			continue
		}
		if argument.Value.Kind != annotation.KindString {
			*diagnostics = append(*diagnostics, occurrenceDiagnostic(
				occurrence,
				"interface-kind",
				"@NamedInterface name must be a string",
			))
			return "", false
		}
		names = append(names, argument.Value.String)
	}
	if len(names) != 1 {
		*diagnostics = append(*diagnostics, occurrenceDiagnostic(
			occurrence,
			"interface-name",
			"@NamedInterface requires exactly one name",
		))
		return "", false
	}
	return validateNamedInterfaceName(
		occurrence,
		names[0],
		diagnostics,
	)
}

func validateNamedInterfaceName(
	occurrence resolve.Occurrence,
	name string,
	diagnostics *[]Diagnostic,
) (string, bool) {
	if !interfaceNamePattern.MatchString(name) {
		*diagnostics = append(*diagnostics, occurrenceDiagnostic(
			occurrence,
			"interface-name",
			fmt.Sprintf("@NamedInterface name %q must match %s", name, interfaceNamePattern),
		))
		return "", false
	}
	return name, true
}

func validateDependencies(model *Model) {
	modules := make(map[string]Module, len(model.modules))
	for _, module := range model.modules {
		modules[module.ID] = module
	}
	for moduleIndex := range model.modules {
		module := &model.modules[moduleIndex]
		for _, dependency := range module.allowed {
			target, ok := modules[dependency.ModuleID]
			switch {
			case dependency.ModuleID == module.ID:
				model.diagnostics = append(model.diagnostics, dependencyDiagnostic(
					*module,
					dependency,
					"self-dependency",
					fmt.Sprintf("module %s must not declare a dependency on itself", module.ID),
				))
			case !ok:
				model.diagnostics = append(model.diagnostics, dependencyDiagnostic(
					*module,
					dependency,
					"unknown-module",
					fmt.Sprintf(
						"module %s allows unknown module %s; use an exact @Module root import path",
						module.ID,
						dependency.ModuleID,
					),
				))
			case dependency.Interface != "" && !hasNamedInterface(target, dependency.Interface):
				model.diagnostics = append(model.diagnostics, dependencyDiagnostic(
					*module,
					dependency,
					"unknown-interface",
					fmt.Sprintf(
						"module %s allows unknown interface %s on module %s",
						module.ID,
						dependency.Interface,
						target.ID,
					),
				))
			}
		}
	}
}

func hasNamedInterface(module Module, name string) bool {
	for _, item := range module.namedInterfaces {
		if item.Name == name {
			return true
		}
	}
	return false
}

type packageImport struct {
	path     string
	position token.Position
	physical token.Position
}

func discoverImportEdges(model *Model, packages []load.Package) {
	seen := make(map[string]struct{})
	for _, pkg := range packages {
		fromIndex, assigned := model.packageOwner[pkg.Path]
		if !assigned {
			continue
		}
		for _, imported := range packageImports(pkg) {
			toIndex, targetAssigned := model.packageOwner[imported.path]
			if !targetAssigned || fromIndex == toIndex {
				continue
			}
			key := pkg.Path + "\x00" + imported.path
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}

			from := model.modules[fromIndex]
			to := model.modules[toIndex]
			api, exported, allowed := importedAPI(from, to, imported.path)
			edge := Edge{
				FromModule:       from.ID,
				ToModule:         to.ID,
				FromPackage:      pkg.Path,
				ToPackage:        imported.path,
				API:              api,
				Exported:         exported,
				Allowed:          allowed,
				Position:         imported.position,
				PhysicalPosition: imported.physical,
			}
			model.edges = append(model.edges, edge)
			switch {
			case !exported:
				model.diagnostics = append(model.diagnostics, importDiagnostic(
					edge,
					"internal-access",
					fmt.Sprintf(
						"module %s package %s imports internal package %s of module %s; import the root API or a declared @NamedInterface package",
						from.ID,
						pkg.Path,
						imported.path,
						to.ID,
					),
				))
			case !allowed:
				model.diagnostics = append(model.diagnostics, importDiagnostic(
					edge,
					"undeclared-dependency",
					fmt.Sprintf(
						"module %s package %s imports %s through package %s without declaring it in @Module allowedDependencies",
						from.ID,
						pkg.Path,
						dependencyIdentity(to.ID, api),
						imported.path,
					),
				))
			}
		}
	}
}

func packageImports(pkg load.Package) []packageImport {
	if pkg.Raw == nil || pkg.Raw.Fset == nil {
		return nil
	}
	var result []packageImport
	for _, source := range pkg.Files {
		if source.Syntax == nil {
			continue
		}
		for _, specification := range source.Syntax.Imports {
			importPath, err := strconv.Unquote(specification.Path.Value)
			if err != nil {
				continue
			}
			result = append(result, packageImport{
				path:     importPath,
				position: pkg.Raw.Fset.PositionFor(specification.Path.Pos(), true),
				physical: pkg.Raw.Fset.PositionFor(specification.Path.Pos(), false),
			})
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].physical.Filename != result[j].physical.Filename {
			return result[i].physical.Filename < result[j].physical.Filename
		}
		if result[i].physical.Offset != result[j].physical.Offset {
			return result[i].physical.Offset < result[j].physical.Offset
		}
		return result[i].path < result[j].path
	})
	return result
}

func importedAPI(from, to Module, packagePath string) (string, bool, bool) {
	if packagePath == to.RootPackage {
		return "", true, allowsDependency(from, to.ID, "")
	}
	var names []string
	for _, item := range to.namedInterfaces {
		if item.PackagePath == packagePath {
			names = append(names, item.Name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "", false, false
	}
	for _, name := range names {
		if allowsDependency(from, to.ID, name) {
			return name, true, true
		}
	}
	return names[0], true, false
}

func allowsDependency(module Module, targetModule, targetInterface string) bool {
	for _, dependency := range module.allowed {
		if dependency.ModuleID == targetModule && dependency.Interface == targetInterface {
			return true
		}
	}
	return false
}

func dependencyIdentity(moduleID, interfaceName string) string {
	if interfaceName == "" {
		return moduleID
	}
	return moduleID + dependencySeparator + interfaceName
}

func importDiagnostic(edge Edge, kind, message string) Diagnostic {
	return Diagnostic{
		Position:         edge.Position,
		PhysicalPosition: edge.PhysicalPosition,
		ModuleID:         edge.FromModule,
		PackagePath:      edge.FromPackage,
		Kind:             kind,
		Message:          message,
	}
}

func discoverCycles(model *Model) {
	adjacency := moduleAdjacency(model.modules, model.edges)
	components := stronglyConnectedComponents(adjacency)
	moduleByID := make(map[string]Module, len(model.modules))
	for _, module := range model.modules {
		moduleByID[module.ID] = module
	}
	for _, members := range components {
		if len(members) < 2 {
			continue
		}
		path := representativeCycle(members, adjacency)
		cycle := Cycle{
			Members: append([]string(nil), members...),
			Path:    path,
		}
		model.cycles = append(model.cycles, cycle)
		module := moduleByID[members[0]]
		model.diagnostics = append(model.diagnostics, Diagnostic{
			Position:         module.Position,
			PhysicalPosition: module.PhysicalPosition,
			ModuleID:         module.ID,
			PackagePath:      module.RootPackage,
			Kind:             "module-cycle",
			Message:          "module dependency cycle: " + strings.Join(path, " -> "),
		})
	}
}

func moduleAdjacency(modules []Module, edges []Edge) map[string][]string {
	result := make(map[string][]string, len(modules))
	seen := make(map[string]struct{})
	for _, module := range modules {
		result[module.ID] = nil
	}
	for _, edge := range edges {
		key := edge.FromModule + "\x00" + edge.ToModule
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		result[edge.FromModule] = append(result[edge.FromModule], edge.ToModule)
	}
	for moduleID := range result {
		sort.Strings(result[moduleID])
	}
	return result
}

type componentSearch struct {
	adjacency map[string][]string
	index     int
	indices   map[string]int
	low       map[string]int
	stack     []string
	onStack   map[string]bool
	result    [][]string
}

func stronglyConnectedComponents(adjacency map[string][]string) [][]string {
	search := componentSearch{
		adjacency: adjacency,
		indices:   make(map[string]int, len(adjacency)),
		low:       make(map[string]int, len(adjacency)),
		onStack:   make(map[string]bool, len(adjacency)),
	}
	nodes := make([]string, 0, len(adjacency))
	for node := range adjacency {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	for _, node := range nodes {
		if _, visited := search.indices[node]; !visited {
			search.connect(node)
		}
	}
	sort.SliceStable(search.result, func(i, j int) bool {
		return search.result[i][0] < search.result[j][0]
	})
	return search.result
}

func (search *componentSearch) connect(node string) {
	search.indices[node] = search.index
	search.low[node] = search.index
	search.index++
	search.stack = append(search.stack, node)
	search.onStack[node] = true

	for _, next := range search.adjacency[node] {
		if _, visited := search.indices[next]; !visited {
			search.connect(next)
			search.low[node] = min(search.low[node], search.low[next])
		} else if search.onStack[next] {
			search.low[node] = min(search.low[node], search.indices[next])
		}
	}
	if search.low[node] != search.indices[node] {
		return
	}

	var component []string
	for len(search.stack) != 0 {
		last := len(search.stack) - 1
		member := search.stack[last]
		search.stack = search.stack[:last]
		search.onStack[member] = false
		component = append(component, member)
		if member == node {
			break
		}
	}
	sort.Strings(component)
	search.result = append(search.result, component)
}

func representativeCycle(members []string, adjacency map[string][]string) []string {
	inComponent := make(map[string]struct{}, len(members))
	for _, member := range members {
		inComponent[member] = struct{}{}
	}
	start := members[0]
	path := []string{start}
	onPath := map[string]bool{start: true}
	if findCyclePath(start, start, adjacency, inComponent, onPath, &path) {
		return path
	}
	return append(append([]string(nil), members...), start)
}

func focusedDependencyOrder(moduleID string, adjacency map[string][]string) ([]string, error) {
	state := make(map[string]uint8, len(adjacency))
	var order []string
	var visit func(string) error
	visit = func(current string) error {
		switch state[current] {
		case 1:
			return fmt.Errorf("focus module %s belongs to a module dependency cycle", moduleID)
		case 2:
			return nil
		}
		state[current] = 1
		for _, dependency := range adjacency[current] {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		state[current] = 2
		order = append(order, current)
		return nil
	}
	if err := visit(moduleID); err != nil {
		return nil, err
	}
	return order, nil
}

func cycleIncluded(cycle Cycle, included map[string]struct{}) bool {
	for _, member := range cycle.Members {
		if _, ok := included[member]; !ok {
			return false
		}
	}
	return true
}

func findCyclePath(
	start string,
	current string,
	adjacency map[string][]string,
	inComponent map[string]struct{},
	onPath map[string]bool,
	path *[]string,
) bool {
	for _, next := range adjacency[current] {
		if _, included := inComponent[next]; !included {
			continue
		}
		if next == start && len(*path) > 1 {
			*path = append(*path, start)
			return true
		}
		if onPath[next] {
			continue
		}
		onPath[next] = true
		*path = append(*path, next)
		if findCyclePath(start, next, adjacency, inComponent, onPath, path) {
			return true
		}
		*path = (*path)[:len(*path)-1]
		delete(onPath, next)
	}
	return false
}

func sortModel(model *Model) {
	for index := range model.modules {
		module := &model.modules[index]
		sort.SliceStable(module.packages, func(i, j int) bool {
			return module.packages[i].Path < module.packages[j].Path
		})
		sort.SliceStable(module.namedInterfaces, func(i, j int) bool {
			if module.namedInterfaces[i].Name != module.namedInterfaces[j].Name {
				return module.namedInterfaces[i].Name < module.namedInterfaces[j].Name
			}
			return module.namedInterfaces[i].PackagePath < module.namedInterfaces[j].PackagePath
		})
		sortDependencies(module.allowed)
	}
	sort.SliceStable(model.modules, func(i, j int) bool {
		return model.modules[i].ID < model.modules[j].ID
	})
	sort.SliceStable(model.unassigned, func(i, j int) bool {
		return model.unassigned[i].Path < model.unassigned[j].Path
	})
	sort.SliceStable(model.edges, func(i, j int) bool {
		left, right := model.edges[i], model.edges[j]
		if left.FromModule != right.FromModule {
			return left.FromModule < right.FromModule
		}
		if left.ToModule != right.ToModule {
			return left.ToModule < right.ToModule
		}
		if left.FromPackage != right.FromPackage {
			return left.FromPackage < right.FromPackage
		}
		return left.ToPackage < right.ToPackage
	})
	sort.SliceStable(model.cycles, func(i, j int) bool {
		return model.cycles[i].Members[0] < model.cycles[j].Members[0]
	})
	sort.SliceStable(model.diagnostics, func(i, j int) bool {
		left, right := model.diagnostics[i], model.diagnostics[j]
		if left.PhysicalPosition.Filename != right.PhysicalPosition.Filename {
			return left.PhysicalPosition.Filename < right.PhysicalPosition.Filename
		}
		if left.PhysicalPosition.Offset != right.PhysicalPosition.Offset {
			return left.PhysicalPosition.Offset < right.PhysicalPosition.Offset
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		return left.Message < right.Message
	})
}

func rebuildPackageOwners(model *Model) {
	model.packageOwner = make(map[string]int)
	for moduleIndex, module := range model.modules {
		for _, pkg := range module.packages {
			model.packageOwner[pkg.Path] = moduleIndex
		}
	}
}

func sortDependencies(dependencies []Dependency) {
	sort.SliceStable(dependencies, func(i, j int) bool {
		if dependencies[i].ModuleID != dependencies[j].ModuleID {
			return dependencies[i].ModuleID < dependencies[j].ModuleID
		}
		return dependencies[i].Interface < dependencies[j].Interface
	})
}

func cloneModule(module Module) Module {
	result := module
	result.packages = append([]Package(nil), module.packages...)
	result.namedInterfaces = append([]NamedInterface(nil), module.namedInterfaces...)
	result.allowed = append([]Dependency(nil), module.allowed...)
	return result
}

func occurrenceDiagnostic(occurrence resolve.Occurrence, kind, message string) Diagnostic {
	return Diagnostic{
		Position:         occurrence.DisplayPosition,
		PhysicalPosition: physicalPosition(occurrence),
		ModuleID:         occurrence.PackagePath,
		PackagePath:      occurrence.PackagePath,
		Kind:             kind,
		Message:          message,
	}
}

func dependencyDiagnostic(module Module, dependency Dependency, kind, message string) Diagnostic {
	return Diagnostic{
		Position:         dependency.Position,
		PhysicalPosition: dependency.PhysicalPosition,
		ModuleID:         module.ID,
		PackagePath:      module.RootPackage,
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

func renderPosition(position token.Position) string {
	if position.Filename == "" {
		return "<unknown>:1:1"
	}
	line := position.Line
	if line <= 0 {
		line = 1
	}
	column := position.Column
	if column <= 0 {
		column = 1
	}
	return fmt.Sprintf("%s:%d:%d", position.Filename, line, column)
}
