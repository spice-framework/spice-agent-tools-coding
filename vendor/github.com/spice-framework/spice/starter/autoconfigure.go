package starter

// AutoConfiguration describes library-owned default beans selected by an
// explicit blank import of a package whose final path element is
// "autoconfigure".
//
// A descriptor package exposes exactly one function with this signature:
//
//	func SpiceAutoConfiguration() starter.AutoConfiguration {
//		return starter.AutoConfiguration{
//			Review: "docs/dependency-review.md",
//			Beans: []starter.AutoBean{
//				{Factory: DefaultClient, Fallback: true},
//			},
//		}
//	}
//
// Spice statically decodes the returned composite literal. It never calls the
// function, evaluates package initializers, or scans packages at runtime.
type AutoConfiguration struct {
	// Review identifies committed maintenance, licensing, and security review
	// material for the library integration.
	Review string
	// Beans lists exported factories owned by the descriptor package.
	Beans []AutoBean
}

// AutoBean describes one direct-call default provider. Factory must be an
// exported package-level function in the same autoconfigure package and must
// use the ordinary Spice provider signature contract.
type AutoBean struct {
	Factory    any
	Name       string
	Aliases    []string
	Qualifiers []string
	Primary    bool
	Fallback   bool
	Order      int64
}
