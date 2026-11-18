package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
)

// threadTestService isolates HTTP authorization and strict parsing from Slack transport.
type threadTestService struct {
	dashboardTestService
	request       opsalerts.ThreadReplyRequest
	actor         string
	reads, writes int
}

// AlertThread returns only a synthetic cached discussion.
func (s *threadTestService) AlertThread(id, before string) (opsalerts.AlertThread, error) {
	s.reads++
	return opsalerts.AlertThread{OccurrenceID: id, NextCursor: before}, s.err
}

// ReplyToAlert records the authenticated actor and exact transport safety flag.
func (s *threadTestService) ReplyToAlert(_ context.Context, actor string, req opsalerts.ThreadReplyRequest) (opsalerts.AlertThread, error) {
	s.writes++
	s.actor, s.request = actor, req
	return opsalerts.AlertThread{OccurrenceID: req.OccurrenceID}, s.err
}

// refreshingThreadTestService records fresh reads separately from cache-only pagination.
type refreshingThreadTestService struct {
	*threadTestService
	refreshes int
	id        string
	ctx       context.Context
	started   time.Time
	metadata  context.Context
}

// RefreshAlertThread records the target and the bounded request context without Slack calls.
func (s *refreshingThreadTestService) RefreshAlertThread(ctx context.Context, id string) (opsalerts.AlertThread, error) {
	s.refreshes++
	s.id, s.ctx = id, ctx
	s.started = time.Now()
	return opsalerts.AlertThread{OccurrenceID: id}, s.err
}

// EnrichThread records the shared read deadline without making directory requests.
func (s *refreshingThreadTestService) EnrichThread(ctx context.Context, _ *opsalerts.AlertThread) {
	s.metadata = ctx
}

// threadRequestContextKey distinguishes the caller's context from a detached refresh.
type threadRequestContextKey struct{}

