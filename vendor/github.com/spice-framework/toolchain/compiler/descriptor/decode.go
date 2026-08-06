// @import { NamedInterface } from "github.com/spice-framework/spice/annotation/modulith"

// Package descriptor statically decodes annotation SDK descriptor functions
// from the compiler's single typed Go program.
//
// @NamedInterface("descriptor")
package descriptor

import (
	"context"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/spice/annotation/sdk"
	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/internal/identity"
)

const sdkPackagePath = "github.com/spice-framework/spice/annotation/sdk"

// Descriptor is one validated public annotation definition and its real Go
// source location.
type Descriptor struct {
	Definition    sdk.Definition
	Handler       sdk.Symbol
	Package       string
	Symbol        string
	Documentation string
	Position      token.Position
	Provenance    annotation.ModuleProvenance
}

// DecodeAll resolves a deterministic reference set and rejects canonical-name
// ambiguity across descriptor packages.
func DecodeAll(
	program *load.Program,
	references []annotation.DefinitionReference,
) ([]Descriptor, error) {
	references = append([]annotation.DefinitionReference(nil), references...)
	sort.Slice(references, func(i, j int) bool {
		if references[i].Package != references[j].Package {
			return references[i].Package < references[j].Package
		}
		return references[i].Symbol < references[j].Symbol
	})
	references = compactReferences(references)
	result := make([]Descriptor, 0, len(references))
	names := make(map[string]annotation.DefinitionReference, len(references))
	for _, reference := range references {
		decoded, err := Decode(program, reference.Package, reference.Symbol)
		if err != nil {
			return nil, err
		}
		if prior, duplicate := names[decoded.Definition.Name]; duplicate {
			return nil, fmt.Errorf(
				"annotation descriptor %s.%s and %s.%s both define canonical name %q",
				prior.Package,
				prior.Symbol,
				reference.Package,
				reference.Symbol,
				decoded.Definition.Name,
			)
		}
		names[decoded.Definition.Name] = reference
		result = append(result, decoded)
	}
	return result, nil
}

// RegistryDefinition adapts the rich public SDK descriptor to the compiler's
// current generic invocation-validation model.
func (descriptor Descriptor) RegistryDefinition() (annotation.Definition, error) {
	targets, err := annotation.NewTargetSet(descriptor.Definition.Targets...)
	if err != nil {
		return annotation.Definition{}, fmt.Errorf(
			"convert annotation descriptor %s.%s targets: %w",
			descriptor.Package,
			descriptor.Symbol,
			err,
		)
	}
	arguments := make(
		[]annotation.ArgumentDefinition,
		len(descriptor.Definition.Arguments),
	)
	for index, argument := range descriptor.Definition.Arguments {
		arguments[index] = annotation.ArgumentDefinition{
			Name:  argument.Name,
			Kinds: append([]annotation.Kind(nil), argument.Kinds...),
			ListElementKinds: append(
				[]annotation.Kind(nil),
				argument.ListElementKinds...,
			),
			ValueDomain: argument.ValueDomain,
			Required:    argument.Required,
			Positional:  argument.Positional,
			Variadic:    argument.Variadic,
		}
	}
	return annotation.Definition{
		Name:       descriptor.Definition.Name,
		Targets:    targets,
		Repeatable: descriptor.Definition.Repeatable,
		Arguments:  arguments,
	}, nil
}

