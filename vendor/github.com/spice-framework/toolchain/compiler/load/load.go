package load

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/spice-framework/spice/annotation"
	annotationparser "github.com/spice-framework/toolchain/compiler/parser"
)

// Options configures one isolated package-loading operation.
type Options struct {
	Dir               string
	Env               []string
	BuildFlags        []string
	Overlay           map[string][]byte
	AuxiliaryPackages []string
	// PrepareGeneratedApplicationEntrypoints supplies a bounded in-memory
	// generated-package stub while the spice_generate build tag excludes stale
	// committed output. The physical application source remains unchanged.
	PrepareGeneratedApplicationEntrypoints bool
	// Tests is reserved for a future test-package model. The bootstrap loader
	// rejects true because go/packages test variants can duplicate logical
	// package and symbol identities.
	Tests bool
}

// Load asks the standard Go package driver to load the requested root package
// patterns once, then builds deterministic package, symbol, and diagnostic
// records from that shared type universe.
func Load(ctx context.Context, options Options, patterns ...string) (*Program, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if diagnostics := requestDiagnostics(options, patterns); len(diagnostics) != 0 {
		program := &Program{diagnostics: diagnostics}
		return program, &LoadError{Diagnostics: program.Diagnostics()}
	}

	overlay := cloneOverlay(options.Overlay)
	if options.PrepareGeneratedApplicationEntrypoints {
		var preparationDiagnostics []Diagnostic
		var preparationErr error
		overlay, preparationDiagnostics, preparationErr = addGeneratedApplicationEntrypointOverlays(options.Dir, overlay)
		if preparationErr != nil {
			program := &Program{diagnostics: []Diagnostic{{
				Kind:    "generated-entrypoint",
				Message: preparationErr.Error(),
			}}}
			return program, &LoadError{Diagnostics: program.Diagnostics()}
		}
		if len(preparationDiagnostics) != 0 {
			program := &Program{diagnostics: preparationDiagnostics}
			return program, &LoadError{Diagnostics: program.Diagnostics()}
		}
	}
	candidates, requestedPackages := discoverCompositionCandidates(
		options,
		patterns,
		overlay,
	)
	config := &packages.Config{
		Context:    ctx,
		Mode:       packages.LoadSyntax | packages.NeedModule,
		Dir:        options.Dir,
		Env:        append([]string(nil), options.Env...),
		BuildFlags: append([]string(nil), options.BuildFlags...),
		Overlay:    overlay,
		Tests:      false,
	}

	auxiliary := normalizedAuxiliaryPackages(options.AuxiliaryPackages)
	loadPatterns := append(append([]string(nil), patterns...), auxiliary...)
	loadPatterns = append(loadPatterns, compositionCandidatePaths(candidates)...)
	roots, loadErr := packages.Load(config, loadPatterns...)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	program := programFromRoots(
		selectProgramRoots(
			roots,
			requestedPackages,
			auxiliary,
			candidates,
		),
		loadErr,
		auxiliary,
	)
	if len(program.diagnostics) > 0 || loadErr != nil {
		return program, &LoadError{Diagnostics: program.Diagnostics()}
	}
	return program, nil
}

func requestDiagnostics(options Options, patterns []string) []Diagnostic {
	if len(patterns) == 0 {
		return []Diagnostic{{Kind: "list", Message: "no package patterns were provided"}}
	}
	if options.Tests {
		return []Diagnostic{{
			Kind:    "configuration",
			Message: "test-variant loading is unsupported; load application packages with Tests disabled",
		}}
	}
	for _, packagePath := range options.AuxiliaryPackages {
		if !validAuxiliaryPackagePath(packagePath) {
			return []Diagnostic{{
				Kind: "configuration",
				Message: fmt.Sprintf(
					"auxiliary package %q must be one exact trimmed Go import path",
					packagePath,
				),
			}}
		}
	}
	return nil
}

func validAuxiliaryPackagePath(packagePath string) bool {
	return packagePath != "" &&
		strings.TrimSpace(packagePath) == packagePath &&
		!strings.HasPrefix(packagePath, ".") &&
		!strings.HasPrefix(packagePath, "/") &&
		!strings.Contains(packagePath, "...") &&
		!strings.ContainsAny(packagePath, "\\ \t\r\n")
}

func normalizedAuxiliaryPackages(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return slicesCompact(result)
}

