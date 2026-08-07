package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	releaseProfile    = "go-module-v1"
	releaseRepository = "spice-agent-tools-coding"
	releaseVersion    = "v0.1.0-preview.1"
)

type releaseMetadata struct {
	Schema     int    `json:"schema"`
	Profile    string `json:"profile"`
	Repository string `json:"repository"`
	Module     string `json:"module"`
	Version    string `json:"version"`
}

func checkReleaseMetadata(root string) error {
	content, err := os.ReadFile(filepath.Join(root, "spice-release.json")) // #nosec G304 -- fixed repository metadata path.
	if err != nil {
		return fmt.Errorf("read release metadata: %w", err)
	}
	return validateReleaseMetadata(content)
}

func validateReleaseMetadata(content []byte) error {
	expected := releaseMetadata{
		Schema:     1,
		Profile:    releaseProfile,
		Repository: releaseRepository,
		Module:     modulePath,
		Version:    releaseVersion,
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var actual releaseMetadata
	if err := decoder.Decode(&actual); err != nil {
		return fmt.Errorf("decode release metadata: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("release metadata has trailing JSON values")
	}
	if actual != expected {
		return fmt.Errorf(
			"release metadata must identify schema 1 profile %q repository %q module %q version %q",
			expected.Profile,
			expected.Repository,
			expected.Module,
			expected.Version,
		)
	}
	canonical, err := json.MarshalIndent(expected, "", "  ")
	if err != nil {
		return fmt.Errorf("encode canonical release metadata: %w", err)
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(content, canonical) {
		return errors.New("release metadata is valid but is not in canonical deterministic form")
	}
	return nil
}
