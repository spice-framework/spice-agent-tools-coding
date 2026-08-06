// @import { NamedInterface } from "github.com/spice-framework/spice/annotation/modulith"

// Package diagnostic defines the shared immutable diagnostic contract consumed
// by Spice command, development, and editor integrations.
//
// @NamedInterface("diagnostic")
package diagnostic

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

const reportSchema = "spice.diagnostics/v1"

// Severity classifies the user-facing impact of a diagnostic.
type Severity string

const (
	// SeverityError prevents the requested compiler operation.
	SeverityError Severity = "error"
	// SeverityWarning identifies actionable input that does not prevent output.
	SeverityWarning Severity = "warning"
	// SeverityInformation supplies relevant compiler metadata.
	SeverityInformation Severity = "information"
	// SeverityHint identifies an optional local improvement.
	SeverityHint Severity = "hint"
)

// Position is a one-based source position. Offset is the zero-based byte
// offset when the compiler has one.
type Position struct {
	Line   int `json:"line"`
	Column int `json:"column"`
	Offset int `json:"offset,omitempty"`
}

// Range is a half-open source range.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// DisplayLocation preserves a source-mapped developer-facing path and range
// when it differs from the physical file loaded by Go.
type DisplayLocation struct {
	Path  string `json:"path"`
	Range Range  `json:"range"`
}

// Location identifies one physical file URI/path/range. Display is present
// when //line mapping or another compiler mapping changes the developer-facing
// position.
type Location struct {
	URI     string           `json:"uri,omitempty"`
	Path    string           `json:"path,omitempty"`
	Range   Range            `json:"range"`
	Display *DisplayLocation `json:"display,omitempty"`
}

// RelatedInformation connects a diagnostic to another precise source range.
type RelatedInformation struct {
	Message  string   `json:"message"`
	Location Location `json:"location"`
}

// TextEdit is one precise, version-checked edit proposed by an integration.
// The caller applying it remains responsible for matching the analyzed
// document version.
type TextEdit struct {
	Location        Location `json:"location"`
	DocumentVersion *int     `json:"document_version,omitempty"`
	NewText         string   `json:"new_text"`
}

// SuggestedFix is a narrowly safe collection of precise source edits.
type SuggestedFix struct {
	Title     string     `json:"title"`
	AppliesTo *Location  `json:"applies_to,omitempty"`
	Edits     []TextEdit `json:"edits"`
}

