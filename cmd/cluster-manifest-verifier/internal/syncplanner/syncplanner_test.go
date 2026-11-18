package syncplanner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	argov1alpha1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openshift/ci-tools-standalone/cmd/cluster-manifest-verifier/internal/gitchanges"
)

func TestCoversPath(t *testing.T) {
	p := New(t.TempDir())
	testCases := []struct {
		name        string
		sourcePath  string
		changedPath string
		want        bool
	}{
		{
			name:        "child file",
			sourcePath:  "clusters/build-clusters/foo",
			changedPath: "clusters/build-clusters/foo/config.yaml",
			want:        true,
		},
		{
			name:        "trailing slash on source",
			sourcePath:  "clusters/build-clusters/foo/",
			changedPath: "clusters/build-clusters/foo/config.yaml",
			want:        true,
		},
		{
			name:        "different app",
			sourcePath:  "clusters/build-clusters/foo",
			changedPath: "clusters/build-clusters/bar/config.yaml",
			want:        false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			app := argov1alpha1.Application{
				Spec: argov1alpha1.ApplicationSpec{
					Source: &argov1alpha1.ApplicationSource{Path: tc.sourcePath},
				},
			}
			if got := p.coversPath(app, tc.changedPath); got != tc.want {
				t.Fatalf("coversPath() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseManifestResources(t *testing.T) {
	p := New(t.TempDir())
	testCases := []struct {
		name       string
		manifest   string
		want       []argov1alpha1.SyncOperationResource
		wantErr    bool
		wantErrSub string
	}{
		{
			name: "multi-document manifest with labels",
			manifest: `apiVersion: v1
kind: ConfigMap
metadata:
  labels:
    ci.openshift.io/gitops-managed: "true"
  name: test
  namespace: default
---
apiVersion: apps/v1
kind: Deployment
metadata:
  labels:
    ci.openshift.io/gitops-managed: "true"
  name: app
`,
			want: []argov1alpha1.SyncOperationResource{
				{Kind: "ConfigMap", Name: "test", Namespace: "default"},
				{Group: "apps", Kind: "Deployment", Name: "app"},
			},
		},
		{
			name: "missing gitops-managed label",
			manifest: `apiVersion: v1
kind: ConfigMap
metadata:
  name: test
  namespace: default
`,
			wantErr:    true,
			wantErrSub: "ConfigMap/test missing label " + GitopsManagedLabel + "=true",
		},
		{
			name: "multi-doc one unlabeled fails",
			manifest: `apiVersion: v1
kind: ConfigMap
metadata:
  labels:
    ci.openshift.io/gitops-managed: "true"
  name: labeled
  namespace: default
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: unlabeled
  namespace: default
`,
			wantErr:    true,
			wantErrSub: "ConfigMap/unlabeled missing label " + GitopsManagedLabel + "=true",
		},
		{
			name: "list requires full sync",
			manifest: `apiVersion: v1
kind: List
metadata:
  labels:
    ci.openshift.io/gitops-managed: "true"
  name: items
`,
			wantErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "manifest.yaml")
			if err := os.WriteFile(path, []byte(tc.manifest), 0o644); err != nil {
				t.Fatalf("write manifest: %v", err)
			}

			got, err := p.parseManifestResources(path)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				if tc.wantErrSub != "" && !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseManifestResources() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("parseManifestResources() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestPlannerBuild(t *testing.T) {
	testCases := []struct {
		name       string
		files      map[string]string
		apps       map[string]argov1alpha1.Application
		changes    []gitchanges.FileChange
		wantErr    bool
		wantErrSub string
		wantPlans  map[string]*Plan
	}{
		{
			name: "orphan changed file fails",
			apps: map[string]argov1alpha1.Application{
				"foo": {
					ObjectMeta: metav1.ObjectMeta{Name: "foo"},
					Spec: argov1alpha1.ApplicationSpec{
						Source: &argov1alpha1.ApplicationSource{Path: "clusters/build-clusters/foo"},
					},
				},
			},
			changes: []gitchanges.FileChange{
				{Path: "clusters/orphan/config.yaml", Status: 'A'},
			},
			wantErr:    true,
			wantErrSub: "not covered by any Application source path",
		},
		{
			name: "uncovered deleted file is ignored",
			apps: map[string]argov1alpha1.Application{
				"foo": {
					ObjectMeta: metav1.ObjectMeta{Name: "foo"},
					Spec: argov1alpha1.ApplicationSpec{
						Source: &argov1alpha1.ApplicationSource{Path: "clusters/build-clusters/foo"},
					},
				},
			},
			changes: []gitchanges.FileChange{
				{Path: "clusters/app.ci/removed-app/deploy.yaml", Status: 'D'},
			},
			wantPlans: map[string]*Plan{},
		},
		{
			name: "covered deleted file plans full sync",
			apps: map[string]argov1alpha1.Application{
				"foo": {
					ObjectMeta: metav1.ObjectMeta{Name: "foo"},
					Spec: argov1alpha1.ApplicationSpec{
						Source: &argov1alpha1.ApplicationSource{Path: "clusters/build-clusters/foo"},
					},
				},
			},
			changes: []gitchanges.FileChange{
				{Path: "clusters/build-clusters/foo/config.yaml", Status: 'D'},
			},
			wantPlans: map[string]*Plan{
				"foo": {Name: "foo", FullSync: true},
			},
		},
		{
			name: "gitops app change is ignored",
			apps: map[string]argov1alpha1.Application{
				"foo": {
					ObjectMeta: metav1.ObjectMeta{Name: "foo"},
					Spec: argov1alpha1.ApplicationSpec{
						Source: &argov1alpha1.ApplicationSource{Path: "clusters/build-clusters/foo"},
					},
				},
			},
			changes: []gitchanges.FileChange{
				{Path: "clusters/gitops/apps/appset.yaml", Status: 'M'},
			},
			wantPlans: map[string]*Plan{},
		},
		{
			name: "covered labeled manifest plans resources",
			files: map[string]string{
				"clusters/build-clusters/foo/config.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  labels:
    ci.openshift.io/gitops-managed: "true"
  name: test
  namespace: default
`,
			},
			apps: map[string]argov1alpha1.Application{
				"foo": {
					ObjectMeta: metav1.ObjectMeta{Name: "foo"},
					Spec: argov1alpha1.ApplicationSpec{
						Source: &argov1alpha1.ApplicationSource{Path: "clusters/build-clusters/foo"},
					},
				},
			},
			changes: []gitchanges.FileChange{
				{Path: "clusters/build-clusters/foo/config.yaml", Status: 'M'},
			},
			wantPlans: map[string]*Plan{
				"foo": {
					Name: "foo",
					Resources: []argov1alpha1.SyncOperationResource{
						{Kind: "ConfigMap", Name: "test", Namespace: "default"},
					},
				},
			},
		},
		{
			name: "covered unlabeled manifest fails with label error",
			files: map[string]string{
				"clusters/build-clusters/foo/config.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: test
  namespace: default
`,
			},
			apps: map[string]argov1alpha1.Application{
				"foo": {
					ObjectMeta: metav1.ObjectMeta{Name: "foo"},
					Spec: argov1alpha1.ApplicationSpec{
						Source: &argov1alpha1.ApplicationSource{Path: "clusters/build-clusters/foo"},
					},
				},
			},
			changes: []gitchanges.FileChange{
				{Path: "clusters/build-clusters/foo/config.yaml", Status: 'M'},
			},
			wantErr:    true,
			wantErrSub: "ConfigMap/test missing label " + GitopsManagedLabel + "=true",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			repoDir := t.TempDir()
			for path, content := range tc.files {
				full := filepath.Join(repoDir, path)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
					t.Fatalf("write %s: %v", path, err)
				}
			}
			planner := New(repoDir)
			plans, err := planner.Build(tc.apps, tc.changes)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				if tc.wantErrSub != "" && !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			if diff := cmp.Diff(tc.wantPlans, plans); diff != "" {
				t.Fatalf("Build() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
