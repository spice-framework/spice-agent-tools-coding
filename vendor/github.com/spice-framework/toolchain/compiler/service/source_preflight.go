package service

import (
	"bytes"
	goscanner "go/scanner"
	"go/token"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spice-framework/toolchain/compiler/diagnostic"
)

const (
	annotationCommentFixTitle = "Convert to a valid Spice annotation comment"
	legacyImportFixTitle      = "Replace @spice.import with @import"
)

func rawAnnotationDiagnostics(
	workspaceRoot string,
	overlay map[string]Document,
) diagnostic.Set {
	var items []diagnostic.Diagnostic
	for filePath, document := range overlay {
		if !strings.EqualFold(filepath.Ext(filePath), ".go") {
			continue
		}
		items = append(
			items,
			rawAnnotationsInDocument(workspaceRoot, filePath, document)...,
		)
	}
	return diagnostic.NewSet(items...)
}

func rawAnnotationsInDocument(
	workspaceRoot string,
	filePath string,
	document Document,
) []diagnostic.Diagnostic {
	fileSet := token.NewFileSet()
	file := fileSet.AddFile(filePath, -1, len(document.Content))
	var sourceScanner goscanner.Scanner
	sourceScanner.Init(file, document.Content, nil, goscanner.ScanComments)

	var items []diagnostic.Diagnostic
	for {
		position, kind, literal := sourceScanner.Scan()
		if kind == token.EOF {
			return items
		}
		if kind != token.ILLEGAL || literal != "@" {
			continue
		}
		offset := file.Offset(position)
		if !isRawAnnotation(document.Content, offset) {
			continue
		}
		sourcePosition := fileSet.Position(position)
		location := diagnostic.SourceLocation(
			workspaceRoot,
			filePath,
			filePath,
			sourcePosition.Line,
			sourcePosition.Column,
			offset,
		)
		editLocation := location
		editLocation.Range.End = editLocation.Range.Start
		if editLocation.Display != nil {
			display := *editLocation.Display
			display.Range = editLocation.Range
			editLocation.Display = &display
		}
		version := document.Version
		items = append(items, diagnostic.New(
			diagnostic.Code("source", "annotation-comment"),
			diagnostic.SeverityError,
			"Spice annotations must remain valid Go comments; "+
				"write // @... and let the editor conceal the comment prefix",
			location,
		).WithFixes(diagnostic.SuggestedFix{
			Title: annotationCommentFixTitle,
			Edits: []diagnostic.TextEdit{{
				Location:        editLocation,
				DocumentVersion: &version,
				NewText:         "// ",
			}},
		}))
	}
}

func isRawAnnotation(content []byte, offset int) bool {
	if offset < 0 || offset >= len(content) || content[offset] != '@' {
		return false
	}
	lineStart := bytes.LastIndexByte(content[:offset], '\n') + 1
	if !containsOnlyIndent(content[lineStart:offset]) {
		return false
	}
	nameEnd, valid := annotationNameEnd(content, offset+1)
	if !valid {
		return false
	}
	return isAnnotationTerminator(content, nameEnd)
}

func containsOnlyIndent(content []byte) bool {
	for _, value := range content {
		if value != ' ' && value != '\t' {
			return false
		}
	}
	return true
}

func annotationNameEnd(content []byte, start int) (int, bool) {
	index := start
	for {
		end, valid := annotationNameSegmentEnd(content, index)
		if !valid {
			return 0, false
		}
		index = end
		if index >= len(content) || content[index] != '.' {
			return index, true
		}
		index++
	}
}

func annotationNameSegmentEnd(content []byte, start int) (int, bool) {
	if start >= len(content) {
		return 0, false
	}
	first, size := utf8.DecodeRune(content[start:])
	if first == utf8.RuneError && size == 1 ||
		!isAnnotationNameStart(first) {
		return 0, false
	}
	index := start + size
	for index < len(content) {
		current, currentSize := utf8.DecodeRune(content[index:])
		if current == utf8.RuneError && currentSize == 1 ||
			!isAnnotationNameContinue(current) {
			break
		}
		index += currentSize
	}
	return index, true
}

func isAnnotationTerminator(content []byte, index int) bool {
	if index >= len(content) {
		return true
	}
	switch content[index] {
	case '(', ' ', '\t', '\r', '\n':
		return true
	default:
		return false
	}
}

func isAnnotationNameStart(value rune) bool {
	return value == '_' || unicode.IsLetter(value)
}

func isAnnotationNameContinue(value rune) bool {
	return isAnnotationNameStart(value) || unicode.IsDigit(value)
}