func slicesCompact(values []string) []string {
	if len(values) == 0 {
		return values
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}

func programFromRoots(
	roots []*packages.Package,
	loadErr error,
	auxiliaryPackages []string,
) *Program {
	program := &Program{}
	if loadErr != nil {
		program.diagnostics = append(program.diagnostics, Diagnostic{
			Kind:    "driver",
			Message: loadErr.Error(),
		})
	}

	auxiliary := make(map[string]struct{}, len(auxiliaryPackages))
	for _, packagePath := range auxiliaryPackages {
		auxiliary[packagePath] = struct{}{}
	}
	for _, root := range roots {
		if root == nil {
			continue
		}
		appendLoadedRoot(
			program,
			root,
			auxiliary,
		)
	}

	sort.SliceStable(program.packages, func(i, j int) bool {
		if program.packages[i].Path != program.packages[j].Path {
			return program.packages[i].Path < program.packages[j].Path
		}
		return program.packages[i].ID < program.packages[j].ID
	})
	sort.SliceStable(program.symbols, func(i, j int) bool {
		left, right := program.symbols[i], program.symbols[j]
		if left.PackagePath != right.PackagePath {
			return left.PackagePath < right.PackagePath
		}
		if leftRank, rightRank := symbolKindRank(left.Kind), symbolKindRank(right.Kind); leftRank != rightRank {
			return leftRank < rightRank
		}
		if left.Receiver != right.Receiver {
			return left.Receiver < right.Receiver
		}
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		if left.PhysicalPosition.Filename != right.PhysicalPosition.Filename {
			return left.PhysicalPosition.Filename < right.PhysicalPosition.Filename
		}
		return left.PhysicalPosition.Offset < right.PhysicalPosition.Offset
	})
	sortDiagnostics(program.diagnostics)
	return program
}

func packageIdentity(pkg *packages.Package) string {
	if pkg.ID != "" {
		return pkg.ID
	}
	return pkg.PkgPath
}

func appendLoadedRoot(
	program *Program,
	root *packages.Package,
	auxiliary map[string]struct{},
) {
	packageErrors := root.Errors
	illTyped := root.IllTyped || len(packageErrors) != 0
	record := packageRecord(root, illTyped)
	_, record.Auxiliary = auxiliary[record.Path]
	program.packages = append(program.packages, record)
	symbols := packageSymbols(root)
	program.symbols = append(program.symbols, symbols...)
	program.diagnostics = append(
		program.diagnostics,
		packageDiagnostics(packageErrors, root.PkgPath)...,
	)
}

func cloneOverlay(overlay map[string][]byte) map[string][]byte {
	if len(overlay) == 0 {
		return nil
	}
	result := make(map[string][]byte, len(overlay))
	for path, content := range overlay {
		result[path] = append([]byte(nil), content...)
	}
	return result
}

func cleanSortedFiles(files []string) []string {
	result := append([]string(nil), files...)
	for i := range result {
		result[i] = filepath.Clean(result[i])
	}
	sort.Strings(result)
	return result
}

func fileSet(files []string) map[string]struct{} {
	result := make(map[string]struct{}, len(files))
	for _, file := range files {
		result[filepath.Clean(file)] = struct{}{}
	}
	return result
}

func sourcePackageName(root *packages.Package, sourceFiles map[string]struct{}) *ast.Ident {
	var selected *ast.Ident
	var selectedPosition token.Position
	for _, file := range root.Syntax {
		if file == nil || file.Name == nil || !sourcePosition(root, file.Name.Pos(), sourceFiles) {
			continue
		}
		position := root.Fset.PositionFor(file.Name.Pos(), true)
		if selected == nil || position.Filename < selectedPosition.Filename ||
			(position.Filename == selectedPosition.Filename && position.Offset < selectedPosition.Offset) {
			selected = file.Name
			selectedPosition = position
		}
	}
	return selected
}

func sourceObject(root *packages.Package, object types.Object, sourceFiles map[string]struct{}) bool {
	return object != nil && sourcePosition(root, object.Pos(), sourceFiles)
}

func sourcePosition(root *packages.Package, position token.Pos, sourceFiles map[string]struct{}) bool {
	if !position.IsValid() || root.Fset == nil {
		return false
	}
	physical := filepath.Clean(root.Fset.PositionFor(position, false).Filename)
	if _, ok := sourceFiles[physical]; ok {
		return true
	}
	display := filepath.Clean(root.Fset.PositionFor(position, true).Filename)
	_, ok := sourceFiles[display]
	return ok
}

func packageRecord(root *packages.Package, illTyped bool) Package {
	files := sourceFileRecords(root)
	compiledGoFiles := make([]string, len(files))
	syntax := make([]*ast.File, len(files))
	for i, file := range files {
		compiledGoFiles[i] = file.PhysicalPath
		syntax[i] = file.Syntax
	}

	dir := ""
	if root.Dir != "" {
		dir = filepath.Clean(root.Dir)
	} else if sourceFiles := cleanSortedFiles(root.GoFiles); len(sourceFiles) > 0 {
		dir = filepath.Dir(sourceFiles[0])
	}
	modulePath := ""
	if root.Module != nil {
		modulePath = root.Module.Path
	}

	return Package{
		ID:              stableSymbolID(SymbolPackage, root.PkgPath, "", ""),
		Path:            root.PkgPath,
		Name:            root.Name,
		Dir:             dir,
		ModulePath:      modulePath,
		Files:           files,
		CompiledGoFiles: compiledGoFiles,
		IllTyped:        illTyped,
		Types:           root.Types,
		TypesInfo:       root.TypesInfo,
		Syntax:          syntax,
		Raw:             root,
	}
}

func sourceFileRecords(root *packages.Package) []SourceFile {
	syntaxByPath := make(map[string][]*ast.File, len(root.Syntax))
	for _, file := range root.Syntax {
		if file == nil || root.Fset == nil {
			continue
		}
		path := filepath.Clean(root.Fset.PositionFor(file.Pos(), false).Filename)
		syntaxByPath[path] = append(syntaxByPath[path], file)
	}

	compiled := cleanSortedFiles(root.CompiledGoFiles)
	result := make([]SourceFile, 0, len(compiled)+len(syntaxByPath))
	for _, path := range compiled {
		var syntax *ast.File
		if queue := syntaxByPath[path]; len(queue) > 0 {
			syntax = queue[0]
			if len(queue) == 1 {
				delete(syntaxByPath, path)
			} else {
				syntaxByPath[path] = queue[1:]
			}
		}
		result = append(result, SourceFile{PhysicalPath: path, Syntax: syntax})
	}

	leftovers := make([]string, 0, len(syntaxByPath))
	for path := range syntaxByPath {
		leftovers = append(leftovers, path)
	}
	sort.Strings(leftovers)
	for _, path := range leftovers {
		for _, syntax := range syntaxByPath[path] {
			result = append(result, SourceFile{PhysicalPath: path, Syntax: syntax})
		}
	}
	return result
}

func packageDiagnostics(
	packageErrors []packages.Error,
	packagePath string,
) []Diagnostic {
	diagnostics := make([]Diagnostic, 0, len(packageErrors))
	for _, packageError := range packageErrors {
		position, filename, line, column := normalizeDiagnosticPosition(packageError.Pos)
		diagnostics = append(diagnostics, Diagnostic{
			PackagePath: packagePath,
			Position:    position,
			Filename:    filename,
			Line:        line,
			Column:      column,
			Kind:        errorKindName(packageError.Kind),
			Message:     packageError.Msg,
		})
	}
	return diagnostics
}

func annotatedMainFunctions(file *ast.File) []*ast.FuncDecl {
	if file == nil || file.Name == nil || file.Name.Name != "main" {
		return nil
	}
	var result []*ast.FuncDecl
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok ||
			function.Recv != nil ||
			function.Name == nil ||
			function.Name.Name != "main" ||
			function.Body == nil ||
			!hasApplicationMarker(file, function.Doc) {
			continue
		}
		result = append(result, function)
	}
	return result
}

