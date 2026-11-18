package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
)

// webFiles makes the UI deployable with one binary and no asset server.
//
//go:embed web/*
var webFiles embed.FS

// alertService is the narrow boundary between HTTP identity and Slack-backed state.
type alertService interface {
	Snapshot() opsalerts.Snapshot
	Act(context.Context, string, opsalerts.ActionRequest) (opsalerts.Occurrence, error)
	Preview(opsalerts.PreviewRequest) ([]opsalerts.Occurrence, error)
}

// pullRequestSource serves the shared GitHub cache without making request-driven API calls.
type pullRequestSource interface {
	Snapshot() pullRequestSnapshot
}

// accessConfig describes the authenticated reverse proxy, never browser-supplied identity.
type accessConfig struct {
	Authorization  string   `json:"authorization"`
	UserHeader     string   `json:"user_header"`
	Operators      []string `json:"operators"`
	TrustedProxies []string `json:"trusted_proxies"`
	ExternalURL    string   `json:"external_url"`
}

// accessPolicy compiles proxy trust and operator authorization once at startup.
type accessPolicy struct {
	header             string
	users              sets.Set[string]
	proxies            []netip.Prefix
	origin             string
	demo               bool
	proxyAuthorization bool
}

// dashboard serves private Slack data only after the proxy and operator checks.
type dashboard struct {
	service         alertService
	access          accessPolicy
	files           http.Handler
	csrfKey         []byte
	sopClient       sopHTTPClient
	metricsProvider metricsProvider
	channelName     string
	pullRequests    pullRequestSource
}

// snapshotResponse supplies browser identity and CSRF protection alongside cached state.
type snapshotResponse struct {
	opsalerts.Snapshot
	User        string           `json:"user"`
	CSRFToken   string           `json:"csrf_token"`
	Demo        bool             `json:"demo"`
	Monitoring  monitoringStatus `json:"monitoring"`
	ChannelName string           `json:"channel_name"`
}

// actionResponse returns the committed occurrence after a confirmed Slack write.
type actionResponse struct {
	Occurrence opsalerts.Occurrence `json:"occurrence"`
}

