// @import { NamedInterface } from "github.com/spice-framework/spice/annotation/modulith"

// Package targetid defines the stable generated-package identity shared by
// lexical entrypoint preparation and typed generation.
//
// @NamedInterface("targetid")
package targetid

import "strings"

// Default derives a deterministic, import-safe target ID from an application
// name. The result begins with a lower-case ASCII letter and otherwise contains
// only lower-case ASCII letters, digits, and single underscores.
func Default(name string) string {
	lower := strings.ToLower(name)
	var result strings.Builder
	for _, character := range []byte(lower) {
		validLetter := character >= 'a' && character <= 'z'
		validDigit := character >= '0' && character <= '9'
		switch {
		case validLetter || (validDigit && result.Len() != 0):
			result.WriteByte(character)
		case result.Len() != 0 &&
			result.String()[result.Len()-1] != '_':
			result.WriteByte('_')
		}
	}
	id := strings.TrimSuffix(result.String(), "_")
	if id == "" {
		return "application"
	}
	return id
}