func hasApplicationMarker(
	file *ast.File,
	comments *ast.CommentGroup,
) bool {
	if comments == nil {
		return false
	}
	names := applicationMarkerSpellings(file)
	for _, comment := range comments.List {
		value := strings.TrimSpace(comment.Text)
		value = strings.TrimSpace(strings.TrimPrefix(value, "//"))
		if annotationName, annotationComment := strings.CutPrefix(value, "@"); annotationComment {
			_, found := names[annotationName]
			if found {
				return true
			}
		}
	}
	return false
}

func applicationMarkerSpellings(file *ast.File) map[string]struct{} {
	result := map[string]struct{}{"Application": {}}
	if file == nil {
		return result
	}
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if comment == nil {
				continue
			}
			directive, recognized, err := annotationparser.ParseImportComment(
				comment.Text,
				token.Position{},
			)
			if !recognized || err != nil {
				continue
			}
			switch directive.Kind {
			case annotation.ImportNamed:
				for _, binding := range directive.Bindings {
					if binding.Imported == "Application" {
						result[binding.Local] = struct{}{}
					}
				}
			case annotation.ImportNamespace:
				result[directive.Namespace+".Application"] = struct{}{}
			}
		}
	}
	return result
}

func normalizeDiagnosticPosition(raw string) (position, filename string, line, column int) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", 0, 0
	}

	prefix, trailing, ok := splitTrailingNumber(raw)
	if !ok {
		filename = filepath.Clean(raw)
		return filename, filename, 0, 0
	}
	prefix2, preceding, hasPreceding := splitTrailingNumber(prefix)
	if hasPreceding {
		filename = filepath.Clean(prefix2)
		line, column = preceding, trailing
	} else {
		filename = filepath.Clean(prefix)
		line = trailing
	}

	position = filename
	if line > 0 {
		position += ":" + strconv.Itoa(line)
	}
	if column > 0 {
		position += ":" + strconv.Itoa(column)
	}
	return position, filename, line, column
}

