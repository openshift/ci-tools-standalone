// Command ops-alert-dashboard serves a Slack-backed operations queue without a database.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	"sigs.k8s.io/yaml"

	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
)

// options keeps process settings separate from source identities and classification data.
type options struct {
	listen        string
	config        string
	classes       string
	credentials   credentialPaths
	syncInterval  time.Duration
	historyWindow time.Duration
	demo          bool
	validateOnly  bool
	views         viewOptions
}

// viewOptions configures browser labels and the independent public GitHub view.
type viewOptions struct {
	channelName     string
	prAuthors       string
	githubTokenFile string
}

// credentialPaths permits a separate history token when a workspace requires it.
type credentialPaths struct {
	writer string
	reader string
}

// settings holds deployment-owned identity configuration, not Slack credentials.
type settings struct {
	Slack      opsalerts.Config `json:"slack"`
	Monitoring monitoringConfig `json:"monitoring"`
	Access     accessConfig     `json:"access"`
}

// parseOptions requires a deliberate production configuration and a bounded rolling history.
func parseOptions(args []string) (options, error) {
	var o options
	flags := flag.NewFlagSet("ops-alert-dashboard", flag.ContinueOnError)
	flags.StringVar(&o.listen, "listen", "127.0.0.1:8080", "HTTP listen address; keep the backend private behind authenticated SSO")
	flags.StringVar(&o.config, "config", "", "Path to Slack source identities and access configuration")
	flags.StringVar(&o.classes, "classification", "", "Path to release/core-services/slack-bot/_ops-alerts.yaml")
	flags.StringVar(&o.credentials.writer, "slack-token-file", "", "File containing the DPTP-bot Slack token (never sent to the browser)")
	flags.StringVar(&o.credentials.reader, "slack-history-token-file", "", "Optional separate history/replies token; defaults to the bot token")
	flags.DurationVar(&o.syncInterval, "sync-interval", time.Minute, "Interval between complete channel reconciliations (browser polls memory every 15s)")
	flags.DurationVar(&o.historyWindow, "history-window", opsalerts.DefaultHistoryWindow, "Rolling visible Slack history window (positive duration)")
	flags.BoolVar(&o.demo, "demo", false, "Use synthetic in-memory Slack data on loopback, with no Slack calls")
	flags.BoolVar(&o.validateOnly, "validate-only", false, "Validate config and classification without connecting to Slack")
	flags.StringVar(&o.views.channelName, "slack-channel-name", "ops-testplatform", "Human-readable Slack channel name displayed in the dashboard")
	flags.StringVar(&o.views.prAuthors, "unmerged-pr-authors", "openshift-bot,openshift-ci-robot", "Exact comma-separated GitHub authors for open openshift/release PRs; empty disables the view")
	flags.StringVar(&o.views.githubTokenFile, "github-token-file", "", "Optional server-side GitHub token file for the read-only PR cache")
	if err := flags.Parse(args); err != nil {
		return o, err
	}
	if flags.NArg() != 0 || o.syncInterval < time.Second {
		return o, fmt.Errorf("no positional arguments are accepted and --sync-interval must be at least 1s")
	}
	if o.historyWindow <= 0 {
		return o, fmt.Errorf("--history-window must be positive")
	}
	if err := validateChannelName(o.views.channelName); err != nil {
		return o, err
	}
	if o.demo {
		host, _, err := net.SplitHostPort(o.listen)
		if err != nil || !isLoopbackHost(host) {
			return o, fmt.Errorf("--demo requires a loopback --listen address")
		}
		if o.config != "" || o.classes != "" || o.credentials.writer != "" || o.credentials.reader != "" || o.views.githubTokenFile != "" {
			return o, fmt.Errorf("--demo cannot be combined with production config or credentials")
		}
		return o, nil
	}
	if o.config == "" || o.classes == "" || (!o.validateOnly && o.credentials.writer == "") {
		return o, fmt.Errorf("--config, --classification and --slack-token-file are required (validation does not require a token)")
	}
	return o, nil
}

// loadSettings validates identity and access before authenticating or writing to Slack.
func loadSettings(o options) (settings, *opsalerts.Classifier, error) {
	var cfg settings
	data, err := os.ReadFile(o.config)
	if err != nil {
		return cfg, nil, fmt.Errorf("reading dashboard configuration: %w", err)
	}
	if err := yaml.UnmarshalStrict(data, &cfg); err != nil {
		return cfg, nil, fmt.Errorf("decoding dashboard configuration: %w", err)
	}
	cfg.Slack.HistoryWindow = o.historyWindow
	data, err = os.ReadFile(o.classes)
	if err != nil {
		return cfg, nil, fmt.Errorf("reading classification configuration: %w", err)
	}
	classes, err := opsalerts.LoadClassification(data)
	if err != nil {
		return cfg, nil, err
	}
	if cfg.Slack.Writer.UserID == "" {
		return cfg, nil, fmt.Errorf("slack.writer.user_id must identify DPTP-bot")
	}
	if _, err := compileAccess(cfg.Access, false); err != nil {
		return cfg, nil, err
	}
	if err := cfg.Monitoring.validate(); err != nil {
		return cfg, nil, err
	}
	// Construction checks source configuration only; this archive is never called.
	if _, err := opsalerts.NewService(cfg.Slack, classes, &slackArchive{}, nil); err != nil {
		return cfg, nil, err
	}
	return cfg, classes, nil
}

