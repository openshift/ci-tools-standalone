package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type agenticRetryInfo struct {
	transient       bool
	after           time.Duration
	githubRateLimit bool
}

// Only recognizable operational failures retry without a new event. Inspect
// every wrapped cause, including joined errors from failed API writes.
func agenticRetryFor(err error) agenticRetryInfo {
	if err == nil {
		return agenticRetryInfo{}
	}
	var result agenticRetryInfo
	switch err {
	case context.DeadlineExceeded, io.EOF, io.ErrUnexpectedEOF,
		syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.EPIPE, syscall.ETIMEDOUT,
		syscall.ENETUNREACH, syscall.EHOSTUNREACH:
		result.transient = true
	}
	if network, ok := err.(net.Error); ok && (network.Timeout() || network.Temporary()) {
		result.transient = true
	}
	if status, ok := err.(apierrors.APIStatus); ok {
		result.transient = retryableAgenticHTTPStatus(int(status.Status().Code)) || apierrors.IsConflict(err) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err)
		if seconds, suggested := apierrors.SuggestsClientDelay(err); result.transient && suggested && seconds > 0 {
			result.after = time.Duration(seconds) * time.Second
		}
	}
	// Prow's requestError is private, but exposes ErrorMessages. Restrict status
	// parsing to that interface and its pinned, anchored format, not arbitrary
	// validation text containing a status code.
	if _, ok := err.(interface{ ErrorMessages() []string }); ok {
		if code, ok := strings.CutPrefix(err.Error(), "status code "); ok {
			if code, _, ok := strings.Cut(code, " not one of "); ok && len(code) == 3 {
				status, _ := strconv.Atoi(code)
				result.transient = retryableAgenticHTTPStatus(status)
				result.githubRateLimit = status == http.StatusTooManyRequests
			}
		}
	}
	// Prow's paginated reads expose only this status-line envelope instead.
	if statusLine, ok := strings.CutPrefix(err.Error(), "return code not 2XX: "); ok {
		if code, _, ok := strings.Cut(statusLine, " "); ok && len(code) == 3 {
			status, _ := strconv.Atoi(code)
			result.transient = retryableAgenticHTTPStatus(status)
			result.githubRateLimit = status == http.StatusTooManyRequests
		}
	}
	if wait := agenticGitHubWait(err.Error()); wait > 0 {
		result.transient, result.githubRateLimit, result.after = true, true, wait
	}
	var causes []error
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		causes = wrapped.Unwrap()
	case interface{ Unwrap() error }:
		causes = []error{wrapped.Unwrap()}
	}
	for _, cause := range causes {
		retry := agenticRetryFor(cause)
		result.transient = result.transient || retry.transient
		result.githubRateLimit = result.githubRateLimit || retry.githubRateLimit
		result.after = max(result.after, retry.after)
	}
	if result.githubRateLimit && result.after == 0 {
		// GitHub recommends at least a minute when no reset/retry hint is exposed.
		result.after = time.Minute
	}
	return result
}

func retryableAgenticHTTPStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || (status >= 500 && status <= 599)
}

// Prow handles short rate-limit waits internally. Longer waits escape as these
// two plain errors; the SDK does not expose their response headers or a public
// retry-after type. Keep this adapter exact and covered against the real client.
func agenticGitHubWait(message string) time.Duration {
	for _, prefix := range []string{"sleep time for token reset exceeds max sleep time (", "sleep time for abuse rate limit exceeds max sleep time ("} {
		value, ok := strings.CutPrefix(message, prefix)
		if !ok {
			continue
		}
		value, ok = strings.CutSuffix(value, ")")
		if !ok {
			return 0
		}
		wait, limit, ok := strings.Cut(value, " > ")
		if !ok {
			return 0
		}
		delay, err := time.ParseDuration(wait)
		maximum, maxErr := time.ParseDuration(limit)
		if err == nil && maxErr == nil && delay > 0 && maximum > 0 && delay >= maximum {
			return delay
		}
	}
	return 0
}
