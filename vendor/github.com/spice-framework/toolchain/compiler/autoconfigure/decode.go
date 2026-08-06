package autoconfigure

import (
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/spice-framework/toolchain/compiler/load"
)

const (
	descriptorFunction = "SpiceAutoConfiguration"
	starterPackagePath = "github.com/spice-framework/spice/starter"
)

// Bean is one statically decoded library-owned default factory.
type Bean struct {
	PackagePath     string
	Factory         string
	Name            string
	Aliases         []string
	Qualifiers      []string
	Primary         bool
	Fallback        bool
	Order           int64
	Position        token.Position
	SourceID        string
	SourceVersion   string
	Review          string
	ModulePath      string
	ModuleVersion   string
	ReplacementPath string
}

// Configuration is one selected package descriptor.
type Configuration struct {
	PackagePath     string
	Position        token.Position
	Review          string
	ModulePath      string
	ModuleVersion   string
	ReplacementPath string
	Beans           []Bean
}

// Decode statically decodes selected descriptors from the existing typed
// program. Descriptor functions and package initialization are never executed.
func Decode(program *load.Program, selected []string) ([]Configuration, error) {
	if program == nil {
		return nil, errors.New("decode auto-configuration: loaded program is nil")
	}
	packageIndex := make(map[string]load.Package)
	for _, pkg := range program.Packages() {
		packageIndex[pkg.Path] = pkg
	}
	configurations := make([]Configuration, 0, len(selected))
	for _, packagePath := range selected {
		pkg, found := packageIndex[packagePath]
		if !found {
			return nil, fmt.Errorf(
				"decode auto-configuration %q: package was not loaded as compiler input",
				packagePath,
			)
		}
		configuration, err := decodePackage(pkg)
		if err != nil {
			return nil, fmt.Errorf("decode auto-configuration %q: %w", packagePath, err)
		}
		configurations = append(configurations, configuration)
	}
	sort.Slice(configurations, func(i, j int) bool {
		return configurations[i].PackagePath < configurations[j].PackagePath
	})
	return configurations, nil
}

func decodePackage(pkg load.Package) (Configuration, error) {
	function, err := descriptorDeclaration(pkg)
	if err != nil {
		return Configuration{}, err
	}
	literal, err := descriptorCompositeLiteral(pkg, function)
	if err != nil {
		return Configuration{}, err
	}
	fields, err := keyedFields(literal)
	if err != nil {
		return Configuration{}, err
	}
	return decodeConfiguration(pkg, function, fields)
}

func descriptorDeclaration(pkg load.Package) (*ast.FuncDecl, error) {
	if pkg.Name != packageName {
		return nil, fmt.Errorf(
			"package name must be %q, got %q",
			packageName,
			pkg.Name,
		)
	}
	var declarations []*ast.FuncDecl
	for _, file := range pkg.Syntax {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Recv == nil && function.Name.Name == descriptorFunction {
				declarations = append(declarations, function)
			}
		}
	}
	if len(declarations) != 1 {
		return nil, fmt.Errorf(
			"package must declare exactly one func %s() starter.AutoConfiguration, found %d",
			descriptorFunction,
			len(declarations),
		)
	}
	return declarations[0], nil
}

func descriptorCompositeLiteral(
	pkg load.Package,
	function *ast.FuncDecl,
) (*ast.CompositeLit, error) {
	object, ok := pkg.TypesInfo.Defs[function.Name].(*types.Func)
	if !ok {
		return nil, errors.New("descriptor function has no typed Go function object")
	}
	signature, ok := object.Type().(*types.Signature)
	if !ok ||
		signature.TypeParams() != nil ||
		signature.Params().Len() != 0 ||
		signature.Results().Len() != 1 ||
		!exactNamedType(signature.Results().At(0).Type(), starterPackagePath, "AutoConfiguration") {
		return nil, fmt.Errorf(
			"%s must have exact signature func() starter.AutoConfiguration",
			descriptorFunction,
		)
	}
	if function.Body == nil || len(function.Body.List) != 1 {
		return nil, errors.New(
			"descriptor body must contain exactly one returned composite literal",
		)
	}
	returned, ok := function.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(returned.Results) != 1 {
		return nil, errors.New(
			"descriptor body must contain exactly one returned composite literal",
		)
	}
	literal, ok := returned.Results[0].(*ast.CompositeLit)
	if !ok {
		return nil, errors.New(
			"descriptor return value must be a starter.AutoConfiguration composite literal",
		)
	}
	return literal, nil
}

