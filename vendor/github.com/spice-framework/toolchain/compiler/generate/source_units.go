package generate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/format"
	"go/token"
	"go/types"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/spice-framework/toolchain/compiler/application"
	"github.com/spice-framework/toolchain/compiler/configuration"
	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/compiler/provider"
)

type providerSourceUnit struct {
	path        string
	packageName string
	origins     []SourceOrigin
	application *sourceUnitApplication
	providers   []provider.Provider
}

type sourceUnitApplication struct {
	target   application.Target
	targetID string
}

type applicationSourceAdapter struct {
	alias      string
	identifier string
}

type providerSourceAdapter struct {
	alias    string
	function string
}

func providerSourceAdapters(
	providers []provider.Provider,
	target Target,
	aliases map[string]string,
) (map[string]providerSourceAdapter, error) {
	providerPaths := make(map[string]string)
	var importPaths []string
	seenImports := make(map[string]struct{})
	for _, item := range providers {
		if !sourceUnitProvidesProvider(item) {
			continue
		}
		unitPath, _, _, err := sourceUnitLocation(
			item,
			target,
		)
		if err != nil {
			return nil, err
		}
		importPath := path.Join(
			target.ModulePath,
			path.Dir(unitPath),
		)
		providerPaths[item.SymbolID] = importPath
		if _, found := seenImports[importPath]; found {
			continue
		}
		seenImports[importPath] = struct{}{}
		importPaths = append(importPaths, importPath)
	}
	sort.Strings(importPaths)
	importAliases := make(map[string]string, len(importPaths))
	for index, importPath := range importPaths {
		preferred := "spice" + exportedGeneratedIdentifier(
			path.Base(importPath),
			"Source"+strconv.Itoa(index),
		)
		importAliases[importPath] = ensureSourceUnitImportAlias(
			aliases,
			importPath,
			preferred,
		)
	}
	result := make(map[string]providerSourceAdapter, len(providerPaths))
	for _, item := range providers {
		importPath := providerPaths[item.SymbolID]
		if importPath == "" {
			continue
		}
		result[item.SymbolID] = providerSourceAdapter{
			alias:    importAliases[importPath],
			function: generatedProviderFunction(item),
		}
	}
	return result, nil
}

func buildApplicationSourceAdapter(
	applicationTarget application.Target,
	target Target,
	aliases map[string]string,
) (applicationSourceAdapter, error) {
	unitPath, _, _, err := applicationSourceUnitLocation(
		applicationTarget,
		target,
	)
	if err != nil {
		return applicationSourceAdapter{}, err
	}
	importPath := path.Join(target.ModulePath, path.Dir(unitPath))
	return applicationSourceAdapter{
		alias: ensureSourceUnitImportAlias(
			aliases,
			importPath,
			"spiceentrypoint",
		),
		identifier: generatedApplicationTargetName(applicationTarget),
	}, nil
}

