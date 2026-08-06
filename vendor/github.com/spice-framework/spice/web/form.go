package web

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/spice-framework/spice/validation"
)

const formMediaType = "application/x-www-form-urlencoded"

// BindingResult is an immutable form binding and validation result. It never
// stores rejected raw values.
type BindingResult struct {
	errors validation.Errors
}

// NewBindingResult constructs a validated result.
func NewBindingResult(
	violations ...validation.Violation,
) (BindingResult, error) {
	result, err := validation.New(violations...)
	if err != nil {
		return BindingResult{}, fmt.Errorf(
			"construct binding result: %w",
			err,
		)
	}
	return BindingResult{errors: result}, nil
}

// Valid reports whether binding and validation succeeded.
func (result BindingResult) Valid() bool {
	return result.errors.Valid()
}

// Errors returns an immutable copy of the result errors.
func (result BindingResult) Errors() validation.Errors {
	return result.errors
}

// Reject returns a new result with one additional safe violation.
func (result BindingResult) Reject(
	field string,
	code string,
	message string,
) (BindingResult, error) {
	violation, err := validation.Field(field, code, message)
	if err != nil {
		return BindingResult{}, fmt.Errorf(
			"reject form field: %w",
			err,
		)
	}
	combined, err := result.errors.Add(violation)
	if err != nil {
		return BindingResult{}, fmt.Errorf(
			"reject form field: %w",
			err,
		)
	}
	return BindingResult{errors: combined}, nil
}

// RejectBinding returns a new result containing one safe request binding
// failure. Raw rejected values and wrapped parser errors are never retained.
func (result BindingResult) RejectBinding(bindingErr error) (BindingResult, error) {
	var failure *BindingError
	if !errors.As(bindingErr, &failure) || failure == nil {
		return result.rejectViolation(
			"",
			"the submitted form is invalid",
		)
	}
	field := failure.Field
	if failure.Location != LocationForm {
		field = ""
	}
	return result.rejectViolation(field, failure.Error())
}

func (result BindingResult) rejectViolation(
	field string,
	message string,
) (BindingResult, error) {
	violation, err := validation.Field(
		field,
		"binding.invalid",
		message,
	)
	if err != nil {
		return BindingResult{}, fmt.Errorf(
			"reject form binding: %w",
			err,
		)
	}
	combined, err := result.errors.Add(violation)
	if err != nil {
		return BindingResult{}, fmt.Errorf(
			"reject form binding: %w",
			err,
		)
	}
	return BindingResult{errors: combined}, nil
}

// DecodeForm strictly reads one URL-encoded form within the configured bound.
// It does not retain the request body or mutate request.Form.
func DecodeForm(
	request *http.Request,
	maxBytes int64,
) (url.Values, error) {
	if request == nil {
		return nil, NewBindingError(
			LocationForm,
			"form",
			"is required",
			nil,
		)
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBodyBytes
	}
	if err := requireFormContentType(
		request.Header.Get("Content-Type"),
	); err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, NewBindingError(
			LocationForm,
			"form",
			"is required",
			nil,
		)
	}
	content, err := io.ReadAll(io.LimitReader(request.Body, maxBytes+1))
	if err != nil {
		return nil, NewBindingError(
			LocationForm,
			"form",
			"could not be read",
			err,
		)
	}
	if int64(len(content)) > maxBytes {
		return nil, NewBindingError(
			LocationForm,
			"form",
			"exceeds the configured byte limit",
			nil,
		)
	}
	values, err := url.ParseQuery(string(content))
	if err != nil {
		return nil, NewBindingError(
			LocationForm,
			"form",
			"must contain valid URL-encoded fields",
			err,
		)
	}
	return cloneValues(values), nil
}

// FormValue returns one optional or required form field. Repeated values fail
// closed because generated scalar bindings cannot choose implicitly.
func FormValue(
	values url.Values,
	name string,
	required bool,
) (string, bool, error) {
	return Parameter(LocationForm, name, values[name], required)
}

// RejectUnknownForm rejects fields outside the generated allowlist without
// exposing their values.
func RejectUnknownForm(values url.Values, allowed []string) error {
	sorted := slices.Clone(allowed)
	slices.Sort(sorted)
	if len(sorted) != len(slices.Compact(sorted)) {
		return errors.New(
			"validate form allowlist: fields must be unique",
		)
	}
	for name := range values {
		if _, found := slices.BinarySearch(sorted, name); !found {
			return NewBindingError(
				LocationForm,
				name,
				"is not allowed",
				nil,
			)
		}
	}
	return nil
}

func requireFormContentType(header string) error {
	mediaType, parameters, err := mime.ParseMediaType(header)
	if err != nil ||
		!strings.EqualFold(mediaType, formMediaType) ||
		(len(parameters) > 1) {
		return NewBindingError(
			LocationHeader,
			"Content-Type",
			"must be application/x-www-form-urlencoded",
			err,
		)
	}
	if charset, present := parameters["charset"]; present &&
		!strings.EqualFold(charset, "utf-8") {
		return NewBindingError(
			LocationHeader,
			"Content-Type",
			"must use UTF-8",
			nil,
		)
	}
	return nil
}

func cloneValues(values url.Values) url.Values {
	result := make(url.Values, len(values))
	for name, items := range values {
		result[name] = slices.Clone(items)
	}
	return result
}
