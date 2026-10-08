package main

import (
	"flag"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
	"sigs.k8s.io/prow/pkg/flagutil"
)

func TestAgenticFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		fail bool
	}{
		{},
		{args: []string{"--agentic-timeout=5m", "--agentic-trusted-author=chai[bot]"}},
		{args: []string{"--agentic-timeout=0s"}, fail: true},
		{args: []string{"--agentic-trusted-author=@chai"}, fail: true},
		{args: []string{"--agentic-state-dir=/state"}, fail: true},
		{args: []string{"--agentic-state-ttl=720h"}, fail: true},
	} {
		var options agenticOptions
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		options.addFlags(fs)
		err := fs.Parse(tc.args)
		if err == nil {
			err = options.validate()
		}
		if tc.fail {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
			if len(tc.args) == 0 {
				require.Equal(t, 20*time.Minute, options.timeout)
			}
		}
	}
}

func TestAgenticEnrollmentNeedsAppAndTrustButNotStorage(t *testing.T) {
	f := newAgenticFixture(t, "auto")
	require.NoError(t, f.a.validateEnrollment())
	f.a.appID = 0
	require.ErrorContains(t, f.a.validateEnrollment(), "GitHub App")
	f.a.appID = 101
	f.a.options.trustedAuthors = flagutil.Strings{}
	require.ErrorContains(t, f.a.validateEnrollment(), "--agentic-trusted-author")
	f.a.watcher.config.Orgs[0].Repos[0].Mode.Agentic = AgenticConfig{}
	require.NoError(t, f.a.validateEnrollment())
}

func TestAgenticConfigRejectsUnsupportedOptions(t *testing.T) {
	for _, source := range []string{"mode: other", "mode: chai\ntimeout: 20m", "mode: chai\ntrusted_authors: [chai]"} {
		var cfg AgenticConfig
		require.Error(t, yaml.Unmarshal([]byte(source), &cfg))
	}
}
