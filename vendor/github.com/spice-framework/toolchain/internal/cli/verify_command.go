package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spice-framework/toolchain/compiler/diagnostic"
	diagnosticadapt "github.com/spice-framework/toolchain/compiler/diagnostic/adapt"
	"github.com/spice-framework/toolchain/compiler/load"
	compilerservice "github.com/spice-framework/toolchain/compiler/service"
	compilerstyle "github.com/spice-framework/toolchain/compiler/style"
)

type diagnosticFormat string

const (
	diagnosticFormatText diagnosticFormat = "text"
	diagnosticFormatJSON diagnosticFormat = "json"
)

type verifyArguments struct {
	format     diagnosticFormat
	formatSet  bool
	patterns   []string
	profile    compilerstyle.Profile
	profileSet bool
}

// NewVerifyHandler constructs the annotation verification command handler.
func NewVerifyHandler(runtime *Runtime) (Handler, error) {
	return newLoaderCommandHandler(
		runtime,
		[]string{"verify"},
		func(runtime *Runtime, invocation Invocation) int {
			return verifyCommand(
				invocation.Arguments,
				invocation.Stdout,
				invocation.Stderr,
				runtime.options,
				runtime.loader,
			)
		},
	)
}

func verifyCommand(
	arguments []string,
	stdout io.Writer,
	stderr io.Writer,
	options load.Options,
	loader programLoader,
) int {
	parsed, err := parseVerifyArguments(arguments)
	if err != nil {
		if writeErr := writef(stderr, "Spice verification failed: %v\n", err); writeErr != nil {
			return 1
		}
		return 2
	}
	return verifyPrepared(parsed, stdout, stderr, options, loader)
}

func verifyPrepared(
	arguments verifyArguments,
	stdout io.Writer,
	stderr io.Writer,
	options load.Options,
	loader programLoader,
) int {
	service, err := newCompilerAnalysisService(options, loader)
	if err != nil {
		return reportVerification(
			arguments.format,
			false,
			"Spice verification failed: compiler metadata error.",
			diagnosticadapt.Failure("metadata", "invalid", err.Error()),
			stdout,
			stderr,
		)
	}
	root := diagnosticWorkspaceRoot(options)
	result, analysisErr := service.Analyze(
		context.Background(),
		compilerservice.Request{
			WorkspaceRoot: root,
			Patterns:      arguments.patterns,
			Mode:          compilerservice.AnalysisValidate,
			Profile:       arguments.profile,
		},
	)
	closeErr := closeCompilerAnalysisService(service)
	if analysisErr != nil {
		return reportVerification(
			arguments.format,
			false,
			"Spice verification failed: compiler analysis error.",
			diagnosticadapt.Failure("analysis", "failed", analysisErr.Error()),
			stdout,
			stderr,
		)
	}
	if closeErr != nil {
		return reportVerification(
			arguments.format,
			false,
			"Spice verification failed: annotation tool shutdown error.",
			diagnosticadapt.Failure(
				"annotation-tool",
				"shutdown",
				closeErr.Error(),
			),
			stdout,
			stderr,
		)
	}
	diagnostics := result.Diagnostics()
	if !diagnostics.Empty() {
		return reportVerification(
			arguments.format,
			false,
			verificationDiagnosticSummary(result, diagnostics),
			diagnostics,
			stdout,
			stderr,
		)
	}

	summary := fmt.Sprintf(
		"Spice verification passed: %d annotations in %d Go files.",
		len(result.Annotations()),
		result.Files(),
	)
	return reportVerification(
		arguments.format,
		true,
		summary,
		diagnostic.NewSet(),
		stdout,
		stderr,
	)
}

func parseVerifyArguments(arguments []string) (verifyArguments, error) {
	result := verifyArguments{format: diagnosticFormatText}
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch {
		case argument == "--format" ||
			strings.HasPrefix(argument, "--format="):
			value, next, err := moduleOptionValue(
				arguments,
				index,
				"--format",
				result.formatSet,
				"text or json",
			)
			if err != nil {
				return verifyArguments{}, err
			}
			index = next
			result.format = diagnosticFormat(value)
			result.formatSet = true
		case argument == "--profile" ||
			strings.HasPrefix(argument, "--profile="):
			value, next, err := moduleOptionValue(
				arguments,
				index,
				"--profile",
				result.profileSet,
				string(compilerstyle.ProfileJavaStructured),
			)
			if err != nil {
				return verifyArguments{}, err
			}
			index = next
			result.profile = compilerstyle.Profile(value)
			result.profileSet = true
		case strings.HasPrefix(argument, "-"):
			return verifyArguments{}, fmt.Errorf(
				"unknown verification option %q",
				argument,
			)
		default:
			result.patterns = append(result.patterns, argument)
		}
	}
	switch result.format {
	case diagnosticFormatText, diagnosticFormatJSON:
	default:
		return verifyArguments{}, fmt.Errorf(
			"unsupported diagnostic format %q; expected text or json",
			result.format,
		)
	}
	if len(result.patterns) == 0 {
		result.patterns = []string{"./..."}
	}
	if err := compilerstyle.ValidateProfile(result.profile); err != nil {
		return verifyArguments{}, err
	}
	return result, nil
}

func verificationDiagnosticSummary(
	result compilerservice.Result,
	diagnostics diagnostic.Set,
) string {
	if modelDiagnostics := result.ApplicationModel().Diagnostics(); len(modelDiagnostics) != 0 {
		return verificationSummary(modelDiagnostics)
	}
	items := diagnostics.Items()
	stage := "compiler"
	if len(items) != 0 {
		segments := strings.Split(items[0].Code, ".")
		if len(segments) >= 3 {
			stage = segments[1]
		}
	}
	description := map[string]string{
		"load":            "package loading",
		"resolution":      "annotation resolution",
		"validation":      "annotation validation",
		"annotation-tool": "annotation tool",
		"provider":        "provider catalog",
		"style":           "style profile",
		"modulith":        "module architecture",
	}[stage]
	if stage == "starter" {
		description = "starter dependency alignment"
		if len(items) != 0 &&
			strings.Contains(items[0].Code, "missing-entrypoint") {
			description = "starter entrypoint"
		}
	}
	if description == "" {
		description = "compiler"
	}
	return fmt.Sprintf(
		"Spice verification failed: %d %s error(s).",
		len(items),
		description,
	)
}

func reportVerification(
	format diagnosticFormat,
	success bool,
	summary string,
	diagnostics diagnostic.Set,
	stdout io.Writer,
	stderr io.Writer,
) int {
	if format == diagnosticFormatJSON {
		content, err := diagnostic.NewReport(
			success,
			summary,
			diagnostics,
		).JSON()
		if err != nil {
			if writeErr := writef(stderr, "Spice verification failed: %v\n", err); writeErr != nil {
				return 1
			}
			return 1
		}
		if _, err := stdout.Write(content); err != nil {
			return 1
		}
		if success {
			return 0
		}
		return 1
	}
	if success {
		if err := writef(stdout, "%s\n", summary); err != nil {
			return 1
		}
		return 0
	}
	for _, item := range diagnostics.Items() {
		if _, err := fmt.Fprintln(stderr, item.Error()); err != nil {
			return 1
		}
	}
	if _, err := fmt.Fprintln(stderr, summary); err != nil {
		return 1
	}
	return 1
}

func diagnosticWorkspaceRoot(options load.Options) string {
	if options.Dir == "" {
		return "."
	}
	return options.Dir
}
