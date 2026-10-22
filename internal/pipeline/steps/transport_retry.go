package steps

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// Transport retry: the Push and PR steps run last, after review and test, so
// one dropped connection there used to fail the run and repeat all of it.
// retryTransport is the only retry owner for those two steps; agent retries
// and the CI checkpoint retry are separate mechanisms and do not use it.
//
// These are vars so tests can shrink them to milliseconds.
var (
	// transportBackoff is the wait before each retry (3 retries, 4 attempts).
	transportBackoff = []time.Duration{5 * time.Second, 20 * time.Second, 60 * time.Second}
	// transportAttemptTimeout bounds one attempt. A hung connection never
	// returns an error, so without it the retry would never run. A timed-out
	// attempt counts as a transport error.
	transportAttemptTimeout = 5 * time.Minute
)

// transportErrorPatterns are lower-cased substrings of git transport failures
// and forge 5xx/timeout failures. Anything else (a rejected push, a 4xx, a
// hook decline) is not worth retrying and fails at once.
var transportErrorPatterns = []string{
	"unexpected disconnect",
	"remote end hung up",
	"index-pack failed",
	"early eof",
	"inflate: data stream error",
	"connection reset",
	"rpc failed",
	"i/o timeout",
	"tls handshake timeout",
	"timeout awaiting response headers",
	"client.timeout exceeded",
}

var (
	// transportForgeStatus matches a forge 5xx as the gh/glab/tea CLIs and the
	// Bitbucket client print it, and as git prints it ("The requested URL
	// returned error: 503").
	transportForgeStatus = regexp.MustCompile(`http 5\d\d|status code: 5\d\d|status 5\d\d|returned error: 5\d\d|\b5\d\d (bad gateway|service unavailable|gateway time-?out|internal server error)`)
	// notTransient overrides the patterns above: a message that carries one of
	// these is a refusal by the remote, whatever else it says ("rpc failed;
	// http 403" is an auth failure, not a dropped connection). Every forge 4xx
	// format is a refusal.
	notTransient = regexp.MustCompile(`non-fast-forward|stale info|hook declined|authentication failed|permission denied|http 4\d\d|status code: 4\d\d|status 4\d\d|returned error: 4\d\d`)
	// gitRPCTransient4xx is git's own "RPC failed; HTTP 4xx" for any code but
	// 401/403/404. It is a deliberate exception to the 4xx rule: the measured
	// GitHub transport test produced a transient "RPC failed; HTTP 400" on a
	// healthy push, and "rpc failed" is a retryable pattern by design.
	gitRPCTransient4xx = regexp.MustCompile(`rpc failed; http (?:40[0256789]|4[1-9]\d)(?: curl \d+ the requested url returned error: \d+)?`)
)

func isTransportError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if notTransient.MatchString(gitRPCTransient4xx.ReplaceAllString(msg, "rpc failed")) {
		return false
	}
	if transportForgeStatus.MatchString(msg) {
		return true
	}
	for _, p := range transportErrorPatterns {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// retryTransport runs attempt, retrying after transport errors and timeouts
// with transportBackoff. attempt receives a context bounded by
// transportAttemptTimeout.
//
// A transport error can arrive after the remote already accepted the work, so
// after each backoff landed (when non-nil) is asked whether the attempt took
// effect; if it did, the retry is skipped and the call succeeds. A landed
// error means unknown, never "not landed": no new attempt runs, the backoff
// slot is spent, and landed is asked again after the next one. When the
// budget is spent the last attempt's error is returned.
func retryTransport(sctx *pipeline.StepContext, what string, attempt func(ctx context.Context) error, landed func(ctx context.Context) (bool, error)) error {
	attempts := len(transportBackoff) + 1
	var err error
	var timedOut bool
	made := 0
	run := func() {
		made++
		attemptCtx, cancel := context.WithTimeout(sctx.Ctx, transportAttemptTimeout)
		err = attempt(attemptCtx)
		timedOut = attemptCtx.Err() == context.DeadlineExceeded
		if err == nil && timedOut {
			err = fmt.Errorf("attempt finished after its %s deadline", transportAttemptTimeout)
		}
		cancel()
	}
	run()
	for i := 0; ; i++ {
		if err == nil || sctx.Ctx.Err() != nil {
			return err
		}
		if !timedOut && !isTransportError(err) {
			return err
		}
		if i == len(transportBackoff) {
			return err
		}
		reason := oneLine(err.Error())
		if timedOut {
			reason = fmt.Sprintf("timed out after %s: %s", transportAttemptTimeout, reason)
		}
		sctx.Log(fmt.Sprintf("%s: transport error (attempt %d/%d), retrying in %s: %s", what, made, attempts, transportBackoff[i], reason))
		if !sleepContext(sctx.Ctx, transportBackoff[i]) {
			return err
		}
		if landed != nil {
			checkCtx, cancel := context.WithTimeout(sctx.Ctx, transportAttemptTimeout)
			ok, lookupErr := landed(checkCtx)
			cancel()
			if lookupErr != nil {
				sctx.Log(fmt.Sprintf("%s: could not confirm whether the earlier attempt landed: %s", what, oneLine(lookupErr.Error())))
				continue
			}
			if ok {
				sctx.Log(fmt.Sprintf("%s: the earlier attempt had already landed; not retrying", what))
				return nil
			}
		}
		run()
	}
}

// retryTransportValue is retryTransport for an idempotent read that returns a
// value. There is no landed check: repeating a read is always safe.
func retryTransportValue[T any](sctx *pipeline.StepContext, what string, read func(ctx context.Context) (T, error)) (T, error) {
	var v T
	err := retryTransport(sctx, what, func(ctx context.Context) error {
		var readErr error
		v, readErr = read(ctx)
		return readErr
	}, nil)
	return v, err
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// oneLine flattens a multi-line CLI error for a single log line.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " | ")), " ")
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}