func renderSourceUnits(
	program *load.Program,
	applicationTarget application.Target,
	providers []provider.Provider,
	configTypes []configuration.Type,
	target Target,
) ([]File, error) {
	configByProvider := configurationProviderIndex(configTypes)
	units := make(map[string]providerSourceUnit)
	applicationPath, applicationPackage, applicationOrigin, err := applicationSourceUnitLocation(applicationTarget, target)
	if err != nil {
		return nil, err
	}
	units[applicationPath] = providerSourceUnit{
		path:        applicationPath,
		packageName: applicationPackage,
		origins:     []SourceOrigin{applicationOrigin},
		application: &sourceUnitApplication{
			target:   applicationTarget,
			targetID: target.ID,
		},
	}
	for _, item := range providers {
		if !sourceUnitProvider(item) {
			continue
		}
		shardPath, packageName, origin, err := sourceUnitLocation(
			item,
			target,
		)
		if err != nil {
			return nil, err
		}
		unit, found := units[shardPath]
		if !found {
			unit = providerSourceUnit{
				path:        shardPath,
				packageName: packageName,
			}
			units[shardPath] = unit
		} else if unit.packageName != packageName {
			return nil, fmt.Errorf(
				"generated provider source unit %s combines packages %s and %s",
				shardPath,
				unit.packageName,
				packageName,
			)
		}
		unit.providers = append(unit.providers, item)
		if !slices.Contains(unit.origins, origin) {
			unit.origins = append(unit.origins, origin)
		}
		units[shardPath] = unit
	}
	paths := make([]string, 0, len(units))
	for shardPath := range units {
		paths = append(paths, shardPath)
	}
	sort.Strings(paths)
	files := make([]File, 0, len(paths))
	for _, shardPath := range paths {
		unit := units[shardPath]
		sort.SliceStable(unit.providers, func(i, j int) bool {
			return unit.providers[i].SymbolID <
				unit.providers[j].SymbolID
		})
		sort.SliceStable(unit.origins, func(i, j int) bool {
			if unit.origins[i].Path != unit.origins[j].Path {
				return unit.origins[i].Path < unit.origins[j].Path
			}
			if unit.origins[i].Line != unit.origins[j].Line {
				return unit.origins[i].Line < unit.origins[j].Line
			}
			return unit.origins[i].Symbol < unit.origins[j].Symbol
		})
		content, mappings, err := renderProviderSourceUnit(
			program,
			unit,
			configByProvider,
		)
		if err != nil {
			return nil, err
		}
		files = append(files, File{
			Path:          unit.path,
			Mode:          0o644,
			SHA256:        contentHash(content),
			Role:          FileRoleSourceUnit,
			PrimarySource: firstSourceOrigin(unit.origins),
			Mappings:      mappings,
			Sources:       append([]SourceOrigin(nil), unit.origins...),
			content:       content,
		})
	}
	return files, nil
}

func sourceUnitProvider(item provider.Provider) bool {
	switch item.Source {
	case provider.SourceBean,
		provider.SourceStarter,
		provider.SourceAutoConfiguration,
		provider.SourceStereotype,
		provider.SourceConfiguration:
		return true
	case provider.SourceEvent:
		return len(item.Interfaces) != 0
	}
	return len(item.Interfaces) != 0
}

func sourceUnitLocation(
	item provider.Provider,
	target Target,
) (string, string, SourceOrigin, error) {
	sourceFile := item.PhysicalPosition.Filename
	if sourceFile == "" {
		sourceFile = item.Position.Filename
	}
	return sourceUnitLocationAt(
		sourceFile,
		item.PackagePath,
		item.SymbolID,
		item.PhysicalPosition,
		item.Position,
		target,
	)
}

func applicationSourceUnitLocation(
	item application.Target,
	target Target,
) (string, string, SourceOrigin, error) {
	sourceFile := item.PhysicalPosition.Filename
	if sourceFile == "" {
		sourceFile = item.Position.Filename
	}
	return sourceUnitLocationAt(
		sourceFile,
		item.PackagePath,
		item.SymbolID,
		item.PhysicalPosition,
		item.Position,
		target,
	)
}

