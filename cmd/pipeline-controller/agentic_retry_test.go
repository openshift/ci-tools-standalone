package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/prow/pkg/github"
)

type agenticRetryTransport func(*http.Request) (*http.Response, error)

func (f agenticRetryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestAgenticRetryHonorsPinnedGitHubErrors(t *testing.T) {
	for _, status := range []int{403, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			_, _, client, err := github.NewClientFromOptions(nil, github.ClientOptions{
				Bases: []string{"https://github.invalid"}, GraphqlEndpoint: "https://github.invalid/graphql",
				Censor: func(data []byte) []byte { return data }, MaxRetries: 1, InitialDelay: time.Nanosecond,
				BaseRoundTripper: agenticRetryTransport(func(request *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: status, Header: http.Header{},
						Body: io.NopCloser(strings.NewReader(`{"message":"failure"}`)), Request: request}, nil
				}),
			})
			require.NoError(t, err)
			_, err = client.GetPullRequest("org", "repo", 42)
			require.Error(t, err)
			retry := agenticRetryFor(fmt.Errorf("request: %w", err))
			require.Equal(t, status != 403, retry.transient)
			if status == 429 {
				require.GreaterOrEqual(t, retry.after, time.Minute)
			}
		})
	}
}

func TestAgenticRetryWaitAndWrappedCauses(t *testing.T) {
	wait := agenticRetryFor(errors.New("sleep time for token reset exceeds max sleep time (11m0s > 1m0s)"))
	require.True(t, wait.transient)
	require.Equal(t, 11*time.Minute, wait.after)
	retry := agenticRetryFor(errors.Join(context.DeadlineExceeded, apierrors.NewTooManyRequests("throttled", 60)))
	require.True(t, retry.transient)
	require.Equal(t, time.Minute, retry.after)
	require.False(t, agenticRetryFor(errors.New("invalid configuration")).transient)
	require.False(t, agenticRetryFor(context.Canceled).transient)
}