func decodeConfiguration(
	pkg load.Package,
	function *ast.FuncDecl,
	fields map[string]ast.Expr,
) (Configuration, error) {
	for name := range fields {
		if name != "Review" && name != "Beans" {
			return Configuration{}, fmt.Errorf("descriptor field %q is unsupported", name)
		}
	}
	review, err := requiredString(pkg.TypesInfo, fields["Review"], "Review")
	if err != nil {
		return Configuration{}, err
	}
	configuration := Configuration{
		PackagePath: pkg.Path,
		Position:    positionOf(pkg, function.Name.Pos()),
		Review:      review,
	}
	setModuleProvenance(pkg, &configuration)
	beans, err := decodeBeans(pkg, configuration, fields["Beans"])
	if err != nil {
		return Configuration{}, err
	}
	configuration.Beans = beans
	return configuration, nil
}

func setModuleProvenance(
	pkg load.Package,
	configuration *Configuration,
) {
	if pkg.Raw != nil && pkg.Raw.Module != nil {
		configuration.ModulePath = pkg.Raw.Module.Path
		configuration.ModuleVersion = pkg.Raw.Module.Version
		if pkg.Raw.Module.Replace != nil {
			configuration.ReplacementPath = pkg.Raw.Module.Replace.Path
			if pkg.Raw.Module.Replace.Version != "" {
				configuration.ReplacementPath += "@" + pkg.Raw.Module.Replace.Version
			}
		}
	}
	if configuration.ModulePath == "" {
		configuration.ModulePath = pkg.ModulePath
	}
	if configuration.ModuleVersion == "" {
		configuration.ModuleVersion = "local"
	}
}

func decodeBeans(
	pkg load.Package,
	configuration Configuration,
	expression ast.Expr,
) ([]Bean, error) {
	if expression == nil {
		return nil, errors.New("descriptor requires a non-empty Beans field")
	}
	beansLiteral, ok := expression.(*ast.CompositeLit)
	if !ok || len(beansLiteral.Elts) == 0 {
		return nil, errors.New("descriptor Beans must be a non-empty composite literal")
	}
	beans := make([]Bean, 0, len(beansLiteral.Elts))
	for index, expression := range beansLiteral.Elts {
		beanLiteral, ok := expression.(*ast.CompositeLit)
		if !ok {
			return nil, fmt.Errorf("beans[%d] must be a starter.AutoBean composite literal", index)
		}
		bean, err := decodeBean(pkg, configuration, beanLiteral)
		if err != nil {
			return nil, fmt.Errorf("beans[%d]: %w", index, err)
		}
		beans = append(beans, bean)
	}
	sort.Slice(beans, func(i, j int) bool {
		return beans[i].Factory < beans[j].Factory
	})
	return beans, nil
}

func decodeBean(
	pkg load.Package,
	configuration Configuration,
	literal *ast.CompositeLit,
) (Bean, error) {
	fields, err := keyedFields(literal)
	if err != nil {
		return Bean{}, err
	}
	allowedFields := map[string]struct{}{
		"Factory": {}, "Name": {}, "Aliases": {}, "Qualifiers": {},
		"Primary": {}, "Fallback": {}, "Order": {},
	}
	for name := range fields {
		if _, ok := allowedFields[name]; !ok {
			return Bean{}, fmt.Errorf("field %q is unsupported", name)
		}
	}
	factoryExpression, found := fields["Factory"]
	if !found {
		return Bean{}, errors.New("factory is required")
	}
	factory, position, err := factoryReference(pkg, factoryExpression)
	if err != nil {
		return Bean{}, err
	}
	if factory.Pkg() == nil || factory.Pkg().Path() != pkg.Path {
		return Bean{}, errors.New("factory must reference a function in the autoconfigure package")
	}
	if !factory.Exported() {
		return Bean{}, errors.New("factory must reference an exported function")
	}
	bean := Bean{
		PackagePath:     pkg.Path,
		Factory:         factory.Name(),
		Position:        position,
		SourceID:        pkg.Path,
		SourceVersion:   configuration.ModuleVersion,
		Review:          configuration.Review,
		ModulePath:      configuration.ModulePath,
		ModuleVersion:   configuration.ModuleVersion,
		ReplacementPath: configuration.ReplacementPath,
	}
	if err := decodeBeanMetadata(pkg.TypesInfo, fields, &bean); err != nil {
		return Bean{}, err
	}
	bean.Aliases = normalizedStrings(bean.Aliases)
	bean.Qualifiers = normalizedStrings(bean.Qualifiers)
	return bean, nil
}

