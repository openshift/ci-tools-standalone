package main

import (
	"fmt"
	"testing"

	v1 "sigs.k8s.io/prow/pkg/apis/prowjobs/v1"
	"sigs.k8s.io/prow/pkg/github"
)

type fakeGhClient struct {
	files map[string][]byte
}

func (f *fakeGhClient) GetPullRequest(org, repo string, number int) (*github.PullRequest, error) {
	return nil, nil
}
func (f *fakeGhClient) CreateComment(org, repo string, number int, comment string) error {
	return nil
}
func (f *fakeGhClient) GetPullRequestChanges(org, repo string, number int) ([]github.PullRequestChange, error) {
	return nil, nil
}
func (f *fakeGhClient) CreateStatus(org, repo, ref string, s github.Status) error { return nil }
func (f *fakeGhClient) AddLabel(org, repo string, number int, label string) error { return nil }
func (f *fakeGhClient) GetIssueLabels(org, repo string, number int) ([]github.Label, error) {
	return nil, nil
}
func (f *fakeGhClient) GetFile(org, repo, filepath, commit string) ([]byte, error) {
	if content, ok := f.files[filepath]; ok {
		return content, nil
	}
	return nil, fmt.Errorf("file not found: %s", filepath)
}

func testProwJob() *v1.ProwJob {
	return &v1.ProwJob{
		Spec: v1.ProwJobSpec{
			Refs: &v1.Refs{
				Org:     "openshift",
				Repo:    "hypershift",
				BaseRef: "main",
			},
		},
	}
}

func TestParseDockerfileSources(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		wantSrcs  []string
		wantBroad bool
	}{
		{
			name:      "selective COPY",
			content:   "FROM golang:1.21\nCOPY go.mod go.sum ./\nCOPY cmd/ cmd/\nCOPY pkg/ pkg/\n",
			wantSrcs:  []string{"go.mod", "go.sum", "cmd", "pkg"},
			wantBroad: false,
		},
		{
			name:      "broad COPY dot",
			content:   "FROM golang:1.21\nCOPY . .\nRUN go build\n",
			wantSrcs:  nil,
			wantBroad: true,
		},
		{
			name:      "broad COPY dot slash",
			content:   "FROM golang:1.21\nCOPY . /app\n",
			wantSrcs:  nil,
			wantBroad: true,
		},
		{
			name:      "COPY --from skipped",
			content:   "FROM golang:1.21 AS builder\nCOPY cmd/ cmd/\nFROM scratch\nCOPY --from=builder /app /app\n",
			wantSrcs:  []string{"cmd"},
			wantBroad: false,
		},
		{
			name:      "multi-stage selective",
			content:   "FROM golang:1.21 AS builder\nCOPY go.mod .\nCOPY pkg/ pkg/\nFROM scratch\nCOPY --from=builder /bin/app /app\n",
			wantSrcs:  []string{"go.mod", "pkg"},
			wantBroad: false,
		},
		{
			name:      "ADD instruction",
			content:   "FROM ubuntu\nADD scripts/ /scripts/\n",
			wantSrcs:  []string{"scripts"},
			wantBroad: false,
		},
		{
			name:      "nil content",
			content:   "",
			wantSrcs:  nil,
			wantBroad: true,
		},
		{
			name:      "COPY with --link flag",
			content:   "FROM golang:1.21\nCOPY --link cmd/ cmd/\n",
			wantSrcs:  []string{"cmd"},
			wantBroad: false,
		},
		{
			name:      "only COPY --from, no build context copies",
			content:   "FROM scratch\nCOPY --from=builder /bin/app /app\n",
			wantSrcs:  []string{},
			wantBroad: false,
		},
		{
			name:      "COPY --from=0 numeric stage",
			content:   "FROM golang:1.21\nCOPY cmd/ cmd/\nFROM scratch\nCOPY --from=0 /bin/app /app\n",
			wantSrcs:  []string{"cmd"},
			wantBroad: false,
		},
		{
			name:      "mixed selective and --from",
			content:   "FROM golang:1.21 AS build\nCOPY go.mod go.sum ./\nRUN go mod download\nCOPY cmd/ cmd/\nCOPY pkg/ pkg/\nRUN go build -o /app ./cmd/server\nFROM registry.access.redhat.com/ubi9/ubi-minimal:latest\nCOPY --from=build /app /usr/local/bin/app\n",
			wantSrcs:  []string{"go.mod", "go.sum", "cmd", "pkg"},
			wantBroad: false,
		},
		{
			name:      "broad COPY in builder stage propagates despite --from in final stage",
			content:   "FROM golang:1.21 AS build\nCOPY . .\nRUN go build -o /app\nFROM scratch\nCOPY --from=build /app /app\n",
			wantSrcs:  nil,
			wantBroad: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var content []byte
			if tt.content != "" {
				content = []byte(tt.content)
			}
			srcs, broad := parseDockerfileSources(content)
			if broad != tt.wantBroad {
				t.Errorf("broadCopy = %v, want %v", broad, tt.wantBroad)
			}
			if !tt.wantBroad {
				if len(srcs) != len(tt.wantSrcs) {
					t.Errorf("got %d sources %v, want %d sources %v", len(srcs), srcs, len(tt.wantSrcs), tt.wantSrcs)
					return
				}
				for i, src := range srcs {
					if src != tt.wantSrcs[i] {
						t.Errorf("source[%d] = %q, want %q", i, src, tt.wantSrcs[i])
					}
				}
			}
		})
	}
}

