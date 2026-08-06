// Package annotation defines the public syntax model used by the Spice compiler.
package annotation

import "go/token"

// Kind identifies the parsed representation of an annotation value.
type Kind string

const (
	KindString     Kind = "string"
	KindInteger    Kind = "integer"
	KindBoolean    Kind = "boolean"
	KindIdentifier Kind = "identifier"
	KindList       Kind = "list"
)

// ValueDomain identifies a compiler-owned semantic value space shared by CLI,
// LSP, and editor clients.
type ValueDomain string

const (
	ValueDomainNone        ValueDomain = ""
	ValueDomainGoInterface ValueDomain = "go-interface"
)

// Value is one typed annotation argument value.
type Value struct {
	Kind       Kind
	String     string
	Integer    int64
	Boolean    bool
	Identifier string
	List       []Value
}

// Argument is either named (Name is set) or positional (Name is empty).
type Argument struct {
	Name  string
	Value Value
}

// Annotation represents one parsed Spice annotation.
type Annotation struct {
	Name      string
	Arguments []Argument
	Position  token.Position
	Raw       string
}
