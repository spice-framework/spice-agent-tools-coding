package annotationhost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spice-framework/spice/annotation/sdk"
	"github.com/spice-framework/spice/annotation/sdk/protocol"
	"github.com/spice-framework/toolchain/internal/identity"
	"github.com/spice-framework/toolchain/internal/moduleenv"
	"golang.org/x/mod/module"
)

const (
	defaultStartTimeout = 30 * time.Second
	defaultCallTimeout  = 10 * time.Second
	defaultStderrBytes  = 256 << 10
)

// Config controls one persistent annotation tool process.
type Config struct {
	Root         string
	ToolPath     string
	SpiceVersion string
	Environment  []string
	StartTimeout time.Duration
	CallTimeout  time.Duration
	StderrBytes  int
}

// Client is one serialized persistent JSON-RPC connection.
type Client struct {
	config      Config
	module      TargetModule
	provenance  PackageIdentity
	command     *exec.Cmd
	cancel      context.CancelFunc
	stdin       io.WriteCloser
	stdout      *bufio.Reader
	stderr      *boundedBuffer
	wait        chan error
	waitOnce    sync.Once
	containment processContainment

	mu                 sync.Mutex
	nextID             uint64
	closed             bool
	handlers           map[string]protocol.Handler
	descriptorPackages []string
}