func TestPathMatchesSource(t *testing.T) {
	tests := []struct {
		changed string
		source  string
		want    bool
	}{
		{"cmd/main.go", "cmd", true},
		{"cmd/sub/main.go", "cmd", true},
		{"pkg/api/types.go", "pkg", true},
		{"README.md", "cmd", false},
		{"go.mod", "go.mod", true},
		{"cmd", "cmd", true},
		{"command/foo.go", "cmd", false},
	}

	for _, tt := range tests {
		t.Run(tt.changed+"_vs_"+tt.source, func(t *testing.T) {
			got := pathMatchesSource(tt.changed, tt.source)
			if got != tt.want {
				t.Errorf("pathMatchesSource(%q, %q) = %v, want %v", tt.changed, tt.source, got, tt.want)
			}
		})
	}
}

func TestEvaluateDockerfileChanges(t *testing.T) {
	selectiveDockerfile := []byte("FROM golang:1.21\nCOPY cmd/ cmd/\nCOPY pkg/ pkg/\n")
	dockerignore := []byte("bin/\nhack/tools/\n")

	tests := []struct {
		name         string
		entries      []dockerfileEntry
		changedFiles []string
		files        map[string][]byte
		want         bool
	}{
		{
			name:         "always trigger on go.mod change",
			entries:      []dockerfileEntry{{Path: "Dockerfile"}},
			changedFiles: []string{"go.mod"},
			files:        map[string][]byte{"Dockerfile": selectiveDockerfile},
			want:         true,
		},
		{
			name:         "always trigger on Dockerfile change",
			entries:      []dockerfileEntry{{Path: "Dockerfile"}},
			changedFiles: []string{"Dockerfile"},
			files:        map[string][]byte{"Dockerfile": selectiveDockerfile},
			want:         true,
		},
		{
			name:         "trigger on matching COPY source",
			entries:      []dockerfileEntry{{Path: "Dockerfile"}},
			changedFiles: []string{"cmd/main.go"},
			files:        map[string][]byte{"Dockerfile": selectiveDockerfile},
			want:         true,
		},
		{
			name:         "skip on unrelated file",
			entries:      []dockerfileEntry{{Path: "Dockerfile"}},
			changedFiles: []string{"docs/README.md"},
			files:        map[string][]byte{"Dockerfile": selectiveDockerfile},
			want:         false,
		},
		{
			name:         "skip on dockerignored file even if in COPY path",
			entries:      []dockerfileEntry{{Path: "Dockerfile"}},
			changedFiles: []string{"bin/hypershift"},
			files: map[string][]byte{
				"Dockerfile":    []byte("FROM golang:1.21\nCOPY bin/ bin/\n"),
				".dockerignore": dockerignore,
			},
			want: false,
		},
		{
			name:         "trigger on .dockerignore change",
			entries:      []dockerfileEntry{{Path: "Dockerfile"}},
			changedFiles: []string{".dockerignore"},
			files:        map[string][]byte{"Dockerfile": selectiveDockerfile},
			want:         true,
		},
		{
			name:         "conservatively trigger on go_binary_targets",
			entries:      []dockerfileEntry{{Path: "Dockerfile", GoBinaryTargets: []string{"./cmd/server"}}},
			changedFiles: []string{"docs/README.md"},
			files:        map[string][]byte{"Dockerfile": selectiveDockerfile},
			want:         true,
		},
		{
			name:         "conservatively trigger on broad COPY",
			entries:      []dockerfileEntry{{Path: "Dockerfile"}},
			changedFiles: []string{"docs/README.md"},
			files:        map[string][]byte{"Dockerfile": []byte("FROM golang:1.21\nCOPY . .\n")},
			want:         true,
		},
		{
			name:         "conservatively trigger on fetch error",
			entries:      []dockerfileEntry{{Path: "Dockerfile.missing"}},
			changedFiles: []string{"docs/README.md"},
			files:        map[string][]byte{},
			want:         true,
		},
		{
			name: "multiple entries, second matches",
			entries: []dockerfileEntry{
				{Path: "Dockerfile"},
				{Path: "Dockerfile.control-plane"},
			},
			changedFiles: []string{"control-plane/main.go"},
			files: map[string][]byte{
				"Dockerfile":               []byte("FROM golang:1.21\nCOPY cmd/ cmd/\n"),
				"Dockerfile.control-plane": []byte("FROM golang:1.21\nCOPY control-plane/ control-plane/\n"),
			},
			want: true,
		},
		{
			name: "multiple entries, none match",
			entries: []dockerfileEntry{
				{Path: "Dockerfile"},
				{Path: "Dockerfile.control-plane"},
			},
			changedFiles: []string{"docs/README.md"},
			files: map[string][]byte{
				"Dockerfile":               []byte("FROM golang:1.21\nCOPY cmd/ cmd/\n"),
				"Dockerfile.control-plane": []byte("FROM golang:1.21\nCOPY control-plane/ control-plane/\n"),
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ghc := &fakeGhClient{files: tt.files}
			got := evaluateDockerfileChanges(tt.entries, tt.changedFiles, testProwJob(), ghc)
			if got != tt.want {
				t.Errorf("evaluateDockerfileChanges() = %v, want %v", got, tt.want)
			}
		})
	}
}
