// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Stall detection. A connection whose network path disappears (a route
// withdrawn, a NAT entry dropped) often never errors: the TCP connection
// just waits, and an S3 request on it hangs for good. So every request
// runs under a watchdog that cancels it when no byte has moved, either
// way, for the stall time, and stalls and network errors are retried a
// few times before the operation fails.

// DefaultStall is how long a request may go without moving a byte before
// it is cancelled and retried. A slow link never trips it; a dead one does.
const DefaultStall = 2 * time.Minute

// Attempts is how many times a request is tried before its error is
// returned.
const Attempts = 4

// backoff is the pause after a failed try: 1 s, 4 s, 9 s. Tests shorten it.
var backoff = func(attempt int) time.Duration { return time.Duration(attempt*attempt) * time.Second }

// ErrStalled means a request moved no data for the stall time.
var ErrStalled = errors.New("the connection stalled (no data moving)")

// watch returns a context that is cancelled, with ErrStalled as its
// cause, when kick hasn't been called for d; done stops the watchdog and
// releases the context.
func watch(parent context.Context, d time.Duration) (ctx context.Context, kick, done func()) {
	ctx, cancel := context.WithCancelCause(parent)
	t := time.AfterFunc(d, func() { cancel(ErrStalled) })
	return ctx, func() { t.Reset(d) }, func() { t.Stop(); cancel(nil) }
}

// transient reports whether an error is worth retrying: a stall, or the
// network failing under a request (reset, unreachable, cut short). Answers
// from the service (not found, access denied) are not.
func transient(err error) bool {
	var p permanent
	if errors.As(err, &p) {
		return false
	}
	if errors.Is(err, ErrStalled) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false // the caller's own cancellation
	}
	for _, e := range []error{syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.ENETUNREACH,
		syscall.EHOSTUNREACH, syscall.ETIMEDOUT, syscall.EPIPE, io.ErrUnexpectedEOF} {
		if errors.Is(err, e) {
			return true
		}
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "connection reset") || strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "unexpected EOF")
}

// permanent marks an error that is an answer, not a network failure, so
// it isn't retried even when it looks like one (a short range read is
// io.ErrUnexpectedEOF on purpose).
type permanent struct{ err error }

func (p permanent) Error() string { return p.err.Error() }
func (p permanent) Unwrap() error { return p.err }

// unwrapPermanent returns the error inside a permanent, so callers can
// compare it with == as before (err == ErrNotFound).
func unwrapPermanent(err error) error {
	var p permanent
	if errors.As(err, &p) && err == error(p) {
		return p.err
	}
	return err
}

// retry runs op up to Attempts times under a watchdog, pausing longer
// after each failure. op calls kick whenever data moves.
func retry(ctx context.Context, stall time.Duration, op func(ctx context.Context, kick func()) error) error {
	var err error
	for attempt := 1; ; attempt++ {
		wctx, kick, done := watch(ctx, stall)
		err = op(wctx, kick)
		stalled := errors.Is(context.Cause(wctx), ErrStalled)
		done()
		if err == nil {
			return nil
		}
		if stalled {
			err = fmt.Errorf("%w for %s: %w", ErrStalled, stall, err)
		}
		if ctx.Err() != nil || attempt == Attempts || !transient(err) {
			if attempt > 1 {
				err = fmt.Errorf("after %d tries: %w", attempt, err)
			}
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(backoff(attempt)):
		}
	}
}

// kickReader calls kick whenever bytes are read through it.
type kickReader struct {
	r    io.Reader
	kick func()
}

func (k kickReader) Read(p []byte) (int, error) {
	n, err := k.r.Read(p)
	if n > 0 {
		k.kick()
	}
	return n, err
}

// kickReadSeeker is a kickReader for upload bodies, which the SDK needs to
// be able to rewind.
type kickReadSeeker struct {
	io.ReadSeeker
	kick func()
}

func (k kickReadSeeker) Read(p []byte) (int, error) {
	n, err := k.ReadSeeker.Read(p)
	if n > 0 {
		k.kick()
	}
	return n, err
}

// watchedAPI puts every call to an S3API under the watchdog and retries.
// GetObject is the exception: its body is read after the call returns,
// so only the request is covered here (with the watchdog carried on into
// the body until it is closed), and S3.Get retries the whole download.
type watchedAPI struct {
	S3API
	stall time.Duration
}

