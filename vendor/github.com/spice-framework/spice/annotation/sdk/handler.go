package sdk

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// Declaration contains normalized, non-executable facts about an annotation
// target. Type identities are import-path-qualified strings.
type Declaration struct {
	Target          Target `json:"target"`
	SymbolID        string `json:"symbol_id"`
	Name            string `json:"name"`
	PackagePath     string `json:"package_path"`
	TypeID          string `json:"type_id,omitempty"`
	ParameterIndex  int    `json:"parameter_index,omitempty"`
	ParameterName   string `json:"parameter_name,omitempty"`
	ParameterTypeID string `json:"parameter_type_id,omitempty"`
}

// Invocation is one normalized explicit descriptor invocation.
type Invocation struct {
	DescriptorPackage string               `json:"descriptor_package"`
	DescriptorSymbol  string               `json:"descriptor_symbol"`
	CanonicalName     string               `json:"canonical_name"`
	Arguments         []InvocationArgument `json:"arguments,omitempty"`
	Declaration       Declaration          `json:"declaration"`
	Facts             map[string]string    `json:"facts,omitempty"`
}

// InvocationArgument retains parsed spelling and a normalized JSON value.
type InvocationArgument struct {
	Name       string          `json:"name,omitempty"`
	Kind       Kind            `json:"kind"`
	Positional bool            `json:"positional,omitempty"`
	Value      json.RawMessage `json:"value"`
}

// HandlerDiagnostic is one annotation-handler-owned source diagnostic.
type HandlerDiagnostic struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// Result is the typed, transport-independent output of one Handler call.
type Result struct {
	Contributions []Contribution
	Diagnostics   []HandlerDiagnostic
}

// Contributions validates and returns one deterministic handler result.
func Contributions(values ...Contribution) (Result, error) {
	seen := make(map[ContributionKind]struct{}, len(values))
	result := make([]Contribution, len(values))
	for index, value := range values {
		if err := value.Validate(); err != nil {
			return Result{}, fmt.Errorf(
				"annotation contribution %d: %w",
				index,
				err,
			)
		}
		if _, duplicate := seen[value.Kind]; duplicate {
			return Result{}, fmt.Errorf(
				"annotation handler returned duplicate %q contributions",
				value.Kind,
			)
		}
		seen[value.Kind] = struct{}{}
		result[index] = value.Clone()
	}
	return Result{Contributions: result}, nil
}

// OneContribution validates and returns a result containing one contribution.
func OneContribution(value Contribution) (Result, error) {
	return Contributions(value)
}

// RequireDescriptor rejects dispatch to a handler for another descriptor.
func (invocation Invocation) RequireDescriptor(
	packagePath string,
	symbol string,
) error {
	if invocation.DescriptorPackage == packagePath &&
		invocation.DescriptorSymbol == symbol {
		return nil
	}
	return fmt.Errorf(
		"annotation handler for %s.%s received descriptor %s.%s",
		packagePath,
		symbol,
		invocation.DescriptorPackage,
		invocation.DescriptorSymbol,
	)
}

// BoundArguments is a validated name-to-value annotation argument map.
type BoundArguments map[string]InvocationArgument

// BindArguments rejects unsupported, duplicate, and malformed argument names.
func BindArguments(
	invocation Invocation,
	positional string,
	allowed ...string,
) (BoundArguments, error) {
	result := make(BoundArguments, len(invocation.Arguments))
	for _, argument := range invocation.Arguments {
		name := argument.Name
		if argument.Positional {
			if name != "" || positional == "" {
				return nil, fmt.Errorf(
					"annotation %s contains an unsupported positional argument",
					invocation.CanonicalName,
				)
			}
			name = positional
		} else if name == "" {
			return nil, fmt.Errorf(
				"annotation %s contains an unnamed non-positional argument",
				invocation.CanonicalName,
			)
		}
		if !slices.Contains(allowed, name) {
			return nil, fmt.Errorf(
				"annotation %s contains unsupported argument %q",
				invocation.CanonicalName,
				name,
			)
		}
		if _, duplicate := result[name]; duplicate {
			return nil, fmt.Errorf(
				"annotation %s repeats argument %q",
				invocation.CanonicalName,
				name,
			)
		}
		result[name] = argument
	}
	return result, nil
}