// Decode resolves and statically decodes one descriptor function. No package
// initializer or function body is executed.
func Decode(
	program *load.Program,
	packagePath string,
	symbolName string,
) (Descriptor, error) {
	pkg, symbol, declaration, err := descriptorFunction(
		program,
		packagePath,
		symbolName,
	)
	if err != nil {
		return Descriptor{}, err
	}
	expression, err := descriptorExpression(declaration)
	if err != nil {
		return Descriptor{}, descriptorError(symbol, "%v", err)
	}
	definition, handler, err := decodeDefinition(pkg.TypesInfo, expression)
	if err != nil {
		return Descriptor{}, descriptorError(symbol, "%v", err)
	}
	definition.Implementation.Tool = identity.NormalizeDescriptorTool(
		packagePath,
		definition.Implementation.Tool,
	)
	nameSuffix := definition.Name
	if separator := strings.LastIndexByte(nameSuffix, '.'); separator >= 0 {
		nameSuffix = nameSuffix[separator+1:]
	}
	if nameSuffix != symbolName {
		return Descriptor{}, descriptorError(
			symbol,
			"definition name %q must end in descriptor symbol %q",
			definition.Name,
			symbolName,
		)
	}
	if err := definition.Validate(); err != nil {
		return Descriptor{}, descriptorError(symbol, "%v", err)
	}
	if handler.Package != packagePath {
		return Descriptor{}, descriptorError(
			symbol,
			"implementation handler must be declared in descriptor package %q",
			packagePath,
		)
	}
	if err := validateHandlerInDescriptorFile(
		pkg,
		declaration,
		handler.Name,
	); err != nil {
		return Descriptor{}, descriptorError(symbol, "%v", err)
	}
	return Descriptor{
		Definition:    definition,
		Handler:       handler,
		Package:       packagePath,
		Symbol:        symbolName,
		Documentation: strings.TrimSpace(declaration.Doc.Text()),
		Position:      symbol.PhysicalPosition,
		Provenance:    moduleProvenance(pkg),
	}, nil
}

func moduleProvenance(pkg load.Package) annotation.ModuleProvenance {
	if pkg.Raw == nil || pkg.Raw.Module == nil {
		return annotation.ModuleProvenance{}
	}
	module := pkg.Raw.Module
	result := annotation.ModuleProvenance{
		Path:      module.Path,
		Version:   module.Version,
		Directory: filepath.Clean(module.Dir),
	}
	if module.Replace != nil {
		result.ReplacementPath = module.Replace.Path
		result.ReplacementVersion = module.Replace.Version
		result.ReplacementDir = filepath.Clean(module.Replace.Dir)
		result.LocalReplacement = module.Replace.Version == ""
	}
	return result
}

func descriptorFunction(
	program *load.Program,
	packagePath string,
	symbolName string,
) (load.Package, load.Symbol, *ast.FuncDecl, error) {
	if program == nil {
		return load.Package{}, load.Symbol{}, nil, fmt.Errorf(
			"decode annotation descriptor: program is nil",
		)
	}
	pkg, found := packageByPath(program, packagePath)
	if !found {
		return load.Package{}, load.Symbol{}, nil, fmt.Errorf(
			"decode annotation descriptor %s.%s: package is not in the typed program",
			packagePath,
			symbolName,
		)
	}
	symbol, found := symbolByName(program, packagePath, symbolName)
	if !found {
		return load.Package{}, load.Symbol{}, nil, fmt.Errorf(
			"decode annotation descriptor %s.%s: exported function was not found",
			packagePath,
			symbolName,
		)
	}
	declaration, ok := symbol.Node.(*ast.FuncDecl)
	if !ok || symbol.Kind != load.SymbolFunction {
		return load.Package{}, load.Symbol{}, nil, descriptorError(
			symbol,
			"must be a package-level function",
		)
	}
	if !token.IsExported(symbolName) {
		return load.Package{}, load.Symbol{}, nil, descriptorError(
			symbol,
			"must be exported",
		)
	}
	if err := validateSignature(symbol.Signature); err != nil {
		return load.Package{}, load.Symbol{}, nil, descriptorError(
			symbol,
			"%v",
			err,
		)
	}
	if declaration.Doc == nil ||
		strings.TrimSpace(declaration.Doc.Text()) == "" {
		return load.Package{}, load.Symbol{}, nil, descriptorError(
			symbol,
			"requires a GoDoc comment",
		)
	}
	if err := validateOneDescriptorPerFile(pkg, declaration); err != nil {
		return load.Package{}, load.Symbol{}, nil, descriptorError(
			symbol,
			"%v",
			err,
		)
	}
	return pkg, symbol, declaration, nil
}

