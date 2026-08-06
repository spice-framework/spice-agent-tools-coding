// Package conversion provides small, strongly typed, reflection-free value
// conversion contracts for application, configuration, and transport layers.
package conversion

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

const maxTypeNameBytes = 128

var (
	typeNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.\-\[\]]*$`)

	// ErrInvalidValue identifies input that cannot be represented by the
	// requested target type. Built-in converters never include the raw input
	// in returned errors.
	ErrInvalidValue = errors.New("value is invalid")
)

// Converter transforms one exact source type into one exact target type.
// Implementations should be deterministic and must not perform hidden I/O.
type Converter[Source, Target any] interface {
	Convert(Source) (Target, error)
}

// ConverterFunc adapts an ordinary function to Converter.
type ConverterFunc[Source, Target any] func(Source) (Target, error)

// Convert invokes the adapted function.
func (convert ConverterFunc[Source, Target]) Convert(
	source Source,
) (Target, error) {
	if convert == nil {
		var zero Target
		return zero, errors.New("conversion function is nil")
	}
	return convert(source)
}

// Then composes two typed converters without a runtime registry or type lookup.
func Then[Source, Intermediate, Target any](
	first Converter[Source, Intermediate],
	second Converter[Intermediate, Target],
) Converter[Source, Target] {
	return ConverterFunc[Source, Target](func(source Source) (Target, error) {
		var zero Target
		if first == nil {
			return zero, errors.New("first conversion is nil")
		}
		if second == nil {
			return zero, errors.New("second conversion is nil")
		}
		intermediate, err := first.Convert(source)
		if err != nil {
			return zero, fmt.Errorf("first conversion: %w", err)
		}
		result, err := second.Convert(intermediate)
		if err != nil {
			return zero, fmt.Errorf("second conversion: %w", err)
		}
		return result, nil
	})
}

// Codec provides an exact bidirectional string representation for T.
type Codec[T any] struct {
	name   string
	parse  func(string) (T, error)
	format func(T) (string, error)
}

// NewCodec validates and constructs a custom deterministic codec.
func NewCodec[T any](
	name string,
	parse func(string) (T, error),
	format func(T) (string, error),
) (Codec[T], error) {
	if len(name) == 0 ||
		len(name) > maxTypeNameBytes ||
		!typeNamePattern.MatchString(name) {
		return Codec[T]{}, errors.New(
			"conversion type name must be a bounded identifier",
		)
	}
	if parse == nil {
		return Codec[T]{}, errors.New("conversion parser is nil")
	}
	if format == nil {
		return Codec[T]{}, errors.New("conversion formatter is nil")
	}
	return Codec[T]{name: name, parse: parse, format: format}, nil
}

// Name returns the stable target type name used in safe diagnostics.
func (codec Codec[T]) Name() string {
	return codec.name
}

// Convert parses a string using the codec.
func (codec Codec[T]) Convert(source string) (T, error) {
	return codec.Parse(source)
}

// Parse converts a string without retaining or reporting the raw input.
func (codec Codec[T]) Parse(source string) (T, error) {
	if codec.parse == nil || codec.name == "" {
		var zero T
		return zero, errors.New("conversion codec is not initialized")
	}
	result, err := codec.parse(source)
	if err != nil {
		var zero T
		return zero, fmt.Errorf("parse %s: %w", codec.name, err)
	}
	return result, nil
}

// Format returns the canonical string representation.
func (codec Codec[T]) Format(value T) (string, error) {
	if codec.format == nil || codec.name == "" {
		return "", errors.New("conversion codec is not initialized")
	}
	result, err := codec.format(value)
	if err != nil {
		return "", fmt.Errorf("format %s: %w", codec.name, err)
	}
	return result, nil
}

// String returns the identity string codec.
func String() Codec[string] {
	return mustCodec(
		"string",
		func(value string) (string, error) { return value, nil },
		func(value string) (string, error) { return value, nil },
	)
}

// Boolean returns the strconv-compatible Boolean codec.
func Boolean() Codec[bool] {
	return mustCodec(
		"boolean",
		func(value string) (bool, error) {
			result, err := strconv.ParseBool(value)
			if err != nil {
				return false, ErrInvalidValue
			}
			return result, nil
		},
		func(value bool) (string, error) {
			return strconv.FormatBool(value), nil
		},
	)
}

// SignedInteger returns a base-10 signed integer codec for a Go bit width.
func SignedInteger(bitSize int) (Codec[int64], error) {
	if !validIntegerBitSize(bitSize) {
		return Codec[int64]{}, fmt.Errorf(
			"signed integer bit size %d is unsupported",
			bitSize,
		)
	}
	return NewCodec(
		fmt.Sprintf("int%d", normalizedBitSize(bitSize)),
		func(value string) (int64, error) {
			result, err := strconv.ParseInt(value, 10, bitSize)
			if err != nil {
				return 0, ErrInvalidValue
			}
			return result, nil
		},
		func(value int64) (string, error) {
			if _, err := strconv.ParseInt(
				strconv.FormatInt(value, 10),
				10,
				bitSize,
			); err != nil {
				return "", ErrInvalidValue
			}
			return strconv.FormatInt(value, 10), nil
		},
	)
}

// UnsignedInteger returns a base-10 unsigned integer codec for a Go bit width.
func UnsignedInteger(bitSize int) (Codec[uint64], error) {
	if !validIntegerBitSize(bitSize) {
		return Codec[uint64]{}, fmt.Errorf(
			"unsigned integer bit size %d is unsupported",
			bitSize,
		)
	}
	return NewCodec(
		fmt.Sprintf("uint%d", normalizedBitSize(bitSize)),
		func(value string) (uint64, error) {
			result, err := strconv.ParseUint(value, 10, bitSize)
			if err != nil {
				return 0, ErrInvalidValue
			}
			return result, nil
		},
		func(value uint64) (string, error) {
			if _, err := strconv.ParseUint(
				strconv.FormatUint(value, 10),
				10,
				bitSize,
			); err != nil {
				return "", ErrInvalidValue
			}
			return strconv.FormatUint(value, 10), nil
		},
	)
}

// Float returns a decimal floating-point codec for a 32- or 64-bit Go value.
func Float(bitSize int) (Codec[float64], error) {
	if bitSize != 32 && bitSize != 64 {
		return Codec[float64]{}, fmt.Errorf(
			"floating-point bit size %d is unsupported",
			bitSize,
		)
	}
	return NewCodec(
		fmt.Sprintf("float%d", bitSize),
		func(value string) (float64, error) {
			result, err := strconv.ParseFloat(value, bitSize)
			if err != nil {
				return 0, ErrInvalidValue
			}
			return result, nil
		},
		func(value float64) (string, error) {
			return strconv.FormatFloat(value, 'g', -1, bitSize), nil
		},
	)
}

// Duration returns the canonical Go duration codec.
func Duration() Codec[time.Duration] {
	return mustCodec(
		"duration",
		func(value string) (time.Duration, error) {
			result, err := time.ParseDuration(value)
			if err != nil {
				return 0, ErrInvalidValue
			}
			return result, nil
		},
		func(value time.Duration) (string, error) {
			return value.String(), nil
		},
	)
}

// Time returns a time codec with one explicit layout and location.
func Time(
	layout string,
	location *time.Location,
) (Codec[time.Time], error) {
	if layout == "" {
		return Codec[time.Time]{}, errors.New("time layout is empty")
	}
	if location == nil {
		return Codec[time.Time]{}, errors.New("time location is nil")
	}
	return NewCodec(
		"time",
		func(value string) (time.Time, error) {
			result, err := time.ParseInLocation(layout, value, location)
			if err != nil {
				return time.Time{}, ErrInvalidValue
			}
			return result, nil
		},
		func(value time.Time) (string, error) {
			return value.In(location).Format(layout), nil
		},
	)
}

// URL returns a URL codec that requires an absolute HTTP or HTTPS URL.
func URL() Codec[*url.URL] {
	return mustCodec(
		"url",
		func(value string) (*url.URL, error) {
			result, err := url.Parse(value)
			if err != nil ||
				result.Host == "" ||
				(result.Scheme != "http" && result.Scheme != "https") {
				return nil, ErrInvalidValue
			}
			return result, nil
		},
		func(value *url.URL) (string, error) {
			if value == nil ||
				value.Host == "" ||
				(value.Scheme != "http" && value.Scheme != "https") {
				return "", ErrInvalidValue
			}
			return value.String(), nil
		},
	)
}

// ParseBoolean parses a Boolean with safe diagnostics.
func ParseBoolean(value string) (bool, error) {
	return Boolean().Parse(value)
}

// ParseSignedInteger parses a base-10 signed integer with a Go bit width.
func ParseSignedInteger(value string, bitSize int) (int64, error) {
	codec, err := SignedInteger(bitSize)
	if err != nil {
		return 0, err
	}
	return codec.Parse(value)
}

// ParseDuration parses a canonical Go duration with safe diagnostics.
func ParseDuration(value string) (time.Duration, error) {
	return Duration().Parse(value)
}

func mustCodec[T any](
	name string,
	parse func(string) (T, error),
	format func(T) (string, error),
) Codec[T] {
	codec, err := NewCodec(name, parse, format)
	if err != nil {
		panic(err)
	}
	return codec
}

func validIntegerBitSize(bitSize int) bool {
	switch bitSize {
	case 0, 8, 16, 32, 64:
		return true
	default:
		return false
	}
}

func normalizedBitSize(bitSize int) int {
	if bitSize == 0 {
		return strconv.IntSize
	}
	return bitSize
}