// TestDashboardAlertThread tests both endpoints at the existing trusted-proxy boundary.
func TestDashboardAlertThread(t *testing.T) {
	const body = `{"occurrence_id":"C:100.000001","request_id":"plain","text":"hello","reconcile_only":true}`
	cases := []struct {
		name, method, body       string
		path                     string
		noOrigin, noCSRF, unauth bool
		reconcile                bool
		freshService             bool
		canceled                 bool
		callerBudget             time.Duration
		serviceErr               error
		status                   int
		code                     string
		errorText                string
		writes                   int
		reads, refreshes         int
		retryAfter               string
	}{
		{name: "cached read", method: "GET", status: 200, reads: 1},
		{name: "fresh read", method: "GET", path: "/api/alert-thread?occurrence_id=C:100.000001&refresh=true", freshService: true, status: 200, refreshes: 1},
		{name: "fresh read preserves an earlier caller deadline", method: "GET", path: "/api/alert-thread?occurrence_id=C:100.000001&refresh=true", freshService: true, callerBudget: 5 * time.Second, status: 200, refreshes: 1},
		{name: "canceled fresh read does not call service", method: "GET", path: "/api/alert-thread?occurrence_id=C:100.000001&refresh=true", freshService: true, canceled: true, status: 503, code: "not_ready"},
		{name: "explicit cache-only pagination", method: "GET", path: "/api/alert-thread?occurrence_id=C:100.000001&before=100.000002&refresh=false", freshService: true, status: 200, reads: 1},
		{name: "fresh read unavailable", method: "GET", path: "/api/alert-thread?occurrence_id=C:100.000001&refresh=true", status: 503, code: "not_ready", errorText: "may be stale"},
		{name: "fresh pagination rejected", method: "GET", path: "/api/alert-thread?occurrence_id=C:100.000001&before=100.000002&refresh=true", freshService: true, status: 400, code: "validation"},
		{name: "ambiguous refresh rejected", method: "GET", path: "/api/alert-thread?occurrence_id=C:100.000001&refresh=true&refresh=false", freshService: true, status: 400, code: "validation"},
		{name: "malformed refresh query rejected", method: "GET", path: "/api/alert-thread?occurrence_id=C:100.000001&refresh=true&refresh=%zz", freshService: true, status: 400, code: "validation"},
		{name: "invalid refresh rejected", method: "GET", path: "/api/alert-thread?occurrence_id=C:100.000001&refresh=yes", freshService: true, status: 400, code: "validation"},
		{name: "empty refresh rejected", method: "GET", path: "/api/alert-thread?occurrence_id=C:100.000001&refresh=", freshService: true, status: 400, code: "validation"},
		{name: "fresh read requires authentication", method: "GET", path: "/api/alert-thread?occurrence_id=C:100.000001&refresh=true", freshService: true, unauth: true, status: 403, code: "forbidden"},
		{name: "fresh read failure redacted", method: "GET", path: "/api/alert-thread?occurrence_id=C:100.000001&refresh=true", freshService: true, serviceErr: errors.Join(opsalerts.ErrNotReady, errors.New("secret-token-upstream-body")), status: 503, code: "not_ready", errorText: "may be stale", refreshes: 1},
		{name: "fresh read cooldown rounded up", method: "GET", path: "/api/alert-thread?occurrence_id=C:100.000001&refresh=true", freshService: true, serviceErr: &opsalerts.ThreadRefreshError{RetryAfter: 1500 * time.Millisecond}, status: 503, code: "not_ready", errorText: "may be stale", retryAfter: "2", refreshes: 1},
		{name: "fresh read cooldown has a positive minimum", method: "GET", path: "/api/alert-thread?occurrence_id=C:100.000001&refresh=true", freshService: true, serviceErr: &opsalerts.ThreadRefreshError{}, status: 503, code: "not_ready", retryAfter: "1", refreshes: 1},
		{name: "fresh read cooldown exact seconds", method: "GET", path: "/api/alert-thread?occurrence_id=C:100.000001&refresh=true", freshService: true, serviceErr: &opsalerts.ThreadRefreshError{RetryAfter: 3 * time.Second}, status: 503, code: "not_ready", retryAfter: "3", refreshes: 1},
		{name: "fresh read negative cooldown is bounded", method: "GET", path: "/api/alert-thread?occurrence_id=C:100.000001&refresh=true", freshService: true, serviceErr: &opsalerts.ThreadRefreshError{RetryAfter: -time.Second}, status: 503, code: "not_ready", retryAfter: "1", refreshes: 1},
		{name: "fresh read wrapped cooldown", method: "GET", path: "/api/alert-thread?occurrence_id=C:100.000001&refresh=true", freshService: true, serviceErr: fmt.Errorf("wrapped: %w", &opsalerts.ThreadRefreshError{RetryAfter: 3 * time.Second}), status: 503, code: "not_ready", retryAfter: "3", refreshes: 1},
		{name: "unsupported method performs no reads or writes", method: "PUT", path: "/api/alert-thread?occurrence_id=C:100.000001&refresh=true", freshService: true, status: 404, code: "not_found"},
		{name: "reconciliation flag", method: "POST", body: body, status: 200, writes: 1, reconcile: true},
		{name: "initial submission omits flag", method: "POST", body: strings.Replace(body, `,"reconcile_only":true`, "", 1), status: 200, writes: 1},
		{name: "explicit false flag", method: "POST", body: strings.Replace(body, `"reconcile_only":true`, `"reconcile_only":false`, 1), status: 200, writes: 1},
		{name: "no origin", method: "POST", body: body, noOrigin: true, status: 403, code: "forbidden"},
		{name: "no CSRF", method: "POST", body: body, noCSRF: true, status: 403, code: "forbidden"},
		{name: "unauthenticated", method: "GET", unauth: true, status: 403, code: "forbidden"},
		{name: "arbitrary target", method: "POST", body: `{"channel_id":"C","text":"hello"}`, status: 400, code: "validation"},
		{name: "actor override", method: "POST", body: `{"actor":"other","text":"hello"}`, status: 400, code: "validation"},
		{name: "channel name override", method: "POST", body: strings.TrimSuffix(body, "}") + `,"channel_name":"other"}`, status: 400, code: "validation"},
		{name: "extra object", method: "POST", body: body + `{}`, status: 400, code: "validation"},
		{name: "duplicate reconcile flag", method: "POST", body: strings.TrimSuffix(body, "}") + `,"reconcile_only":false}`, status: 400, code: "validation"},
		{name: "case variant reconcile flag", method: "POST", body: strings.TrimSuffix(body, "}") + `,"Reconcile_only":false}`, status: 400, code: "validation"},
		{name: "null reconcile flag", method: "POST", body: strings.Replace(body, `"reconcile_only":true`, `"reconcile_only":null`, 1), status: 400, code: "validation"},
		{name: "numeric reconcile flag", method: "POST", body: strings.Replace(body, `"reconcile_only":true`, `"reconcile_only":1`, 1), status: 400, code: "validation"},
		{name: "string reconcile flag", method: "POST", body: strings.Replace(body, `"reconcile_only":true`, `"reconcile_only":"true"`, 1), status: 400, code: "validation"},
		{name: "case variant text", method: "POST", body: strings.Replace(body, `"text":`, `"Text":`, 1), status: 400, code: "validation"},
		{name: "duplicate text", method: "POST", body: strings.TrimSuffix(body, "}") + `,"text":"different"}`, status: 400, code: "validation"},
		{name: "null required string", method: "POST", body: strings.Replace(body, `"text":"hello"`, `"text":null`, 1), status: 400, code: "validation"},
		{name: "oversized", method: "POST", body: `{"text":"` + strings.Repeat("a", 17000) + `"}`, status: 400, code: "validation"},
		{name: "uncertain redacted", method: "POST", body: body, serviceErr: errors.Join(opsalerts.ErrDeliveryUncertain, errors.New("secret-token-upstream-body")), status: 502, code: "delivery_uncertain", writes: 1},
		{name: "unresolved reference is actionable", method: "POST", body: body, serviceErr: &opsalerts.ThreadReferenceError{Reference: "@missing"}, status: 400, code: "validation", errorText: `cannot resolve Slack reference "@missing"`, writes: 1},
		{name: "workspace loading is a definite rejection", method: "POST", body: body, serviceErr: &opsalerts.ThreadReferenceLookupError{}, status: 503, code: "not_ready", errorText: "no reply was posted", writes: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := &threadTestService{}
			service.err = tc.serviceErr
			fresh := &refreshingThreadTestService{threadTestService: service}
			var backend alertService = service
			if tc.freshService {
				backend = fresh
			}
			d, err := newDashboard(backend, testAccess(), false)
			if err != nil {
				t.Fatal(err)
			}
			d.channelName = "ops-team"
			path := tc.path
			if path == "" {
				path = "/api/alert-thread?occurrence_id=C:100.000001&before=100.000002"
			}
			r := httptest.NewRequest(tc.method, path, strings.NewReader(tc.body))
			r = r.WithContext(context.WithValue(r.Context(), threadRequestContextKey{}, "request context"))
			if tc.callerBudget > 0 {
				ctx, cancel := context.WithTimeout(r.Context(), tc.callerBudget)
				defer cancel()
				r = r.WithContext(ctx)
			}
			if tc.canceled {
				ctx, cancel := context.WithCancel(r.Context())
				cancel()
				r = r.WithContext(ctx)
			}
			r.RemoteAddr = "127.0.0.1:1234"
			if !tc.unauth {
				r.Header.Set("X-Forwarded-Email", "operator@example.test")
			}
			if !tc.noOrigin {
				r.Header.Set("Origin", d.access.origin)
			}
			if !tc.noCSRF {
				r.Header.Set("X-CSRF-Token", d.csrfToken("operator@example.test"))
			}
			r.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			d.ServeHTTP(response, r)
			if diff := cmp.Diff(tc.status, response.Code); diff != "" {
				t.Fatalf("unexpected status (-want +got):\n%s\n%s", diff, response.Body.String())
			}
			if diff := cmp.Diff(tc.writes, service.writes); diff != "" {
				t.Errorf("unexpected writes (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.reads, service.reads); diff != "" {
				t.Errorf("unexpected cached reads (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.refreshes, fresh.refreshes); diff != "" {
				t.Errorf("unexpected fresh reads (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.retryAfter, response.Header().Get("Retry-After")); diff != "" {
				t.Errorf("unexpected retry delay (-want +got):\n%s", diff)
			}
			if fresh.refreshes > 0 {
				deadline, bounded := fresh.ctx.Deadline()
				if fresh.id != "C:100.000001" || fresh.ctx.Value(threadRequestContextKey{}) != "request context" || !bounded {
					t.Errorf("refresh lost its target or bounded request context: target %q, bounded %t", fresh.id, bounded)
				}
				budget := 10 * time.Second
				if tc.callerBudget > 0 {
					budget = min(budget, tc.callerBudget)
				}
				if deadline.After(fresh.started.Add(budget)) {
					t.Errorf("refresh deadline exceeds its %s budget", budget)
				}
				if tc.status == 200 {
					if fresh.metadata == nil {
						t.Fatal("successful thread read was not enriched")
					}
					metadataDeadline, hasDeadline := fresh.metadata.Deadline()
					if !hasDeadline || metadataDeadline.After(deadline) || fresh.metadata.Value(threadRequestContextKey{}) != "request context" {
						t.Error("enrichment lost the bounded refresh context")
					}
				}
			}
			if strings.Contains(response.Body.String(), "secret-token") {
				t.Fatal("upstream error leaked")
			}
			if tc.status == 200 {
				var got alertThreadResponse
				if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if got.User != "operator@example.test" || got.CSRFToken != d.csrfToken(got.User) {
					t.Fatalf("identity response %#v", got)
				}
				if tc.method == "GET" {
					wantCursor := r.URL.Query().Get("before")
					if got.Thread.OccurrenceID != "C:100.000001" || got.Thread.NextCursor != wantCursor {
						t.Errorf("unexpected read target %q or cursor %q", got.Thread.OccurrenceID, got.Thread.NextCursor)
					}
				}
				if tc.method == "POST" && (service.request.ReconcileOnly != tc.reconcile || service.actor != got.User || service.request.ChannelName != "ops-team") {
					t.Fatalf("request %#v actor %q", service.request, service.actor)
				}
			} else {
				var got apiError
				if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if diff := cmp.Diff(tc.code, got.Code); diff != "" {
					t.Errorf("unexpected code (-want +got):\n%s", diff)
				}
				if tc.errorText != "" && !strings.Contains(got.Error, tc.errorText) {
					t.Errorf("missing actionable error %q in %q", tc.errorText, got.Error)
				}
			}
		})
	}
}
