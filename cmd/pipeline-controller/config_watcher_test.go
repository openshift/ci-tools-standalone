package main

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v2"
)

func TestRequiredLabelsConfig(t *testing.T) {
	const doc = `
orgs:
- org: Azure
  repos:
  - name: ARO-HCP
    branches:
    - main
    mode:
      trigger: auto
      required_labels:
      - acknowledge-critical-fixes-only
`
	var enabled enabledConfig
	if err := yaml.Unmarshal([]byte(doc), &enabled); err != nil {
		t.Fatalf("failed to parse enabled config: %v", err)
	}

	watcher := &watcher{config: enabled}
	config := watcher.getConfig()["Azure"]["ARO-HCP"]
	if got, want := config.RequiredLabels, []string{"acknowledge-critical-fixes-only"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("required labels = %v, want %v", got, want)
	}
}