// apiError exposes a stable retry decision without exposing internal state.
type apiError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// newDashboard validates the proxy boundary before making Slack data available.
func newDashboard(service alertService, cfg accessConfig, demo bool) (*dashboard, error) {
	policy, err := compileAccess(cfg, demo)
	if err != nil {
		return nil, err
	}
	assets, err := fs.Sub(webFiles, "web")
	if err != nil {
		return nil, fmt.Errorf("opening embedded dashboard: %w", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("creating CSRF key: %w", err)
	}
	return &dashboard{service: service, access: policy, files: http.FileServer(http.FS(assets)), csrfKey: key, sopClient: newSOPClient()}, nil
}

// compileAccess rejects an accidentally public identity-header trust boundary.
func compileAccess(cfg accessConfig, demo bool) (accessPolicy, error) {
	policy := accessPolicy{header: cfg.UserHeader, users: sets.New[string](), demo: demo}
	switch cfg.Authorization {
	case "":
	case "proxy":
		if demo {
			return policy, fmt.Errorf("access.authorization proxy cannot be combined with demo mode")
		}
		if len(cfg.Operators) != 0 {
			return policy, fmt.Errorf("access.authorization proxy cannot be combined with access.operators")
		}
		if len(cfg.TrustedProxies) == 0 {
			return policy, fmt.Errorf("access.authorization proxy requires explicit access.trusted_proxies")
		}
		policy.proxyAuthorization = true
	default:
		return policy, fmt.Errorf("access.authorization must be empty or proxy")
	}
	if policy.header == "" {
		policy.header = "X-Forwarded-Email"
	}
	if strings.ContainsAny(policy.header, " \r\n:") {
		return policy, fmt.Errorf("access.user_header is invalid")
	}
	origin, err := url.Parse(cfg.ExternalURL)
	if err != nil || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || (origin.Path != "" && origin.Path != "/") {
		return policy, fmt.Errorf("access.external_url must be an origin without a path")
	}
	if origin.Scheme != "https" && (!demo || origin.Scheme != "http" || !isLoopbackHost(origin.Hostname())) {
		return policy, fmt.Errorf("access.external_url must use https (loopback demo may use http)")
	}
	policy.origin = origin.Scheme + "://" + origin.Host
	for _, user := range cfg.Operators {
		user = strings.ToLower(strings.TrimSpace(user))
		if !validIdentity(user) {
			return policy, fmt.Errorf("access.operators contains an invalid identity")
		}
		policy.users.Insert(user)
	}
	if !demo && !policy.proxyAuthorization && policy.users.Len() == 0 {
		return policy, fmt.Errorf("access.operators must allow at least one authenticated operator")
	}
	proxies := cfg.TrustedProxies
	if len(proxies) == 0 {
		proxies = []string{"127.0.0.1/32", "::1/128"}
	}
	for _, raw := range proxies {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Bits() == 0 {
			return policy, fmt.Errorf("access.trusted_proxies must contain explicit non-universal CIDRs")
		}
		policy.proxies = append(policy.proxies, prefix)
	}
	return policy, nil
}

// validIdentity rejects ambiguous or malformed proxy identities before authorization.
func validIdentity(user string) bool {
	return user != "" && len(user) <= 256 && strings.IndexFunc(user, invalidIdentityRune) == -1
}

// invalidIdentityRune excludes header lists, control characters and embedded whitespace.
func invalidIdentityRune(r rune) bool {
	return r == ',' || unicode.IsControl(r) || unicode.IsSpace(r)
}

// isLoopbackHost keeps unauthenticated demo mode on the local machine.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// actor uses only identities injected by an allowlisted reverse proxy.
func (p accessPolicy) actor(r *http.Request) (string, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "", fmt.Errorf("invalid proxy address")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "", fmt.Errorf("invalid proxy address")
	}
	trusted := false
	for _, prefix := range p.proxies {
		if prefix.Contains(ip.Unmap()) {
			trusted = true
			break
		}
	}
	if !trusted {
		return "", fmt.Errorf("requests must arrive through the configured authenticated proxy")
	}
	if p.demo {
		if !ip.IsLoopback() {
			return "", fmt.Errorf("demo requests must be local")
		}
		return "demo@localhost", nil
	}
	values := r.Header.Values(p.header)
	if len(values) != 1 {
		return "", fmt.Errorf("authenticated identity is required")
	}
	user := strings.ToLower(strings.TrimSpace(values[0]))
	if !utf8.ValidString(values[0]) || strings.IndexFunc(values[0], unicode.IsControl) != -1 || !validIdentity(user) {
		return "", fmt.Errorf("authenticated identity is invalid")
	}
	if !p.proxyAuthorization && !p.users.Has(user) {
		return "", fmt.Errorf("operator access is required")
	}
	return user, nil
}

