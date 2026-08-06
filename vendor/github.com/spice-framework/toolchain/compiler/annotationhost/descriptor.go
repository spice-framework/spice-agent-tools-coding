package annotationhost

import (
	"fmt"
	"slices"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/spice/annotation/sdk"
)

// ValidateDescriptor proves that one statically decoded descriptor and its
// declared handler belong to this client's standard Go-resolved tool.
func (client *Client) ValidateDescriptor(
	descriptorPackage string,
	descriptorSymbol string,
	definition sdk.Definition,
	provenance annotation.ModuleProvenance,
) error {
	if client == nil {
		return fmt.Errorf(
			"validate annotation descriptor %q: tool client is nil",
			definition.Name,
		)
	}
	if err := ValidateDescriptorToolModule(
		provenance,
		client.Provenance(),
	); err != nil {
		return err
	}
	if !slices.Contains(
		client.DescriptorPackages(),
		descriptorPackage,
	) {
		return fmt.Errorf(
			"annotation descriptor %q package %q is not declared by its tool",
			definition.Name,
			descriptorPackage,
		)
	}
	for _, handler := range client.Handlers() {
		if handler.Descriptor.Package != descriptorPackage ||
			handler.Descriptor.Name != descriptorSymbol {
			continue
		}
		return nil
	}
	return fmt.Errorf(
		"annotation descriptor %q requires a missing tool registration for %s.%s",
		definition.Name,
		descriptorPackage,
		descriptorSymbol,
	)
}
