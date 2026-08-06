package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	codegen "github.com/spice-framework/toolchain/compiler/generate"
	"github.com/spice-framework/toolchain/compiler/load"
)

const (
	maxGeneratedManifestCount = 256
	maxGeneratedManifestSize  = 4 << 20
)

type generatedArguments struct {
	source    string
	generated string
	target    string
	format    string
	line      int
}

type generatedLocation struct {
	Path   string `json:"path"`
	Line   int    `json:"line,omitempty"`
	Column int    `json:"column,omitempty"`
}

type generatedMatch struct {
	Target       string                 `json:"target"`
	ManifestPath string                 `json:"manifest_path"`
	Role         codegen.FileRole       `json:"role"`
	Kind         string                 `json:"kind,omitempty"`
	Contribution string                 `json:"contribution,omitempty"`
	Source       generatedLocation      `json:"source"`
	Generated    generatedLocation      `json:"generated"`
	GeneratedEnd *generatedLocation     `json:"generated_end,omitempty"`
	Related      []codegen.SourceOrigin `json:"related_sources,omitempty"`
}

type generatedQueryResult struct {
	Direction string            `json:"direction"`
	Query     generatedLocation `json:"query"`
	Matches   []generatedMatch  `json:"matches"`
}

// NewGeneratedHandler constructs the generated-source navigation handler.
func NewGeneratedHandler(runtime *Runtime) (Handler, error) {
	return newCommandHandler(
		runtime,
		[]string{"generated"},
		func(runtime *Runtime, invocation Invocation) int {
			return generatedCommand(
				invocation.Arguments,
				invocation.Stdout,
				invocation.Stderr,
				runtime.options,
			)
		},
	)
}

func generatedCommand(
	arguments []string,
	stdout io.Writer,
	stderr io.Writer,
	options load.Options,
) int {
	parsed, err := parseGeneratedArguments(arguments)
	if err != nil {
		if writeErr := writef(stderr, "Spice generated lookup failed: %v\n", err); writeErr != nil {
			return 1
		}
		return 2
	}
	result, err := locateGenerated(parsed, options.Dir)
	if err != nil {
		if writeErr := writef(stderr, "Spice generated lookup failed: %v\n", err); writeErr != nil {
			return 1
		}
		return 1
	}
	if err := writeGeneratedResult(stdout, parsed.format, result); err != nil {
		return 1
	}
	return 0
}

func parseGeneratedArguments(arguments []string) (generatedArguments, error) {
	result := generatedArguments{format: "text"}
	for index := 0; index < len(arguments); index++ {
		next, err := parseGeneratedArgument(arguments, index, &result)
		if err != nil {
			return generatedArguments{}, err
		}
		index = next
	}
	if (result.source == "") == (result.generated == "") {
		return generatedArguments{}, errors.New(
			"exactly one of --source or --generated is required",
		)
	}
	if result.format != "text" && result.format != "json" {
		return generatedArguments{}, fmt.Errorf(
			"unsupported --format %q; expected text or json",
			result.format,
		)
	}
	return result, nil
}

func parseGeneratedArgument(
	arguments []string,
	index int,
	result *generatedArguments,
) (int, error) {
	name := arguments[index]
	switch name {
	case "--source", "--generated", "--target", "--format", "--line":
	default:
		return index, fmt.Errorf("unknown generated lookup argument %q", name)
	}
	value, next, err := generatedArgumentValue(arguments, index)
	if err != nil {
		return index, err
	}
	switch name {
	case "--source":
		result.source = value
	case "--generated":
		result.generated = value
	case "--target":
		result.target = value
	case "--format":
		result.format = value
	case "--line":
		line, parseErr := strconv.Atoi(value)
		if parseErr != nil || line <= 0 {
			return index, errors.New("--line must be a positive integer")
		}
		result.line = line
	}
	return next, nil
}

func generatedArgumentValue(
	arguments []string,
	index int,
) (string, int, error) {
	if index+1 >= len(arguments) || strings.TrimSpace(arguments[index+1]) == "" {
		return "", index, fmt.Errorf("%s requires a value", arguments[index])
	}
	return arguments[index+1], index + 1, nil
}

func locateGenerated(
	arguments generatedArguments,
	directory string,
) (generatedQueryResult, error) {
	moduleRoot, err := generatedModuleRoot(directory)
	if err != nil {
		return generatedQueryResult{}, err
	}
	queryPath := arguments.source
	direction := "source-to-generated"
	if arguments.generated != "" {
		queryPath = arguments.generated
		direction = "generated-to-source"
	}
	normalized, err := generatedModuleRelativePath(moduleRoot, directory, queryPath)
	if err != nil {
		return generatedQueryResult{}, err
	}
	manifests, err := readGeneratedManifests(moduleRoot, arguments.target)
	if err != nil {
		return generatedQueryResult{}, err
	}
	result := generatedQueryResult{
		Direction: direction,
		Query: generatedLocation{
			Path: normalized,
			Line: arguments.line,
		},
		Matches: make([]generatedMatch, 0),
	}
	for _, manifest := range manifests {
		if direction == "source-to-generated" {
			result.Matches = append(
				result.Matches,
				sourceGeneratedMatches(manifest, normalized, arguments.line)...,
			)
			continue
		}
		result.Matches = append(
			result.Matches,
			generatedSourceMatches(manifest, normalized, arguments.line)...,
		)
	}
	sortGeneratedMatches(result.Matches)
	if len(result.Matches) == 0 {
		location := normalized
		if arguments.line > 0 {
			location += ":" + strconv.Itoa(arguments.line)
		}
		return generatedQueryResult{}, fmt.Errorf(
			"no owned source mapping found for %s",
			location,
		)
	}
	return result, nil
}

