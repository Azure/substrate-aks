package installer

import (
	"context"
	"io"
	"path/filepath"
)

type heldLockKey struct{}

// WithHeldLock records that the caller holds c's installation lock, taken
// with Lock, for the commands run under the returned context. The caller
// must keep holding it until those commands return.
func WithHeldLock(ctx context.Context, c Config) context.Context {
	return context.WithValue(ctx, heldLockKey{}, filepath.Clean(c.workspace()))
}

// LockFor locks c's installation, unless ctx records that the caller already
// holds that installation's lock. Closing the result releases only a lock
// LockFor took.
func LockFor(ctx context.Context, c Config) (io.Closer, error) {
	if held, ok := ctx.Value(heldLockKey{}).(string); ok && held == filepath.Clean(c.workspace()) {
		return nopCloser{}, nil
	}
	return Lock(c)
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }
