package annotation

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Target identifies a Go declaration kind that an annotation may describe.
type Target string

const (
	TargetPackage   Target = "package"
	TargetType      Target = "type"
	TargetFunction  Target = "function"
	TargetMethod    Target = "method"
	TargetParameter Target = "parameter"
	TargetVariable  Target = "variable"
	TargetConstant  Target = "constant"
)

var orderedTargets = []Target{
	TargetPackage,
	TargetType,
	TargetFunction,
	TargetMethod,
	TargetParameter,
	TargetVariable,
	TargetConstant,
}

// TargetSet is an immutable bit set of supported annotation targets.
type TargetSet uint8

const (
	targetPackageMask TargetSet = 1 << iota
	targetTypeMask
	targetFunctionMask
	targetMethodMask
	targetParameterMask
	targetVariableMask
	targetConstantMask
)

// NewTargetSet creates a target set from declaration kinds.
func NewTargetSet(values ...Target) (TargetSet, error) {
	var result TargetSet
	for _, value := range values {
		mask := maskForTarget(value)
		if mask == 0 {
			return 0, fmt.Errorf("unknown annotation target %q", value)
		}
		result |= mask
	}
	return result, nil
}

// Targets creates a target set or panics when a package-owned definition uses
// an unknown target. Runtime and user-supplied metadata should use NewTargetSet
// when it needs error-returning validation.
func Targets(values ...Target) TargetSet {
	result, err := NewTargetSet(values...)
	if err != nil {
		panic(err)
	}
	return result
}

// Contains reports whether the set permits target.
func (set TargetSet) Contains(target Target) bool {
	mask := maskForTarget(target)
	return mask != 0 && set&mask != 0
}

// Values returns permitted targets in a stable, human-oriented order.
func (set TargetSet) Values() []Target {
	result := make([]Target, 0, len(orderedTargets))
	for _, target := range orderedTargets {
		if set.Contains(target) {
			result = append(result, target)
		}
	}
	return result
}

// String returns a deterministic comma-separated target list.
func (set TargetSet) String() string {
	values := set.Values()
	parts := make([]string, len(values))
	for index, value := range values {
		parts[index] = string(value)
	}
	return strings.Join(parts, ", ")
}

func maskForTarget(target Target) TargetSet {
	switch target {
	case TargetPackage:
		return targetPackageMask
	case TargetType:
		return targetTypeMask
	case TargetFunction:
		return targetFunctionMask
	case TargetMethod:
		return targetMethodMask
	case TargetParameter:
		return targetParameterMask
	case TargetVariable:
		return targetVariableMask
	case TargetConstant:
		return targetConstantMask
	default:
		return 0
	}
}

// ArgumentDefinition describes one supported annotation argument.
type ArgumentDefinition struct {
	Name             string
	Kinds            []Kind
	ListElementKinds []Kind
	ValueDomain      ValueDomain
	Required         bool
	Positional       bool
	Variadic         bool
}

// Definition describes one annotation and where it may be used.
type Definition struct {
	Name       string
	Targets    TargetSet
	Repeatable bool
	Arguments  []ArgumentDefinition
}

// Registry is an immutable-by-construction annotation definition lookup.
type Registry struct {
	definitions map[string]Definition
	names       []string
}

// NewRegistry validates definitions and creates a deterministic registry.
func NewRegistry(definitions ...Definition) (Registry, error) {
	registry := Registry{
		definitions: make(map[string]Definition, len(definitions)),
		names:       make([]string, 0, len(definitions)),
	}

	for _, definition := range definitions {
		if err := validateDefinition(definition); err != nil {
			return Registry{}, err
		}
		if _, exists := registry.definitions[definition.Name]; exists {
			return Registry{}, fmt.Errorf("duplicate annotation definition %q", definition.Name)
		}
		registry.definitions[definition.Name] = cloneDefinition(definition)
		registry.names = append(registry.names, definition.Name)
	}

	sort.Strings(registry.names)
	return registry, nil
}

// MustRegistry creates a registry or panics.
func MustRegistry(definitions ...Definition) Registry {
	registry, err := NewRegistry(definitions...)
	if err != nil {
		panic(err)
	}
	return registry
}

