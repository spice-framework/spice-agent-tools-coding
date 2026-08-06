package annotation

import "go/token"

// ImportKind identifies the binding form of one file-scoped annotation import.
type ImportKind string

const (
	// ImportNamed binds one or more descriptor symbols directly in the file.
	ImportNamed ImportKind = "named"
	// ImportNamespace binds a descriptor package behind one local qualifier.
	ImportNamespace ImportKind = "namespace"
)

// ImportBinding maps one exported descriptor symbol to its file-local name.
type ImportBinding struct {
	Imported string
	Local    string
}

// ImportDirective is one valid-Go @import declaration comment.
type ImportDirective struct {
	Kind      ImportKind
	Package   string
	Namespace string
	Bindings  []ImportBinding
	Position  token.Position
	// PhysicalPosition retains the unadjusted loaded-file identity when the
	// source uses a //line directive.
	PhysicalPosition token.Position
	Raw              string
}

// DefinitionReference is the explicit source identity selected for an
// annotation invocation.
type DefinitionReference struct {
	Package string
	Symbol  string
}

// ModuleProvenance identifies the Go-selected module content behind a
// descriptor or tool package.
type ModuleProvenance struct {
	Path               string
	Version            string
	Directory          string
	ReplacementPath    string
	ReplacementVersion string
	ReplacementDir     string
	LocalReplacement   bool
}
