package sdk

import (
	"errors"
	"fmt"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// FunctionResultFactNamespace reserves invocation facts that describe the
	// ordered results of one ordinary Go function declaration.
	FunctionResultFactNamespace = "go.function.results."
	// FunctionResultCountFact is the required result count when the function
	// result fact set is present.
	FunctionResultCountFact = FunctionResultFactNamespace + "count"
	// MaximumFunctionResultFacts bounds one function signature transported to
	// an annotation handler.
	MaximumFunctionResultFacts = 64
	// MaximumFunctionResultTypeIDBytes bounds each readable type identity.
	MaximumFunctionResultTypeIDBytes  = 32 << 10
	maximumFunctionResultPackageBytes = 2048
	maximumFunctionResultNameBytes    = 512
)

const (
	functionResultTypeIDField        = "type_id"
	functionResultCanonicalTypeField = "canonical_type_id"
	functionResultKindField          = "kind"
	functionResultOriginPackageField = "named_origin.package"
	functionResultOriginNameField    = "named_origin.name"
	functionResultRequiredFieldCount = 3
	maximumFunctionResultFields      = 5
)

// GoTypeKind classifies the effective Go type after aliases are removed and a
// named type is reduced to its underlying kind. It is intentionally
// independent of any annotation or framework.
type GoTypeKind string

const (
	GoTypeArray         GoTypeKind = "array"
	GoTypeBasic         GoTypeKind = "basic"
	GoTypeChannel       GoTypeKind = "channel"
	GoTypeInterface     GoTypeKind = "interface"
	GoTypeMap           GoTypeKind = "map"
	GoTypePointer       GoTypeKind = "pointer"
	GoTypeSignature     GoTypeKind = "signature"
	GoTypeSlice         GoTypeKind = "slice"
	GoTypeStruct        GoTypeKind = "struct"
	GoTypeTuple         GoTypeKind = "tuple"
	GoTypeTypeParameter GoTypeKind = "type-parameter"
	GoTypeUnion         GoTypeKind = "union"
)

// FunctionResultFact is one ordered, non-executable function result fact.
// TypeID preserves the readable source identity, including a declared alias.
// CanonicalTypeID is the same type after top-level alias removal. Kind
// classifies its effective underlying type. NamedOriginPackage and
// NamedOriginName identify the declaration that owns an unaliased named type,
// including the origin of an instantiation.
type FunctionResultFact struct {
	TypeID             string
	CanonicalTypeID    string
	Kind               GoTypeKind
	NamedOriginPackage string
	NamedOriginName    string
}

// Validate rejects malformed or unbounded function result metadata.
func (fact FunctionResultFact) Validate() error {
	if err := validateFunctionResultTypeID("type ID", fact.TypeID); err != nil {
		return err
	}
	if err := validateFunctionResultTypeID(
		"canonical type ID",
		fact.CanonicalTypeID,
	); err != nil {
		return err
	}
	if !validGoTypeKind(fact.Kind) {
		return fmt.Errorf("function result Go type kind %q is unsupported", fact.Kind)
	}
	if err := validateFunctionResultOrigin(fact); err != nil {
		return err
	}
	return nil
}

// EncodeFunctionResultFacts returns a new deterministic fact set for the
// complete ordered result list. A zero-result function is represented by an
// explicit count of zero.
func EncodeFunctionResultFacts(
	results []FunctionResultFact,
) (map[string]string, error) {
	if len(results) > MaximumFunctionResultFacts {
		return nil, fmt.Errorf(
			"function result count %d exceeds %d",
			len(results),
			MaximumFunctionResultFacts,
		)
	}
	facts := make(
		map[string]string,
		1+len(results)*maximumFunctionResultFields,
	)
	facts[FunctionResultCountFact] = strconv.Itoa(len(results))
	for index, result := range results {
		if err := result.Validate(); err != nil {
			return nil, fmt.Errorf("function result %d: %w", index, err)
		}
		prefix := functionResultPrefix(index)
		facts[prefix+functionResultTypeIDField] = result.TypeID
		facts[prefix+functionResultCanonicalTypeField] = result.CanonicalTypeID
		facts[prefix+functionResultKindField] = string(result.Kind)
		if result.NamedOriginPackage != "" {
			facts[prefix+functionResultOriginPackageField] = result.NamedOriginPackage
		}
		if result.NamedOriginName != "" {
			facts[prefix+functionResultOriginNameField] = result.NamedOriginName
		}
	}
	return facts, nil
}

// DecodeFunctionResultFacts validates and decodes the reserved result fact
// namespace. The Boolean is false for a v1alpha2 invocation produced before
// these optional map entries existed. Unrelated invocation facts are ignored.
func DecodeFunctionResultFacts(
	facts map[string]string,
) ([]FunctionResultFact, bool, error) {
	countText, present := facts[FunctionResultCountFact]
	hasNamespace := false
	reservedCount := 0
	for key := range facts {
		if strings.HasPrefix(key, FunctionResultFactNamespace) {
			hasNamespace = true
			reservedCount++
		}
	}
	if !hasNamespace {
		return nil, false, nil
	}
	if !present {
		return nil, false, errors.New(
			"function result facts require a count",
		)
	}
	count, err := parseFunctionResultCount(countText)
	if err != nil {
		return nil, false, err
	}
	if reservedCount > 1+count*maximumFunctionResultFields {
		return nil, false, errors.New(
			"function result facts contain too many reserved entries",
		)
	}
	results := make([]FunctionResultFact, count)
	fields := make([]int, count)
	for key, value := range facts {
		if !strings.HasPrefix(key, FunctionResultFactNamespace) ||
			key == FunctionResultCountFact {
			continue
		}
		index, field, parseErr := parseFunctionResultKey(key, count)
		if parseErr != nil {
			return nil, false, parseErr
		}
		if applyErr := applyFunctionResultFact(
			key,
			field,
			value,
			&results[index],
			&fields[index],
		); applyErr != nil {
			return nil, false, applyErr
		}
	}
	for index, result := range results {
		if fields[index] != functionResultRequiredFieldCount {
			return nil, false, fmt.Errorf(
				"function result %d requires type ID, canonical type ID, and kind facts",
				index,
			)
		}
		if err := result.Validate(); err != nil {
			return nil, false, fmt.Errorf("function result %d: %w", index, err)
		}
	}
	return results, true, nil
}