// readToken accepts only a secret file so tokens never appear in process arguments.
func readToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading Slack token file: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("slack token file is empty")
	}
	return token, nil
}

// productionService verifies that action posts will use the expected bot identity.
func productionService(ctx context.Context, cfg settings, classes *opsalerts.Classifier, paths credentialPaths) (*opsalerts.Service, error) {
	writerToken, err := readToken(paths.writer)
	if err != nil {
		return nil, err
	}
	readerToken := writerToken
	if paths.reader != "" {
		readerToken, err = readToken(paths.reader)
		if err != nil {
			return nil, err
		}
	}
	writer := newSlackClient(writerToken)
	if err := verifySlackIdentity(ctx, writer, &cfg.Slack); err != nil {
		return nil, err
	}
	reader := newSlackClient(readerToken)
	identity, err := reader.AuthTestContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("checking Slack history identity: %w", err)
	}
	if identity.TeamID != cfg.Slack.WorkspaceID {
		return nil, fmt.Errorf("history token belongs to a different Slack workspace")
	}
	users := &workspaceUsers{client: reader, lifetime: ctx}
	return opsalerts.NewService(cfg.Slack, classes, &slackArchive{reader: reader, writer: writer, directory: &slackDirectory{client: reader, workspace: users}}, nil)
}

// synchronize reconciles Slack independently of fast browser reads.
func synchronize(ctx context.Context, service *opsalerts.Service, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := service.Sync(ctx); err != nil && ctx.Err() == nil {
			logrus.WithError(err).Warn("Slack archive reconciliation incomplete")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// synchronizeConversations refreshes one active thread independently of full channel discovery.
func synchronizeConversations(ctx context.Context, service *opsalerts.Service) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		readCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
		err := service.SyncConversation(readCtx)
		cancel()
		if err != nil && ctx.Err() == nil {
			logrus.WithError(err).Debug("Chai conversation refresh incomplete")
		}
	}
}

// shutdownOnCancel lets active requests finish without keeping the process alive forever.
func shutdownOnCancel(ctx context.Context, server *http.Server) {
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		logrus.WithError(err).Warn("dashboard shutdown incomplete")
	}
}

// run starts the independent binary with either a real archive or an explicit safe demo.
func run(ctx context.Context, args []string) error {
	o, err := parseOptions(args)
	if err != nil {
		return err
	}
	var cfg settings
	var service *opsalerts.Service
	if o.demo {
		service, err = demoService(ctx, time.Now().UTC(), o.historyWindow)
		if o.syncInterval == time.Minute {
			o.syncInterval = 2 * time.Second
		}
	} else {
		var classes *opsalerts.Classifier
		cfg, classes, err = loadSettings(o)
		if err == nil && !o.validateOnly {
			service, err = productionService(ctx, cfg, classes, o.credentials)
		}
	}
	if err != nil {
		return err
	}
	if o.validateOnly {
		if _, err := newPullRequests(splitAuthors(o.views.prAuthors), "", false); err != nil {
			return err
		}
		logrus.Info("dashboard configuration is valid")
		return nil
	}
	provider, err := newMetricsProvider(cfg.Monitoring, o.demo)
	if err != nil {
		return err
	}
	prs, err := newPullRequests(splitAuthors(o.views.prAuthors), o.views.githubTokenFile, o.demo)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", o.listen)
	if err != nil {
		return fmt.Errorf("listening for dashboard requests: %w", err)
	}
	if o.demo {
		cfg.Access.ExternalURL = "http://" + listener.Addr().String()
	}
	page, err := newDashboard(service, cfg.Access, o.demo)
	if err != nil {
		if closeErr := listener.Close(); closeErr != nil {
			logrus.WithError(closeErr).Warn("closing unused dashboard listener")
		}
		return err
	}
	page.metricsProvider = provider
	page.channelName = strings.TrimPrefix(strings.TrimSpace(o.views.channelName), "#")
	page.pullRequests = prs
	server := &http.Server{Handler: page, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: time.Minute, IdleTimeout: time.Minute, MaxHeaderBytes: 16 * 1024}
	go synchronize(ctx, service, o.syncInterval)
	go synchronizeConversations(ctx, service)
	go prs.Run(ctx)
	go shutdownOnCancel(ctx, server)
	logrus.WithFields(logrus.Fields{"listen": listener.Addr().String(), "demo": o.demo}).Info("ops alert dashboard listening")
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving dashboard: %w", err)
	}
	return nil
}

// main ties service shutdown to the normal process signals.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		logrus.WithError(err).Fatal("ops alert dashboard stopped")
	}
}