func splitTrailingNumber(value string) (string, int, bool) {
	separator := strings.LastIndexByte(value, ':')
	if separator < 0 || separator == len(value)-1 {
		return value, 0, false
	}
	number, err := strconv.Atoi(value[separator+1:])
	if err != nil {
		return value, 0, false
	}
	return value[:separator], number, true
}

func errorKindName(kind packages.ErrorKind) string {
	switch kind {
	case packages.UnknownError:
		return "unknown"
	case packages.ListError:
		return "list"
	case packages.ParseError:
		return "parse"
	case packages.TypeError:
		return "type"
	default:
		return "unknown"
	}
}

func packageSymbols(root *packages.Package) []Symbol {
	if root.Fset == nil || root.TypesInfo == nil {
		return nil
	}

	sourceFiles := fileSet(root.GoFiles)
	symbols := make([]Symbol, 0)
	if name := sourcePackageName(root, sourceFiles); name != nil {
		symbols = append(symbols, Symbol{
			ID:               stableSymbolID(SymbolPackage, root.PkgPath, "", ""),
			DisplayLabel:     root.PkgPath,
			Kind:             SymbolPackage,
			Name:             root.Name,
			PackagePath:      root.PkgPath,
			Position:         root.Fset.PositionFor(name.Pos(), true),
			PhysicalPosition: root.Fset.PositionFor(name.Pos(), false),
			Node:             name,
		})
	}

	for _, file := range root.Syntax {
		if file == nil {
			continue
		}
		for _, declaration := range file.Decls {
			symbols = append(symbols, declarationSymbols(root, declaration, sourceFiles)...)
		}
	}
	return symbols
}

func declarationSymbols(root *packages.Package, declaration ast.Decl, sourceFiles map[string]struct{}) []Symbol {
	switch declaration := declaration.(type) {
	case *ast.GenDecl:
		return generalDeclarationSymbols(root, declaration, sourceFiles)
	case *ast.FuncDecl:
		symbol, ok := functionSymbol(root, declaration, sourceFiles)
		if ok {
			return []Symbol{symbol}
		}
	}
	return nil
}

func generalDeclarationSymbols(
	root *packages.Package,
	declaration *ast.GenDecl,
	sourceFiles map[string]struct{},
) []Symbol {
	var symbols []Symbol
	for _, specification := range declaration.Specs {
		switch specification := specification.(type) {
		case *ast.TypeSpec:
			if symbol, ok := typeSymbol(root, specification, sourceFiles); ok {
				symbols = append(symbols, symbol)
			}
		case *ast.ValueSpec:
			symbols = append(symbols, valueSymbols(root, specification, sourceFiles)...)
		}
	}
	return symbols
}

func typeSymbol(root *packages.Package, specification *ast.TypeSpec, sourceFiles map[string]struct{}) (Symbol, bool) {
	if specification.Name.Name == "_" {
		return Symbol{}, false
	}
	object := root.TypesInfo.Defs[specification.Name]
	if object == nil || !sourceObject(root, object, sourceFiles) {
		return Symbol{}, false
	}
	return objectSymbol(root, object, specification, SymbolType, ""), true
}