// FunctionResultFacts decodes the generic result metadata attached to this
// invocation. See DecodeFunctionResultFacts for absence behavior.
func (invocation Invocation) FunctionResultFacts() (
	[]FunctionResultFact,
	bool,
	error,
) {
	return DecodeFunctionResultFacts(invocation.Facts)
}

func functionResultPrefix(index int) string {
	return FunctionResultFactNamespace + strconv.Itoa(index) + "."
}

func parseFunctionResultCount(value string) (int, error) {
	count, err := strconv.Atoi(value)
	if err != nil || count < 0 || strconv.Itoa(count) != value {
		return 0, fmt.Errorf(
			"function result count %q must be a canonical non-negative integer",
			value,
		)
	}
	if count > MaximumFunctionResultFacts {
		return 0, fmt.Errorf(
			"function result count %d exceeds %d",
			count,
			MaximumFunctionResultFacts,
		)
	}
	return count, nil
}

func parseFunctionResultKey(key string, count int) (int, string, error) {
	remainder := strings.TrimPrefix(key, FunctionResultFactNamespace)
	indexText, field, found := strings.Cut(remainder, ".")
	index, err := strconv.Atoi(indexText)
	if !found || err != nil || index < 0 || strconv.Itoa(index) != indexText {
		return 0, "", fmt.Errorf("function result fact %q has an invalid index", key)
	}
	if index >= count {
		return 0, "", fmt.Errorf(
			"function result fact %q exceeds result count %d",
			key,
			count,
		)
	}
	knownFields := []string{
		functionResultTypeIDField,
		functionResultCanonicalTypeField,
		functionResultKindField,
		functionResultOriginPackageField,
		functionResultOriginNameField,
	}
	if slices.Contains(knownFields, field) {
		return index, field, nil
	}
	return 0, "", fmt.Errorf("function result fact %q is unknown", key)
}

func applyFunctionResultFact(
	key string,
	field string,
	value string,
	result *FunctionResultFact,
	requiredFields *int,
) error {
	if len(value) > MaximumFunctionResultTypeIDBytes {
		return fmt.Errorf(
			"function result fact %q exceeds %d bytes",
			key,
			MaximumFunctionResultTypeIDBytes,
		)
	}
	switch field {
	case functionResultTypeIDField:
		result.TypeID = value
		*requiredFields++
	case functionResultCanonicalTypeField:
		result.CanonicalTypeID = value
		*requiredFields++
	case functionResultKindField:
		result.Kind = GoTypeKind(value)
		*requiredFields++
	case functionResultOriginPackageField:
		result.NamedOriginPackage = value
	case functionResultOriginNameField:
		result.NamedOriginName = value
	default:
		return fmt.Errorf("function result fact %q is unknown", key)
	}
	return nil
}

func validateFunctionResultTypeID(label, value string) error {
	if value == "" || value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return fmt.Errorf("function result %s must be non-empty, trimmed UTF-8", label)
	}
	if len(value) > MaximumFunctionResultTypeIDBytes {
		return fmt.Errorf(
			"function result %s exceeds %d bytes",
			label,
			MaximumFunctionResultTypeIDBytes,
		)
	}
	if strings.ContainsRune(value, 0) {
		return fmt.Errorf("function result %s contains a NUL byte", label)
	}
	return nil
}

func validateFunctionResultOrigin(fact FunctionResultFact) error {
	if fact.NamedOriginPackage != strings.TrimSpace(fact.NamedOriginPackage) ||
		!utf8.ValidString(fact.NamedOriginPackage) ||
		strings.ContainsRune(fact.NamedOriginPackage, 0) ||
		len(fact.NamedOriginPackage) > maximumFunctionResultPackageBytes {
		return errors.New("function result named-origin package is invalid")
	}
	if fact.NamedOriginName != "" &&
		(!token.IsIdentifier(fact.NamedOriginName) ||
			len(fact.NamedOriginName) > maximumFunctionResultNameBytes) {
		return errors.New("function result named-origin name is invalid")
	}
	if fact.NamedOriginPackage != "" && fact.NamedOriginName == "" {
		return errors.New("function result named-origin package requires a name")
	}
	return nil
}

func validGoTypeKind(kind GoTypeKind) bool {
	switch kind {
	case GoTypeArray, GoTypeBasic, GoTypeChannel, GoTypeInterface, GoTypeMap,
		GoTypePointer, GoTypeSignature, GoTypeSlice, GoTypeStruct,
		GoTypeTuple, GoTypeTypeParameter, GoTypeUnion:
		return true
	default:
		return false
	}
}