// Start authorizes, resolves, launches, initializes, and describes one tool.
func Start(ctx context.Context, config Config) (*Client, error) {
	if ctx == nil {
		return nil, errors.New("start annotation tool context must not be nil")
	}
	config = defaultConfig(config)
	module, err := ReadTargetModule(config.Root)
	if err != nil {
		return nil, err
	}
	if authorizeErr := module.AuthorizeTool(config.ToolPath); authorizeErr != nil {
		return nil, authorizeErr
	}
	resolveCtx, cancelResolve := context.WithTimeout(ctx, config.StartTimeout)
	provenance, err := ResolvePackage(
		resolveCtx,
		module,
		config.ToolPath,
		config.Environment,
	)
	cancelResolve()
	if err != nil {
		return nil, err
	}
	processCtx, cancelProcess := context.WithCancel(context.Background())
	command := exec.CommandContext( // #nosec G204 -- executable/verb are fixed and the authorized full tool path is one argument.
		processCtx,
		"go",
		"tool",
		config.ToolPath,
		"--spice-stdio",
	)
	command.Dir = module.Root
	command.Env = offlineEnvironment(
		config.Environment,
		moduleenv.OfflineMode(module.Root, config.Environment),
	)
	stdin, err := command.StdinPipe()
	if err != nil {
		cancelProcess()
		return nil, fmt.Errorf("open annotation tool stdin: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancelProcess()
		closeErr := stdin.Close()
		return nil, errors.Join(
			fmt.Errorf("open annotation tool stdout: %w", err),
			closeErr,
		)
	}
	stderr := newBoundedBuffer(config.StderrBytes)
	command.Stderr = stderr
	configureToolProcess(command)
	if startErr := command.Start(); startErr != nil {
		cancelProcess()
		closeErr := stdin.Close()
		return nil, errors.Join(
			fmt.Errorf(
				"start annotation tool %q: %w%s",
				config.ToolPath,
				startErr,
				renderStderr(stderr.String()),
			),
			closeErr,
		)
	}
	containment, err := containToolProcess(command)
	if err != nil {
		cancelProcess()
		killErr := command.Process.Kill()
		waitErr := command.Wait()
		closeErr := stdin.Close()
		return nil, errors.Join(err, killErr, waitErr, closeErr)
	}
	client := &Client{
		config:      config,
		module:      module,
		provenance:  provenance,
		command:     command,
		cancel:      cancelProcess,
		stdin:       stdin,
		stdout:      bufio.NewReader(stdout),
		stderr:      stderr,
		wait:        make(chan error, 1),
		containment: containment,
		handlers:    make(map[string]protocol.Handler),
	}
	startCtx, cancelStart := context.WithTimeout(ctx, config.StartTimeout)
	err = client.initialize(startCtx)
	cancelStart()
	if err != nil {
		return nil, errors.Join(err, client.abort())
	}
	return client, nil
}

// Provenance returns the standard Go-selected tool package identity.
func (client *Client) Provenance() PackageIdentity {
	if client == nil {
		return PackageIdentity{}
	}
	return clonePackageIdentity(client.provenance)
}

// Handlers returns stable inspectable handler declarations.
func (client *Client) Handlers() []protocol.Handler {
	if client == nil {
		return nil
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	result := make([]protocol.Handler, 0, len(client.handlers))
	for _, handler := range client.handlers {
		handler.Capabilities = append([]string(nil), handler.Capabilities...)
		result = append(result, handler)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Descriptor.Package != result[j].Descriptor.Package {
			return result[i].Descriptor.Package <
				result[j].Descriptor.Package
		}
		return result[i].Descriptor.Name < result[j].Descriptor.Name
	})
	return result
}

// DescriptorPackages returns the stable public descriptor packages declared
// by the tool during protocol negotiation.
func (client *Client) DescriptorPackages() []string {
	if client == nil {
		return nil
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	return append([]string(nil), client.descriptorPackages...)
}

// Analyze dispatches one invocation to an initialized declared handler.
func (client *Client) Analyze(
	ctx context.Context,
	params protocol.AnalyzeParams,
) (protocol.AnalyzeResult, error) {
	if client == nil {
		return protocol.AnalyzeResult{}, errors.New(
			"annotation tool client is nil",
		)
	}
	key := handlerKey(params.Descriptor)
	if _, found := client.handlers[key]; !found {
		return protocol.AnalyzeResult{}, fmt.Errorf(
			"annotation tool %q does not register descriptor %s.%s",
			client.config.ToolPath,
			params.Descriptor.Package,
			params.Descriptor.Name,
		)
	}
	var result protocol.AnalyzeResult
	err := client.call(ctx, "analyze", params, &result)
	return result, err
}

// Close requests graceful shutdown and always terminates the owned process.
func (client *Client) Close(ctx context.Context) error {
	if client == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("close annotation tool context must not be nil")
	}
	if !client.healthy() {
		return nil
	}
	var result struct{}
	callErr := client.call(ctx, "shutdown", protocol.ShutdownParams{}, &result)
	if callErr != nil {
		return callErr
	}
	return client.finish(ctx)
}

func (client *Client) initialize(ctx context.Context) error {
	var initialized protocol.InitializeResult
	if err := client.call(ctx, "initialize", protocol.InitializeParams{
		Protocol:      sdk.ProtocolV1Alpha2,
		SpiceVersion:  client.config.SpiceVersion,
		WorkspaceRoot: client.module.Root,
		ToolPath:      client.config.ToolPath,
	}, &initialized); err != nil {
		return fmt.Errorf("initialize annotation tool: %w", err)
	}
	if err := client.validateIdentity(initialized); err != nil {
		return err
	}
	var described protocol.DescribeResult
	if err := client.call(
		ctx,
		"describe",
		protocol.DescribeParams{},
		&described,
	); err != nil {
		return fmt.Errorf("describe annotation tool: %w", err)
	}
	descriptorPackages, err := validateDescriptorPackages(
		described.DescriptorPackages,
		client.provenance.Module.Path,
		client.provenance.Path,
	)
	if err != nil {
		return fmt.Errorf(
			"annotation tool %q descriptor packages are invalid: %w",
			client.config.ToolPath,
			err,
		)
	}
	handlers, err := validateHandlers(described.Handlers)
	if err != nil {
		return fmt.Errorf(
			"annotation tool %q description is invalid: %w",
			client.config.ToolPath,
			err,
		)
	}
	client.descriptorPackages = descriptorPackages
	client.handlers = handlers
	return nil
}

func (client *Client) validateIdentity(
	identity protocol.InitializeResult,
) error {
	module := client.provenance.Module
	if identity.Protocol != sdk.ProtocolV1Alpha2 ||
		identity.ToolPath != client.config.ToolPath ||
		identity.ModulePath != module.Path ||
		identity.ModuleVersion != module.Version {
		return fmt.Errorf(
			"annotation tool identity mismatch: got protocol=%q tool=%q module=%s@%s, expected protocol=%q tool=%q module=%s@%s",
			identity.Protocol,
			identity.ToolPath,
			identity.ModulePath,
			identity.ModuleVersion,
			sdk.ProtocolV1Alpha2,
			client.config.ToolPath,
			module.Path,
			module.Version,
		)
	}
	return nil
}

func (client *Client) call(
	ctx context.Context,
	method string,
	params any,
	result any,
) error {
	if ctx == nil {
		return errors.New("annotation tool call context must not be nil")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.closed {
		return fmt.Errorf(
			"annotation tool %q is closed",
			client.config.ToolPath,
		)
	}
	callCtx, cancel := boundedContext(ctx, client.config.CallTimeout)
	defer cancel()
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("encode annotation tool %s parameters: %w", method, err)
	}
	client.nextID++
	request := protocol.Request{
		JSONRPC: "2.0",
		ID:      client.nextID,
		Method:  method,
		Params:  paramsJSON,
	}
	if err := protocol.WriteMessage(client.stdin, request); err != nil {
		return client.fail(method, err)
	}
	response := make(chan callResponse, 1)
	go func() {
		var value protocol.Response
		readErr := protocol.ReadMessage(client.stdout, &value)
		response <- callResponse{value: value, err: readErr}
	}()
	select {
	case <-callCtx.Done():
		abortErr := client.abortLocked()
		return errors.Join(
			fmt.Errorf(
				"annotation tool %s failed: %w",
				method,
				callCtx.Err(),
			),
			abortErr,
		)
	case received := <-response:
		if received.err != nil {
			return client.fail(method, received.err)
		}
		if received.value.JSONRPC != "2.0" ||
			received.value.ID != request.ID {
			return client.fail(
				method,
				fmt.Errorf(
					"response identity is jsonrpc=%q id=%d, expected jsonrpc=2.0 id=%d",
					received.value.JSONRPC,
					received.value.ID,
					request.ID,
				),
			)
		}
		if received.value.Error != nil {
			return fmt.Errorf(
				"annotation tool %s returned JSON-RPC error %d: %s",
				method,
				received.value.Error.Code,
				received.value.Error.Message,
			)
		}
		if err := json.Unmarshal(received.value.Result, result); err != nil {
			return client.fail(method, fmt.Errorf("decode result: %w", err))
		}
		return nil
	}
}

type callResponse struct {
	value protocol.Response
	err   error
}

func (client *Client) fail(method string, err error) error {
	abortErr := client.abortLocked()
	return errors.Join(
		fmt.Errorf(
			"annotation tool %s protocol failure: %w%s",
			method,
			err,
			renderStderr(client.stderr.String()),
		),
		abortErr,
	)
}

func (client *Client) abort() error {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.abortLocked()
}

func (client *Client) finish(ctx context.Context) error {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.closed {
		return nil
	}
	closeErr := ignoreClosedPipe(client.stdin.Close())
	wait := client.waitForExitLocked()
	select {
	case waitErr := <-wait:
		client.closed = true
		client.cancel()
		releaseErr := client.containment.release()
		if waitErr != nil {
			return errors.Join(closeErr, releaseErr, fmt.Errorf(
				"annotation tool %q exited after shutdown: %w%s",
				client.config.ToolPath,
				waitErr,
				renderStderr(client.stderr.String()),
			))
		}
		return errors.Join(closeErr, releaseErr)
	case <-ctx.Done():
		return errors.Join(
			closeErr,
			fmt.Errorf(
				"wait for annotation tool shutdown: %w",
				ctx.Err(),
			),
			client.abortLocked(),
		)
	}
}

func (client *Client) abortLocked() error {
	if client.closed {
		return nil
	}
	client.closed = true
	terminateErr := client.containment.terminate()
	client.cancel()
	closeErr := ignoreClosedPipe(client.stdin.Close())
	waitErr := <-client.waitForExitLocked()
	if waitErr != nil &&
		!strings.Contains(waitErr.Error(), "signal: killed") &&
		!strings.Contains(waitErr.Error(), "process already finished") {
		return errors.Join(terminateErr, closeErr, fmt.Errorf(
			"annotation tool %q exited: %w%s",
			client.config.ToolPath,
			waitErr,
			renderStderr(client.stderr.String()),
		))
	}
	return errors.Join(terminateErr, closeErr)
}

func (client *Client) waitForExitLocked() <-chan error {
	client.waitOnce.Do(func() {
		go func() {
			client.wait <- client.command.Wait()
			close(client.wait)
		}()
	})
	return client.wait
}

func ignoreClosedPipe(err error) error {
	if errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return nil
	}
	return err
}