// Diagnostic is one stable compiler problem.
type Diagnostic struct {
	Code     string   `json:"code"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Location Location `json:"location"`

	Related []RelatedInformation `json:"related,omitempty"`
	Fixes   []SuggestedFix       `json:"fixes,omitempty"`
}

// New constructs one normalized diagnostic. Compiler-generated diagnostics
// default to error severity and a stable spice.compiler.unknown code when a
// caller omits those values.
func New(
	code string,
	severity Severity,
	message string,
	location Location,
) Diagnostic {
	return normalize(Diagnostic{
		Code:     code,
		Severity: severity,
		Message:  message,
		Location: location,
	})
}

// Code constructs a stable namespaced code from compiler stage and failure
// kind identifiers.
func Code(stage, kind string) string {
	return "spice." + codeSegment(stage, "compiler") + "." +
		codeSegment(kind, "unknown")
}

// SourceLocation constructs a location from developer-facing and physical
// source identities. The physical path supplies the file URI; the display
// path and one-based coordinates remain user-facing.
func SourceLocation(
	workspaceRoot string,
	displayPath string,
	physicalPath string,
	line int,
	column int,
	offset int,
) Location {
	return SourceMappedLocation(
		workspaceRoot,
		displayPath,
		physicalPath,
		line,
		column,
		offset,
		line,
		column,
		offset,
	)
}

// SourceMappedLocation constructs a physical location and an optional
// developer-facing mapping. Both coordinate sets are one-based.
func SourceMappedLocation(
	workspaceRoot string,
	displayPath string,
	physicalPath string,
	displayLine int,
	displayColumn int,
	displayOffset int,
	physicalLine int,
	physicalColumn int,
	physicalOffset int,
) Location {
	physicalStart, physicalEnd := sourceRange(
		physicalLine,
		physicalColumn,
		physicalOffset,
	)
	displayStart, displayEnd := sourceRange(
		displayLine,
		displayColumn,
		displayOffset,
	)
	absolutePath := absoluteSourcePath(
		workspaceRoot,
		physicalPath,
		displayPath,
	)
	display := displaySourcePath(displayPath)
	return Location{
		URI:  fileURI(absolutePath),
		Path: absolutePath,
		Range: Range{
			Start: physicalStart,
			End:   physicalEnd,
		},
		Display: mappedDisplay(
			display,
			Range{Start: displayStart, End: displayEnd},
			absolutePath,
			Range{Start: physicalStart, End: physicalEnd},
		),
	}
}

// WithRelated returns a defensive copy with deterministic related information.
func (diagnostic Diagnostic) WithRelated(
	related ...RelatedInformation,
) Diagnostic {
	result := cloneDiagnostic(diagnostic)
	result.Related = append([]RelatedInformation(nil), related...)
	sortRelated(result.Related)
	return normalize(result)
}

// WithFixes returns a defensive copy with deterministic safe suggested fixes.
func (diagnostic Diagnostic) WithFixes(fixes ...SuggestedFix) Diagnostic {
	result := cloneDiagnostic(diagnostic)
	result.Fixes = cloneFixes(fixes)
	sortFixes(result.Fixes)
	return normalize(result)
}

// Error renders a stable compiler-style diagnostic.
func (diagnostic Diagnostic) Error() string {
	item := normalize(diagnostic)
	path := item.Location.Path
	position := item.Location.Range.Start
	if item.Location.Display != nil {
		path = item.Location.Display.Path
		position = item.Location.Display.Range.Start
	}
	if path == "" {
		path = "<unknown>"
	}
	return fmt.Sprintf(
		"%s:%d:%d: [%s] %s",
		path,
		position.Line,
		position.Column,
		item.Code,
		item.Message,
	)
}

// Set is an immutable-by-construction, deterministically sorted collection.
type Set struct {
	items []Diagnostic
}

// NewSet defensively copies, normalizes, and sorts diagnostics.
func NewSet(items ...Diagnostic) Set {
	result := make([]Diagnostic, len(items))
	for index, item := range items {
		result[index] = normalize(item)
	}
	sortDiagnostics(result)
	return Set{items: result}
}

// Items returns a deep defensive copy.
func (set Set) Items() []Diagnostic {
	result := make([]Diagnostic, len(set.items))
	for index, item := range set.items {
		result[index] = cloneDiagnostic(item)
	}
	return result
}

// Len reports the number of diagnostics.
func (set Set) Len() int {
	return len(set.items)
}

// Empty reports whether the set contains no diagnostics.
func (set Set) Empty() bool {
	return len(set.items) == 0
}

// Merge returns one sorted defensive set.
func Merge(sets ...Set) Set {
	var items []Diagnostic
	for _, set := range sets {
		items = append(items, set.Items()...)
	}
	return NewSet(items...)
}

// Report is the stable machine-readable diagnostic envelope.
type Report struct {
	Schema      string       `json:"schema"`
	Success     bool         `json:"success"`
	Summary     string       `json:"summary"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// NewReport constructs a defensive report.
func NewReport(success bool, summary string, set Set) Report {
	return Report{
		Schema:      reportSchema,
		Success:     success,
		Summary:     summary,
		Diagnostics: set.Items(),
	}
}

// JSON renders deterministic indented JSON terminated by a newline.
func (report Report) JSON() ([]byte, error) {
	content, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode diagnostic report: %w", err)
	}
	return append(content, '\n'), nil
}

