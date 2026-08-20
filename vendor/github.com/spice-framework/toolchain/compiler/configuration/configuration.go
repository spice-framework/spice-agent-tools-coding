// Package configuration validates reflection-free configuration declarations
// from the shared typed compiler program.
package configuration

import (
	"fmt"
	"go/token"
	"go/types"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/spice/annotation/sdk"
	runtimeconfig "github.com/spice-framework/spice/config"
	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/compiler/modulith"
	"github.com/spice-framework/toolchain/compiler/provider"
	"github.com/spice-framework/toolchain/compiler/resolve"
)

const fieldTagName = "spice"

// Field is one validated configuration property and its exact Go destination.
type Field struct {
	Index            int
	Name             string
	Type             types.Type
	TypeID           string
	Key              string
	Kind             runtimeconfig.Kind
	Module           string
	Environment      string
	Default          string
	HasDefault       bool
	Required         bool
	Secret           bool
	Position         token.Position
	PhysicalPosition token.Position
}

// Type is one validated @ConfigurationProperties named struct.
type Type struct {
	Symbol           load.Symbol
	SymbolID         string
	Name             string
	PackagePath      string
	Type             types.Type
	TypeID           string
	Prefix           string
	Module           string
	Position         token.Position
	PhysicalPosition token.Position
	fields           []Field
}

// Fields returns configuration fields in declaration order.
func (t Type) Fields() []Field {
	return append([]Field(nil), t.fields...)
}