func defaultConfig(config Config) Config {
	if config.StartTimeout <= 0 {
		config.StartTimeout = defaultStartTimeout
	}
	if config.CallTimeout <= 0 {
		config.CallTimeout = defaultCallTimeout
	}
	if config.StderrBytes <= 0 {
		config.StderrBytes = defaultStderrBytes
	}
	return config
}

func boundedContext(
	parent context.Context,
	maximum time.Duration,
) (context.Context, context.CancelFunc) {
	if deadline, found := parent.Deadline(); found &&
		time.Until(deadline) <= maximum {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, maximum)
}

func validateDescriptorPackages(
	values []string,
	modulePath string,
	toolPath string,
) ([]string, error) {
	if len(values) == 0 {
		return nil, errors.New(
			"at least one descriptor package is required",
		)
	}
	result := append([]string(nil), values...)
	for _, packagePath := range result {
		if strings.TrimSpace(packagePath) != packagePath ||
			module.CheckImportPath(packagePath) != nil {
			return nil, fmt.Errorf(
				"descriptor package %q is not a valid Go import path",
				packagePath,
			)
		}
		withinToolModule := packagePath == modulePath ||
			strings.HasPrefix(packagePath, modulePath+"/")
		officialCoreDescriptor := toolPath == identity.AnnotationTool &&
			identity.OfficialDescriptorPackage(packagePath)
		if !withinToolModule && !officialCoreDescriptor {
			return nil, fmt.Errorf(
				"descriptor package %q is outside tool module %q",
				packagePath,
				modulePath,
			)
		}
	}
	sort.Strings(result)
	for index := 1; index < len(result); index++ {
		if result[index] == result[index-1] {
			return nil, fmt.Errorf(
				"duplicate descriptor package %q",
				result[index],
			)
		}
	}
	return result, nil
}