// ServeHTTP applies the same authorization to the page, assets and JSON endpoints.
func (d *dashboard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data: https://slack-edge.com https://*.slack-edge.com https://slack-imgs.com https://*.slack-imgs.com https://slack.com https://*.slack.com https://secure.gravatar.com; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/readyz" {
		status := http.StatusServiceUnavailable
		if d.service.Snapshot().Status.Phase == "ready" {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		return
	}
	actor, err := d.access.actor(r)
	if err != nil {
		writeJSON(w, http.StatusForbidden, apiError{Error: err.Error(), Code: "forbidden"})
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/snapshot":
		writeJSON(w, http.StatusOK, snapshotResponse{Snapshot: d.service.Snapshot(), User: actor, CSRFToken: d.csrfToken(actor), Demo: d.access.demo, Monitoring: monitoringStatus{Enabled: d.metricsProvider != nil, Source: "core-ci"}, ChannelName: d.channelName})
	case r.Method == http.MethodGet && r.URL.Path == "/api/unmerged-prs":
		if d.pullRequests == nil {
			writeJSON(w, http.StatusOK, pullRequestSnapshot{Phase: "disabled", PRs: []pullRequest{}})
			return
		}
		writeJSON(w, http.StatusOK, d.pullRequests.Snapshot())
	case r.Method == http.MethodGet && r.URL.Path == "/api/metrics":
		d.metrics(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/sop":
		d.sop(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/actions":
		d.action(w, r, actor)
	case r.Method == http.MethodPost && r.URL.Path == "/api/action-preview":
		d.preview(w, r, actor)
	case r.Method == http.MethodGet && r.URL.Path == "/api/conversations":
		d.conversations(w, r, actor)
	case r.Method == http.MethodPost && r.URL.Path == "/api/conversations":
		d.converse(w, r, actor)
	case (r.Method == http.MethodGet || r.Method == http.MethodPost) && r.URL.Path == "/api/alert-thread":
		d.alertThread(w, r, actor)
	case r.Method == http.MethodGet && (r.URL.Path == "/" || r.URL.Path == "/app.js" || r.URL.Path == "/reports.js" || r.URL.Path == "/chat.js" || r.URL.Path == "/threads.js" || r.URL.Path == "/style.css"):
		d.files.ServeHTTP(w, r)
	default:
		writeJSON(w, http.StatusNotFound, apiError{Error: "endpoint not found", Code: "not_found"})
	}
}

// csrfToken binds a read-only snapshot token to the authenticated operator.
func (d *dashboard) csrfToken(actor string) string {
	h := hmac.New(sha256.New, d.csrfKey)
	// Hash.Write cannot fail and always consumes the complete input.
	_, _ = h.Write([]byte(actor))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// action validates browser provenance before passing server-derived identity to Slack.
func (d *dashboard) action(w http.ResponseWriter, r *http.Request, actor string) {
	if r.Header.Get("Origin") != d.access.origin || !hmac.Equal([]byte(r.Header.Get("X-CSRF-Token")), []byte(d.csrfToken(actor))) {
		writeJSON(w, http.StatusForbidden, apiError{Error: "same-origin CSRF validation failed", Code: "forbidden"})
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, apiError{Error: "application/json is required", Code: "validation"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request opsalerts.ActionRequest
	if err := decoder.Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "invalid action JSON", Code: "validation"})
		return
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "exactly one action is required", Code: "validation"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	occurrence, err := d.service.Act(ctx, actor, request)
	if err != nil {
		status, code := actionError(err)
		writeJSON(w, status, apiError{Error: err.Error(), Code: code})
		return
	}
	writeJSON(w, http.StatusOK, actionResponse{Occurrence: occurrence})
}

// actionError preserves the distinction between rejected, stale and uncertain requests.
func actionError(err error) (int, string) {
	switch {
	case errors.Is(err, opsalerts.ErrValidation):
		return http.StatusBadRequest, "validation"
	case errors.Is(err, opsalerts.ErrNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, opsalerts.ErrConflict):
		return http.StatusConflict, "conflict"
	case errors.Is(err, opsalerts.ErrNotReady):
		return http.StatusServiceUnavailable, "not_ready"
	case errors.Is(err, opsalerts.ErrDeliveryFailed):
		return http.StatusBadGateway, "delivery_failed"
	case errors.Is(err, opsalerts.ErrDeliveryUncertain):
		return http.StatusBadGateway, "delivery_uncertain"
	default:
		logrus.WithError(err).Error("alert action failed")
		return http.StatusInternalServerError, "internal"
	}
}

// writeJSON keeps API errors and successful responses equally safe to render.
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		logrus.WithError(err).Debug("dashboard response disconnected")
	}
}
