package devloop

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
)

// StructuralFingerprint hashes the valid-Go structure that can affect Spice's
// application model while excluding ordinary function and method bodies.
//
// Annotation comments, imports, declarations, signatures, fields, types, and
// top-level values remain part of the fingerprint. A body-only edit can
// therefore reuse a previously generated plan and proceed directly to the Go
// build without concealing structural changes from the compiler.
func StructuralFingerprint(name string, content []byte) ([sha256.Size]byte, error) {
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(
		files,
		name,
		content,
		parser.ParseComments|parser.SkipObjectResolution,
	)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf(
			"parse development source structure %s: %w",
			name,
			err,
		)
	}
	tokenFile := files.File(parsed.Pos())
	var normalized bytes.Buffer
	previous := 0
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		start := tokenFile.Offset(function.Body.Lbrace)
		end := tokenFile.Offset(function.Body.Rbrace) + 1
		normalized.Write(content[previous:start])
		normalized.WriteString("{}")
		previous = end
	}
	normalized.Write(content[previous:])
	return sha256.Sum256(normalized.Bytes()), nil
}