func sourceUnitLocationAt(
	sourceFile string,
	packagePath string,
	symbolID string,
	physicalPosition token.Position,
	position token.Position,
	target Target,
) (string, string, SourceOrigin, error) {
	if sourceFile == "" {
		return "", "", SourceOrigin{}, fmt.Errorf(
			"contribution %s has no physical source file",
			symbolID,
		)
	}
	relative, err := filepath.Rel(target.ModuleRoot, sourceFile)
	moduleRelative := err == nil && filepath.IsLocal(relative)
	packageName := "spicegen"
	var sourcePath string
	var shardDir string
	if moduleRelative {
		sourcePath = filepath.ToSlash(relative)
		sourceDirectory := path.Dir(sourcePath)
		if sourceDirectory == "." {
			sourceDirectory = "_root"
		}
		shardDir = path.Join(
			target.OutputDir,
			"sources",
			generatedSourceDirectory(sourceDirectory),
		)
	} else {
		digest := sha256.Sum256([]byte(packagePath))
		shardDir = path.Join(
			target.OutputDir,
			"sources",
			"_external",
			hex.EncodeToString(digest[:6]),
		)
		sourcePath = packagePath + "/" + filepath.Base(sourceFile)
	}
	base := strings.TrimSuffix(
		filepath.Base(sourceFile),
		filepath.Ext(sourceFile),
	)
	if base == "" {
		return "", "", SourceOrigin{}, fmt.Errorf(
			"contribution %s source file has no base name",
			symbolID,
		)
	}
	origin := SourceOrigin{
		Path:   sourcePath,
		Line:   physicalPosition.Line,
		Column: physicalPosition.Column,
		Symbol: symbolID,
	}
	if origin.Line == 0 {
		origin.Line = position.Line
	}
	if origin.Column == 0 {
		origin.Column = position.Column
	}
	return path.Join(
		shardDir,
		base+"_spice_gen.go",
	), packageName, origin, nil
}

// generatedSourceDirectory preserves the source tree while escaping directory
// names that have special import semantics in Go. In particular, placing an
// adapter below sources/internal would prevent the generated target package
// from importing it because Go applies the internal-package rule at that
// nested boundary. The Source metadata remains the exact original path.
func generatedSourceDirectory(sourceDirectory string) string {
	segments := strings.Split(sourceDirectory, "/")
	for index, segment := range segments {
		switch segment {
		case "internal", "vendor":
			segments[index] = segment + "_"
		}
	}
	return path.Join(segments...)
}

func renderProviderSourceUnit(
	program *load.Program,
	unit providerSourceUnit,
	configByProvider map[string]configuration.Type,
) ([]byte, []SourceMapping, error) {
	aliases, lifecycleAlias, fmtAlias, configAlias := sourceUnitImportAliases(
		program,
		unit,
		configByProvider,
	)
	var source bytes.Buffer
	source.WriteString("//go:build !" + AnalysisBuildTag + "\n\n")
	source.WriteString("// Code generated by Spice. DO NOT EDIT.\n")
	for _, origin := range unit.origins {
		fmt.Fprintf(
			&source,
			"// Source: %s:%d\n",
			origin.Path,
			origin.Line,
		)
	}
	source.WriteString("\n")
	fmt.Fprintf(&source, "package %s\n\n", unit.packageName)
	writeImports(&source, aliases)
	if unit.application != nil {
		writeSourceUnitApplication(&source, *unit.application)
	}
	if err := writeProviderSourceUnitDeclarations(
		&source,
		unit,
		configByProvider,
		aliases,
		lifecycleAlias,
		fmtAlias,
		configAlias,
	); err != nil {
		return nil, nil, err
	}
	formatted, err := format.Source(source.Bytes())
	if err != nil {
		return nil, nil, fmt.Errorf(
			"format provider source unit %s: %w",
			unit.path,
			err,
		)
	}
	mappings, err := providerSourceUnitMappings(unit, formatted, aliases)
	if err != nil {
		return nil, nil, err
	}
	return formatted, mappings, nil
}

func sourceUnitImportAliases(
	program *load.Program,
	unit providerSourceUnit,
	configByProvider map[string]configuration.Type,
) (map[string]string, string, string, string) {
	aliases := aliasesForTypes(
		sourceUnitTypes(unit, configByProvider),
		unit.packageName,
	)
	lifecycleAlias := ""
	if slices.ContainsFunc(
		unit.providers,
		sourceUnitConstructsProvider,
	) {
		lifecycleAlias = ensureSourceUnitImportAlias(
			aliases,
			lifecyclePath,
			"spicelifecycle",
		)
	}
	fmtAlias := ""
	if slices.ContainsFunc(
		unit.providers,
		func(item provider.Provider) bool {
			return sourceUnitNeedsFormatting(item, configByProvider)
		},
	) {
		fmtAlias = ensureSourceUnitImportAlias(aliases, "fmt", "fmt")
	}
	configAlias := ""
	if slices.ContainsFunc(
		unit.providers,
		func(item provider.Provider) bool {
			return item.Source == provider.SourceConfiguration
		},
	) {
		configAlias = ensureSourceUnitImportAlias(
			aliases,
			configPath,
			"spiceconfig",
		)
	}
	addSourceUnitConstructorAliases(program, unit.providers, aliases)
	return aliases, lifecycleAlias, fmtAlias, configAlias
}

