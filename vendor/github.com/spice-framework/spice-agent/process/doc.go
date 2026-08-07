// Package process defines provider-neutral executable resolution and process
// launch contracts used by compiled tools and daemon applications.
//
// The package describes lookup intent, process intent, and ownership only.
// Platform path resolution, launch, process-tree containment, and resource
// joining belong to injected implementations. In particular, this package
// does not claim that an operating system can universally contain descendants.
//
// @import { NamedInterface } from "github.com/spice-framework/spice/annotation/modulith"
// @NamedInterface("process")
package process
