package cli

import (
	"errors"

	"github.com/spice-framework/toolchain/compiler/load"
)

// Runtime owns the explicit compiler and Go command seams used by production
// command handlers. It contains no process-global registry or mutable package
// state.
type Runtime struct {
	options load.Options
	loader  programLoader
	builder buildExecutor
	tester  testExecutor
}

// NewRuntime constructs the production CLI runtime.
func NewRuntime() *Runtime {
	return newRuntime(
		load.Options{},
		load.Load,
		executeGoBuild,
		executeGoTest,
	)
}

func newRuntime(
	options load.Options,
	loader programLoader,
	builder buildExecutor,
	tester testExecutor,
) *Runtime {
	return &Runtime{
		options: cloneLoadOptions(options),
		loader:  loader,
		builder: builder,
		tester:  tester,
	}
}

func (runtime *Runtime) validate() error {
	if runtime == nil {
		return errors.New("spice CLI runtime is nil")
	}
	if err := runtime.validateLoader(); err != nil {
		return err
	}
	if err := runtime.validateBuilder(); err != nil {
		return err
	}
	return runtime.validateTester()
}

func (runtime *Runtime) validateLoader() error {
	if runtime == nil {
		return errors.New("spice CLI runtime is nil")
	}
	if runtime.loader == nil {
		return errors.New("spice CLI package loader is nil")
	}
	return nil
}

func (runtime *Runtime) validateBuilder() error {
	if runtime == nil {
		return errors.New("spice CLI runtime is nil")
	}
	if runtime.builder == nil {
		return errors.New("spice CLI build executor is nil")
	}
	return nil
}

func (runtime *Runtime) validateTester() error {
	if runtime == nil {
		return errors.New("spice CLI runtime is nil")
	}
	if runtime.tester == nil {
		return errors.New("spice CLI test executor is nil")
	}
	return nil
}

func cloneLoadOptions(options load.Options) load.Options {
	result := options
	result.Env = append([]string(nil), options.Env...)
	result.BuildFlags = append([]string(nil), options.BuildFlags...)
	result.AuxiliaryPackages = append(
		[]string(nil),
		options.AuxiliaryPackages...,
	)
	if options.Overlay != nil {
		result.Overlay = make(map[string][]byte, len(options.Overlay))
		for filename, content := range options.Overlay {
			result.Overlay[filename] = append([]byte(nil), content...)
		}
	}
	return result
}
