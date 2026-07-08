package main

import (
	"bytes"
	"strings"

	"github.com/moby/buildkit/frontend/dockerfile/command"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
	"github.com/sirupsen/logrus"

	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
)

type dockerfileEntry struct {
	Path            string   `json:"path"`
	GoBinaryTargets []string `json:"go_binary_targets,omitempty"`
}

var alwaysTriggerFiles = map[string]bool{
	"go.mod":        true,
	"go.sum":        true,
	"Makefile":      true,
	".dockerignore": true,
}

func evaluateDockerfileChanges(entries []dockerfileEntry, changedFiles []string, pj *v1.ProwJob, ghc minimalGhClient) bool {
	for _, f := range changedFiles {
		if alwaysTriggerFiles[f] {
			return true
		}
	}

	for _, entry := range entries {
		for _, f := range changedFiles {
			if f == entry.Path {
				return true
			}
		}

		if len(entry.GoBinaryTargets) > 0 {
			// COPY-all with go_binary_targets is handled by CNTRLPLANE-3781.
			// Conservatively trigger the test.
			return true
		}

		dockerfileContent, err := fetchFile(ghc, pj, entry.Path)
		if err != nil {
			logrus.WithError(err).WithField("dockerfile", entry.Path).Warn("Failed to fetch Dockerfile, conservatively triggering test")
			return true
		}

		sourcePaths, broadCopy := parseDockerfileSources(dockerfileContent)
		if broadCopy {
			return true
		}

		dockerignoreContent, _ := fetchFile(ghc, pj, ".dockerignore")
		pm := buildIgnoreMatcher(dockerignoreContent)

		for _, changedFile := range changedFiles {
			if pm != nil {
				excluded, _ := pm.MatchesOrParentMatches(changedFile)
				if excluded {
					continue
				}
			}
			for _, src := range sourcePaths {
				if pathMatchesSource(changedFile, src) {
					return true
				}
			}
		}
	}
	return false
}

func fetchFile(ghc minimalGhClient, pj *v1.ProwJob, path string) ([]byte, error) {
	if pj.Spec.Refs == nil {
		return nil, nil
	}
	return ghc.GetFile(pj.Spec.Refs.Org, pj.Spec.Refs.Repo, path, pj.Spec.Refs.BaseRef)
}

// parseDockerfileSources uses the buildkit parser to extract COPY/ADD source
// paths from a Dockerfile. It returns the list of source paths and whether a
// broad copy (COPY . .) was detected.
func parseDockerfileSources(content []byte) (sources []string, broadCopy bool) {
	if content == nil {
		return nil, true
	}

	result, err := parser.Parse(bytes.NewReader(content))
	if err != nil {
		logrus.WithError(err).Warn("Failed to parse Dockerfile, treating as broad copy")
		return nil, true
	}

	for _, child := range result.AST.Children {
		switch strings.ToLower(child.Value) {
		case command.Copy, command.Add:
			srcs, isBroad := extractCopySources(child)
			if isBroad {
				return nil, true
			}
			sources = append(sources, srcs...)
		}
	}

	return sources, false
}

// extractCopySources extracts the source paths from a COPY or ADD AST node.
// Returns nil, true if the instruction copies from "." (broad copy).
// Returns empty slice for --from=<stage> instructions (inter-stage copies).
func extractCopySources(node *parser.Node) ([]string, bool) {
	for _, flag := range node.Flags {
		if strings.HasPrefix(flag, "--from=") {
			return nil, false
		}
	}

	var args []string
	for n := node.Next; n != nil; n = n.Next {
		args = append(args, n.Value)
	}

	if len(args) < 2 {
		return nil, true
	}

	// Last arg is destination; everything else is source
	srcs := args[:len(args)-1]
	var result []string
	for _, src := range srcs {
		cleaned := strings.TrimRight(src, "/")
		if cleaned == "." || cleaned == "" {
			return nil, true
		}
		result = append(result, cleaned)
	}
	return result, false
}

func buildIgnoreMatcher(dockerignoreContent []byte) *patternmatcher.PatternMatcher {
	if dockerignoreContent == nil {
		return nil
	}
	patterns, err := ignorefile.ReadAll(bytes.NewReader(dockerignoreContent))
	if err != nil {
		logrus.WithError(err).Warn("Failed to parse .dockerignore")
		return nil
	}
	pm, err := patternmatcher.New(patterns)
	if err != nil {
		logrus.WithError(err).Warn("Failed to compile .dockerignore patterns")
		return nil
	}
	return pm
}

// pathMatchesSource checks whether a changed file falls within a COPY source path.
func pathMatchesSource(changedFile, source string) bool {
	if changedFile == source {
		return true
	}
	if strings.HasPrefix(changedFile, source+"/") {
		return true
	}
	return false
}
