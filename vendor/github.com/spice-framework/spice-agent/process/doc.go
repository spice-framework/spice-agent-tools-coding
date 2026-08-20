// Package process defines provider-neutral executable resolution and process
// launch contracts used by compiled tools and daemon applications.
//
// The package describes lookup intent, process intent, verified executable
// ownership, and child ownership. VerifyExecutable opens and hashes one exact
// executable object without resolving or launching it. On systems that cannot
// execute a held descriptor, MaterializeForLaunch creates and reverifies an
// exact private snapshot while preserving explicit lease ownership. Platform
// path resolution, launch, process-tree containment, and resource joining
// remain injected implementations. In particular, this package does not claim
// that an operating system can universally contain descendants.
//
// @import { NamedInterface } from "github.com/spice-framework/spice/annotation/modulith"
// @NamedInterface("process")
package process