func valueSymbols(root *packages.Package, specification *ast.ValueSpec, sourceFiles map[string]struct{}) []Symbol {
	var symbols []Symbol
	for _, name := range specification.Names {
		if name.Name == "_" {
			continue
		}
		object := root.TypesInfo.Defs[name]
		if object == nil || !sourceObject(root, object, sourceFiles) {
			continue
		}
		kind := SymbolVariable
		if _, ok := object.(*types.Const); ok {
			kind = SymbolConstant
		}
		symbols = append(symbols, objectSymbol(root, object, name, kind, ""))
	}
	return symbols
}

func functionSymbol(root *packages.Package, declaration *ast.FuncDecl, sourceFiles map[string]struct{}) (Symbol, bool) {
	if declaration.Name.Name == "_" || (declaration.Recv == nil && declaration.Name.Name == "init") {
		return Symbol{}, false
	}
	object, ok := root.TypesInfo.Defs[declaration.Name].(*types.Func)
	if !ok || !sourceObject(root, object, sourceFiles) {
		return Symbol{}, false
	}
	signature, ok := object.Type().(*types.Signature)
	if !ok {
		return Symbol{}, false
	}
	if declaration.Recv == nil {
		symbol := objectSymbol(root, object, declaration, SymbolFunction, "")
		symbol.Signature = signature
		return symbol, true
	}
	receiver, err := normalizedReceiverName(signature)
	if err != nil {
		// Ill-formed receiver declarations are already reported by go/types.
		return Symbol{}, false
	}
	symbol := objectSymbol(root, object, declaration, SymbolMethod, receiver)
	symbol.Signature = signature
	return symbol, true
}

func objectSymbol(root *packages.Package, object types.Object, node ast.Node, kind SymbolKind, receiver string) Symbol {
	name := object.Name()
	return Symbol{
		ID:               stableSymbolID(kind, root.PkgPath, receiver, name),
		DisplayLabel:     symbolDisplayLabel(root.PkgPath, receiver, name),
		Kind:             kind,
		Name:             name,
		PackagePath:      root.PkgPath,
		Receiver:         receiver,
		Position:         root.Fset.PositionFor(object.Pos(), true),
		PhysicalPosition: root.Fset.PositionFor(object.Pos(), false),
		Object:           object,
		Node:             node,
	}
}

const stableSymbolIDPrefix = "spice:symbol:v1|"

// stableSymbolID serializes the structured logical identity without relying on
// any delimiter being absent from package paths or identifiers. Lengths count
// UTF-8 bytes because Go strings and serialized IDs are byte sequences.
func stableSymbolID(kind SymbolKind, packagePath, receiver, name string) string {
	var builder strings.Builder
	builder.Grow(len(stableSymbolIDPrefix) + len(kind) + len(packagePath) + len(receiver) + len(name) + 32)
	builder.WriteString(stableSymbolIDPrefix)
	builder.WriteString(string(kind))
	builder.WriteByte('|')
	appendStableSymbolField(&builder, packagePath)
	builder.WriteByte('|')
	appendStableSymbolField(&builder, receiver)
	builder.WriteByte('|')
	appendStableSymbolField(&builder, name)
	return builder.String()
}

func appendStableSymbolField(builder *strings.Builder, value string) {
	builder.WriteString(strconv.Itoa(len(value)))
	builder.WriteByte(':')
	builder.WriteString(value)
}

func symbolDisplayLabel(packagePath, receiver, name string) string {
	if name == "" {
		return packagePath
	}
	if receiver == "" {
		return packagePath + "." + name
	}
	return packagePath + "." + receiver + "." + name
}

func symbolKindRank(kind SymbolKind) int {
	switch kind {
	case SymbolPackage:
		return 0
	case SymbolType:
		return 1
	case SymbolFunction:
		return 2
	case SymbolMethod:
		return 3
	case SymbolVariable:
		return 4
	case SymbolConstant:
		return 5
	default:
		return 6
	}
}

func normalizedReceiverName(signature *types.Signature) (string, error) {
	if signature == nil || signature.Recv() == nil {
		return "", fmt.Errorf("method signature has no receiver")
	}
	receiverType := signature.Recv().Type()
	if pointer, ok := receiverType.(*types.Pointer); ok {
		receiverType = pointer.Elem()
	}
	named, ok := receiverType.(*types.Named)
	if !ok {
		return "", fmt.Errorf("receiver %s is not a named type", types.TypeString(receiverType, nil))
	}
	origin := named.Origin()
	if origin == nil || origin.Obj() == nil {
		return "", fmt.Errorf("receiver %s has no defining origin", types.TypeString(receiverType, nil))
	}
	return origin.Obj().Name(), nil
}
