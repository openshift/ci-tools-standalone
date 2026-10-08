package main

import (
	"fmt"
	"os"
	"reflect"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

// AgenticConfig hands second-stage execution to Chai without changing the fallback trigger.
type AgenticConfig struct {
	Mode string `yaml:"mode,omitempty"`
}

func (a AgenticConfig) enabled() bool { return a.Mode == "chai" }

func (a *AgenticConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var raw struct {
		Mode  string                 `yaml:"mode,omitempty"`
		Extra map[string]interface{} `yaml:",inline"`
	}
	if err := unmarshal(&raw); err != nil {
		return err
	}
	for key := range raw.Extra {
		return fmt.Errorf("unsupported repository agentic option %q; timeout and trusted authors are controller flags", key)
	}
	a.Mode = raw.Mode
	if a.Mode != "" && !a.enabled() {
		return fmt.Errorf("unsupported agentic mode %q", a.Mode)
	}
	return nil
}

type RepoMode struct {
	Trigger string        `yaml:"trigger,omitempty"`
	Agentic AgenticConfig `yaml:"agentic,omitempty"`
}

// RepoItem represents a repository configuration that can be either a string or an object
type RepoItem struct {
	Name     string
	Branches []string
	Mode     RepoMode
}

// UnmarshalYAML implements custom unmarshaling to support both string and object formats
func (r *RepoItem) UnmarshalYAML(unmarshal func(interface{}) error) error {
	// Try to unmarshal as a string first (backwards compatibility)
	var repoString string
	if err := unmarshal(&repoString); err == nil {
		r.Name = repoString
		r.Mode.Trigger = "auto" // default to auto for backwards compatibility
		return nil
	}

	// If string unmarshaling failed, try as a struct
	type rawRepo struct {
		Name     string   `yaml:"name"`
		Branches []string `yaml:"branches,omitempty"`
		Mode     RepoMode `yaml:"mode,omitempty"`
	}
	var raw rawRepo
	if err := unmarshal(&raw); err != nil {
		return err
	}

	r.Name = raw.Name
	r.Branches = raw.Branches
	r.Mode = raw.Mode
	if r.Mode.Trigger == "" {
		r.Mode.Trigger = "auto" // default to auto if not specified
	}
	if r.Mode.Agentic.enabled() && r.Mode.Trigger != "auto" && r.Mode.Trigger != "manual" && r.Mode.Trigger != "lgtm" {
		return fmt.Errorf("repository %s: unsupported trigger %q", r.Name, r.Mode.Trigger)
	}
	return nil
}

// enabled config struct represents the YAML file structure of enabled repos and orgs
type enabledConfig struct {
	Orgs []struct {
		Org   string     `yaml:"org"`
		Repos []RepoItem `yaml:"repos"`
	} `yaml:"orgs"`
}

// RepoConfig contains configuration for a single repository
type RepoConfig struct {
	Trigger  string
	Branches []string // If empty, all branches are enabled
	Agentic  AgenticConfig
}

// watcher struct encapsulates the file watcher and configuration
type watcher struct {
	filePath string
	config   enabledConfig
	mutex    sync.Mutex
	logger   *logrus.Entry
	onChange func()
}

func newWatcher(filePath string, logger *logrus.Entry) *watcher {
	watcher := &watcher{
		filePath: filePath,
		logger:   logger,
	}

	return watcher
}

func (w *watcher) watch() {
	// Use polling instead of fsnotify because git-sync doesn't trigger filesystem events
	ticker := time.NewTicker(3 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		if err := w.reloadConfig(); err != nil {
			w.logger.WithError(err).Error("Failed to reload config")
		}
	}
}

func (w *watcher) reloadConfig() error {
	yamlFile, err := os.Open(w.filePath)
	if err != nil {
		return err
	}

	defer yamlFile.Close()

	decoder := yaml.NewDecoder(yamlFile)
	var next enabledConfig
	err = decoder.Decode(&next)
	if err != nil {
		return err
	}
	w.mutex.Lock()
	changed := !reflect.DeepEqual(w.config, next)
	w.config = next
	onChange := w.onChange
	w.mutex.Unlock()
	if changed && w.logger != nil {
		w.logger.Info("Config change detected, config reloaded successfully")
	}
	if changed && onChange != nil {
		onChange() // Outside the lock: listeners may read the new configuration.
	}

	return nil
}

func (w *watcher) setOnChange(onChange func()) {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	w.onChange = onChange
}

func (w *watcher) getConfig() map[string]map[string]RepoConfig {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	ret := map[string]map[string]RepoConfig{}
	for _, org := range w.config.Orgs {
		repoConfigs := map[string]RepoConfig{}
		for _, repo := range org.Repos {
			repoConfigs[repo.Name] = RepoConfig{
				Trigger:  repo.Mode.Trigger,
				Branches: repo.Branches,
				Agentic:  repo.Mode.Agentic,
			}
		}
		ret[org.Org] = repoConfigs
	}
	return ret
}