func call[T any](a *watchedAPI, ctx context.Context, f func(ctx context.Context) (T, error)) (T, error) {
	var out T
	err := retry(ctx, a.stall, func(ctx context.Context, _ func()) error {
		var err error
		out, err = f(ctx)
		return err
	})
	return out, err
}

func (a *watchedAPI) PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	body, seekable := in.Body.(io.ReadSeeker)
	var out *s3.PutObjectOutput
	err := retry(ctx, a.stall, func(ctx context.Context, kick func()) error {
		cp := *in
		if seekable {
			if _, err := body.Seek(0, io.SeekStart); err != nil {
				return err
			}
			cp.Body = kickReadSeeker{body, kick}
		}
		var err error
		out, err = a.S3API.PutObject(ctx, &cp, opts...)
		return err
	})
	return out, err
}

func (a *watchedAPI) GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	wctx, kick, done := watch(ctx, a.stall)
	out, err := a.S3API.GetObject(wctx, in, opts...)
	if err != nil {
		if errors.Is(context.Cause(wctx), ErrStalled) {
			err = fmt.Errorf("%w for %s: %w", ErrStalled, a.stall, err)
		}
		done()
		return nil, err
	}
	kick()
	out.Body = &watchedBody{r: kickReader{out.Body, kick}, c: out.Body, ctx: wctx, stall: a.stall, done: done}
	return out, nil
}

// watchedBody keeps the watchdog running while a download's body is read,
// and says it stalled (rather than just "context canceled") when it did.
type watchedBody struct {
	r     io.Reader
	c     io.Closer
	ctx   context.Context
	stall time.Duration
	done  func()
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil && err != io.EOF && errors.Is(context.Cause(b.ctx), ErrStalled) {
		err = fmt.Errorf("%w for %s: %w", ErrStalled, b.stall, err)
	}
	return n, err
}

func (b *watchedBody) Close() error {
	err := b.c.Close()
	b.done()
	return err
}

func (a *watchedAPI) HeadObject(ctx context.Context, in *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	return call(a, ctx, func(ctx context.Context) (*s3.HeadObjectOutput, error) { return a.S3API.HeadObject(ctx, in, opts...) })
}

func (a *watchedAPI) DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, opts ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	return call(a, ctx, func(ctx context.Context) (*s3.DeleteObjectOutput, error) {
		return a.S3API.DeleteObject(ctx, in, opts...)
	})
}

func (a *watchedAPI) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	return call(a, ctx, func(ctx context.Context) (*s3.ListObjectsV2Output, error) {
		return a.S3API.ListObjectsV2(ctx, in, opts...)
	})
}

func (a *watchedAPI) ListObjectVersions(ctx context.Context, in *s3.ListObjectVersionsInput, opts ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error) {
	return call(a, ctx, func(ctx context.Context) (*s3.ListObjectVersionsOutput, error) {
		return a.S3API.ListObjectVersions(ctx, in, opts...)
	})
}

func (a *watchedAPI) PutObjectRetention(ctx context.Context, in *s3.PutObjectRetentionInput, opts ...func(*s3.Options)) (*s3.PutObjectRetentionOutput, error) {
	return call(a, ctx, func(ctx context.Context) (*s3.PutObjectRetentionOutput, error) {
		return a.S3API.PutObjectRetention(ctx, in, opts...)
	})
}

func (a *watchedAPI) GetObjectLockConfiguration(ctx context.Context, in *s3.GetObjectLockConfigurationInput, opts ...func(*s3.Options)) (*s3.GetObjectLockConfigurationOutput, error) {
	return call(a, ctx, func(ctx context.Context) (*s3.GetObjectLockConfigurationOutput, error) {
		return a.S3API.GetObjectLockConfiguration(ctx, in, opts...)
	})
}

func (a *watchedAPI) RestoreObject(ctx context.Context, in *s3.RestoreObjectInput, opts ...func(*s3.Options)) (*s3.RestoreObjectOutput, error) {
	return call(a, ctx, func(ctx context.Context) (*s3.RestoreObjectOutput, error) {
		return a.S3API.RestoreObject(ctx, in, opts...)
	})
}
