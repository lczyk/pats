package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrPlace marks a failure of the place a sandbox runs on, not of what ran in
// it: an lxc call pats made for itself that lost its server (docs/spec/lxd-retry.md).
var ErrPlace = errors.New("the place failed")

// lxdTries is how many times a transient lxc failure is tried, at one layer.
const lxdTries = 3

// placeErr is a transient failure, after tries tries.
type placeErr struct {
	cause error
	tries int
	// final is a failure no outer layer tries again: its own layer could
	// not (an image build's publish, a failed try that would not go).
	final bool
}

func (e *placeErr) Error() string {
	if e.tries > 1 {
		return fmt.Sprintf("%v (%d tries, transient)", e.cause, e.tries)
	}
	return fmt.Sprintf("%v (transient)", e.cause)
}

func (e *placeErr) Unwrap() []error { return []error{ErrPlace, e.cause} }

// placeTries is how many tries the transient failure in err's chain had, 0
// for none.
func placeTries(err error) int {
	var pe *placeErr
	if errors.As(err, &pe) {
		return pe.tries
	}
	return 0
}

// retryable says whether err is a transient failure of one try that an
// outer layer may try again.
func retryable(err error) bool {
	var pe *placeErr
	return errors.As(err, &pe) && pe.tries == 1 && !pe.final
}

// final marks the transient failure in err's chain as one no outer layer
// tries again.
func final(err error) error {
	var pe *placeErr
	if errors.As(err, &pe) {
		pe.final = true
	}
	return err
}

// lxdMissing says whether err is lxd's "not found", and nothing else: a lookup
// that lost its server is never a miss, whatever its text holds.
func lxdMissing(err error) bool {
	return err != nil && !errors.Is(err, ErrPlace) && notFound(err)
}

// transientTexts are what lxc prints when it could not talk to its server,
// or lost it mid-call. the list errs short: a text it misses fails as any
// other error does.
var transientTexts = []string{
	"connection reset by peer",
	"broken pipe",
	"connection refused",
	"i/o timeout",
	"no route to host",
	"tls handshake timeout",
	"unexpected eof",
	"close 1006",
}

// transientText says whether s, lxc's own output, reads as a lost link.
func transientText(s string) bool {
	s = strings.ToLower(s)
	for _, t := range transientTexts {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}

// transientTarLine says whether the stderr of an exec of pats's own tar
// ends in lxc's own failure for a lost link. only the last line counts, and
// only with lxc's prefix: tar's own lines start "tar: " and may carry names
// the instance chose.
func transientTarLine(stderr string) bool {
	line := lastLine(strings.TrimRight(stderr, "\n"))
	return strings.HasPrefix(line, "Error: ") && transientText(line)
}

// asPlace wraps err as a transient failure of one try when transient says so.
func asPlace(err error, transient bool) error {
	if err == nil || !transient || placeTries(err) > 0 {
		return err
	}
	return &placeErr{cause: err, tries: 1}
}

// retry runs fn up to lxdTries times while it fails transiently in one try,
// with the backoff between, and stops when ctx ends. a failure that ran out
// of tries says how many.
func (l *lxd) retry(ctx context.Context, fn func() error) error {
	backoff := l.p.t.lxdBackoff
	for i := 1; ; i++ {
		err := fn()
		if !retryable(err) {
			return err
		}
		// the failure keeps its own wrapping: only its count changes.
		var pe *placeErr
		errors.As(err, &pe)
		pe.tries = i
		if i >= lxdTries {
			return err
		}
		select {
		case <-ctx.Done():
			pe.cause = errors.Join(pe.cause, ctx.Err())
			return err
		case <-l.p.clock.After(backoff[min(i-1, len(backoff)-1)]):
		}
	}
}

// lxcIdem is lxc for a read or an idempotent write: one whose second try
// cannot do what the first did not.
func (l *lxd) lxcIdem(ctx context.Context, args ...string) (string, error) {
	var out string
	err := l.retry(ctx, func() error {
		var err error
		out, err = l.lxc(ctx, args...)
		return err
	})
	return out, err
}