func normalize(item Diagnostic) Diagnostic {
	item.Code = normalizeCode(item.Code)
	item.Severity = normalizeSeverity(item.Severity)
	item.Message = strings.TrimSpace(item.Message)
	item.Location = normalizeLocation(item.Location)
	item.Related = cloneRelated(item.Related)
	sortRelated(item.Related)
	item.Fixes = cloneFixes(item.Fixes)
	sortFixes(item.Fixes)
	return item
}

func normalizeCode(value string) string {
	segments := strings.Split(strings.TrimSpace(value), ".")
	normalized := make([]string, 0, len(segments))
	for _, segment := range segments {
		if converted := codeSegment(segment, ""); converted != "" {
			normalized = append(normalized, converted)
		}
	}
	if len(normalized) >= 3 && normalized[0] == "spice" {
		return strings.Join(normalized, ".")
	}
	return Code("compiler", "unknown")
}

func codeSegment(value, fallback string) string {
	var result strings.Builder
	separator := false
	for _, character := range strings.TrimSpace(value) {
		switch {
		case unicode.IsLetter(character) || unicode.IsDigit(character):
			if separator && result.Len() != 0 {
				result.WriteByte('-')
			}
			result.WriteRune(unicode.ToLower(character))
			separator = false
		default:
			separator = true
		}
	}
	normalized := strings.Trim(result.String(), "-")
	if normalized == "" {
		return fallback
	}
	return normalized
}

func normalizeSeverity(value Severity) Severity {
	switch value {
	case SeverityError,
		SeverityWarning,
		SeverityInformation,
		SeverityHint:
		return value
	default:
		return SeverityError
	}
}

func normalizeLocation(location Location) Location {
	location.Path = displaySourcePath(location.Path)
	location.Range.Start = normalizePosition(location.Range.Start)
	location.Range.End = normalizePosition(location.Range.End)
	if positionBefore(location.Range.End, location.Range.Start) {
		location.Range.End = location.Range.Start
	}
	if location.Display != nil {
		display := *location.Display
		display.Path = displaySourcePath(display.Path)
		display.Range.Start = normalizePosition(display.Range.Start)
		display.Range.End = normalizePosition(display.Range.End)
		if positionBefore(display.Range.End, display.Range.Start) {
			display.Range.End = display.Range.Start
		}
		location.Display = &display
	}
	return location
}

func normalizePosition(position Position) Position {
	if position.Line <= 0 {
		position.Line = 1
	}
	if position.Column <= 0 {
		position.Column = 1
	}
	if position.Offset < 0 {
		position.Offset = 0
	}
	return position
}

func positionBefore(left, right Position) bool {
	if left.Line != right.Line {
		return left.Line < right.Line
	}
	if left.Column != right.Column {
		return left.Column < right.Column
	}
	return left.Offset < right.Offset
}

func displaySourcePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "<") {
		return ""
	}
	return filepath.ToSlash(filepath.Clean(value))
}

func sourceRange(
	line int,
	column int,
	offset int,
) (Position, Position) {
	if line <= 0 {
		line = 1
	}
	if column <= 0 {
		column = 1
	}
	if offset < 0 {
		offset = 0
	}
	return Position{
			Line:   line,
			Column: column,
			Offset: offset,
		},
		Position{
			Line:   line,
			Column: column + 1,
			Offset: offset + 1,
		}
}

func mappedDisplay(
	path string,
	sourceRange Range,
	physicalPath string,
	physicalRange Range,
) *DisplayLocation {
	if path == "" ||
		(path == physicalPath && sourceRange == physicalRange) {
		return nil
	}
	return &DisplayLocation{Path: path, Range: sourceRange}
}

