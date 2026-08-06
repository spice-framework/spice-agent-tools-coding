package config

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"strconv"
	"strings"
)

const defaultJSONMaxBytes int64 = 1 << 20

var jsonBaseNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)

// JSONOptions controls a rooted JSON file source.
type JSONOptions struct {
	// Required makes the base <name>.json file mandatory. Profile files remain
	// optional because deployment profiles commonly share one artifact.
	Required bool
	// MaxBytes bounds each individual file. Zero uses 1 MiB.
	MaxBytes int64
}

// JSONSource loads one base object and ordered active-profile objects from a
// rooted directory.
type JSONSource struct {
	name      string
	directory string
	baseName  string
	required  bool
	maxBytes  int64
}

// NewJSONSource validates a rooted JSON source. baseName is a portable filename
// stem, not a relative path.
func NewJSONSource(
	name string,
	directory string,
	baseName string,
	options JSONOptions,
) (JSONSource, error) {
	if name == "" || strings.TrimSpace(name) != name {
		return JSONSource{}, fmt.Errorf("JSON configuration source has an invalid name %q", name)
	}
	if directory == "" {
		return JSONSource{}, errors.New("JSON configuration source requires a directory")
	}
	if !jsonBaseNamePattern.MatchString(baseName) {
		return JSONSource{}, fmt.Errorf("JSON configuration base name %q must match %s", baseName, jsonBaseNamePattern)
	}
	maxBytes := options.MaxBytes
	if maxBytes == 0 {
		maxBytes = defaultJSONMaxBytes
	}
	if maxBytes < 0 {
		return JSONSource{}, errors.New("JSON configuration MaxBytes must not be negative")
	}
	return JSONSource{
		name:      name,
		directory: directory,
		baseName:  baseName,
		required:  options.Required,
		maxBytes:  maxBytes,
	}, nil
}

// Name returns the source identity used in provenance.
func (s JSONSource) Name() string {
	return s.name
}

// Load reads the base file followed by one optional file per active profile.
func (s JSONSource) Load(
	ctx context.Context,
	request Request,
) (map[string]string, error) {
	if cause := context.Cause(ctx); cause != nil {
		return nil, cause
	}
	root, err := os.OpenRoot(s.directory)
	if err != nil {
		return nil, fmt.Errorf("open configuration root: %w", err)
	}
	result, loadErr := s.loadFiles(ctx, root, request)
	closeErr := root.Close()
	if loadErr != nil {
		return nil, loadErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close configuration root: %w", closeErr)
	}
	return result, nil
}

func (s JSONSource) loadFiles(
	ctx context.Context,
	root *os.Root,
	request Request,
) (map[string]string, error) {
	result := make(map[string]string)
	baseFile := s.baseName + ".json"
	if err := s.mergeFile(ctx, root, baseFile, s.required, result); err != nil {
		return nil, err
	}
	for _, profile := range request.Profiles() {
		filename := s.baseName + "-" + profile + ".json"
		if err := s.mergeFile(ctx, root, filename, false, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s JSONSource) mergeFile(
	ctx context.Context,
	root *os.Root,
	filename string,
	required bool,
	result map[string]string,
) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	file, err := root.Open(filename)
	if err != nil {
		if !required && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open configuration file %s: %w", filename, err)
	}
	content, readErr := readBounded(file, s.maxBytes)
	closeErr := file.Close()
	if readErr != nil {
		return fmt.Errorf("read configuration file %s: %w", filename, readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close configuration file %s: %w", filename, closeErr)
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	values, err := decodeJSONObject(content)
	if err != nil {
		return fmt.Errorf("decode configuration file %s: %w", filename, err)
	}
	maps.Copy(result, values)
	return nil
}

func readBounded(reader io.Reader, maxBytes int64) ([]byte, error) {
	buffered := bufio.NewReader(io.LimitReader(reader, maxBytes+1))
	content, err := io.ReadAll(buffered)
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > maxBytes {
		return nil, fmt.Errorf("file exceeds %d byte limit", maxBytes)
	}
	return content, nil
}

func decodeJSONObject(content []byte) (map[string]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	first, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("read root object: %w", err)
	}
	if delimiter, ok := first.(json.Delim); !ok || delimiter != '{' {
		return nil, errors.New("configuration JSON root must be an object")
	}
	result := make(map[string]string)
	if err := decodeObject(decoder, "", result); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("configuration JSON contains a trailing value")
		}
		return nil, fmt.Errorf("read trailing JSON content: %w", err)
	}
	return result, nil
}

func decodeObject(
	decoder *json.Decoder,
	prefix string,
	result map[string]string,
) error {
	seen := make(map[string]struct{})
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("read object key: %w", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return errors.New("configuration JSON object key is not a string")
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("configuration JSON object contains duplicate key %q", key)
		}
		seen[key] = struct{}{}
		fullKey := key
		if prefix != "" {
			fullKey = prefix + "." + key
		}
		if !propertyKeyPattern.MatchString(fullKey) {
			return fmt.Errorf("flattened configuration key %q must match %s", fullKey, propertyKeyPattern)
		}

		value, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("read value for key %q: %w", fullKey, err)
		}
		if err := decodeObjectValue(decoder, fullKey, value, result); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("close configuration object: %w", err)
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != '}' {
		return errors.New("configuration JSON object is not closed")
	}
	return nil
}

func decodeObjectValue(
	decoder *json.Decoder,
	fullKey string,
	value any,
	result map[string]string,
) error {
	if delimiter, isDelimiter := value.(json.Delim); isDelimiter {
		switch delimiter {
		case '{':
			return decodeObject(decoder, fullKey, result)
		case '[':
			return fmt.Errorf("configuration key %q must not contain an array", fullKey)
		default:
			return fmt.Errorf("configuration key %q has unexpected delimiter %q", fullKey, delimiter)
		}
	}
	scalar, err := jsonScalar(value)
	if err != nil {
		return fmt.Errorf("configuration key %q: %w", fullKey, err)
	}
	if _, collision := result[fullKey]; collision {
		return fmt.Errorf("flattened configuration key %q is declared more than once", fullKey)
	}
	result[fullKey] = scalar
	return nil
}

func jsonScalar(value any) (string, error) {
	switch value := value.(type) {
	case string:
		return value, nil
	case bool:
		return strconv.FormatBool(value), nil
	case json.Number:
		return value.String(), nil
	case nil:
		return "", errors.New("null values are not supported")
	default:
		return "", fmt.Errorf("unsupported scalar type %T", value)
	}
}
