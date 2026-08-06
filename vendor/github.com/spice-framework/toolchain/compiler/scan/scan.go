// Package scan discovers Spice annotations in Go source files.
package scan

import (
	"fmt"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spice-framework/spice/annotation"
	annotationparser "github.com/spice-framework/toolchain/compiler/parser"
)

// Target identifies the Go declaration associated with an annotation.
// It aliases the public annotation target model for compatibility.
type Target = annotation.Target

const (
	TargetPackage   = annotation.TargetPackage
	TargetType      = annotation.TargetType
	TargetFunction  = annotation.TargetFunction
	TargetMethod    = annotation.TargetMethod
	TargetParameter = annotation.TargetParameter
	TargetVariable  = annotation.TargetVariable
	TargetConstant  = annotation.TargetConstant
)

// Occurrence associates one annotation with a Go declaration.
type Occurrence struct {
	Annotation annotation.Annotation
	Target     Target
	Name       string
	File       string
}

// Result is the deterministic output of scanning a source tree.
type Result struct {
	Files       int
	Occurrences []Occurrence
}

// PathRoot converts common package patterns such as ./... into a filesystem root.
func PathRoot(input string) string {
	input = strings.TrimSpace(input)
	if input == "" || input == "./..." || input == "..." {
		return "."
	}
	return strings.TrimSuffix(input, "/...")
}

// Tree scans every non-test and test Go file under root, excluding generated
// dependency and repository-management directories.
func Tree(root string) (Result, error) {
	root = PathRoot(root)
	set := token.NewFileSet()
	result := Result{}

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "vendor", "node_modules", ".spice":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		occurrences, err := scanFile(set, path)
		if err != nil {
			return err
		}
		result.Files++
		result.Occurrences = append(result.Occurrences, occurrences...)
		return nil
	})
	if err != nil {
		return Result{}, err
	}

	sort.Slice(result.Occurrences, func(i, j int) bool {
		left := result.Occurrences[i]
		right := result.Occurrences[j]
		if left.File != right.File {
			return left.File < right.File
		}
		if left.Annotation.Position.Line != right.Annotation.Position.Line {
			return left.Annotation.Position.Line < right.Annotation.Position.Line
		}
		return left.Annotation.Name < right.Annotation.Name
	})
	return result, nil
}

func scanFile(set *token.FileSet, path string) ([]Occurrence, error) {
	file, err := goparser.ParseFile(set, path, nil, goparser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var occurrences []Occurrence
	if file.Doc != nil {
		packageOccurrences, err := parseGroup(set, file.Doc, TargetPackage, file.Name.Name, path)
		if err != nil {
			return nil, err
		}
		occurrences = append(occurrences, packageOccurrences...)
	}
	for _, declaration := range file.Decls {
		declarationOccurrences, err := scanDeclaration(
			set,
			file,
			declaration,
			path,
		)
		if err != nil {
			return nil, err
		}
		occurrences = append(occurrences, declarationOccurrences...)
	}
	return occurrences, nil
}

func scanDeclaration(
	set *token.FileSet,
	file *ast.File,
	declaration ast.Decl,
	path string,
) ([]Occurrence, error) {
	switch node := declaration.(type) {
	case *ast.FuncDecl:
		return scanFunction(set, file, node, path)
	case *ast.GenDecl:
		return scanGeneralDeclaration(set, node, path)
	default:
		return nil, nil
	}
}

func scanFunction(
	set *token.FileSet,
	file *ast.File,
	declaration *ast.FuncDecl,
	path string,
) ([]Occurrence, error) {
	target := TargetFunction
	if declaration.Recv != nil {
		target = TargetMethod
	}
	var result []Occurrence
	if declaration.Doc != nil {
		values, err := parseGroup(
			set,
			declaration.Doc,
			target,
			declaration.Name.Name,
			path,
		)
		if err != nil {
			return nil, err
		}
		result = append(result, values...)
	}
	if declaration.Type.Params == nil {
		return result, nil
	}
	lowerBound := declaration.Type.Params.Opening
	for _, field := range declaration.Type.Params.List {
		documentation := field.Doc
		if documentation == nil {
			documentation = scanParameterDocumentation(
				set,
				file,
				field,
				lowerBound,
			)
		}
		if documentation == nil {
			lowerBound = field.End()
			continue
		}
		name := "<parameter>"
		if len(field.Names) == 1 {
			name = field.Names[0].Name
		}
		values, err := parseGroup(
			set,
			documentation,
			TargetParameter,
			name,
			path,
		)
		if err != nil {
			return nil, err
		}
		result = append(result, values...)
		lowerBound = field.End()
	}
	return result, nil
}

func scanParameterDocumentation(
	set *token.FileSet,
	file *ast.File,
	field *ast.Field,
	lowerBound token.Pos,
) *ast.CommentGroup {
	if set == nil || file == nil || field == nil {
		return nil
	}
	fieldLine := set.Position(field.Pos()).Line
	var selected *ast.CommentGroup
	for _, group := range file.Comments {
		if group == nil || group.Pos() <= lowerBound ||
			group.End() >= field.Pos() {
			continue
		}
		if set.Position(group.End()).Line+1 != fieldLine {
			continue
		}
		if selected == nil || group.Pos() > selected.Pos() {
			selected = group
		}
	}
	return selected
}

func scanGeneralDeclaration(
	set *token.FileSet,
	declaration *ast.GenDecl,
	path string,
) ([]Occurrence, error) {
	target := targetForToken(declaration.Tok)
	var occurrences []Occurrence
	if declaration.Doc != nil {
		groupOccurrences, err := parseGroup(
			set,
			declaration.Doc,
			target,
			firstSpecName(declaration.Specs),
			path,
		)
		if err != nil {
			return nil, err
		}
		occurrences = append(occurrences, groupOccurrences...)
	}
	for _, specification := range declaration.Specs {
		documentation, name := specDocumentation(specification)
		if documentation == nil {
			continue
		}
		groupOccurrences, err := parseGroup(set, documentation, target, name, path)
		if err != nil {
			return nil, err
		}
		occurrences = append(occurrences, groupOccurrences...)
	}
	return occurrences, nil
}

func parseGroup(set *token.FileSet, group *ast.CommentGroup, target Target, name, path string) ([]Occurrence, error) {
	var result []Occurrence
	for _, comment := range group.List {
		lines := strings.Split(comment.Text, "\n")
		for offset, line := range lines {
			position := set.Position(comment.Pos())
			position.Line += offset
			parsed, ok, err := annotationparser.ParseComment(line, position)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			result = append(result, Occurrence{
				Annotation: parsed,
				Target:     target,
				Name:       name,
				File:       path,
			})
		}
	}
	return result, nil
}

func targetForToken(value token.Token) Target {
	if value == token.TYPE {
		return TargetType
	}
	if value == token.CONST {
		return TargetConstant
	}
	return TargetVariable
}

func firstSpecName(specs []ast.Spec) string {
	for _, spec := range specs {
		_, name := specDocumentation(spec)
		if name != "" {
			return name
		}
	}
	return "<declaration>"
}

func specDocumentation(spec ast.Spec) (*ast.CommentGroup, string) {
	switch node := spec.(type) {
	case *ast.TypeSpec:
		return node.Doc, node.Name.Name
	case *ast.ValueSpec:
		name := "<value>"
		if len(node.Names) > 0 {
			name = node.Names[0].Name
		}
		return node.Doc, name
	default:
		return nil, ""
	}
}