func sourceUnitTypes(
	unit providerSourceUnit,
	configByProvider map[string]configuration.Type,
) []types.Type {
	var values []types.Type
	for _, item := range unit.providers {
		values = append(values, item.Output)
		for _, dependency := range item.Dependencies {
			values = append(values, dependency.Type)
		}
		for _, binding := range item.Interfaces {
			values = append(values, binding.Type)
		}
		if configType, found := configByProvider[item.SymbolID]; found {
			for _, field := range configType.Fields() {
				values = append(values, field.Type)
			}
		}
	}
	return values
}

func sourceUnitNeedsFormatting(
	item provider.Provider,
	configByProvider map[string]configuration.Type,
) bool {
	if sourceUnitConstructsProvider(item) && item.ReturnsError {
		return true
	}
	configType, found := configByProvider[item.SymbolID]
	return item.Source == provider.SourceConfiguration &&
		found && len(configType.Fields()) != 0
}

func addSourceUnitConstructorAliases(
	program *load.Program,
	providers []provider.Provider,
	aliases map[string]string,
) {
	for _, item := range providers {
		constructor := providerConstructor(item)
		if constructor.PackagePath == "" {
			continue
		}
		packageName := path.Base(constructor.PackagePath)
		if pkg, found := packageByPath(
			program.Packages(),
			constructor.PackagePath,
		); found {
			packageName = pkg.Name
		}
		ensureSourceUnitImportAlias(
			aliases,
			constructor.PackagePath,
			packageName,
		)
	}
}

func writeProviderSourceUnitDeclarations(
	source *bytes.Buffer,
	unit providerSourceUnit,
	configByProvider map[string]configuration.Type,
	aliases map[string]string,
	lifecycleAlias string,
	fmtAlias string,
	configAlias string,
) error {
	for _, item := range unit.providers {
		switch {
		case sourceUnitConstructsProvider(item):
			writeSourceUnitProvider(
				source,
				item,
				aliases,
				lifecycleAlias,
				fmtAlias,
			)
		case item.Source == provider.SourceConfiguration:
			configType, found := configByProvider[item.SymbolID]
			if !found {
				return fmt.Errorf(
					"configuration provider %s has no typed configuration metadata",
					item.SymbolID,
				)
			}
			writeSourceUnitConfigurationBinder(
				source,
				configType,
				aliases,
				configAlias,
				fmtAlias,
			)
		}
		writeSourceUnitInterfaceAssertions(source, item, aliases)
	}
	return nil
}

func writeSourceUnitApplication(
	source *bytes.Buffer,
	item sourceUnitApplication,
) {
	name := generatedApplicationTargetName(item.target)
	fmt.Fprintf(
		source,
		"// %s identifies the generated target selected by %s.\n",
		name,
		item.target.SymbolID,
	)
	fmt.Fprintf(
		source,
		"const %s = %s\n\n",
		name,
		strconv.Quote(item.targetID),
	)
}

func generatedApplicationTargetName(item application.Target) string {
	digest := sha256.Sum256([]byte(item.SymbolID))
	name := exportedGeneratedIdentifier(item.Name, "Application")
	return "ApplicationTarget" + name + "_" + hex.EncodeToString(digest[:4])
}