func absoluteSourcePath(
	workspaceRoot,
	physicalPath,
	displayPath string,
) string {
	source := strings.TrimSpace(physicalPath)
	if source == "" {
		source = strings.TrimSpace(displayPath)
	}
	if source == "" || strings.HasPrefix(source, "<") {
		return ""
	}
	if !filepath.IsAbs(source) {
		root := workspaceRoot
		if root == "" {
			root = "."
		}
		source = filepath.Join(root, source)
	}
	absolute, err := filepath.Abs(source)
	if err != nil {
		return ""
	}
	return filepath.ToSlash(filepath.Clean(absolute))
}

func fileURI(absolutePath string) string {
	if absolutePath == "" {
		return ""
	}
	slashed := absolutePath
	native := filepath.FromSlash(absolutePath)
	volume := filepath.VolumeName(native)
	if volume != "" && !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return (&url.URL{Scheme: "file", Path: slashed}).String()
}

func sortDiagnostics(items []Diagnostic) {
	sort.SliceStable(items, func(leftIndex, rightIndex int) bool {
		left := items[leftIndex]
		right := items[rightIndex]
		switch {
		case left.Location.URI != right.Location.URI:
			return left.Location.URI < right.Location.URI
		case left.Location.Range.Start.Offset !=
			right.Location.Range.Start.Offset:
			return left.Location.Range.Start.Offset <
				right.Location.Range.Start.Offset
		case left.Location.Path != right.Location.Path:
			return left.Location.Path < right.Location.Path
		case left.Location.Range.Start != right.Location.Range.Start:
			return positionBefore(
				left.Location.Range.Start,
				right.Location.Range.Start,
			)
		case left.Location.Range.End != right.Location.Range.End:
			return positionBefore(
				left.Location.Range.End,
				right.Location.Range.End,
			)
		case left.Code != right.Code:
			return left.Code < right.Code
		case left.Severity != right.Severity:
			return left.Severity < right.Severity
		default:
			return left.Message < right.Message
		}
	})
}

func sortRelated(items []RelatedInformation) {
	sort.SliceStable(items, func(leftIndex, rightIndex int) bool {
		left := items[leftIndex]
		right := items[rightIndex]
		if left.Location.URI != right.Location.URI {
			return left.Location.URI < right.Location.URI
		}
		if left.Location.Range.Start != right.Location.Range.Start {
			return positionBefore(
				left.Location.Range.Start,
				right.Location.Range.Start,
			)
		}
		return left.Message < right.Message
	})
}

func sortFixes(items []SuggestedFix) {
	sort.SliceStable(items, func(leftIndex, rightIndex int) bool {
		return items[leftIndex].Title < items[rightIndex].Title
	})
}

func cloneDiagnostic(item Diagnostic) Diagnostic {
	item.Location = cloneLocation(item.Location)
	item.Related = cloneRelated(item.Related)
	item.Fixes = cloneFixes(item.Fixes)
	return item
}

func cloneRelated(items []RelatedInformation) []RelatedInformation {
	result := make([]RelatedInformation, len(items))
	for index, item := range items {
		result[index] = item
		result[index].Location = cloneLocation(item.Location)
	}
	return result
}

func cloneFixes(items []SuggestedFix) []SuggestedFix {
	result := make([]SuggestedFix, len(items))
	for index, item := range items {
		result[index] = item
		if item.AppliesTo != nil {
			location := cloneLocation(*item.AppliesTo)
			result[index].AppliesTo = &location
		}
		result[index].Edits = make([]TextEdit, len(item.Edits))
		for editIndex, edit := range item.Edits {
			result[index].Edits[editIndex] = edit
			result[index].Edits[editIndex].Location = cloneLocation(
				edit.Location,
			)
			if edit.DocumentVersion != nil {
				version := *edit.DocumentVersion
				result[index].Edits[editIndex].DocumentVersion = &version
			}
		}
	}
	return result
}

func cloneLocation(location Location) Location {
	if location.Display != nil {
		display := *location.Display
		location.Display = &display
	}
	return location
}