type loadedGeneratedManifest struct {
	path     string
	manifest codegen.Manifest
}

func readGeneratedManifests(
	moduleRoot string,
	target string,
) (result []loadedGeneratedManifest, resultErr error) {
	root, err := os.OpenRoot(moduleRoot)
	if err != nil {
		return nil, fmt.Errorf("open module root %q: %w", moduleRoot, err)
	}
	defer closeGeneratedRoot(root, &resultErr)
	entries, err := fs.ReadDir(root.FS(), ".spice")
	if err != nil {
		return nil, fmt.Errorf("read generated ownership directory .spice: %w", err)
	}
	if len(entries) > maxGeneratedManifestCount {
		return nil, fmt.Errorf(
			"generated ownership directory contains more than %d entries",
			maxGeneratedManifestCount,
		)
	}
	result = make([]loadedGeneratedManifest, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".manifest.json") {
			continue
		}
		manifestPath := path.Join(".spice", entry.Name())
		content, readErr := readBoundedGeneratedManifest(root, manifestPath)
		if readErr != nil {
			return nil, readErr
		}
		var manifest codegen.Manifest
		if unmarshalErr := json.Unmarshal(content, &manifest); unmarshalErr != nil {
			return nil, fmt.Errorf(
				"decode generated ownership manifest %s: %w",
				manifestPath,
				unmarshalErr,
			)
		}
		if manifest.Schema != codegen.SchemaVersion {
			return nil, fmt.Errorf(
				"generated ownership manifest %s uses schema %d; run spice generate with schema %d",
				manifestPath,
				manifest.Schema,
				codegen.SchemaVersion,
			)
		}
		if target != "" && manifest.Target.ID != target {
			continue
		}
		result = append(result, loadedGeneratedManifest{
			path:     manifestPath,
			manifest: manifest,
		})
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].manifest.Target.ID < result[right].manifest.Target.ID
	})
	if target != "" && len(result) == 0 {
		return nil, fmt.Errorf("generated target %q has no ownership manifest", target)
	}
	return result, nil
}

func readBoundedGeneratedManifest(
	root *os.Root,
	manifestPath string,
) (content []byte, resultErr error) {
	file, err := root.Open(manifestPath)
	if err != nil {
		return nil, fmt.Errorf(
			"open generated ownership manifest %s: %w",
			manifestPath,
			err,
		)
	}
	defer closeGeneratedFile(file, manifestPath, &resultErr)
	content, err = io.ReadAll(io.LimitReader(file, maxGeneratedManifestSize+1))
	if err != nil {
		return nil, fmt.Errorf(
			"read generated ownership manifest %s: %w",
			manifestPath,
			err,
		)
	}
	if len(content) > maxGeneratedManifestSize {
		return nil, fmt.Errorf(
			"generated ownership manifest %s exceeds the %d-byte limit",
			manifestPath,
			maxGeneratedManifestSize,
		)
	}
	return content, nil
}

func closeGeneratedRoot(root *os.Root, resultErr *error) {
	if err := root.Close(); err != nil {
		*resultErr = errors.Join(*resultErr, fmt.Errorf("close module root: %w", err))
	}
}

func closeGeneratedFile(
	file *os.File,
	manifestPath string,
	resultErr *error,
) {
	if err := file.Close(); err != nil {
		*resultErr = errors.Join(
			*resultErr,
			fmt.Errorf(
				"close generated ownership manifest %s: %w",
				manifestPath,
				err,
			),
		)
	}
}

func sourceGeneratedMatches(
	loaded loadedGeneratedManifest,
	sourcePath string,
	line int,
) []generatedMatch {
	result := make([]generatedMatch, 0)
	for _, file := range loaded.manifest.Files {
		for _, mapping := range file.Mappings {
			if mapping.Source.Path != sourcePath ||
				(line > 0 && mapping.Source.Line != line) {
				continue
			}
			result = append(result, generatedMappingMatch(loaded, file, mapping))
		}
		if len(file.Mappings) != 0 ||
			file.PrimarySource == nil ||
			file.PrimarySource.Path != sourcePath ||
			(line > 0 && file.PrimarySource.Line != line) {
			continue
		}
		result = append(result, generatedFileMatch(loaded, file, *file.PrimarySource))
	}
	return result
}