func validateHandlers(
	values []protocol.Handler,
) (map[string]protocol.Handler, error) {
	result := make(map[string]protocol.Handler, len(values))
	for _, handler := range values {
		if strings.TrimSpace(handler.Descriptor.Package) == "" ||
			strings.TrimSpace(handler.Descriptor.Name) == "" {
			return nil, errors.New(
				"every handler requires a descriptor symbol",
			)
		}
		key := handlerKey(handler.Descriptor)
		if _, duplicate := result[key]; duplicate {
			return nil, fmt.Errorf(
				"duplicate descriptor registration %s.%s",
				handler.Descriptor.Package,
				handler.Descriptor.Name,
			)
		}
		handler.Capabilities = append([]string(nil), handler.Capabilities...)
		sort.Strings(handler.Capabilities)
		result[key] = handler
	}
	return result, nil
}

func handlerKey(symbol sdk.Symbol) string {
	return symbol.Package + "\x00" + symbol.Name
}

func clonePackageIdentity(value PackageIdentity) PackageIdentity {
	result := value
	result.Module = cloneModuleIdentity(value.Module)
	return result
}

func cloneModuleIdentity(value ModuleIdentity) ModuleIdentity {
	result := value
	if value.Replacement != nil {
		replacement := cloneModuleIdentity(*value.Replacement)
		result.Replacement = &replacement
	}
	return result
}