func decodeBeanMetadata(
	info *types.Info,
	fields map[string]ast.Expr,
	bean *Bean,
) error {
	var err error
	if expression, ok := fields["Name"]; ok {
		bean.Name, err = optionalString(info, expression, "Name")
		if err != nil {
			return err
		}
	}
	if expression, ok := fields["Aliases"]; ok {
		bean.Aliases, err = stringList(info, expression, "Aliases")
		if err != nil {
			return err
		}
	}
	if expression, ok := fields["Qualifiers"]; ok {
		bean.Qualifiers, err = stringList(info, expression, "Qualifiers")
		if err != nil {
			return err
		}
	}
	if expression, ok := fields["Primary"]; ok {
		bean.Primary, err = boolValue(info, expression, "Primary")
		if err != nil {
			return err
		}
	}
	if expression, ok := fields["Fallback"]; ok {
		bean.Fallback, err = boolValue(info, expression, "Fallback")
		if err != nil {
			return err
		}
	}
	if bean.Primary && bean.Fallback {
		return errors.New("primary and fallback cannot both be true")
	}
	if expression, ok := fields["Order"]; ok {
		bean.Order, err = integerValue(info, expression, "Order")
		if err != nil {
			return err
		}
	}
	return nil
}

func keyedFields(literal *ast.CompositeLit) (map[string]ast.Expr, error) {
	result := make(map[string]ast.Expr, len(literal.Elts))
	for _, expression := range literal.Elts {
		field, ok := expression.(*ast.KeyValueExpr)
		if !ok {
			return nil, errors.New("composite literals must use keyed fields")
		}
		name, ok := field.Key.(*ast.Ident)
		if !ok {
			return nil, errors.New("composite literal field name must be an identifier")
		}
		if _, duplicate := result[name.Name]; duplicate {
			return nil, fmt.Errorf("field %q is duplicated", name.Name)
		}
		result[name.Name] = field.Value
	}
	return result, nil
}

func factoryReference(
	pkg load.Package,
	expression ast.Expr,
) (*types.Func, token.Position, error) {
	var identifier *ast.Ident
	switch value := expression.(type) {
	case *ast.Ident:
		identifier = value
	case *ast.SelectorExpr:
		identifier = value.Sel
	default:
		return nil, token.Position{}, errors.New("factory must be a direct typed Go function reference")
	}
	object, ok := pkg.TypesInfo.ObjectOf(identifier).(*types.Func)
	if !ok {
		return nil, token.Position{}, errors.New("factory must be a direct typed Go function reference")
	}
	signature, ok := object.Type().(*types.Signature)
	if !ok || signature.Recv() != nil || signature.TypeParams() != nil {
		return nil, token.Position{}, errors.New("factory must reference a non-generic package-level function")
	}
	return object, positionOf(pkg, identifier.Pos()), nil
}

func requiredString(info *types.Info, expression ast.Expr, name string) (string, error) {
	value, err := optionalString(info, expression, name)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
		return "", fmt.Errorf("%s must be a non-empty trimmed string constant", name)
	}
	return value, nil
}

func optionalString(info *types.Info, expression ast.Expr, name string) (string, error) {
	value := info.Types[expression].Value
	if value == nil || value.Kind() != constant.String {
		return "", fmt.Errorf("%s must be a string constant", name)
	}
	return constant.StringVal(value), nil
}

func boolValue(info *types.Info, expression ast.Expr, name string) (bool, error) {
	value := info.Types[expression].Value
	if value == nil || value.Kind() != constant.Bool {
		return false, fmt.Errorf("%s must be a Boolean constant", name)
	}
	return constant.BoolVal(value), nil
}

func integerValue(info *types.Info, expression ast.Expr, name string) (int64, error) {
	value := info.Types[expression].Value
	if value == nil || value.Kind() != constant.Int {
		return 0, fmt.Errorf("%s must be an integer constant", name)
	}
	result, exact := constant.Int64Val(value)
	if !exact {
		return 0, fmt.Errorf("%s exceeds the signed 64-bit range", name)
	}
	return result, nil
}

func stringList(info *types.Info, expression ast.Expr, name string) ([]string, error) {
	literal, ok := expression.(*ast.CompositeLit)
	if !ok {
		return nil, fmt.Errorf("%s must be a string slice composite literal", name)
	}
	result := make([]string, len(literal.Elts))
	for index, item := range literal.Elts {
		value, err := optionalString(info, item, name+"["+strconv.Itoa(index)+"]")
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
			return nil, fmt.Errorf("%s[%d] must be a non-empty trimmed string", name, index)
		}
		result[index] = value
	}
	return result, nil
}

func normalizedStrings(values []string) []string {
	result := slices.Clone(values)
	slices.Sort(result)
	return slices.Compact(result)
}

func exactNamedType(value types.Type, packagePath, name string) bool {
	named, ok := value.(*types.Named)
	if !ok || named.Obj() == nil || named.Obj().Pkg() == nil {
		return false
	}
	return named.Obj().Pkg().Path() == packagePath && named.Obj().Name() == name
}

func positionOf(pkg load.Package, position token.Pos) token.Position {
	if pkg.Raw == nil || pkg.Raw.Fset == nil {
		return token.Position{}
	}
	return pkg.Raw.Fset.PositionFor(position, true)
}
