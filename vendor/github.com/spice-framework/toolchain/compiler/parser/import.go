package parser

import (
	"errors"
	"fmt"
	"go/token"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spice-framework/spice/annotation"
)

const (
	importDirectiveName       = "@import"
	legacyImportDirectiveName = "@spice.import"
)

var errLegacyImportDirective = errors.New(
	"@spice.import is no longer supported; replace it with @import",
)

// IsLegacyImportError reports whether parsing found the retired import
// spelling. Callers use this to attach the hard-cut diagnostic and exact
// replacement without accepting the old directive.
func IsLegacyImportError(err error) bool {
	return errors.Is(err, errLegacyImportDirective)
}

// ParseImportComment parses one file-scoped annotation import declaration. The
// bool distinguishes ordinary comments from malformed import declarations.
func ParseImportComment(
	input string,
	position token.Position,
) (annotation.ImportDirective, bool, error) {
	raw := input
	var comment bool
	input, position, comment = annotationCommentBody(input, position)
	if !comment {
		return annotation.ImportDirective{}, false, nil
	}
	if directivePrefix(input, legacyImportDirectiveName) {
		return annotation.ImportDirective{}, true, errLegacyImportDirective
	}
	if !directivePrefix(input, importDirectiveName) {
		return annotation.ImportDirective{}, false, nil
	}
	parser := importParser{
		input:    input,
		offset:   len(importDirectiveName),
		position: position,
	}
	directive, err := parser.parse()
	if err != nil {
		return annotation.ImportDirective{}, true, err
	}
	directive.Raw = raw
	return directive, true, nil
}

func directivePrefix(input, name string) bool {
	return strings.HasPrefix(input, name) &&
		(len(input) == len(name) ||
			unicode.IsSpace(rune(input[len(name)])))
}

type importParser struct {
	input    string
	offset   int
	position token.Position
}

func (parser *importParser) parse() (annotation.ImportDirective, error) {
	if !parser.requireSpace() {
		return annotation.ImportDirective{}, parser.errorf(
			"annotation import requires a binding clause",
		)
	}
	if parser.consume('*') {
		return parser.parseNamespace()
	}
	if parser.consume('{') {
		return parser.parseNamed()
	}
	return annotation.ImportDirective{}, parser.errorf(
		"annotation import requires '{ ... }' or '* as alias'",
	)
}

func (parser *importParser) parseNamespace() (annotation.ImportDirective, error) {
	if !parser.requireSpace() || !parser.consumeWord("as") || !parser.requireSpace() {
		return annotation.ImportDirective{}, parser.errorf(
			"namespace annotation import requires '* as alias'",
		)
	}
	namespace := parser.parseIdentifier()
	if namespace == "" || namespace == "_" {
		return annotation.ImportDirective{}, parser.errorf(
			"namespace annotation import requires a usable alias",
		)
	}
	packagePath, err := parser.parseFromPackage()
	if err != nil {
		return annotation.ImportDirective{}, err
	}
	return annotation.ImportDirective{
		Kind:      annotation.ImportNamespace,
		Package:   packagePath,
		Namespace: namespace,
		Position:  parser.position,
	}, nil
}

func (parser *importParser) parseNamed() (annotation.ImportDirective, error) {
	parser.skipSpace()
	if parser.consume('}') {
		return annotation.ImportDirective{}, parser.errorf(
			"named annotation import requires at least one symbol",
		)
	}
	var bindings []annotation.ImportBinding
	seenImported := make(map[string]struct{})
	for {
		imported := parser.parseIdentifier()
		if imported == "" || imported == "_" || !exportedIdentifier(imported) {
			return annotation.ImportDirective{}, parser.errorf(
				"named annotation imports require exported descriptor symbols",
			)
		}
		if _, duplicate := seenImported[imported]; duplicate {
			return annotation.ImportDirective{}, parser.errorf(
				"annotation import repeats symbol %q",
				imported,
			)
		}
		seenImported[imported] = struct{}{}
		local := imported
		parser.skipSpace()
		if parser.consumeWord("as") {
			if !parser.requireSpace() {
				return annotation.ImportDirective{}, parser.errorf(
					"annotation import alias requires a local name",
				)
			}
			local = parser.parseIdentifier()
			if local == "" || local == "_" {
				return annotation.ImportDirective{}, parser.errorf(
					"annotation import alias requires a usable local name",
				)
			}
		}
		bindings = append(bindings, annotation.ImportBinding{
			Imported: imported,
			Local:    local,
		})
		parser.skipSpace()
		if parser.consume('}') {
			break
		}
		if !parser.consume(',') {
			return annotation.ImportDirective{}, parser.errorf(
				"expected ',' or '}' in named annotation import",
			)
		}
		parser.skipSpace()
		if parser.peek() == '}' {
			return annotation.ImportDirective{}, parser.errorf(
				"trailing commas are not supported in annotation imports",
			)
		}
	}
	packagePath, err := parser.parseFromPackage()
	if err != nil {
		return annotation.ImportDirective{}, err
	}
	return annotation.ImportDirective{
		Kind:     annotation.ImportNamed,
		Package:  packagePath,
		Bindings: bindings,
		Position: parser.position,
	}, nil
}

