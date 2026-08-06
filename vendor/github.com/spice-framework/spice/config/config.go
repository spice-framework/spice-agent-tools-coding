// Package config provides the small reflection-free runtime used by generated
// Spice configuration binders.
package config

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spice-framework/spice/conversion"
)

const redactedValue = "<redacted>"

var (
	propertyKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[a-z][a-z0-9-]*)*$`)
	profilePattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	environmentPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
)

// Kind identifies one generated property's scalar representation.
type Kind string

const (
	// KindString identifies a string property.
	KindString Kind = "string"
	// KindBoolean identifies a Boolean property accepted by conversion.Boolean.
	KindBoolean Kind = "boolean"
	// KindInteger identifies a base-10 signed 64-bit integer property.
	KindInteger Kind = "integer"
	// KindDuration identifies a conversion.Duration property.
	KindDuration Kind = "duration"
)

// Property is generated metadata for one exact configuration key.
type Property struct {
	Key         string
	Kind        Kind
	Description string
	Module      string
	Environment string
	Default     string
	HasDefault  bool
	Required    bool
	Secret      bool
}

// Schema is immutable-by-construction generated configuration metadata.
type Schema struct {
	properties []Property
	byKey      map[string]Property
}

// NewSchema validates and sorts generated property metadata.
func NewSchema(properties ...Property) (Schema, error) {
	result := Schema{
		properties: append([]Property(nil), properties...),
		byKey:      make(map[string]Property, len(properties)),
	}
	sort.SliceStable(result.properties, func(i, j int) bool {
		return result.properties[i].Key < result.properties[j].Key
	})
	environmentKeys := make(map[string]string)
	for index, property := range result.properties {
		if err := validateProperty(property); err != nil {
			return Schema{}, fmt.Errorf("configuration property %d: %w", index, err)
		}
		if _, duplicate := result.byKey[property.Key]; duplicate {
			return Schema{}, fmt.Errorf("duplicate configuration property %q", property.Key)
		}
		if property.Environment != "" {
			if previous, duplicate := environmentKeys[property.Environment]; duplicate {
				return Schema{}, fmt.Errorf(
					"configuration properties %q and %q use the same environment variable %q",
					previous,
					property.Key,
					property.Environment,
				)
			}
			environmentKeys[property.Environment] = property.Key
		}
		result.byKey[property.Key] = property
	}
	return result, nil
}

// MustSchema returns a validated schema or panics for invalid package-owned
// generated metadata.
func MustSchema(properties ...Property) Schema {
	schema, err := NewSchema(properties...)
	if err != nil {
		panic(err)
	}
	return schema
}

// Properties returns property metadata in key order.
func (s Schema) Properties() []Property {
	return append([]Property(nil), s.properties...)
}

func validateProperty(property Property) error {
	if !propertyKeyPattern.MatchString(property.Key) {
		return fmt.Errorf("key %q must match %s", property.Key, propertyKeyPattern)
	}
	switch property.Kind {
	case KindString, KindBoolean, KindInteger, KindDuration:
	default:
		return fmt.Errorf("key %q has unsupported kind %q", property.Key, property.Kind)
	}
	if property.Environment != "" && !environmentPattern.MatchString(property.Environment) {
		return fmt.Errorf(
			"key %q environment variable %q must match %s",
			property.Key,
			property.Environment,
			environmentPattern,
		)
	}
	if property.HasDefault {
		if err := validateScalar(property, property.Default); err != nil {
			return fmt.Errorf("key %q default is invalid: %w", property.Key, err)
		}
	}
	return nil
}

// Request is the immutable source-load request.
type Request struct {
	profiles []string
	schema   Schema
}

// Profiles returns active profiles in caller precedence order.
func (r Request) Profiles() []string {
	return append([]string(nil), r.profiles...)
}

// Properties returns the generated schema in key order.
func (r Request) Properties() []Property {
	return r.schema.Properties()
}

// Source contributes configuration values. Sources must perform no hidden
// network access; source ordering is the resolver's precedence contract.
type Source interface {
	Name() string
	Load(context.Context, Request) (map[string]string, error)
}

// Options controls deterministic source resolution.
type Options struct {
	Profiles     []string
	AllowUnknown bool
}

// Origin records where the winning value came from.
type Origin struct {
	Source  string
	Default bool
}

// Entry is one resolved raw value with provenance and redaction metadata.
type Entry struct {
	Key    string
	Value  string
	Origin Origin
	Secret bool
}

// Snapshot is one immutable-by-construction resolved configuration view.
type Snapshot struct {
	entries map[string]Entry
	keys    []string
}

// Keys returns resolved keys in lexical order.
func (s Snapshot) Keys() []string {
	return append([]string(nil), s.keys...)
}

// Entry returns one resolved value and its provenance.
func (s Snapshot) Entry(key string) (Entry, bool) {
	entry, ok := s.entries[key]
	return entry, ok
}

// Lookup returns one raw value. Generated binders use this for optional string
// and custom scalar decoding.
func (s Snapshot) Lookup(key string) (string, bool) {
	entry, ok := s.entries[key]
	return entry.Value, ok
}

// RequiredString returns one required raw string.
func (s Snapshot) RequiredString(key string) (string, error) {
	value, ok := s.Lookup(key)
	if !ok {
		return "", fmt.Errorf("configuration property %q is not resolved", key)
	}
	return value, nil
}

// Boolean decodes one required Boolean without including its raw value in an
// error, which keeps secret values out of diagnostics.
func (s Snapshot) Boolean(key string) (bool, error) {
	value, err := s.RequiredString(key)
	if err != nil {
		return false, err
	}
	result, parseErr := conversion.ParseBoolean(value)
	if parseErr != nil {
		return false, fmt.Errorf("configuration property %q is not a boolean", key)
	}
	return result, nil
}

// Integer decodes one required base-10 signed 64-bit integer.
func (s Snapshot) Integer(key string) (int64, error) {
	value, err := s.RequiredString(key)
	if err != nil {
		return 0, err
	}
	result, parseErr := conversion.ParseSignedInteger(value, 64)
	if parseErr != nil {
		return 0, fmt.Errorf("configuration property %q is not an integer", key)
	}
	return result, nil
}

// Duration decodes one required Go duration.
func (s Snapshot) Duration(key string) (time.Duration, error) {
	value, err := s.RequiredString(key)
	if err != nil {
		return 0, err
	}
	result, parseErr := conversion.ParseDuration(value)
	if parseErr != nil {
		return 0, fmt.Errorf("configuration property %q is not a duration", key)
	}
	return result, nil
}

// Redacted returns a complete key/value copy safe for logs and diagnostics.
func (s Snapshot) Redacted() map[string]string {
	result := make(map[string]string, len(s.entries))
	for key, entry := range s.entries {
		if entry.Secret {
			result[key] = redactedValue
		} else {
			result[key] = entry.Value
		}
	}
	return result
}

// String renders deterministic redacted key/value lines.
func (s Snapshot) String() string {
	redacted := s.Redacted()
	var output strings.Builder
	for index, key := range s.keys {
		if index != 0 {
			output.WriteByte('\n')
		}
		output.WriteString(key)
		output.WriteByte('=')
		output.WriteString(redacted[key])
	}
	return output.String()
}

// Resolve merges defaults and sources from lowest to highest precedence,
// validates required/known/scalar properties, and preserves winning origins.
func Resolve(
	ctx context.Context,
	schema Schema,
	options Options,
	sources ...Source,
) (Snapshot, error) {
	if ctx == nil {
		return Snapshot{}, errors.New("resolve configuration: context is nil")
	}
	profiles, err := validateProfiles(options.Profiles)
	if err != nil {
		return Snapshot{}, err
	}
	entries := defaultEntries(schema)
	request := Request{profiles: profiles, schema: schema}
	if err := mergeSources(ctx, entries, schema, request, options.AllowUnknown, sources); err != nil {
		return Snapshot{}, err
	}
	if err := validateResolvedEntries(entries, schema); err != nil {
		return Snapshot{}, err
	}
	return newSnapshot(entries), nil
}

func defaultEntries(schema Schema) map[string]Entry {
	entries := make(map[string]Entry, len(schema.properties))
	for _, property := range schema.properties {
		if property.HasDefault {
			entries[property.Key] = Entry{
				Key:    property.Key,
				Value:  property.Default,
				Origin: Origin{Source: "default", Default: true},
				Secret: property.Secret,
			}
		}
	}
	return entries
}

func mergeSources(
	ctx context.Context,
	entries map[string]Entry,
	schema Schema,
	request Request,
	allowUnknown bool,
	sources []Source,
) error {
	sourceNames := make(map[string]struct{}, len(sources))
	for index, source := range sources {
		if cause := context.Cause(ctx); cause != nil {
			return fmt.Errorf("resolve configuration: %w", cause)
		}
		if source == nil {
			return fmt.Errorf("resolve configuration: source %d is nil", index)
		}
		name := source.Name()
		if name == "" || strings.TrimSpace(name) != name {
			return fmt.Errorf("resolve configuration: source %d has an invalid name %q", index, name)
		}
		if _, duplicate := sourceNames[name]; duplicate {
			return fmt.Errorf("resolve configuration: duplicate source name %q", name)
		}
		sourceNames[name] = struct{}{}
		values, loadErr := source.Load(ctx, request)
		if loadErr != nil {
			return fmt.Errorf("load configuration source %q: %w", name, loadErr)
		}
		if err := mergeValues(entries, schema, name, values, allowUnknown); err != nil {
			return err
		}
	}
	return nil
}

func validateResolvedEntries(entries map[string]Entry, schema Schema) error {
	for _, property := range schema.properties {
		entry, ok := entries[property.Key]
		if !ok {
			if property.Required {
				return fmt.Errorf("required configuration property %q is missing", property.Key)
			}
			continue
		}
		if err := validateScalar(property, entry.Value); err != nil {
			return fmt.Errorf(
				"configuration property %q from source %q is invalid: %w",
				property.Key,
				entry.Origin.Source,
				err,
			)
		}
	}
	return nil
}

func validateProfiles(profiles []string) ([]string, error) {
	result := append([]string(nil), profiles...)
	seen := make(map[string]struct{}, len(result))
	for index, profile := range result {
		if !profilePattern.MatchString(profile) {
			return nil, fmt.Errorf(
				"configuration profile %d %q must match %s",
				index,
				profile,
				profilePattern,
			)
		}
		if _, duplicate := seen[profile]; duplicate {
			return nil, fmt.Errorf("configuration profile %q is active more than once", profile)
		}
		seen[profile] = struct{}{}
	}
	return result, nil
}

func mergeValues(
	entries map[string]Entry,
	schema Schema,
	sourceName string,
	values map[string]string,
	allowUnknown bool,
) error {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		property, known := schema.byKey[key]
		if !known && !allowUnknown {
			return fmt.Errorf("configuration source %q contains unknown property %q", sourceName, key)
		}
		entries[key] = Entry{
			Key:    key,
			Value:  values[key],
			Origin: Origin{Source: sourceName},
			Secret: known && property.Secret,
		}
	}
	return nil
}

func validateScalar(property Property, value string) error {
	switch property.Kind {
	case KindString:
		return nil
	case KindBoolean:
		if _, err := conversion.ParseBoolean(value); err != nil {
			return errors.New("value must be a boolean")
		}
	case KindInteger:
		if _, err := conversion.ParseSignedInteger(value, 64); err != nil {
			return errors.New("value must be a base-10 signed 64-bit integer")
		}
	case KindDuration:
		if _, err := conversion.ParseDuration(value); err != nil {
			return errors.New("value must be a Go duration")
		}
	default:
		return fmt.Errorf("unsupported kind %q", property.Kind)
	}
	return nil
}

func newSnapshot(entries map[string]Entry) Snapshot {
	keys := make([]string, 0, len(entries))
	copyEntries := make(map[string]Entry, len(entries))
	for key, entry := range entries {
		keys = append(keys, key)
		copyEntries[key] = entry
	}
	sort.Strings(keys)
	return Snapshot{entries: copyEntries, keys: keys}
}

// Decoder is implemented by generated reflection-free binders.
type Decoder[T any] func(Snapshot) (T, error)

// Validator applies one typed post-decode validation rule.
type Validator[T any] func(context.Context, T) error

// Decode invokes a generated binder and typed validators in declaration order.
func Decode[T any](
	ctx context.Context,
	snapshot Snapshot,
	decoder Decoder[T],
	validators ...Validator[T],
) (T, error) {
	var zero T
	if ctx == nil {
		return zero, errors.New("decode configuration: context is nil")
	}
	if decoder == nil {
		return zero, errors.New("decode configuration: decoder is nil")
	}
	value, err := decoder(snapshot)
	if err != nil {
		return zero, fmt.Errorf("decode configuration: %w", err)
	}
	for index, validator := range validators {
		if cause := context.Cause(ctx); cause != nil {
			return zero, fmt.Errorf("validate configuration: %w", cause)
		}
		if validator == nil {
			return zero, fmt.Errorf("validate configuration: validator %d is nil", index)
		}
		if err := validator(ctx, value); err != nil {
			return zero, fmt.Errorf("validate configuration with validator %d: %w", index, err)
		}
	}
	return value, nil
}

// MapSource is an immutable in-memory source useful for explicit overrides and
// tests.
type MapSource struct {
	name   string
	values map[string]string
}

// NewMapSource copies values into a named source.
func NewMapSource(name string, values map[string]string) (MapSource, error) {
	if name == "" || strings.TrimSpace(name) != name {
		return MapSource{}, fmt.Errorf("map configuration source has an invalid name %q", name)
	}
	copyValues := make(map[string]string, len(values))
	maps.Copy(copyValues, values)
	return MapSource{name: name, values: copyValues}, nil
}

// Name returns the source identity used in provenance.
func (s MapSource) Name() string {
	return s.name
}

// Load returns a fresh value map.
func (s MapSource) Load(ctx context.Context, _ Request) (map[string]string, error) {
	if cause := context.Cause(ctx); cause != nil {
		return nil, cause
	}
	result := make(map[string]string, len(s.values))
	maps.Copy(result, s.values)
	return result, nil
}

// LookupEnv is an injectable environment lookup function.
type LookupEnv func(string) (string, bool)

// EnvironmentSource reads only schema-declared environment variables.
type EnvironmentSource struct {
	name   string
	prefix string
	lookup LookupEnv
}

// NewEnvironmentSource creates an explicit environment source. prefix must be
// empty or a portable uppercase environment prefix.
func NewEnvironmentSource(
	name string,
	prefix string,
	lookup LookupEnv,
) (EnvironmentSource, error) {
	if name == "" || strings.TrimSpace(name) != name {
		return EnvironmentSource{}, fmt.Errorf("environment configuration source has an invalid name %q", name)
	}
	if prefix != "" && !environmentPattern.MatchString(prefix) {
		return EnvironmentSource{}, fmt.Errorf("environment prefix %q must match %s", prefix, environmentPattern)
	}
	if lookup == nil {
		return EnvironmentSource{}, errors.New("environment configuration source requires a lookup function")
	}
	return EnvironmentSource{name: name, prefix: prefix, lookup: lookup}, nil
}

// OSEnvironment creates a source backed by os.LookupEnv.
func OSEnvironment(prefix string) (EnvironmentSource, error) {
	return NewEnvironmentSource("environment", prefix, os.LookupEnv)
}

// Name returns the source identity used in provenance.
func (s EnvironmentSource) Name() string {
	return s.name
}

// Load checks only schema-derived or explicitly named environment variables.
func (s EnvironmentSource) Load(
	ctx context.Context,
	request Request,
) (map[string]string, error) {
	result := make(map[string]string)
	environmentKeys := make(map[string]string)
	for _, property := range request.Properties() {
		if cause := context.Cause(ctx); cause != nil {
			return nil, cause
		}
		name := property.Environment
		if name == "" {
			name = s.prefix + environmentKey(property.Key)
		}
		if previous, collision := environmentKeys[name]; collision {
			return nil, fmt.Errorf(
				"configuration properties %q and %q map to environment variable %q",
				previous,
				property.Key,
				name,
			)
		}
		environmentKeys[name] = property.Key
		if value, ok := s.lookup(name); ok {
			result[property.Key] = value
		}
	}
	return result, nil
}

func environmentKey(propertyKey string) string {
	replacer := strings.NewReplacer(".", "_", "-", "_")
	return strings.ToUpper(replacer.Replace(propertyKey))
}
