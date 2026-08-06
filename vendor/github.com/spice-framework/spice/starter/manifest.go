// Package starter provides runtime starter contracts and compatibility aliases
// for portable integration metadata in annotation/sdk/starter.
package starter

import startersdk "github.com/spice-framework/spice/annotation/sdk/starter"

const (
	// Schema is the current starter manifest wire schema.
	Schema = startersdk.Schema
	// APIVersion is the compiler/runtime contract implemented by this Spice line.
	APIVersion = startersdk.APIVersion

	// ActivationExplicitConstructor requires application or generated code to
	// call a declared constructor.
	ActivationExplicitConstructor = startersdk.ActivationExplicitConstructor
	// ActivationExplicitAnnotation requires a declared qualified annotation and
	// compiler selection of its declared entrypoints.
	ActivationExplicitAnnotation = startersdk.ActivationExplicitAnnotation
)

// Portable starter metadata is owned by annotation/sdk/starter. These aliases
// preserve the pre-extraction runtime import path while starter packages move
// into independently versioned repositories.
type (
	ActivationMode = startersdk.ActivationMode
	EntryPoint     = startersdk.EntryPoint
	Dependency     = startersdk.Dependency
	ArgumentSpec   = startersdk.ArgumentSpec
	AnnotationSpec = startersdk.AnnotationSpec
	OptionSpec     = startersdk.OptionSpec
	FeatureSpec    = startersdk.FeatureSpec
	Activation     = startersdk.Activation
	Spec           = startersdk.Spec
	Manifest       = startersdk.Manifest
)

// New validates and deterministically normalizes a starter specification.
func New(spec Spec) (Manifest, error) {
	return startersdk.New(spec)
}

// Must constructs a manifest or panics for package-owned invalid metadata.
func Must(spec Spec) Manifest {
	return startersdk.Must(spec)
}

// Parse strictly decodes and validates one starter manifest.
func Parse(content []byte) (Manifest, error) {
	return startersdk.Parse(content)
}