// Diagnostic is one deterministic source-positioned configuration failure.
type Diagnostic struct {
	Position         token.Position
	PhysicalPosition token.Position
	SymbolID         string
	Field            string
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

// Catalog is the immutable-by-convention configuration declaration result.
type Catalog struct {
	types       []Type
	diagnostics []Diagnostic
}

// Types returns configuration types in stable symbol order.
func (c Catalog) Types() []Type {
	result := make([]Type, len(c.types))
	copy(result, c.types)
	for index := range result {
		result[index].fields = append([]Field(nil), c.types[index].fields...)
	}
	return result
}

// Diagnostics returns deterministic configuration diagnostics.
func (c Catalog) Diagnostics() []Diagnostic {
	return append([]Diagnostic(nil), c.diagnostics...)
}

// Build validates every resolved @ConfigurationProperties declaration without reloading
// packages, reflecting on runtime values, or executing application code.
func Build(program *load.Program, resolution resolve.Result, modules modulith.Model) Catalog {
	if program == nil {
		return Catalog{diagnostics: []Diagnostic{{
			Kind:    "internal",
			Message: "configuration catalog requires a loaded program",
		}}}
	}

	symbols := symbolIndex(program.Symbols())
	fileSets := packageFileSets(program)
	seen := make(map[string]resolve.Occurrence)
	catalog := Catalog{}
	for _, occurrence := range resolution.Occurrences {
		if !occurrence.HasContribution(
			sdk.ContributionConfiguration,
		) {
			continue
		}
		if previous, duplicate := seen[occurrence.SymbolID]; duplicate {
			catalog.diagnostics = append(catalog.diagnostics, occurrenceDiagnostic(
				occurrence,
				"duplicate-annotation",
				fmt.Sprintf(
					"@ConfigurationProperties type %q is declared more than once; first declaration is at %s",
					occurrence.Name,
					renderPosition(previous.DisplayPosition),
				),
			))
			continue
		}
		seen[occurrence.SymbolID] = occurrence

		symbol, ok := symbols[occurrence.SymbolID]
		if !ok {
			catalog.diagnostics = append(catalog.diagnostics, occurrenceDiagnostic(
				occurrence,
				"missing-symbol",
				fmt.Sprintf("@ConfigurationProperties target %q has no stable typed symbol in the loaded program", occurrence.Name),
			))
			continue
		}
		configType, diagnostics := analyzeType(
			occurrence,
			symbol,
			fileSets[symbol.PackagePath],
			modules,
		)
		catalog.diagnostics = append(catalog.diagnostics, diagnostics...)
		if len(diagnostics) == 0 {
			catalog.types = append(catalog.types, configType)
		}
	}
	sort.SliceStable(catalog.types, func(i, j int) bool {
		return catalog.types[i].SymbolID < catalog.types[j].SymbolID
	})
	catalog.diagnostics = append(catalog.diagnostics, duplicatePropertyDiagnostics(catalog.types)...)
	sortDiagnostics(catalog.diagnostics)
	return catalog
}

func analyzeType(
	occurrence resolve.Occurrence,
	symbol load.Symbol,
	fileSet *token.FileSet,
	modules modulith.Model,
) (Type, []Diagnostic) {
	named, problem := configurationType(occurrence, symbol)
	if problem != nil {
		return Type{}, []Diagnostic{symbolDiagnostic(occurrence, symbol, problem.kind, problem.message)}
	}
	prefix, problem := configurationPrefix(occurrence, symbol)
	if problem != nil {
		return Type{}, []Diagnostic{symbolDiagnostic(occurrence, symbol, problem.kind, problem.message)}
	}
	module := ""
	if owner, ok := modules.Owner(symbol.PackagePath); ok {
		module = owner.ID
	}
	result := Type{
		Symbol:           symbol,
		SymbolID:         symbol.ID,
		Name:             symbol.Name,
		PackagePath:      symbol.PackagePath,
		Type:             named,
		TypeID:           provider.TypeID(named),
		Prefix:           prefix,
		Module:           module,
		Position:         occurrence.DisplayPosition,
		PhysicalPosition: physicalPosition(occurrence),
	}
	structure, ok := named.Underlying().(*types.Struct)
	if !ok {
		return Type{}, []Diagnostic{symbolDiagnostic(
			occurrence,
			symbol,
			"internal",
			fmt.Sprintf("@ConfigurationProperties %s lost its validated struct type", symbol.DisplayLabel),
		)}
	}
	var diagnostics []Diagnostic
	for index := 0; index < structure.NumFields(); index++ {
		field, diagnostic, included := analyzeField(
			result,
			structure.Field(index),
			structure.Tag(index),
			index,
			fileSet,
		)
		if diagnostic != nil {
			diagnostics = append(diagnostics, *diagnostic)
		}
		if included && diagnostic == nil {
			result.fields = append(result.fields, field)
		}
	}
	return result, diagnostics
}

type problem struct {
	kind    string
	message string
}

func configurationType(occurrence resolve.Occurrence, symbol load.Symbol) (*types.Named, *problem) {
	label := symbol.DisplayLabel
	if label == "" {
		label = symbol.ID
	}
	if occurrence.Target != annotation.TargetType || symbol.Kind != load.SymbolType {
		return nil, &problem{
			kind:    "invalid-target",
			message: fmt.Sprintf("@ConfigurationProperties %s must target a named struct type", label),
		}
	}
	if !token.IsExported(symbol.Name) {
		return nil, &problem{
			kind:    "unexported-type",
			message: fmt.Sprintf("@ConfigurationProperties %s must be exported so target-scoped generated code can construct it", label),
		}
	}
	typeName, ok := symbol.Object.(*types.TypeName)
	if !ok || typeName.IsAlias() {
		return nil, &problem{
			kind:    "invalid-type",
			message: fmt.Sprintf("@ConfigurationProperties %s must be a defined named struct; aliases are not supported", label),
		}
	}
	named, ok := typeName.Type().(*types.Named)
	if !ok {
		return nil, &problem{
			kind:    "invalid-type",
			message: fmt.Sprintf("@ConfigurationProperties %s must be a defined named struct", label),
		}
	}
	if named.TypeParams() != nil && named.TypeParams().Len() > 0 {
		return nil, &problem{
			kind:    "generic",
			message: fmt.Sprintf("@ConfigurationProperties %s must not declare type parameters", label),
		}
	}
	if _, ok := named.Underlying().(*types.Struct); !ok {
		return nil, &problem{
			kind:    "invalid-type",
			message: fmt.Sprintf("@ConfigurationProperties %s must have a struct underlying type", label),
		}
	}
	return named, nil
}

func configurationPrefix(occurrence resolve.Occurrence, symbol load.Symbol) (string, *problem) {
	if contribution, found := occurrence.DescriptorContribution(
		sdk.ContributionConfiguration,
	); found {
		return contribution.Configuration.Prefix, nil
	}
	if len(occurrence.Annotation.Arguments) == 0 {
		return "", nil
	}
	if len(occurrence.Annotation.Arguments) != 1 {
		return "", &problem{
			kind:    "arguments",
			message: fmt.Sprintf("@ConfigurationProperties %s accepts only the optional named string argument prefix", symbol.DisplayLabel),
		}
	}
	argument := occurrence.Annotation.Arguments[0]
	if argument.Name != "prefix" || argument.Value.Kind != annotation.KindString {
		return "", &problem{
			kind:    "arguments",
			message: fmt.Sprintf("@ConfigurationProperties %s accepts only the optional named string argument prefix", symbol.DisplayLabel),
		}
	}
	return argument.Value.String, nil
}

func analyzeField(
	owner Type,
	variable *types.Var,
	rawTag string,
	index int,
	fileSet *token.FileSet,
) (Field, *Diagnostic, bool) {
	position, physical := fieldPositions(variable, fileSet)
	tag, tagged := reflect.StructTag(rawTag).Lookup(fieldTagName)
	if !variable.Exported() {
		if tagged && tag != "-" {
			diagnostic := fieldDiagnostic(
				owner,
				variable.Name(),
				position,
				physical,
				"unexported",
				fmt.Sprintf("@ConfigurationProperties field %s.%s is unexported and cannot be generated", owner.Name, variable.Name()),
			)
			return Field{}, &diagnostic, false
		}
		return Field{}, nil, false
	}
	if variable.Embedded() {
		diagnostic := fieldDiagnostic(
			owner,
			variable.Name(),
			position,
			physical,
			"embedded",
			fmt.Sprintf("@ConfigurationProperties field %s.%s is embedded; flatten configuration explicitly", owner.Name, variable.Name()),
		)
		return Field{}, &diagnostic, false
	}
	if !tagged {
		diagnostic := fieldDiagnostic(
			owner,
			variable.Name(),
			position,
			physical,
			"missing-tag",
			fmt.Sprintf("@ConfigurationProperties field %s.%s requires a %s tag or %s:\"-\"", owner.Name, variable.Name(), fieldTagName, fieldTagName),
		)
		return Field{}, &diagnostic, false
	}
	if tag == "-" {
		return Field{}, nil, false
	}

	options, tagProblem := parseFieldTag(tag)
	if tagProblem != nil {
		diagnostic := fieldDiagnostic(owner, variable.Name(), position, physical, tagProblem.kind, tagProblem.message)
		return Field{}, &diagnostic, false
	}
	kind, supported := scalarKind(variable.Type())
	if !supported {
		diagnostic := fieldDiagnostic(
			owner,
			variable.Name(),
			position,
			physical,
			"unsupported-type",
			fmt.Sprintf(
				"@ConfigurationProperties field %s.%s has unsupported type %s; supported scalar types are string, bool, signed integers, and time.Duration",
				owner.Name,
				variable.Name(),
				provider.TypeID(variable.Type()),
			),
		)
		return Field{}, &diagnostic, false
	}
	if !accessibleType(variable.Type()) {
		diagnostic := fieldDiagnostic(
			owner,
			variable.Name(),
			position,
			physical,
			"unexported-type",
			fmt.Sprintf(
				"@ConfigurationProperties field %s.%s uses unexported type %s, which target-scoped generated code cannot name",
				owner.Name,
				variable.Name(),
				provider.TypeID(variable.Type()),
			),
		)
		return Field{}, &diagnostic, false
	}
	key := options.key
	if owner.Prefix != "" {
		key = owner.Prefix + "." + key
	}
	field := Field{
		Index:            index,
		Name:             variable.Name(),
		Type:             variable.Type(),
		TypeID:           provider.TypeID(variable.Type()),
		Key:              key,
		Kind:             kind,
		Module:           owner.Module,
		Environment:      options.environment,
		Default:          options.defaultValue,
		HasDefault:       options.hasDefault,
		Required:         options.required,
		Secret:           options.secret,
		Position:         position,
		PhysicalPosition: physical,
	}
	if err := validateField(field); err != nil {
		diagnostic := fieldDiagnostic(owner, variable.Name(), position, physical, "invalid-property", err.Error())
		return Field{}, &diagnostic, false
	}
	return field, nil, true
}

func accessibleType(value types.Type) bool {
	switch typed := value.(type) {
	case *types.Named:
		return typed.Obj() == nil || token.IsExported(typed.Obj().Name())
	case *types.Alias:
		return typed.Obj() == nil || token.IsExported(typed.Obj().Name())
	default:
		return true
	}
}

type fieldOptions struct {
	key          string
	environment  string
	defaultValue string
	hasDefault   bool
	required     bool
	secret       bool
}

func parseFieldTag(tag string) (fieldOptions, *problem) {
	parts := strings.Split(tag, ",")
	if len(parts) == 0 || parts[0] == "" {
		return fieldOptions{}, &problem{kind: "invalid-tag", message: `configuration tag requires a property key, for example spice:"server.port"`}
	}
	result := fieldOptions{key: parts[0]}
	seen := make(map[string]struct{}, len(parts)-1)
	for _, rawOption := range parts[1:] {
		if optionProblem := applyFieldOption(&result, seen, tag, rawOption); optionProblem != nil {
			return fieldOptions{}, optionProblem
		}
	}
	return result, nil
}

func applyFieldOption(result *fieldOptions, seen map[string]struct{}, tag, rawOption string) *problem {
	name, value, hasValue := strings.Cut(rawOption, "=")
	if name == "" {
		return &problem{kind: "invalid-tag", message: fmt.Sprintf("configuration tag %q contains an empty option", tag)}
	}
	if _, duplicate := seen[name]; duplicate {
		return &problem{kind: "invalid-tag", message: fmt.Sprintf("configuration tag %q repeats option %q", tag, name)}
	}
	seen[name] = struct{}{}
	switch name {
	case "required":
		if hasValue {
			return booleanOptionProblem(tag, name)
		}
		result.required = true
	case "secret":
		if hasValue {
			return booleanOptionProblem(tag, name)
		}
		result.secret = true
	case "default":
		if !hasValue {
			return valueOptionProblem(tag, name)
		}
		result.defaultValue = value
		result.hasDefault = true
	case "env":
		if !hasValue || value == "" {
			return valueOptionProblem(tag, name)
		}
		result.environment = value
	default:
		return &problem{kind: "invalid-tag", message: fmt.Sprintf("configuration tag %q contains unknown option %q", tag, name)}
	}
	return nil
}

func booleanOptionProblem(tag, name string) *problem {
	return &problem{
		kind:    "invalid-tag",
		message: fmt.Sprintf("configuration tag %q option %q does not accept a value", tag, name),
	}
}

func valueOptionProblem(tag, name string) *problem {
	return &problem{
		kind:    "invalid-tag",
		message: fmt.Sprintf("configuration tag %q option %q requires a value", tag, name),
	}
}

func scalarKind(value types.Type) (runtimeconfig.Kind, bool) {
	unalias := types.Unalias(value)
	if named, ok := unalias.(*types.Named); ok {
		object := named.Obj()
		if object != nil && object.Pkg() != nil &&
			object.Pkg().Path() == "time" && object.Name() == "Duration" {
			return runtimeconfig.KindDuration, true
		}
	}
	basic, ok := unalias.Underlying().(*types.Basic)
	if !ok {
		return "", false
	}
	kind := basic.Kind()
	if kind == types.String {
		return runtimeconfig.KindString, true
	}
	if kind == types.Bool {
		return runtimeconfig.KindBoolean, true
	}
	if kind == types.Int || kind == types.Int8 || kind == types.Int16 ||
		kind == types.Int32 || kind == types.Int64 {
		return runtimeconfig.KindInteger, true
	}
	return "", false
}

func validateField(field Field) error {
	property := runtimeconfig.Property{
		Key:         field.Key,
		Kind:        field.Kind,
		Module:      field.Module,
		Environment: field.Environment,
		Default:     field.Default,
		HasDefault:  field.HasDefault,
		Required:    field.Required,
		Secret:      field.Secret,
	}
	if _, err := runtimeconfig.NewSchema(property); err != nil {
		return fmt.Errorf("@ConfigurationProperties field %s is invalid: %w", field.Name, err)
	}
	if field.HasDefault && field.Kind == runtimeconfig.KindInteger {
		if err := validateIntegerDefault(field.Type, field.Default); err != nil {
			return fmt.Errorf("@ConfigurationProperties field %s default is invalid: %w", field.Name, err)
		}
	}
	return nil
}

func validateIntegerDefault(valueType types.Type, value string) error {
	basic, ok := types.Unalias(valueType).Underlying().(*types.Basic)
	if !ok {
		return nil
	}
	bitSize := 0
	if basic.Kind() == types.Int8 {
		bitSize = 8
	}
	if basic.Kind() == types.Int16 {
		bitSize = 16
	}
	if basic.Kind() == types.Int32 {
		bitSize = 32
	}
	if basic.Kind() == types.Int64 {
		bitSize = 64
	}
	if _, err := strconv.ParseInt(value, 10, bitSize); err != nil {
		return fmt.Errorf("value is outside the range of %s", provider.TypeID(valueType))
	}
	return nil
}

func duplicatePropertyDiagnostics(configTypes []Type) []Diagnostic {
	keys := make(map[string]Field)
	environments := make(map[string]Field)
	var diagnostics []Diagnostic
	for _, configType := range configTypes {
		for _, field := range configType.fields {
			if previous, duplicate := keys[field.Key]; duplicate {
				diagnostics = append(diagnostics, duplicateFieldDiagnostic(
					configType,
					field,
					"duplicate-key",
					fmt.Sprintf(
						"configuration property %q duplicates %s at %s",
						field.Key,
						previous.Name,
						renderPosition(previous.Position),
					),
				))
			} else {
				keys[field.Key] = field
			}
			if field.Environment == "" {
				continue
			}
			if previous, duplicate := environments[field.Environment]; duplicate {
				diagnostics = append(diagnostics, duplicateFieldDiagnostic(
					configType,
					field,
					"duplicate-environment",
					fmt.Sprintf(
						"configuration environment variable %q duplicates %s at %s",
						field.Environment,
						previous.Name,
						renderPosition(previous.Position),
					),
				))
			} else {
				environments[field.Environment] = field
			}
		}
	}
	return diagnostics
}

func duplicateFieldDiagnostic(owner Type, field Field, kind, message string) Diagnostic {
	return Diagnostic{
		Position:         field.Position,
		PhysicalPosition: field.PhysicalPosition,
		SymbolID:         owner.SymbolID,
		Field:            field.Name,
		Kind:             kind,
		Message:          message,
	}
}

func symbolIndex(symbols []load.Symbol) map[string]load.Symbol {
	result := make(map[string]load.Symbol, len(symbols))
	for _, symbol := range symbols {
		result[symbol.ID] = symbol
	}
	return result
}

func packageFileSets(program *load.Program) map[string]*token.FileSet {
	result := make(map[string]*token.FileSet)
	for _, pkg := range program.Packages() {
		if pkg.Raw != nil && pkg.Raw.Fset != nil {
			result[pkg.Path] = pkg.Raw.Fset
		}
	}
	return result
}

func fieldPositions(variable *types.Var, fileSet *token.FileSet) (token.Position, token.Position) {
	if fileSet == nil || variable == nil || !variable.Pos().IsValid() {
		return token.Position{}, token.Position{}
	}
	return fileSet.PositionFor(variable.Pos(), true), fileSet.PositionFor(variable.Pos(), false)
}

func occurrenceDiagnostic(occurrence resolve.Occurrence, kind, message string) Diagnostic {
	return Diagnostic{
		Position:         occurrence.DisplayPosition,
		PhysicalPosition: physicalPosition(occurrence),
		SymbolID:         occurrence.SymbolID,
		Kind:             kind,
		Message:          message,
	}
}

func symbolDiagnostic(occurrence resolve.Occurrence, symbol load.Symbol, kind, message string) Diagnostic {
	diagnostic := occurrenceDiagnostic(occurrence, kind, message)
	if diagnostic.Position.Filename == "" {
		diagnostic.Position = symbol.Position
	}
	if diagnostic.PhysicalPosition.Filename == "" {
		diagnostic.PhysicalPosition = symbol.PhysicalPosition
	}
	return diagnostic
}

func fieldDiagnostic(
	owner Type,
	field string,
	position token.Position,
	physical token.Position,
	kind string,
	message string,
) Diagnostic {
	if position.Filename == "" {
		position = owner.Position
	}
	if physical.Filename == "" {
		physical = owner.PhysicalPosition
	}
	return Diagnostic{
		Position:         position,
		PhysicalPosition: physical,
		SymbolID:         owner.SymbolID,
		Field:            field,
		Kind:             kind,
		Message:          message,
	}
}

func physicalPosition(occurrence resolve.Occurrence) token.Position {
	return token.Position{Filename: occurrence.PhysicalFile, Offset: occurrence.PhysicalOffset}
}

func renderPosition(position token.Position) string {
	if position.Filename == "" {
		return "<unknown>"
	}
	return position.String()
}

func sortDiagnostics(diagnostics []Diagnostic) {
	sort.SliceStable(diagnostics, func(i, j int) bool {
		left, right := diagnostics[i], diagnostics[j]
		if left.PhysicalPosition.Filename != right.PhysicalPosition.Filename {
			return left.PhysicalPosition.Filename < right.PhysicalPosition.Filename
		}
		if left.PhysicalPosition.Offset != right.PhysicalPosition.Offset {
			return left.PhysicalPosition.Offset < right.PhysicalPosition.Offset
		}
		if left.SymbolID != right.SymbolID {
			return left.SymbolID < right.SymbolID
		}
		if left.Field != right.Field {
			return left.Field < right.Field
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		return left.Message < right.Message
	})
}