// Lookup returns a defensive copy of the named definition.
func (registry Registry) Lookup(name string) (Definition, bool) {
	definition, ok := registry.definitions[name]
	if !ok {
		return Definition{}, false
	}
	return cloneDefinition(definition), true
}

// Definitions returns every definition sorted by name.
func (registry Registry) Definitions() []Definition {
	result := make([]Definition, 0, len(registry.names))
	for _, name := range registry.names {
		if definition, ok := registry.Lookup(name); ok {
			result = append(result, definition)
		}
	}
	return result
}

func validateDefinition(definition Definition) error {
	if strings.TrimSpace(definition.Name) == "" {
		return fmt.Errorf("annotation definition name is required")
	}
	if strings.TrimSpace(definition.Name) != definition.Name {
		return fmt.Errorf("annotation definition %q must not contain surrounding whitespace", definition.Name)
	}
	if definition.Targets == 0 {
		return fmt.Errorf("annotation definition %q requires at least one target", definition.Name)
	}
	if unknownBits := definition.Targets & ^Targets(orderedTargets...); unknownBits != 0 {
		return fmt.Errorf("annotation definition %q contains unknown target bits", definition.Name)
	}

	seenArguments := make(map[string]struct{}, len(definition.Arguments))
	positionalArguments := 0
	for _, argument := range definition.Arguments {
		if err := validateArgumentDefinition(
			definition.Name,
			argument,
			seenArguments,
		); err != nil {
			return err
		}
		if argument.Positional {
			positionalArguments++
		}
	}
	if positionalArguments > 1 {
		return fmt.Errorf("annotation definition %q contains multiple positional arguments; only one is supported", definition.Name)
	}
	return nil
}

func validateArgumentDefinition(
	definitionName string,
	argument ArgumentDefinition,
	seen map[string]struct{},
) error {
	if strings.TrimSpace(argument.Name) == "" {
		return fmt.Errorf(
			"annotation definition %q contains an argument without a name",
			definitionName,
		)
	}
	if strings.TrimSpace(argument.Name) != argument.Name {
		return fmt.Errorf(
			"annotation definition %q argument %q must not contain surrounding whitespace",
			definitionName,
			argument.Name,
		)
	}
	if _, exists := seen[argument.Name]; exists {
		return fmt.Errorf(
			"annotation definition %q contains duplicate argument %q",
			definitionName,
			argument.Name,
		)
	}
	seen[argument.Name] = struct{}{}
	if len(argument.Kinds) == 0 {
		return fmt.Errorf(
			"annotation definition %q argument %q requires at least one value kind",
			definitionName,
			argument.Name,
		)
	}
	if len(argument.ListElementKinds) != 0 &&
		!slices.Contains(argument.Kinds, KindList) {
		return fmt.Errorf(
			"annotation definition %q argument %q defines list element kinds without accepting list values",
			definitionName,
			argument.Name,
		)
	}
	if argument.ValueDomain != ValueDomainNone &&
		argument.ValueDomain != ValueDomainGoInterface {
		return fmt.Errorf(
			"annotation definition %q argument %q contains unknown value domain %q",
			definitionName,
			argument.Name,
			argument.ValueDomain,
		)
	}
	if argument.ValueDomain == ValueDomainGoInterface &&
		!slices.Contains(argument.Kinds, KindIdentifier) {
		return fmt.Errorf(
			"annotation definition %q argument %q uses value domain %q without accepting identifier values",
			definitionName,
			argument.Name,
			argument.ValueDomain,
		)
	}
	if argument.Variadic && !argument.Positional {
		return fmt.Errorf(
			"annotation definition %q argument %q must be positional when variadic",
			definitionName,
			argument.Name,
		)
	}
	return nil
}

func cloneDefinition(definition Definition) Definition {
	result := definition
	result.Arguments = make([]ArgumentDefinition, len(definition.Arguments))
	for index, argument := range definition.Arguments {
		result.Arguments[index] = argument
		result.Arguments[index].Kinds = append([]Kind(nil), argument.Kinds...)
		result.Arguments[index].ListElementKinds = append(
			[]Kind(nil),
			argument.ListElementKinds...,
		)
	}
	return result
}
