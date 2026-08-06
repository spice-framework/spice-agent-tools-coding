package coding

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"unicode/utf8"

	"github.com/spice-framework/spice-agent/tool"
)

const readInputSchema = `{"type":"object","additionalProperties":false,"required":["path"],"properties":{"path":{"type":"string","minLength":1},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1}}}`

type readTool struct {
	config     Config
	definition tool.Definition
}

type readArguments struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset,omitempty"`
	Limit  int64  `json:"limit,omitempty"`
}

type readContent struct {
	OK         bool   `json:"ok"`
	Path       string `json:"path"`
	Offset     int64  `json:"offset"`
	Bytes      int    `json:"bytes"`
	TotalBytes int64  `json:"total_bytes"`
	NextOffset int64  `json:"next_offset,omitempty"`
	Truncated  bool   `json:"truncated"`
	Encoding   string `json:"encoding"`
	Content    string `json:"content"`
	SHA256     string `json:"sha256,omitempty"`
}

// NewRead constructs the exact Spice Agent read tool binding without touching
// the configured worktree.
func NewRead(config Config) (tool.Tool, error) {
	normalized, err := validatedConfig(config)
	if err != nil {
		return nil, err
	}
	definition, err := tool.NewDefinition(
		"read",
		"Read one bounded page from a file inside the configured worktree.",
		json.RawMessage(readInputSchema),
		tool.CapabilityFilesystemRead,
	)
	if err != nil {
		return nil, err
	}
	return &readTool{config: normalized, definition: definition}, nil
}

func (reader *readTool) Definition() tool.Definition { return reader.definition.Clone() }

func (reader *readTool) Execute(ctx context.Context, call tool.Call, reporter tool.Reporter) tool.Result {
	if err := validateCall(call, reader.definition.Name()); err != nil {
		return failureResult(call.ID(), err)
	}
	if err := contextFailure(ctx); err != nil {
		return failureResult(call.ID(), err)
	}
	var arguments readArguments
	if err := decodeArguments(call.Arguments(), &arguments); err != nil {
		return failureResult(call.ID(), err)
	}
	if arguments.Offset < 0 {
		return failureResult(call.ID(), invalidArguments("read offset must not be negative"))
	}
	if arguments.Limit == 0 {
		arguments.Limit = reader.config.MaxReadBytes
	}
	if arguments.Limit < 1 || arguments.Limit > reader.config.MaxReadBytes {
		return failureResult(call.ID(), invalidArguments("read limit exceeds the configured per-call bound"))
	}
	if err := reportProgress(ctx, reporter, call.ID(), "reading bounded file page"); err != nil {
		return failureResult(call.ID(), err)
	}
	content, err := reader.read(ctx, arguments)
	if err != nil {
		return failureResult(call.ID(), err)
	}
	result, err := newSuccessResult(call.ID(), content)
	if err == nil {
		return result
	}
	content.Encoding = "base64"
	content.Content = base64.StdEncoding.EncodeToString([]byte(content.Content))
	return successResult(call.ID(), content)
}

func (reader *readTool) read(ctx context.Context, arguments readArguments) (readContent, error) {
	path, err := parseRelativePath(arguments.Path, false)
	if err != nil {
		return readContent{}, err
	}
	workspace, err := openWorktree(reader.config.Root)
	if err != nil {
		return readContent{}, err
	}
	defer workspace.Close() //nolint:errcheck // Read-only root close cannot alter the result.
	file, info, err := openRegularFile(workspace, path)
	if err != nil {
		return readContent{}, err
	}
	defer file.Close() //nolint:errcheck // Read-only close cannot alter the result.
	if arguments.Offset > info.Size() {
		return readContent{}, invalidArguments("read offset exceeds the file size")
	}
	if _, seekErr := file.Seek(arguments.Offset, io.SeekStart); seekErr != nil {
		return readContent{}, executionFailure("read_failed", "requested file offset could not be selected")
	}
	data, err := io.ReadAll(io.LimitReader(file, arguments.Limit+1))
	if err != nil {
		return readContent{}, executionFailure("read_failed", "requested file page could not be read")
	}
	if err := contextFailure(ctx); err != nil {
		return readContent{}, err
	}
	truncated := int64(len(data)) > arguments.Limit
	if truncated {
		data = data[:arguments.Limit]
	}
	content := readContent{
		OK: true, Path: path.display, Offset: arguments.Offset, Bytes: len(data),
		TotalBytes: info.Size(), Truncated: truncated, Encoding: "utf-8", Content: string(data),
	}
	if truncated {
		content.NextOffset = arguments.Offset + int64(len(data))
	}
	if !utf8.Valid(data) {
		content.Encoding = "base64"
		content.Content = base64.StdEncoding.EncodeToString(data)
	}
	if arguments.Offset == 0 && !truncated && int64(len(data)) == info.Size() {
		digest := sha256.Sum256(data)
		content.SHA256 = hex.EncodeToString(digest[:])
	}
	return content, nil
}

func validatedConfig(config Config) (Config, error) {
	normalized := normalize(config)
	if err := normalized.validate(); err != nil {
		return Config{}, err
	}
	return cloneConfig(normalized), nil
}

var _ tool.Tool = (*readTool)(nil)
