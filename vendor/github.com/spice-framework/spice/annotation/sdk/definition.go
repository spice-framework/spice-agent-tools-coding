// Package sdk defines the stable, statically decoded Spice annotation
// descriptor contract. Descriptor functions are ordinary Go declarations, but
// the Spice compiler reads their composite literals without executing them.
package sdk

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/spice-framework/spice/annotation"
)

// Target identifies an allowed declaration target.
type Target = annotation.Target

const (
	TargetPackage   = annotation.TargetPackage
	TargetType      = annotation.TargetType
	TargetFunction  = annotation.TargetFunction
	TargetMethod    = annotation.TargetMethod
	TargetParameter = annotation.TargetParameter
	TargetVariable  = annotation.TargetVariable
	TargetConstant  = annotation.TargetConstant
)

// Kind identifies an accepted annotation argument representation.
type Kind = annotation.Kind

const (
	KindString     = annotation.KindString
	KindInteger    = annotation.KindInteger
	KindBoolean    = annotation.KindBoolean
	KindIdentifier = annotation.KindIdentifier
	KindList       = annotation.KindList
)

// ValueDomain identifies an SDK-defined semantic value space. Domains let the
// shared Spice compiler provide type-aware validation, completion, navigation,
// and code actions without an editor or compiler switch on an annotation name.
type ValueDomain = annotation.ValueDomain

const (
	// ValueDomainNone uses only the argument's lexical Kinds metadata.
	ValueDomainNone = annotation.ValueDomainNone
	// ValueDomainGoInterface accepts named runtime Go interface type
	// expressions resolved from the consuming application's typed program.
	ValueDomainGoInterface = annotation.ValueDomainGoInterface
)

// ProtocolVersion identifies a compatible native plugin protocol.
type ProtocolVersion string

const (
	// ProtocolV1Alpha2 is the typed-handler stdio plugin contract.
	ProtocolV1Alpha2 ProtocolVersion = "spice.annotation/v1alpha2"
)

// Argument describes one supported annotation argument.
type Argument struct {
	Name             string
	Kinds            []Kind
	ListElementKinds []Kind
	ValueDomain      ValueDomain
	AllowedValues    []string
	Description      string
	Default          string
	Required         bool
	Positional       bool
	Variadic         bool
}

// Example is one documentation example shown by editors and generated GoDoc.
type Example struct {
	Title string
	Code  string
}

// Compatibility documents the public lifecycle of an annotation contract.
type Compatibility struct {
	Since        string
	MinimumSpice string
}

// Symbol identifies one real Go declaration.
type Symbol struct {
	Package string
	Name    string
}

// Implementation identifies the native tool handler behind a descriptor.
type Implementation struct {
	Tool     string
	Handler  Handler
	Protocol ProtocolVersion
}

// Definition is the complete public, inspectable annotation descriptor.
type Definition struct {
	Name           string
	Summary        string
	Targets        []Target
	Repeatable     bool
	Arguments      []Argument
	Examples       []Example
	Compatibility  Compatibility
	Implementation Implementation
}

// Validate fails closed on incomplete or ambiguous descriptor metadata.
func (definition Definition) Validate() error {
	if err := validateName(definition.Name); err != nil {
		return err
	}
	if strings.TrimSpace(definition.Summary) == "" {
		return fmt.Errorf("annotation definition %q requires a summary", definition.Name)
	}
	if len(definition.Targets) == 0 {
		return fmt.Errorf(
			"annotation definition %q requires at least one target",
			definition.Name,
		)
	}
	seenTargets := make(map[Target]struct{}, len(definition.Targets))
	for _, target := range definition.Targets {
		if !slices.Contains(knownTargets, target) {
			return fmt.Errorf(
				"annotation definition %q contains unknown target %q",
				definition.Name,
				target,
			)
		}
		if _, duplicate := seenTargets[target]; duplicate {
			return fmt.Errorf(
				"annotation definition %q contains duplicate target %q",
				definition.Name,
				target,
			)
		}
		seenTargets[target] = struct{}{}
	}
	if err := validateArguments(definition.Name, definition.Arguments); err != nil {
		return err
	}
	if strings.TrimSpace(definition.Compatibility.Since) == "" ||
		strings.TrimSpace(definition.Compatibility.MinimumSpice) == "" {
		return fmt.Errorf(
			"annotation definition %q requires since and minimum Spice compatibility versions",
			definition.Name,
		)
	}
	if len(definition.Examples) == 0 {
		return fmt.Errorf(
			"annotation definition %q requires at least one documented example",
			definition.Name,
		)
	}
	for index, example := range definition.Examples {
		if strings.TrimSpace(example.Title) == "" ||
			strings.TrimSpace(example.Code) == "" {
			return fmt.Errorf(
				"annotation definition %q example %d requires title and code",
				definition.Name,
				index,
			)
		}
	}
	return validateImplementation(definition.Name, definition.Implementation)
}

var knownTargets = []Target{
	TargetPackage,
	TargetType,
	TargetFunction,
	TargetMethod,
	TargetParameter,
	TargetVariable,
	TargetConstant,
}