func (parser *importParser) parseFromPackage() (string, error) {
	if !parser.requireSpace() ||
		!parser.consumeWord("from") ||
		!parser.requireSpace() {
		return "", parser.errorf(
			"annotation import requires 'from \"package/path\"'",
		)
	}
	packagePath, err := parser.parseQuotedString()
	if err != nil {
		return "", err
	}
	parser.skipSpace()
	if !parser.eof() {
		return "", parser.errorf("unexpected trailing annotation import content")
	}
	if err := validateAnnotationImportPath(packagePath); err != nil {
		return "", parser.errorf("%v", err)
	}
	return packagePath, nil
}

func (parser *importParser) parseQuotedString() (string, error) {
	if parser.peek() != '"' && parser.peek() != '`' {
		return "", parser.errorf("annotation import package must be a Go string")
	}
	quote := parser.peek()
	start := parser.offset
	parser.offset++
	for !parser.eof() {
		current := parser.peek()
		parser.offset++
		if current == quote {
			value, err := strconv.Unquote(parser.input[start:parser.offset])
			if err != nil {
				return "", parser.errorf(
					"invalid annotation import package: %v",
					err,
				)
			}
			return value, nil
		}
		if current == '\\' && quote == '"' && !parser.eof() {
			parser.offset++
		}
	}
	return "", parser.errorf("unterminated annotation import package")
}

func (parser *importParser) parseIdentifier() string {
	start := parser.offset
	if parser.eof() {
		return ""
	}
	character, size := utf8.DecodeRuneInString(parser.input[parser.offset:])
	if !unicode.IsLetter(character) && character != '_' {
		return ""
	}
	parser.offset += size
	for !parser.eof() {
		character, size = utf8.DecodeRuneInString(parser.input[parser.offset:])
		if !unicode.IsLetter(character) &&
			!unicode.IsDigit(character) &&
			character != '_' {
			break
		}
		parser.offset += size
	}
	return parser.input[start:parser.offset]
}

func (parser *importParser) consumeWord(word string) bool {
	if !strings.HasPrefix(parser.input[parser.offset:], word) {
		return false
	}
	end := parser.offset + len(word)
	if end < len(parser.input) {
		character, _ := utf8.DecodeRuneInString(parser.input[end:])
		if unicode.IsLetter(character) ||
			unicode.IsDigit(character) ||
			character == '_' {
			return false
		}
	}
	parser.offset = end
	return true
}

func (parser *importParser) requireSpace() bool {
	start := parser.offset
	parser.skipSpace()
	return parser.offset > start
}

func (parser *importParser) skipSpace() {
	for !parser.eof() {
		character, size := utf8.DecodeRuneInString(parser.input[parser.offset:])
		if !unicode.IsSpace(character) {
			return
		}
		parser.offset += size
	}
}

func (parser *importParser) consume(expected byte) bool {
	if parser.eof() || parser.input[parser.offset] != expected {
		return false
	}
	parser.offset++
	return true
}

func (parser *importParser) peek() byte {
	if parser.eof() {
		return 0
	}
	return parser.input[parser.offset]
}

func (parser *importParser) eof() bool {
	return parser.offset >= len(parser.input)
}

func (parser *importParser) errorf(format string, arguments ...any) error {
	column := parser.position.Column
	if column <= 0 {
		column = 1
	}
	return fmt.Errorf(
		"%s:%d:%d: %s",
		parser.position.Filename,
		max(parser.position.Line, 1),
		column+parser.offset,
		fmt.Sprintf(format, arguments...),
	)
}

func exportedIdentifier(value string) bool {
	first, _ := utf8.DecodeRuneInString(value)
	return unicode.IsUpper(first)
}

func validateAnnotationImportPath(value string) error {
	switch {
	case value == "":
		return fmt.Errorf("annotation import package must not be empty")
	case strings.TrimSpace(value) != value:
		return fmt.Errorf("annotation import package must not contain surrounding whitespace")
	case strings.Contains(value, "\\"):
		return fmt.Errorf("annotation import package must use '/' separators")
	case strings.HasPrefix(value, ".") || strings.HasPrefix(value, "/"):
		return fmt.Errorf("annotation import package must be an absolute Go import path")
	case strings.Contains(value, "//"):
		return fmt.Errorf("annotation import package contains an empty path segment")
	default:
		return nil
	}
}
