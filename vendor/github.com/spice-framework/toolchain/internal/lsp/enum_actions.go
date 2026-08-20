package lsp

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"

	compilerservice "github.com/spice-framework/toolchain/compiler/service"
)

func enumCodeActions(
	source document,
	requestRange protocolRange,
	enums []compilerservice.Enum,
) []protocolCodeAction {
	var result []protocolCodeAction
	for _, enum := range enums {
		if pathKey(enum.Location.Path) != pathKey(source.path) {
			continue
		}
		anchor := protocolRangeFromCompiler(enum.Location.Range, source.content)
		if !rangesOverlap(anchor, requestRange) {
			continue
		}
		action, available := enumHelperCodeAction(source, enum)
		if available {
			result = append(result, action)
		}
	}
	return result
}

func enumHelperCodeAction(
	source document,
	enum compilerservice.Enum,
) (protocolCodeAction, bool) {
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(
		fileSet,
		source.path,
		source.content,
		parser.SkipObjectResolution,
	)
	if err != nil || parsed == nil {
		return protocolCodeAction{}, false
	}
	parseName := "Parse" + enum.Name
	hasParse := false
	hasString := false
	hasValid := false
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if function.Recv == nil {
			hasParse = hasParse || function.Name.Name == parseName
			continue
		}
		if receiverTypeName(function.Recv) != enum.Name {
			continue
		}
		hasString = hasString || function.Name.Name == "String"
		hasValid = hasValid || function.Name.Name == "Valid"
	}
	if hasParse && hasString && hasValid {
		return protocolCodeAction{}, false
	}

	stringBacked := enum.Underlying == "string"
	needsFormat := !hasParse || !hasString && !stringBacked
	formatQualifier := "fmt"
	var edits []protocolEdit
	if needsFormat {
		var imported bool
		var safe bool
		formatQualifier, imported, safe = existingFormatImport(parsed)
		if !safe {
			return protocolCodeAction{}, false
		}
		if !imported {
			importEdit, ok := formatImportEdit(
				fileSet,
				parsed,
				source.content,
			)
			if !ok {
				return protocolCodeAction{}, false
			}
			edits = append(edits, importEdit)
		}
	}

	helperSource := renderEnumHelpers(
		enum,
		formatQualifier,
		!hasParse,
		!hasString,
		!hasValid,
	)
	if helperSource == "" {
		return protocolCodeAction{}, false
	}
	edits = append(edits, protocolEdit{
		Range: protocolRangeAtOffsets(
			source.content,
			len(source.content),
			len(source.content),
		),
		NewText: helperInsertionPrefix(source.content) + helperSource,
	})
	return protocolCodeAction{
		Title:       "Generate enum helpers for " + enum.Name,
		Kind:        "refactor.rewrite",
		IsPreferred: true,
		Edit: &protocolWorkspaceEdit{DocumentChanges: []protocolDocumentEdit{{
			TextDocument: protocolOptionalVersionedDocument{
				URI:     source.uri,
				Version: source.version,
			},
			Edits: edits,
		}}},
	}, true
}

func receiverTypeName(fields *ast.FieldList) string {
	if fields == nil || len(fields.List) != 1 {
		return ""
	}
	expression := fields.List[0].Type
	if pointer, ok := expression.(*ast.StarExpr); ok {
		expression = pointer.X
	}
	identifier, ok := expression.(*ast.Ident)
	if !ok {
		return ""
	}
	return identifier.Name
}

func existingFormatImport(file *ast.File) (string, bool, bool) {
	for _, specification := range file.Imports {
		path, err := strconv.Unquote(specification.Path.Value)
		if err != nil || path != "fmt" {
			continue
		}
		if specification.Name == nil || specification.Name.Name == "fmt" {
			return "fmt", true, true
		}
		switch specification.Name.Name {
		case ".":
			return "", true, true
		case "_":
			return "", true, false
		default:
			return specification.Name.Name, true, true
		}
	}
	return "fmt", false, true
}

func formatImportEdit(
	fileSet *token.FileSet,
	file *ast.File,
	content []byte,
) (protocolEdit, bool) {
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.IMPORT {
			continue
		}
		if general.Lparen.IsValid() {
			offset, valid := tokenOffset(fileSet, general.Lparen+1, content)
			if !valid {
				return protocolEdit{}, false
			}
			return protocolEdit{
				Range:   protocolRangeAtOffsets(content, offset, offset),
				NewText: "\n\t\"fmt\"",
			}, true
		}
		offset, valid := tokenOffset(fileSet, general.Pos(), content)
		if !valid {
			return protocolEdit{}, false
		}
		return protocolEdit{
			Range:   protocolRangeAtOffsets(content, offset, offset),
			NewText: "import \"fmt\"\n",
		}, true
	}
	offset, valid := tokenOffset(fileSet, file.Name.End(), content)
	if !valid {
		return protocolEdit{}, false
	}
	return protocolEdit{
		Range:   protocolRangeAtOffsets(content, offset, offset),
		NewText: "\n\nimport \"fmt\"",
	}, true
}

func tokenOffset(
	fileSet *token.FileSet,
	position token.Pos,
	content []byte,
) (int, bool) {
	if fileSet == nil || !position.IsValid() {
		return 0, false
	}
	offset := fileSet.PositionFor(position, false).Offset
	return offset, offset >= 0 && offset <= len(content)
}

func renderEnumHelpers(
	enum compilerservice.Enum,
	formatQualifier string,
	includeParse bool,
	includeString bool,
	includeValid bool,
) string {
	formatCall := func(name string) string {
		if formatQualifier == "" {
			return name
		}
		return formatQualifier + "." + name
	}
	var content strings.Builder
	if includeParse {
		fmt.Fprintf(
			&content,
			"func Parse%s(value %s) (%s, error) {\n\tresult := %s(value)\n\tif !result.Valid() {\n\t\tvar zero %s\n\t\treturn zero, %s(\"invalid %s value %%v\", value)\n\t}\n\treturn result, nil\n}\n",
			enum.Name,
			enum.Underlying,
			enum.Name,
			enum.Name,
			enum.Name,
			formatCall("Errorf"),
			enum.Name,
		)
	}
	if includeString {
		if content.Len() != 0 {
			content.WriteString("\n")
		}
		fmt.Fprintf(&content, "func (value %s) String() string {\n", enum.Name)
		if enum.Underlying == "string" {
			content.WriteString("\treturn string(value)\n")
		} else {
			fmt.Fprintf(
				&content,
				"\treturn %s(%s(value))\n",
				formatCall("Sprint"),
				enum.Underlying,
			)
		}
		content.WriteString("}\n")
	}
	if includeValid {
		if content.Len() != 0 {
			content.WriteString("\n")
		}
		fmt.Fprintf(&content, "func (value %s) Valid() bool {\n\tswitch value {\n", enum.Name)
		content.WriteString("\tcase ")
		for index, member := range enum.Members {
			if index != 0 {
				content.WriteString(", ")
			}
			content.WriteString(member.Name)
		}
		content.WriteString(":\n\t\treturn true\n\tdefault:\n\t\treturn false\n\t}\n}\n")
	}
	return content.String()
}

func helperInsertionPrefix(content []byte) string {
	if len(content) == 0 {
		return ""
	}
	if content[len(content)-1] == '\n' {
		return "\n"
	}
	return "\n\n"
}