// PositionalIdentifiers validates and decodes an invocation made exclusively
// from one or more positional Go identifier expressions.
func PositionalIdentifiers(
	invocation Invocation,
) ([]string, error) {
	if len(invocation.Arguments) == 0 {
		return nil, fmt.Errorf(
			"annotation %s requires at least one positional identifier",
			invocation.CanonicalName,
		)
	}
	result := make([]string, len(invocation.Arguments))
	seen := make(map[string]struct{}, len(invocation.Arguments))
	for index, argument := range invocation.Arguments {
		if !argument.Positional || argument.Name != "" {
			return nil, fmt.Errorf(
				"annotation %s accepts only positional identifiers",
				invocation.CanonicalName,
			)
		}
		value, err := (BoundArguments{
			"value": argument,
		}).Identifier("value", true)
		if err != nil {
			return nil, fmt.Errorf(
				"annotation %s argument %d: %w",
				invocation.CanonicalName,
				index,
				err,
			)
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, fmt.Errorf(
				"annotation %s repeats identifier %q",
				invocation.CanonicalName,
				value,
			)
		}
		seen[value] = struct{}{}
		result[index] = value
	}
	return result, nil
}

// String returns a decoded string argument.
func (arguments BoundArguments) String(
	name string,
	required bool,
) (string, error) {
	argument, found := arguments[name]
	if !found {
		if required {
			return "", fmt.Errorf(
				"annotation argument %q is required",
				name,
			)
		}
		return "", nil
	}
	if argument.Kind != KindString {
		return "", fmt.Errorf(
			"annotation argument %q must be a string",
			name,
		)
	}
	var result string
	if err := decodeArgumentValue(name, argument.Value, &result); err != nil {
		return "", err
	}
	if required && strings.TrimSpace(result) == "" {
		return "", fmt.Errorf(
			"annotation argument %q must not be empty",
			name,
		)
	}
	return result, nil
}

// Strings returns a decoded string-list argument.
func (arguments BoundArguments) Strings(name string) ([]string, error) {
	argument, found := arguments[name]
	if !found {
		return nil, nil
	}
	if argument.Kind != KindList {
		return nil, fmt.Errorf(
			"annotation argument %q must be a list",
			name,
		)
	}
	var result []string
	if err := decodeArgumentValue(name, argument.Value, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// Boolean returns a decoded Boolean argument.
func (arguments BoundArguments) Boolean(name string) (bool, error) {
	argument, found := arguments[name]
	if !found {
		return false, nil
	}
	if argument.Kind != KindBoolean {
		return false, fmt.Errorf(
			"annotation argument %q must be boolean",
			name,
		)
	}
	var result bool
	if err := decodeArgumentValue(name, argument.Value, &result); err != nil {
		return false, err
	}
	return result, nil
}

// Integer returns a decoded integer argument.
func (arguments BoundArguments) Integer(name string) (int64, error) {
	argument, found := arguments[name]
	if !found {
		return 0, nil
	}
	if argument.Kind != KindInteger {
		return 0, fmt.Errorf(
			"annotation argument %q must be an integer",
			name,
		)
	}
	var result int64
	if err := decodeArgumentValue(name, argument.Value, &result); err != nil {
		return 0, err
	}
	return result, nil
}

// BeanIdentity decodes the conventional optional name and aliases arguments
// used by constructible bean descriptors. An explicitly empty name is
// rejected; aliases are validated by the contribution boundary.
func BeanIdentity(
	arguments BoundArguments,
) (string, []string, error) {
	name, err := arguments.String("name", false)
	if err != nil {
		return "", nil, err
	}
	if _, present := arguments["name"]; present &&
		strings.TrimSpace(name) == "" {
		return "", nil, errors.New(
			"annotation argument \"name\" must not be empty",
		)
	}
	aliases, err := arguments.Strings("aliases")
	if err != nil {
		return "", nil, err
	}
	return name, aliases, nil
}

// Identifier returns one decoded Go identifier expression argument.
func (arguments BoundArguments) Identifier(
	name string,
	required bool,
) (string, error) {
	argument, found := arguments[name]
	if !found {
		if required {
			return "", fmt.Errorf(
				"annotation argument %q is required",
				name,
			)
		}
		return "", nil
	}
	if argument.Kind != KindIdentifier {
		return "", fmt.Errorf(
			"annotation argument %q must be an identifier",
			name,
		)
	}
	var result string
	if err := decodeArgumentValue(name, argument.Value, &result); err != nil {
		return "", err
	}
	if strings.TrimSpace(result) == "" ||
		strings.TrimSpace(result) != result {
		return "", fmt.Errorf(
			"annotation argument %q must be a trimmed identifier",
			name,
		)
	}
	return result, nil
}

func decodeArgumentValue(
	name string,
	content json.RawMessage,
	destination any,
) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf(
			"decode annotation argument %q: %w",
			name,
			err,
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return fmt.Errorf(
			"decode annotation argument %q: %w",
			name,
			err,
		)
	}
	return nil
}
