package validation

import (
	"context"
	"errors"
	"fmt"
)

// Validator performs layer-neutral validation for one exact Go type.
// Validation failures belong in Errors; returned error values identify
// operational failures such as cancellation or unavailable dependencies.
type Validator[T any] interface {
	Validate(context.Context, T) (Errors, error)
}

// ValidatorFunc adapts an ordinary typed function to Validator.
type ValidatorFunc[T any] func(context.Context, T) (Errors, error)

// Validate invokes the adapted function and rejects nil contexts/functions.
func (validate ValidatorFunc[T]) Validate(
	ctx context.Context,
	value T,
) (Errors, error) {
	if ctx == nil {
		return Errors{}, errors.New("validation context is nil")
	}
	if validate == nil {
		return Errors{}, errors.New("validation function is nil")
	}
	if cause := context.Cause(ctx); cause != nil {
		return Errors{}, fmt.Errorf("validation canceled: %w", cause)
	}
	result, err := validate(ctx, value)
	if err != nil {
		return Errors{}, err
	}
	if cause := context.Cause(ctx); cause != nil {
		return Errors{}, fmt.Errorf("validation canceled: %w", cause)
	}
	return result, nil
}

// Validate applies validators in caller order and returns their immutable
// violations in the same order. It stops on cancellation, an operational
// error, or the package violation limit.
func Validate[T any](
	ctx context.Context,
	value T,
	validators ...Validator[T],
) (Errors, error) {
	if ctx == nil {
		return Errors{}, errors.New("validation context is nil")
	}
	result := Errors{}
	for index, validator := range validators {
		if cause := context.Cause(ctx); cause != nil {
			return Errors{}, fmt.Errorf(
				"validation canceled before validator %d: %w",
				index,
				cause,
			)
		}
		if validator == nil {
			return Errors{}, fmt.Errorf(
				"validator %d is nil",
				index,
			)
		}
		current, err := validator.Validate(ctx, value)
		if err != nil {
			return Errors{}, fmt.Errorf(
				"validator %d: %w",
				index,
				err,
			)
		}
		if cause := context.Cause(ctx); cause != nil {
			return Errors{}, fmt.Errorf(
				"validation canceled by validator %d: %w",
				index,
				cause,
			)
		}
		result, err = Join(result, current)
		if err != nil {
			return Errors{}, fmt.Errorf(
				"validator %d result: %w",
				index,
				err,
			)
		}
	}
	return result, nil
}