func generatedSourceMatches(
	loaded loadedGeneratedManifest,
	generatedPath string,
	line int,
) []generatedMatch {
	result := make([]generatedMatch, 0)
	for _, file := range loaded.manifest.Files {
		if file.Path != generatedPath {
			continue
		}
		for _, mapping := range file.Mappings {
			if line > 0 &&
				(line < mapping.Generated.StartLine ||
					line > mapping.Generated.EndLine) {
				continue
			}
			result = append(result, generatedMappingMatch(loaded, file, mapping))
		}
		if len(file.Mappings) == 0 && file.PrimarySource != nil {
			result = append(result, generatedFileMatch(loaded, file, *file.PrimarySource))
		}
	}
	return result
}

func generatedMappingMatch(
	loaded loadedGeneratedManifest,
	file codegen.ManifestFile,
	mapping codegen.SourceMapping,
) generatedMatch {
	end := generatedLocation{
		Path:   file.Path,
		Line:   mapping.Generated.EndLine,
		Column: mapping.Generated.EndColumn,
	}
	return generatedMatch{
		Target:       loaded.manifest.Target.ID,
		ManifestPath: loaded.path,
		Role:         file.Role,
		Kind:         mapping.Kind,
		Contribution: mapping.Contribution,
		Source: generatedLocation{
			Path:   mapping.Source.Path,
			Line:   mapping.Source.Line,
			Column: mapping.Source.Column,
		},
		Generated: generatedLocation{
			Path:   file.Path,
			Line:   mapping.Generated.StartLine,
			Column: mapping.Generated.StartColumn,
		},
		GeneratedEnd: &end,
		Related: append(
			[]codegen.SourceOrigin(nil),
			mapping.RelatedSource...,
		),
	}
}

func generatedFileMatch(
	loaded loadedGeneratedManifest,
	file codegen.ManifestFile,
	source codegen.SourceOrigin,
) generatedMatch {
	return generatedMatch{
		Target:       loaded.manifest.Target.ID,
		ManifestPath: loaded.path,
		Role:         file.Role,
		Source: generatedLocation{
			Path:   source.Path,
			Line:   source.Line,
			Column: source.Column,
		},
		Generated: generatedLocation{Path: file.Path},
		Related: append(
			[]codegen.SourceOrigin(nil),
			file.RelatedSources...,
		),
	}
}

func sortGeneratedMatches(matches []generatedMatch) {
	sort.Slice(matches, func(left, right int) bool {
		leftMatch := matches[left]
		rightMatch := matches[right]
		leftKey := fmt.Sprintf(
			"%s\x00%s\x00%09d\x00%s\x00%s",
			leftMatch.Target,
			leftMatch.Generated.Path,
			leftMatch.Generated.Line,
			leftMatch.Kind,
			leftMatch.Contribution,
		)
		rightKey := fmt.Sprintf(
			"%s\x00%s\x00%09d\x00%s\x00%s",
			rightMatch.Target,
			rightMatch.Generated.Path,
			rightMatch.Generated.Line,
			rightMatch.Kind,
			rightMatch.Contribution,
		)
		return leftKey < rightKey
	})
}

func generatedModuleRoot(directory string) (string, error) {
	if directory == "" {
		directory = "."
	}
	current, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("resolve working directory %q: %w", directory, err)
	}
	for {
		info, statErr := os.Stat(filepath.Join(current, "go.mod"))
		if statErr == nil && !info.IsDir() {
			return current, nil
		}
		if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
			return "", fmt.Errorf("inspect module root %q: %w", current, statErr)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf(
				"find Go module from %q: no go.mod in this directory or a parent",
				directory,
			)
		}
		current = parent
	}
}

func generatedModuleRelativePath(
	moduleRoot string,
	directory string,
	input string,
) (string, error) {
	if directory == "" {
		directory = "."
	}
	candidate := input
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(directory, candidate)
	}
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve lookup path %q: %w", input, err)
	}
	relative, err := filepath.Rel(moduleRoot, absolute)
	if err != nil || (!filepath.IsLocal(relative) && relative != ".") {
		return "", fmt.Errorf("lookup path %q is outside module %q", input, moduleRoot)
	}
	return filepath.ToSlash(relative), nil
}

func writeGeneratedResult(
	writer io.Writer,
	format string,
	result generatedQueryResult,
) error {
	if format == "json" {
		encoder := json.NewEncoder(writer)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		return encoder.Encode(result)
	}
	if _, err := fmt.Fprintf(
		writer,
		"%s %s",
		result.Direction,
		formatGeneratedLocation(result.Query),
	); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(writer); err != nil {
		return err
	}
	for _, match := range result.Matches {
		if _, err := fmt.Fprintf(
			writer,
			"  %s %s -> %s",
			match.Target,
			formatGeneratedLocation(match.Source),
			formatGeneratedLocation(match.Generated),
		); err != nil {
			return err
		}
		if match.Kind != "" {
			if _, err := fmt.Fprintf(
				writer,
				" [%s %s]",
				match.Kind,
				match.Contribution,
			); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(writer); err != nil {
			return err
		}
	}
	return nil
}

func formatGeneratedLocation(location generatedLocation) string {
	result := location.Path
	if location.Line > 0 {
		result += ":" + strconv.Itoa(location.Line)
		if location.Column > 0 {
			result += ":" + strconv.Itoa(location.Column)
		}
	}
	return result
}
