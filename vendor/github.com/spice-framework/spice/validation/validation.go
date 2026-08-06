// Package validation provides immutable, source-safe application validation
// errors for generated form and controller boundaries.
package validation

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const (
	maxViolations   = 128
	maxFieldBytes   = 256
	maxCodeBytes    = 128
	maxMessageBytes = 1024
)

var tokenPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]*$`)

// Violation is one safe field or object validation failure. It intentionally
// carries no rejected value.
type Violation struct {
	Field   string
	Code    string
	Message string
}

// Errors is an immutable ordered validation result.
type Errors struct {
	violations []Violation
}

// New validates and defensively copies violations in caller order.
func New(violations ...Violation) (Errors, error) {
	if len(violations) > maxViolations {
		return Errors{}, fmt.Errorf(
			"construct validation errors: at most %d violations are allowed",
			maxViolations,
		)
	}
	result := make([]Violation, len(violations))
	for index, violation := range violations {
		if err := validateViolation(violation); err != nil {
			return Errors{}, fmt.Errorf(
				"construct validation errors: violation %d: %w",
				index,
				err,
			)
		}
		result[index] = violation
	}
	return Errors{violations: result}, nil
}

// Field constructs one field violation.
func Field(field, code, message string) (Violation, error) {
	violation := Violation{Field: field, Code: code, Message: message}
	if err := validateViolation(violation); err != nil {
		return Violation{}, err
	}
	return violation, nil
}

// Object constructs one object-level violation.
func Object(code, message string) (Violation, error) {
	return Field("", code, message)
}

// Valid reports whether there are no violations.
func (validation Errors) Valid() bool {
	return len(validation.violations) == 0
}

// Len returns the number of violations.
func (validation Errors) Len() int {
	return len(validation.violations)
}

// All returns a defensive copy in stable insertion order.
func (validation Errors) All() []Violation {
	return slices.Clone(validation.violations)
}

// ForField returns violations for one exact field in insertion order.
func (validation Errors) ForField(field string) []Violation {
	result := make([]Violation, 0)
	for _, violation := range validation.violations {
		if violation.Field == field {
			result = append(result, violation)
		}
	}
	return result
}

// Add returns a new result containing the existing and additional violations.
func (validation Errors) Add(
	violations ...Violation,
) (Errors, error) {
	if len(violations) == 0 {
		return Errors{violations: validation.All()}, nil
	}
	combined := append(validation.All(), violations...)
	return New(combined...)
}

// Join returns a new result containing each input in argument order.
func Join(values ...Errors) (Errors, error) {
	total := 0
	for _, value := range values {
		total += value.Len()
		if total > maxViolations {
			return Errors{}, fmt.Errorf(
				"join validation errors: at most %d violations are allowed",
				maxViolations,
			)
		}
	}
	combined := make([]Violation, 0, total)
	for _, value := range values {
		combined = append(combined, value.violations...)
	}
	return New(combined...)
}

func validateViolation(violation Violation) error {
	if violation.Field != "" {
		if len(violation.Field) > maxFieldBytes ||
			!tokenPattern.MatchString(violation.Field) {
			return errors.New(
				"field must be a bounded dot-separated identifier",
			)
		}
	}
	if len(violation.Code) == 0 ||
		len(violation.Code) > maxCodeBytes ||
		!tokenPattern.MatchString(violation.Code) {
		return errors.New("code must be a bounded identifier")
	}
	if len(violation.Message) == 0 ||
		len(violation.Message) > maxMessageBytes ||
		strings.TrimSpace(violation.Message) != violation.Message {
		return errors.New(
			"message must be non-empty, bounded, and trimmed",
		)
	}
	return nil
}