func writeSourceUnitInterfaceAssertions(
	source *bytes.Buffer,
	item provider.Provider,
	aliases map[string]string,
) {
	for _, binding := range item.Interfaces {
		marker := generatedAssertionName(item, binding)
		fmt.Fprintf(
			source,
			"// %s identifies the explicit @Implements binding for %s.\n",
			marker,
			item.SymbolID,
		)
		fmt.Fprintf(
			source,
			"var _ %s = %s\n\n",
			renderedTypeInPackage(binding.Type, aliases),
			renderedInterfaceAssertionValue(item.Output, aliases),
		)
	}
}

func renderedInterfaceAssertionValue(
	output types.Type,
	aliases map[string]string,
) string {
	rendered := renderedTypeInPackage(output, aliases)
	if _, pointer := types.Unalias(output).(*types.Pointer); pointer {
		return "(" + rendered + ")(nil)"
	}
	if _, structure := types.Unalias(output).Underlying().(*types.Struct); structure {
		return rendered + "{}"
	}
	return "*new(" + rendered + ")"
}

func providerSourceUnitMappings(
	unit providerSourceUnit,
	formatted []byte,
	aliases map[string]string,
) ([]SourceMapping, error) {
	mappings := make([]SourceMapping, 0)
	if unit.application != nil {
		origin, found := sourceOriginForSymbol(
			unit.origins,
			unit.application.target.SymbolID,
		)
		if !found {
			return nil, fmt.Errorf(
				"application source unit %s has no source origin",
				unit.application.target.SymbolID,
			)
		}
		name := generatedApplicationTargetName(unit.application.target)
		line, column, found := generatedIdentifierPosition(formatted, name)
		if !found {
			return nil, fmt.Errorf(
				"application target %s has no generated position",
				name,
			)
		}
		mappings = append(mappings, SourceMapping{
			Kind:         "application-target",
			Contribution: unit.application.target.SymbolID,
			Source:       origin,
			Generated:    generatedIdentifierRange(line, column, name),
		})
	}
	for _, item := range unit.providers {
		origin, found := sourceOriginForSymbol(unit.origins, item.SymbolID)
		if !found {
			return nil, fmt.Errorf(
				"provider source unit %s has no source origin",
				item.SymbolID,
			)
		}
		if sourceUnitProvidesProvider(item) {
			mapping, err := providerSourceUnitMapping(
				item,
				origin,
				formatted,
			)
			if err != nil {
				return nil, err
			}
			mappings = append(mappings, mapping)
		}
		for _, binding := range item.Interfaces {
			mapping, err := interfaceSourceUnitMapping(
				item,
				binding,
				origin,
				formatted,
				aliases,
			)
			if err != nil {
				return nil, err
			}
			mappings = append(mappings, mapping)
		}
	}
	return mappings, nil
}

func providerSourceUnitMapping(
	item provider.Provider,
	origin SourceOrigin,
	formatted []byte,
) (SourceMapping, error) {
	name := generatedProviderFunction(item)
	line, column, found := generatedIdentifierPosition(formatted, name)
	if !found {
		return SourceMapping{}, fmt.Errorf(
			"provider constructor adapter %s has no generated position",
			name,
		)
	}
	return SourceMapping{
		Kind:         sourceUnitProviderMappingKind(item),
		Contribution: item.SymbolID,
		Source:       origin,
		Generated:    generatedIdentifierRange(line, column, name),
	}, nil
}

func interfaceSourceUnitMapping(
	item provider.Provider,
	binding provider.InterfaceBinding,
	origin SourceOrigin,
	formatted []byte,
	aliases map[string]string,
) (SourceMapping, error) {
	marker := generatedAssertionName(item, binding)
	contract := renderedTypeInPackage(binding.Type, aliases)
	line, column, found := generatedInterfaceAssertionPosition(
		formatted,
		marker,
		contract,
	)
	if !found {
		return SourceMapping{}, fmt.Errorf(
			"interface assertion %s has no generated position",
			marker,
		)
	}
	return SourceMapping{
		Kind:         "interface-assertion",
		Contribution: item.SymbolID + "#implements:" + binding.TypeID,
		Source:       origin,
		Generated:    generatedIdentifierRange(line, column, contract),
	}, nil
}