func compactReferences(
	values []annotation.DefinitionReference,
) []annotation.DefinitionReference {
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

func packageByPath(
	program *load.Program,
	packagePath string,
) (load.Package, bool) {
	for _, pkg := range program.Packages() {
		if pkg.Path == packagePath {
			return pkg, true
		}
	}
	return load.Package{}, false
}

func symbolByName(
	program *load.Program,
	packagePath string,
	name string,
) (load.Symbol, bool) {
	for _, symbol := range program.Symbols() {
		if symbol.PackagePath == packagePath &&
			symbol.Receiver == "" &&
			symbol.Name == name {
			return symbol, true
		}
	}
	return load.Symbol{}, false
}

func validateSignature(signature *types.Signature) error {
	if signature == nil ||
		signature.Recv() != nil ||
		signature.Params().Len() != 0 ||
		signature.Results().Len() != 1 ||
		signature.Variadic() ||
		signature.TypeParams().Len() != 0 {
		return fmt.Errorf(
			"must have exact signature func() sdk.Definition",
		)
	}
	if !isSDKDefinition(signature.Results().At(0).Type()) {
		return fmt.Errorf(
			"must return exact %s.Definition",
			sdkPackagePath,
		)
	}
	return nil
}

func isSDKDefinition(value types.Type) bool {
	return isNamedType(value, sdkPackagePath, "Definition")
}

func isNamedType(value types.Type, packagePath string, name string) bool {
	named, ok := value.(*types.Named)
	if !ok || named.Obj() == nil || named.Obj().Pkg() == nil {
		return false
	}
	return named.Obj().Name() == name &&
		named.Obj().Pkg().Path() == packagePath
}

func validHandlerSignature(signature *types.Signature) bool {
	if signature == nil ||
		signature.Recv() != nil ||
		signature.Variadic() ||
		signature.TypeParams().Len() != 0 ||
		signature.Params().Len() != 2 ||
		signature.Results().Len() != 2 {
		return false
	}
	return isNamedType(
		signature.Params().At(0).Type(),
		"context",
		"Context",
	) &&
		isNamedType(
			signature.Params().At(1).Type(),
			sdkPackagePath,
			"Invocation",
		) &&
		isNamedType(
			signature.Results().At(0).Type(),
			sdkPackagePath,
			"Result",
		) &&
		types.Identical(
			signature.Results().At(1).Type(),
			types.Universe.Lookup("error").Type(),
		)
}

func validateHandlerInDescriptorFile(
	pkg load.Package,
	descriptor *ast.FuncDecl,
	handlerName string,
) error {
	for _, file := range pkg.Syntax {
		if file == nil ||
			descriptor.Pos() < file.Pos() ||
			descriptor.End() > file.End() {
			continue
		}
		for _, item := range file.Decls {
			function, ok := item.(*ast.FuncDecl)
			if !ok || function.Name == nil ||
				function.Name.Name != handlerName {
				continue
			}
			if function.Recv != nil || function.Body == nil ||
				!token.IsExported(handlerName) {
				return fmt.Errorf(
					"implementation handler %q must be an exported package-level function with a body",
					handlerName,
				)
			}
			return nil
		}
		return fmt.Errorf(
			"implementation handler %q must be declared in the descriptor's Go file",
			handlerName,
		)
	}
	return fmt.Errorf("source file was not found in the typed program")
}

func validateOneDescriptorPerFile(
	pkg load.Package,
	declaration *ast.FuncDecl,
) error {
	for _, file := range pkg.Syntax {
		if file == nil ||
			declaration.Pos() < file.Pos() ||
			declaration.End() > file.End() {
			continue
		}
		count := 0
		for _, item := range file.Decls {
			function, ok := item.(*ast.FuncDecl)
			if !ok || function.Recv != nil || !token.IsExported(function.Name.Name) {
				continue
			}
			object, ok := pkg.TypesInfo.Defs[function.Name].(*types.Func)
			if !ok {
				continue
			}
			signature, ok := object.Type().(*types.Signature)
			if ok &&
				signature.Results().Len() == 1 &&
				isSDKDefinition(signature.Results().At(0).Type()) {
				count++
			}
		}
		if count != 1 {
			return fmt.Errorf(
				"must be the only exported annotation descriptor in its Go file",
			)
		}
		return nil
	}
	return fmt.Errorf("source file was not found in the typed program")
}

func descriptorExpression(declaration *ast.FuncDecl) (ast.Expr, error) {
	if declaration.Body == nil || len(declaration.Body.List) != 1 {
		return nil, fmt.Errorf(
			"body must contain only 'return sdk.Definition{...}'",
		)
	}
	statement, ok := declaration.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(statement.Results) != 1 {
		return nil, fmt.Errorf(
			"body must contain only 'return sdk.Definition{...}'",
		)
	}
	if _, ok := statement.Results[0].(*ast.CompositeLit); !ok {
		return nil, fmt.Errorf("must return one static sdk.Definition composite literal")
	}
	return statement.Results[0], nil
}

func decodeDefinition(
	info *types.Info,
	expression ast.Expr,
) (sdk.Definition, sdk.Symbol, error) {
	fields, err := keyedFields(expression, map[string]struct{}{
		"Name":           {},
		"Summary":        {},
		"Targets":        {},
		"Repeatable":     {},
		"Arguments":      {},
		"Examples":       {},
		"Compatibility":  {},
		"Implementation": {},
	})
	if err != nil {
		return sdk.Definition{}, sdk.Symbol{}, fmt.Errorf("decode definition: %w", err)
	}
	var result sdk.Definition
	result.Name, err = optionalString(info, fields, "Name")
	if err != nil {
		return sdk.Definition{}, sdk.Symbol{}, err
	}
	result.Summary, err = optionalString(info, fields, "Summary")
	if err != nil {
		return sdk.Definition{}, sdk.Symbol{}, err
	}
	result.Targets, err = optionalTargets(info, fields, "Targets")
	if err != nil {
		return sdk.Definition{}, sdk.Symbol{}, err
	}
	result.Repeatable, err = optionalBool(info, fields, "Repeatable")
	if err != nil {
		return sdk.Definition{}, sdk.Symbol{}, err
	}
	result.Arguments, err = optionalArguments(info, fields, "Arguments")
	if err != nil {
		return sdk.Definition{}, sdk.Symbol{}, err
	}
	result.Examples, err = optionalExamples(info, fields, "Examples")
	if err != nil {
		return sdk.Definition{}, sdk.Symbol{}, err
	}
	if expression, found := fields["Compatibility"]; found {
		result.Compatibility, err = compatibilityValue(info, expression)
		if err != nil {
			return sdk.Definition{}, sdk.Symbol{}, fieldError("Compatibility", err)
		}
	}
	var handler sdk.Symbol
	if expression, found := fields["Implementation"]; found {
		result.Implementation, handler, err = implementationValue(info, expression)
		if err != nil {
			return sdk.Definition{}, sdk.Symbol{}, fieldError("Implementation", err)
		}
	}
	return result, handler, nil
}

func optionalString(
	info *types.Info,
	fields map[string]ast.Expr,
	name string,
) (string, error) {
	expression, found := fields[name]
	if !found {
		return "", nil
	}
	value, err := stringValue(info, expression)
	if err != nil {
		return "", fieldError(name, err)
	}
	return value, nil
}

func optionalBool(
	info *types.Info,
	fields map[string]ast.Expr,
	name string,
) (bool, error) {
	expression, found := fields[name]
	if !found {
		return false, nil
	}
	value, err := boolValue(info, expression)
	if err != nil {
		return false, fieldError(name, err)
	}
	return value, nil
}

func optionalTargets(
	info *types.Info,
	fields map[string]ast.Expr,
	name string,
) ([]sdk.Target, error) {
	expression, found := fields[name]
	if !found {
		return nil, nil
	}
	value, err := targetValues(info, expression)
	if err != nil {
		return nil, fieldError(name, err)
	}
	return value, nil
}

func optionalArguments(
	info *types.Info,
	fields map[string]ast.Expr,
	name string,
) ([]sdk.Argument, error) {
	expression, found := fields[name]
	if !found {
		return nil, nil
	}
	value, err := argumentValues(info, expression)
	if err != nil {
		return nil, fieldError(name, err)
	}
	return value, nil
}

func optionalExamples(
	info *types.Info,
	fields map[string]ast.Expr,
	name string,
) ([]sdk.Example, error) {
	expression, found := fields[name]
	if !found {
		return nil, nil
	}
	value, err := exampleValues(info, expression)
	if err != nil {
		return nil, fieldError(name, err)
	}
	return value, nil
}

func argumentValues(
	info *types.Info,
	expression ast.Expr,
) ([]sdk.Argument, error) {
	elements, err := compositeElements(expression)
	if err != nil {
		return nil, err
	}
	result := make([]sdk.Argument, 0, len(elements))
	for index, element := range elements {
		item, err := decodeArgument(info, element)
		if err != nil {
			return nil, fmt.Errorf("argument %d: %w", index, err)
		}
		result = append(result, item)
	}
	return result, nil
}

func decodeArgument(
	info *types.Info,
	expression ast.Expr,
) (sdk.Argument, error) {
	fields, err := keyedFields(expression, map[string]struct{}{
		"Name":             {},
		"Kinds":            {},
		"ListElementKinds": {},
		"ValueDomain":      {},
		"AllowedValues":    {},
		"Description":      {},
		"Default":          {},
		"Required":         {},
		"Positional":       {},
		"Variadic":         {},
	})
	if err != nil {
		return sdk.Argument{}, err
	}
	var result sdk.Argument
	result.Name, err = optionalString(info, fields, "Name")
	if err != nil {
		return sdk.Argument{}, err
	}
	result.Kinds, err = optionalKinds(info, fields, "Kinds")
	if err != nil {
		return sdk.Argument{}, err
	}
	result.ListElementKinds, err = optionalKinds(
		info,
		fields,
		"ListElementKinds",
	)
	if err != nil {
		return sdk.Argument{}, err
	}
	valueDomain, err := optionalString(info, fields, "ValueDomain")
	if err != nil {
		return sdk.Argument{}, err
	}
	result.ValueDomain = sdk.ValueDomain(valueDomain)
	result.AllowedValues, err = optionalStrings(
		info,
		fields,
		"AllowedValues",
	)
	if err != nil {
		return sdk.Argument{}, err
	}
	result.Description, err = optionalString(info, fields, "Description")
	if err != nil {
		return sdk.Argument{}, err
	}
	result.Default, err = optionalString(info, fields, "Default")
	if err != nil {
		return sdk.Argument{}, err
	}
	result.Required, err = optionalBool(info, fields, "Required")
	if err != nil {
		return sdk.Argument{}, err
	}
	result.Positional, err = optionalBool(info, fields, "Positional")
	if err != nil {
		return sdk.Argument{}, err
	}
	result.Variadic, err = optionalBool(info, fields, "Variadic")
	if err != nil {
		return sdk.Argument{}, err
	}
	return result, nil
}

func optionalKinds(
	info *types.Info,
	fields map[string]ast.Expr,
	name string,
) ([]sdk.Kind, error) {
	expression, found := fields[name]
	if !found {
		return nil, nil
	}
	value, err := kindValues(info, expression)
	if err != nil {
		return nil, fieldError(name, err)
	}
	return value, nil
}

func optionalStrings(
	info *types.Info,
	fields map[string]ast.Expr,
	name string,
) ([]string, error) {
	expression, found := fields[name]
	if !found {
		return nil, nil
	}
	value, err := stringValues(info, expression)
	if err != nil {
		return nil, fieldError(name, err)
	}
	return value, nil
}

func exampleValues(
	info *types.Info,
	expression ast.Expr,
) ([]sdk.Example, error) {
	elements, err := compositeElements(expression)
	if err != nil {
		return nil, err
	}
	result := make([]sdk.Example, 0, len(elements))
	for index, element := range elements {
		fields, fieldErr := keyedFields(element, map[string]struct{}{
			"Title": {},
			"Code":  {},
		})
		if fieldErr != nil {
			return nil, fmt.Errorf("example %d: %w", index, fieldErr)
		}
		var item sdk.Example
		if value, found := fields["Title"]; found {
			item.Title, err = stringValue(info, value)
		}
		if err == nil {
			if value, found := fields["Code"]; found {
				item.Code, err = stringValue(info, value)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("example %d: %w", index, err)
		}
		result = append(result, item)
	}
	return result, nil
}

func compatibilityValue(
	info *types.Info,
	expression ast.Expr,
) (sdk.Compatibility, error) {
	fields, err := keyedFields(expression, map[string]struct{}{
		"Since":        {},
		"MinimumSpice": {},
	})
	if err != nil {
		return sdk.Compatibility{}, err
	}
	var result sdk.Compatibility
	if value, found := fields["Since"]; found {
		result.Since, err = stringValue(info, value)
	}
	if err == nil {
		if value, found := fields["MinimumSpice"]; found {
			result.MinimumSpice, err = stringValue(info, value)
		}
	}
	return result, err
}

func implementationValue(
	info *types.Info,
	expression ast.Expr,
) (sdk.Implementation, sdk.Symbol, error) {
	fields, err := keyedFields(expression, map[string]struct{}{
		"Tool":     {},
		"Handler":  {},
		"Protocol": {},
	})
	if err != nil {
		return sdk.Implementation{}, sdk.Symbol{}, err
	}
	var result sdk.Implementation
	var handler sdk.Symbol
	if value, found := fields["Tool"]; found {
		result.Tool, err = stringValue(info, value)
	}
	if err == nil {
		if value, found := fields["Handler"]; found {
			handler, err = handlerValue(info, value)
			if err == nil {
				result.Handler = decodedHandler
			}
		}
	}
	if err == nil {
		if value, found := fields["Protocol"]; found {
			var protocol string
			protocol, err = stringValue(info, value)
			result.Protocol = sdk.ProtocolVersion(protocol)
		}
	}
	return result, handler, err
}

func decodedHandler(context.Context, sdk.Invocation) (sdk.Result, error) {
	panic("statically decoded annotation handler must never execute")
}

func handlerValue(
	info *types.Info,
	expression ast.Expr,
) (sdk.Symbol, error) {
	var object types.Object
	switch value := expression.(type) {
	case *ast.Ident:
		object = info.Uses[value]
	case *ast.SelectorExpr:
		object = info.Uses[value.Sel]
	default:
		return sdk.Symbol{}, fmt.Errorf(
			"must be a package-level handler function reference",
		)
	}
	function, ok := object.(*types.Func)
	if !ok || function.Pkg() == nil {
		return sdk.Symbol{}, fmt.Errorf(
			"must be a package-level handler function reference",
		)
	}
	signature, ok := function.Type().(*types.Signature)
	if !ok || !validHandlerSignature(signature) {
		return sdk.Symbol{}, fmt.Errorf(
			"must have exact signature func(context.Context, sdk.Invocation) (sdk.Result, error)",
		)
	}
	return sdk.Symbol{
		Package: function.Pkg().Path(),
		Name:    function.Name(),
	}, nil
}

func targetValues(info *types.Info, expression ast.Expr) ([]sdk.Target, error) {
	values, err := stringValues(info, expression)
	if err != nil {
		return nil, err
	}
	result := make([]sdk.Target, len(values))
	for index, value := range values {
		result[index] = sdk.Target(value)
	}
	return result, nil
}

func kindValues(info *types.Info, expression ast.Expr) ([]sdk.Kind, error) {
	values, err := stringValues(info, expression)
	if err != nil {
		return nil, err
	}
	result := make([]sdk.Kind, len(values))
	for index, value := range values {
		result[index] = sdk.Kind(value)
	}
	return result, nil
}

func stringValues(info *types.Info, expression ast.Expr) ([]string, error) {
	elements, err := compositeElements(expression)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(elements))
	for index, element := range elements {
		value, valueErr := stringValue(info, element)
		if valueErr != nil {
			return nil, fmt.Errorf("element %d: %w", index, valueErr)
		}
		result = append(result, value)
	}
	return result, nil
}

func compositeElements(expression ast.Expr) ([]ast.Expr, error) {
	literal, ok := expression.(*ast.CompositeLit)
	if !ok {
		return nil, fmt.Errorf("must be a static composite literal")
	}
	result := make([]ast.Expr, 0, len(literal.Elts))
	for _, element := range literal.Elts {
		if _, keyed := element.(*ast.KeyValueExpr); keyed {
			return nil, fmt.Errorf("slice literals must not contain keyed elements")
		}
		result = append(result, element)
	}
	return result, nil
}

func keyedFields(
	expression ast.Expr,
	allowed map[string]struct{},
) (map[string]ast.Expr, error) {
	literal, ok := expression.(*ast.CompositeLit)
	if !ok {
		return nil, fmt.Errorf("must be a static composite literal")
	}
	result := make(map[string]ast.Expr, len(literal.Elts))
	for _, element := range literal.Elts {
		item, ok := element.(*ast.KeyValueExpr)
		if !ok {
			return nil, fmt.Errorf("struct literals must use named fields")
		}
		name, ok := item.Key.(*ast.Ident)
		if !ok {
			return nil, fmt.Errorf("struct field name must be an identifier")
		}
		if _, supported := allowed[name.Name]; !supported {
			return nil, fmt.Errorf("unknown field %q", name.Name)
		}
		if _, duplicate := result[name.Name]; duplicate {
			return nil, fmt.Errorf("duplicate field %q", name.Name)
		}
		result[name.Name] = item.Value
	}
	return result, nil
}

func stringValue(info *types.Info, expression ast.Expr) (string, error) {
	if !literalOrExportedConstant(info, expression, token.STRING) {
		return "", fmt.Errorf("must be a string literal or exported Go string constant")
	}
	value := info.Types[expression].Value
	if value == nil || value.Kind() != constant.String {
		return "", fmt.Errorf("must be a string literal or exported Go string constant")
	}
	return constant.StringVal(value), nil
}

func boolValue(info *types.Info, expression ast.Expr) (bool, error) {
	identifier, literal := expression.(*ast.Ident)
	if !literal || identifier.Name != "true" && identifier.Name != "false" {
		return false, fmt.Errorf("must be a Boolean literal")
	}
	value := info.Types[expression].Value
	if value == nil || value.Kind() != constant.Bool {
		return false, fmt.Errorf("must be a Boolean literal")
	}
	return constant.BoolVal(value), nil
}

func literalOrExportedConstant(
	info *types.Info,
	expression ast.Expr,
	literalKind token.Token,
) bool {
	if literal, ok := expression.(*ast.BasicLit); ok {
		return literal.Kind == literalKind
	}
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok || selector.Sel == nil || !token.IsExported(selector.Sel.Name) {
		return false
	}
	object, ok := info.Uses[selector.Sel].(*types.Const)
	return ok && object.Pkg() != nil
}

func fieldError(name string, err error) error {
	return fmt.Errorf("field %s: %w", name, err)
}

func descriptorError(
	symbol load.Symbol,
	format string,
	arguments ...any,
) error {
	position := symbol.PhysicalPosition
	location := symbol.PackagePath + "." + symbol.Name
	if position.Filename != "" {
		location = fmt.Sprintf(
			"%s:%d:%d",
			position.Filename,
			max(position.Line, 1),
			max(position.Column, 1),
		)
	}
	return fmt.Errorf(
		"%s: annotation descriptor %s.%s %s",
		location,
		symbol.PackagePath,
		symbol.Name,
		fmt.Sprintf(format, arguments...),
	)
}