var knownKinds = []Kind{
	KindString,
	KindInteger,
	KindBoolean,
	KindIdentifier,
	KindList,
}

var knownValueDomains = []ValueDomain{
	ValueDomainNone,
	ValueDomainGoInterface,
}

func validateName(name string) error {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(name) != name {
		return errors.New("annotation definition name must be non-empty without surrounding whitespace")
	}
	for segment := range strings.SplitSeq(name, ".") {
		if !validIdentifier(segment) {
			return fmt.Errorf("annotation definition name %q is not a qualified Go identifier", name)
		}
	}
	return nil
}

func validateArguments(definitionName string, arguments []Argument) error {
	seen := make(map[string]struct{}, len(arguments))
	positional := 0
	for _, argument := range arguments {
		if err := validateArgument(definitionName, argument, seen); err != nil {
			return err
		}
		if argument.Positional {
			positional++
		}
	}
	if positional > 1 {
		return fmt.Errorf(
			"annotation definition %q contains multiple positional arguments",
			definitionName,
		)
	}
	return nil
}

func validateArgument(
	definitionName string,
	argument Argument,
	seen map[string]struct{},
) error {
	if !validIdentifier(argument.Name) {
		return fmt.Errorf(
			"annotation definition %q contains invalid argument name %q",
			definitionName,
			argument.Name,
		)
	}
	if _, duplicate := seen[argument.Name]; duplicate {
		return fmt.Errorf(
			"annotation definition %q contains duplicate argument %q",
			definitionName,
			argument.Name,
		)
	}
	seen[argument.Name] = struct{}{}
	if len(argument.Kinds) == 0 {
		return fmt.Errorf(
			"annotation definition %q argument %q requires a value kind",
			definitionName,
			argument.Name,
		)
	}
	if err := validateKinds(definitionName, argument.Name, argument.Kinds); err != nil {
		return err
	}
	if !slices.Contains(knownValueDomains, argument.ValueDomain) {
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
	if len(argument.ListElementKinds) > 0 &&
		!slices.Contains(argument.Kinds, KindList) {
		return fmt.Errorf(
			"annotation definition %q argument %q defines list element kinds without accepting lists",
			definitionName,
			argument.Name,
		)
	}
	if err := validateKinds(
		definitionName,
		argument.Name,
		argument.ListElementKinds,
	); err != nil {
		return fmt.Errorf("list elements: %w", err)
	}
	if duplicate := duplicateString(argument.AllowedValues); duplicate != "" {
		return fmt.Errorf(
			"annotation definition %q argument %q repeats allowed value %q",
			definitionName,
			argument.Name,
			duplicate,
		)
	}
	if strings.TrimSpace(argument.Description) == "" {
		return fmt.Errorf(
			"annotation definition %q argument %q requires documentation",
			definitionName,
			argument.Name,
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

func validateKinds(
	definitionName string,
	argumentName string,
	kinds []Kind,
) error {
	for _, kind := range kinds {
		if !slices.Contains(knownKinds, kind) {
			return fmt.Errorf(
				"annotation definition %q argument %q contains unknown kind %q",
				definitionName,
				argumentName,
				kind,
			)
		}
	}
	if duplicate := duplicateKind(kinds); duplicate != "" {
		return fmt.Errorf(
			"annotation definition %q argument %q repeats kind %q",
			definitionName,
			argumentName,
			duplicate,
		)
	}
	return nil
}

func duplicateKind(values []Kind) Kind {
	seen := make(map[Kind]struct{}, len(values))
	for _, value := range values {
		if _, duplicate := seen[value]; duplicate {
			return value
		}
		seen[value] = struct{}{}
	}
	return ""
}

func duplicateString(values []string) string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, duplicate := seen[value]; duplicate {
			return value
		}
		seen[value] = struct{}{}
	}
	return ""
}

func validateImplementation(
	definitionName string,
	implementation Implementation,
) error {
	if !validImportPath(implementation.Tool) {
		return fmt.Errorf(
			"annotation definition %q contains invalid tool package %q",
			definitionName,
			implementation.Tool,
		)
	}
	if implementation.Handler == nil {
		return fmt.Errorf(
			"annotation definition %q requires a typed handler",
			definitionName,
		)
	}
	if implementation.Protocol != ProtocolV1Alpha2 {
		return fmt.Errorf(
			"annotation definition %q uses unsupported protocol %q",
			definitionName,
			implementation.Protocol,
		)
	}
	return nil
}

// Handler is the exact executable contract implemented by one annotation.
// Descriptor source stores a package-level function of this type. The compiler
// validates and identifies that symbol statically; it never calls the function.
type Handler func(context.Context, Invocation) (Result, error)

func validIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for index, character := range value {
		if character == '_' ||
			character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}

func validImportPath(value string) bool {
	return value != "" &&
		value == strings.TrimSpace(value) &&
		!strings.Contains(value, "\\") &&
		!strings.HasPrefix(value, ".") &&
		path.Clean(value) == value &&
		strings.Contains(value, "/")
}