func generatedInterfaceAssertionPosition(
	content []byte,
	marker string,
	contract string,
) (int, int, bool) {
	markerOffset := bytes.Index(content, []byte(marker))
	if markerOffset < 0 {
		return 0, 0, false
	}
	prefix := []byte("var _ " + contract + " =")
	statementOffset := bytes.Index(content[markerOffset:], prefix)
	if statementOffset < 0 {
		return 0, 0, false
	}
	offset := markerOffset + statementOffset + len("var _ ")
	line := bytes.Count(content[:offset], []byte{'\n'}) + 1
	lineStart := bytes.LastIndexByte(content[:offset], '\n') + 1
	return line, offset - lineStart + 1, true
}

func generatedIdentifierRange(
	line int,
	column int,
	name string,
) GeneratedRange {
	return GeneratedRange{
		StartLine:   line,
		StartColumn: column,
		EndLine:     line,
		EndColumn:   column + len(name),
	}
}

func sourceUnitConstructsProvider(item provider.Provider) bool {
	switch item.Source {
	case provider.SourceBean,
		provider.SourceStarter,
		provider.SourceAutoConfiguration,
		provider.SourceStereotype:
		return true
	case provider.SourceConfiguration, provider.SourceEvent:
		return false
	}
	return false
}

func sourceUnitProvidesProvider(item provider.Provider) bool {
	return sourceUnitConstructsProvider(item) ||
		item.Source == provider.SourceConfiguration
}

func sourceUnitProviderMappingKind(item provider.Provider) string {
	if item.Source == provider.SourceConfiguration {
		return "configuration-binding"
	}
	return "provider-construction"
}

func ensureSourceUnitImportAlias(
	aliases map[string]string,
	importPath string,
	preferred string,
) string {
	if alias := aliases[importPath]; alias != "" {
		return alias
	}
	used := make(map[string]struct{}, len(aliases))
	for _, alias := range aliases {
		used[alias] = struct{}{}
	}
	alias := preferred
	for suffix := 2; ; suffix++ {
		if _, exists := used[alias]; !exists {
			break
		}
		alias = preferred + strconv.Itoa(suffix)
	}
	aliases[importPath] = alias
	return alias
}

func providerConstructor(item provider.Provider) load.Symbol {
	constructor := item.Constructor
	if constructor.PackagePath != "" && constructor.Name != "" {
		return constructor
	}
	return load.Symbol{
		PackagePath: item.PackagePath,
		Name:        item.Symbol.Name,
	}
}

func generatedProviderFunction(item provider.Provider) string {
	digest := sha256.Sum256([]byte(item.SymbolID))
	prefix := "Construct"
	if item.Source == provider.SourceConfiguration {
		prefix = "Bind"
	}
	name := exportedGeneratedIdentifier(
		semanticProviderName(item),
		"Provider",
	)
	return prefix + name + "_" + hex.EncodeToString(digest[:4])
}

