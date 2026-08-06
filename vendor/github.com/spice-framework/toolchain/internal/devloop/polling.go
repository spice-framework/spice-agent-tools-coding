package devloop

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	defaultPollInterval = 500 * time.Millisecond
	defaultMaximumFiles = 100_000
)

// PollingConfig configures the bounded recursive polling watcher.
type PollingConfig struct {
	Root      string
	Interval  time.Duration
	Maximum   int
	PathRules PathRules
}

// PollingWatcher is the portable recursive watcher and native-watch fallback.
type PollingWatcher struct {
	events chan FileEvent
	errors chan error
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// NewPollingWatcher snapshots the workspace and starts bounded recursive
// content polling. Existing files form the baseline and do not emit events.
func NewPollingWatcher(
	ctx context.Context,
	config PollingConfig,
	clock Clock,
) (*PollingWatcher, error) {
	if ctx == nil {
		return nil, errors.New("polling watcher context must not be nil")
	}
	if config.Root == "" {
		return nil, errors.New("polling watcher root must not be empty")
	}
	root, err := filepath.Abs(config.Root)
	if err != nil {
		return nil, fmt.Errorf("resolve polling watcher root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect polling watcher root: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("polling watcher root must be a directory")
	}
	if config.Interval == 0 {
		config.Interval = defaultPollInterval
	}
	if config.Interval <= 0 {
		return nil, errors.New("polling watcher interval must be positive")
	}
	if config.Maximum == 0 {
		config.Maximum = defaultMaximumFiles
	}
	if config.Maximum <= 0 {
		return nil, errors.New("polling watcher maximum file count must be positive")
	}
	filter, err := NewPathFilter(config.PathRules)
	if err != nil {
		return nil, err
	}
	if clock == nil {
		clock = realClock{}
	}
	baseline, err := scanWorkspace(ctx, root, filter, config.Maximum)
	if err != nil {
		return nil, err
	}

	watchCtx, cancel := context.WithCancel(ctx)
	watcher := &PollingWatcher{
		events: make(chan FileEvent, 256),
		errors: make(chan error, 1),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	timer := clock.NewTimer(config.Interval)
	go watcher.poll(
		watchCtx,
		root,
		filter,
		config.Maximum,
		config.Interval,
		timer,
		baseline,
	)
	return watcher, nil
}

// Events returns the ordered filesystem event stream.
func (watcher *PollingWatcher) Events() <-chan FileEvent {
	return watcher.events
}

// Errors returns recoverable scan errors. Repeated errors may be coalesced.
func (watcher *PollingWatcher) Errors() <-chan error {
	return watcher.errors
}

// Close stops polling and waits for owned resources to be released.
func (watcher *PollingWatcher) Close() error {
	watcher.once.Do(func() {
		watcher.cancel()
		<-watcher.done
	})
	return nil
}

type fileStamp struct {
	mode   fs.FileMode
	size   int64
	digest [sha256.Size]byte
}

func (watcher *PollingWatcher) poll(
	ctx context.Context,
	root string,
	filter PathFilter,
	maximum int,
	interval time.Duration,
	timer Timer,
	previous map[string]fileStamp,
) {
	defer close(watcher.done)
	defer close(watcher.events)
	defer close(watcher.errors)
	defer stopAndDrainTimer(timer)

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C():
			timer.Reset(interval)
			current, err := scanWorkspace(ctx, root, filter, maximum)
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					watcher.reportError(ctx, err)
				}
			} else {
				if !watcher.emitChanges(ctx, previous, current) {
					return
				}
				previous = current
			}
		}
	}
}

func (watcher *PollingWatcher) reportError(ctx context.Context, err error) {
	select {
	case watcher.errors <- err:
	case <-ctx.Done():
	default:
	}
}

func (watcher *PollingWatcher) emitChanges(
	ctx context.Context,
	previous map[string]fileStamp,
	current map[string]fileStamp,
) bool {
	paths := make([]string, 0, len(previous)+len(current))
	for filePath := range previous {
		paths = append(paths, filePath)
	}
	for filePath := range current {
		if _, found := previous[filePath]; !found {
			paths = append(paths, filePath)
		}
	}
	sort.Strings(paths)
	for _, filePath := range paths {
		before, existed := previous[filePath]
		after, exists := current[filePath]
		var kind ChangeKind
		switch {
		case !existed && exists:
			kind = ChangeCreate
		case existed && !exists:
			kind = ChangeRemove
		case before != after:
			kind = ChangeWrite
		default:
			continue
		}
		select {
		case watcher.events <- FileEvent{Path: filePath, Kind: kind}:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

func scanWorkspace(
	ctx context.Context,
	rootPath string,
	filter PathFilter,
	maximum int,
) (snapshot map[string]fileStamp, returnErr error) {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("open polling watcher root: %w", err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, root.Close())
	}()

	snapshot = make(map[string]fileStamp)
	err = fs.WalkDir(root.FS(), ".", func(
		filePath string,
		entry fs.DirEntry,
		walkErr error,
	) error {
		if walkErr != nil {
			return walkErr
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		normalized := filepath.ToSlash(filePath)
		if entry.IsDir() {
			if normalized != "." && filter.SkipDirectory(normalized) {
				return fs.SkipDir
			}
			return nil
		}
		if !filter.Match(normalized) {
			return nil
		}
		if len(snapshot) >= maximum {
			return fmt.Errorf(
				"polling watcher relevant file count exceeds %d",
				maximum,
			)
		}
		stamp, stampErr := stampFile(root, filePath)
		if stampErr != nil {
			return fmt.Errorf("hash watched file %s: %w", normalized, stampErr)
		}
		snapshot[normalized] = stamp
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan polling watcher root: %w", err)
	}
	return snapshot, nil
}

func stampFile(root *os.Root, filePath string) (fileStamp, error) {
	file, err := root.Open(filePath)
	if err != nil {
		return fileStamp{}, err
	}
	info, statErr := file.Stat()
	if statErr != nil {
		closeErr := file.Close()
		return fileStamp{}, errors.Join(statErr, closeErr)
	}
	if !info.Mode().IsRegular() {
		closeErr := file.Close()
		return fileStamp{}, errors.Join(
			errors.New("watched path must be a regular file"),
			closeErr,
		)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return fileStamp{}, errors.Join(copyErr, closeErr)
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return fileStamp{
		mode:   info.Mode(),
		size:   info.Size(),
		digest: digest,
	}, nil
}
