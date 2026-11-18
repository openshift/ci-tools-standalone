package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
	"k8s.io/apimachinery/pkg/util/sets"
)

// alertThreadService keeps original Slack discussions independent of Chai history.
type alertThreadService interface {
	AlertThread(string, string) (opsalerts.AlertThread, error)
	ReplyToAlert(context.Context, string, opsalerts.ThreadReplyRequest) (opsalerts.AlertThread, error)
}

// alertThreadRefresher shares targeted Slack reads across authenticated discussion viewers.
type alertThreadRefresher interface {
	RefreshAlertThread(context.Context, string) (opsalerts.AlertThread, error)
}

// alertThreadResponse rebinds every result to the authenticated actor.
type alertThreadResponse struct {
	User      string                `json:"user"`
	CSRFToken string                `json:"csrf_token"`
	Thread    opsalerts.AlertThread `json:"thread"`
}

// alertThread serves cached pages, requested Slack refreshes and guarded original-thread writes.
func (d *dashboard) alertThread(w http.ResponseWriter, r *http.Request, actor string) {
	service, ok := d.service.(alertThreadService)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, apiError{Error: "Slack discussion is unavailable", Code: "not_ready"})
		return
	}
	timeout := 45 * time.Second
	if r.Method == http.MethodGet {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	var thread opsalerts.AlertThread
	var err error
	if r.Method == http.MethodGet {
		query, parseErr := url.ParseQuery(r.URL.RawQuery)
		if parseErr != nil {
			writeJSON(w, http.StatusBadRequest, apiError{Error: "invalid discussion query", Code: "validation"})
			return
		}
		refresh := query["refresh"]
		if len(refresh) > 1 || len(refresh) == 1 && refresh[0] != "true" && refresh[0] != "false" {
			writeJSON(w, http.StatusBadRequest, apiError{Error: "refresh must be true or false", Code: "validation"})
			return
		}
		fresh := len(refresh) == 1 && refresh[0] == "true"
		if fresh && query.Get("before") != "" {
			writeJSON(w, http.StatusBadRequest, apiError{Error: "older discussion pages are cache-only", Code: "validation"})
			return
		}
		if fresh {
			refresher, available := service.(alertThreadRefresher)
			if !available {
				writeJSON(w, http.StatusServiceUnavailable, apiError{Error: "Slack discussion refresh is unavailable; displayed messages may be stale", Code: "not_ready"})
				return
			}
			err = ctx.Err()
			if err != nil {
				err = errors.Join(opsalerts.ErrNotReady, err)
			}
			if err == nil {
				thread, err = refresher.RefreshAlertThread(ctx, query.Get("occurrence_id"))
			}
		} else {
			thread, err = service.AlertThread(query.Get("occurrence_id"), query.Get("before"))
		}
	} else {
		if r.Header.Get("Origin") != d.access.origin || !hmac.Equal([]byte(r.Header.Get("X-CSRF-Token")), []byte(d.csrfToken(actor))) {
			writeJSON(w, http.StatusForbidden, apiError{Error: "same-origin CSRF validation failed", Code: "forbidden"})
			return
		}
		mediaType, _, parseErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if parseErr != nil || mediaType != "application/json" {
			writeJSON(w, http.StatusUnsupportedMediaType, apiError{Error: "application/json is required", Code: "validation"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil || !utf8.Valid(body) {
			writeJSON(w, http.StatusBadRequest, apiError{Error: "invalid or oversized reply JSON", Code: "validation"})
			return
		}
		req, decodeErr := decodeThreadReply(body)
		if decodeErr != nil {
			writeJSON(w, http.StatusBadRequest, apiError{Error: "invalid reply JSON", Code: "validation"})
			return
		}
		req.ChannelName = d.channelName
		thread, err = service.ReplyToAlert(ctx, actor, req)
	}
	if err != nil {
		status, code := actionError(err)
		if r.Method == http.MethodGet {
			var refreshError *opsalerts.ThreadRefreshError
			if errors.As(err, &refreshError) {
				retry := max(time.Second, refreshError.RetryAfter)
				seconds := int64(retry / time.Second)
				if retry%time.Second != 0 {
					seconds++
				}
				w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
			}
			writeJSON(w, status, apiError{Error: "Slack discussion could not be loaded (" + code + "); displayed messages may be stale", Code: code})
			return
		}
		var referenceError *opsalerts.ThreadReferenceError
		if errors.As(err, &referenceError) {
			writeJSON(w, status, apiError{Error: referenceError.Error(), Code: code})
			return
		}
		var lookupError *opsalerts.ThreadReferenceLookupError
		if errors.As(err, &lookupError) {
			writeJSON(w, status, apiError{Error: lookupError.Error(), Code: code})
			return
		}
		writeJSON(w, status, apiError{Error: "Slack discussion request could not be completed (" + code + ")", Code: code})
		return
	}
	if directory, ok := d.service.(opsalerts.ThreadDirectory); ok {
		metadataCtx, stop := context.WithTimeout(ctx, 8*time.Second)
		directory.EnrichThread(metadataCtx, &thread)
		stop()
	}
	writeJSON(w, http.StatusOK, alertThreadResponse{User: actor, CSRFToken: d.csrfToken(actor), Thread: thread})
}

// decodeThreadReply rejects duplicate, case-folded and null transport flags before a write can be selected.
func decodeThreadReply(body []byte) (opsalerts.ThreadReplyRequest, error) {
	var req opsalerts.ThreadReplyRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return req, fmt.Errorf("reply must be an object")
	}
	seen := sets.New[string]()
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return req, err
		}
		name, ok := key.(string)
		if !ok || seen.Has(name) {
			return req, fmt.Errorf("duplicate or invalid reply field")
		}
		seen.Insert(name)
		value, err := decoder.Token()
		if err != nil {
			return req, err
		}
		if name == "reconcile_only" {
			flag, ok := value.(bool)
			if !ok {
				return req, fmt.Errorf("reconcile_only must be a boolean")
			}
			req.ReconcileOnly = flag
			continue
		}
		text, ok := value.(string)
		if !ok {
			return req, fmt.Errorf("reply fields must be strings")
		}
		switch name {
		case "occurrence_id":
			req.OccurrenceID = text
		case "request_id":
			req.RequestID = text
		case "text":
			req.Text = text
		default:
			return req, fmt.Errorf("unknown reply field")
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return req, fmt.Errorf("invalid reply object")
	}
	if !seen.HasAll("occurrence_id", "request_id", "text") {
		return req, fmt.Errorf("missing reply field")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return req, fmt.Errorf("trailing reply JSON")
	}
	return req, nil
}