func writeSourceUnitProvider(
	source *bytes.Buffer,
	item provider.Provider,
	aliases map[string]string,
	lifecycleAlias string,
	fmtAlias string,
) {
	functionName := generatedProviderFunction(item)
	fmt.Fprintf(
		source,
		"// %s performs the direct construction selected for bean %q.\n",
		functionName,
		item.Name,
	)
	fmt.Fprintf(source, "// Spice source identity: %s.\n", item.SymbolID)
	fmt.Fprintf(source, "func %s(", functionName)
	for index, dependency := range item.Dependencies {
		if index != 0 {
			source.WriteString(", ")
		}
		fmt.Fprintf(
			source,
			"dependency%d %s",
			index,
			renderedTypeInPackage(
				dependency.Type,
				aliases,
			),
		)
	}
	fmt.Fprintf(
		source,
		") (%s, %s.Cleanup, error) {\n",
		renderedTypeInPackage(item.Output, aliases),
		lifecycleAlias,
	)
	if item.Construction == provider.ConstructionAllocate {
		fmt.Fprintf(
			source,
			"\treturn new(%s.%s), nil, nil\n",
			aliases[item.PackagePath],
			item.Symbol.Name,
		)
		source.WriteString("}\n\n")
		return
	}
	constructor := providerConstructor(item)
	arguments := make([]string, len(item.Dependencies))
	for index := range arguments {
		arguments[index] = "dependency" + strconv.Itoa(index)
	}
	call := aliases[constructor.PackagePath] + "." +
		constructor.Name + "(" + strings.Join(arguments, ", ") + ")"
	if constructor.Kind == load.SymbolMethod {
		call = arguments[0] + "." + constructor.Name +
			"(" + strings.Join(arguments[1:], ", ") + ")"
	}
	switch {
	case item.ReturnsCleanup && item.ReturnsError:
		fmt.Fprintf(source, "\tvalue, cleanup, err := %s\n", call)
	case item.ReturnsCleanup:
		fmt.Fprintf(source, "\tvalue, cleanup := %s\n", call)
	case item.ReturnsError:
		fmt.Fprintf(source, "\tvalue, err := %s\n", call)
	default:
		fmt.Fprintf(source, "\tvalue := %s\n", call)
	}
	if item.ReturnsError {
		source.WriteString("\tif err != nil {\n")
		fmt.Fprintf(
			source,
			"\t\tvar zero %s\n",
			renderedTypeInPackage(item.Output, aliases),
		)
		fmt.Fprintf(
			source,
			"\t\treturn zero, nil, %s.Errorf(%s, err)\n",
			fmtAlias,
			strconv.Quote(
				"construct bean "+item.Name+
					" ("+item.OutputTypeID+
					", source "+item.SymbolID+"): %w",
			),
		)
		source.WriteString("\t}\n")
	}
	cleanup := "nil"
	if item.ReturnsCleanup {
		cleanup = "cleanup"
	}
	fmt.Fprintf(source, "\treturn value, %s, nil\n", cleanup)
	source.WriteString("}\n\n")
}

func generatedAssertionName(
	item provider.Provider,
	binding provider.InterfaceBinding,
) string {
	digest := sha256.Sum256(
		[]byte(item.SymbolID + "\x00" + binding.TypeID),
	)
	concrete := exportedGeneratedIdentifier(
		semanticProviderName(item),
		"Provider",
	)
	contract := exportedGeneratedIdentifier(
		generatedTypeName(binding.Type),
		"Interface",
	)
	return "spiceImplements" + concrete + "As" + contract +
		"_" + hex.EncodeToString(digest[:4])
}

func aliasesForTypes(
	values []types.Type,
	localPackageName string,
) map[string]string {
	names := make(map[string]string)
	aliases := make(map[string]string)
	for _, value := range values {
		addTypeImportName(names, aliases, value)
	}
	paths := make([]string, 0, len(names))
	for importPath := range names {
		paths = append(paths, importPath)
	}
	sort.Strings(paths)
	used := map[string]struct{}{
		"spicegen":       {},
		localPackageName: {},
	}
	for _, importPath := range paths {
		base := names[importPath]
		alias := base
		for suffix := 2; ; suffix++ {
			if _, exists := used[alias]; !exists {
				break
			}
			alias = base + strconv.Itoa(suffix)
		}
		used[alias] = struct{}{}
		aliases[importPath] = alias
	}
	return aliases
}

func renderedTypeInPackage(
	value types.Type,
	aliases map[string]string,
) string {
	return types.TypeString(value, func(pkg *types.Package) string {
		if pkg == nil {
			return ""
		}
		if alias, ok := aliases[pkg.Path()]; ok {
			return alias
		}
		return pkg.Name()
	})
}
