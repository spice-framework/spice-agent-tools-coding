package generate

import (
	"go/token"
	"go/types"
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spice-framework/toolchain/compiler/provider"
)

func providerVariableNames(providers []provider.Provider) []string {
	result := make([]string, len(providers))
	baseCounts := make(map[string]int, len(providers))
	for index, item := range providers {
		baseCounts[providerVariableBase(item, index)]++
	}
	used := make(map[string]int, len(providers))
	for index, item := range providers {
		base := providerVariableBase(item, index)
		if baseCounts[base] > 1 {
			packageName := localGeneratedIdentifier(
				path.Base(item.PackagePath),
				"pkg",
			)
			base = packageName + exportedGeneratedIdentifier(base, "Bean")
		}
		used[base]++
		result[index] = base
		if used[base] > 1 {
			result[index] += strconv.Itoa(used[base])
		}
	}
	return result
}

func providerVariableBase(item provider.Provider, index int) string {
	return localGeneratedIdentifier(
		semanticProviderName(item),
		"provider"+strconv.Itoa(index),
	)
}

func semanticProviderName(item provider.Provider) string {
	name := item.Name
	if name == "" {
		name = item.Symbol.Name
	}
	if !item.ExplicitName && (name == "New" || name == "new") {
		if outputName := generatedTypeName(item.Output); outputName != "Interface" {
			return outputName
		}
	}
	if !item.ExplicitName &&
		(strings.HasPrefix(name, "New") ||
			strings.HasPrefix(name, "new")) &&
		len(name) > len("New") {
		trimmed := name[len("New"):]
		if token.IsIdentifier(trimmed) {
			name = trimmed
		}
	}
	return name
}

func localGeneratedIdentifier(name, fallback string) string {
	name = normalizeGeneratedIdentifier(name, fallback)
	name = lowerGeneratedInitialism(name)
	if !token.IsIdentifier(name) {
		return fallback
	}
	if generatedLocalReserved(name) {
		return name + "Bean"
	}
	return name
}

func lowerGeneratedInitialism(name string) string {
	runes := []rune(name)
	if len(runes) == 0 {
		return name
	}
	end := 1
	for end < len(runes) && unicode.IsUpper(runes[end]) {
		if end+1 < len(runes) && unicode.IsLower(runes[end+1]) {
			break
		}
		end++
	}
	for index := range end {
		runes[index] = unicode.ToLower(runes[index])
	}
	return string(runes)
}

func exportedGeneratedIdentifier(name, fallback string) string {
	name = normalizeGeneratedIdentifier(name, fallback)
	first, size := utf8.DecodeRuneInString(name)
	name = string(unicode.ToUpper(first)) + name[size:]
	if !token.IsIdentifier(name) || !token.IsExported(name) {
		return fallback
	}
	return name
}

func normalizeGeneratedIdentifier(name, fallback string) string {
	if token.IsIdentifier(name) {
		return name
	}
	var builder strings.Builder
	upperNext := false
	for _, character := range name {
		if unicode.IsLetter(character) ||
			character == '_' ||
			builder.Len() != 0 && unicode.IsDigit(character) {
			if upperNext {
				character = unicode.ToUpper(character)
				upperNext = false
			}
			builder.WriteRune(character)
			continue
		}
		upperNext = builder.Len() != 0
	}
	if normalized := builder.String(); token.IsIdentifier(normalized) {
		return normalized
	}
	return fallback
}

func generatedLocalReserved(name string) bool {
	switch name {
	case "application",
		"append",
		"authorizer",
		"cap",
		"clear",
		"close",
		"complex",
		"configurationSchema",
		"configurationSnapshot",
		"copy",
		"ctx",
		"delete",
		"dependencies",
		"error",
		"err",
		"false",
		"httpObservers",
		"imag",
		"len",
		"logger",
		"make",
		"managementMetrics",
		"max",
		"min",
		"new",
		"nil",
		"observers",
		"options",
		"panic",
		"print",
		"println",
		"real",
		"recover",
		"true":
		return true
	}
	return false
}

func generatedTypeName(value types.Type) string {
	switch typed := types.Unalias(value).(type) {
	case *types.Named:
		if typed.Obj() != nil {
			return typed.Obj().Name()
		}
	case *types.Pointer:
		return generatedTypeName(typed.Elem())
	}
	return "Interface"
}

func dependencyKey(providerID string, parameter int) string {
	return providerID + "\x00" + strconv.Itoa(parameter)
}
