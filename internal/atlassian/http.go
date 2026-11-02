// Package atlassian holds the parts shared by the Confluence and Jira integrations:
// HTTP plumbing, OAuth 2.0 with Data Center and per-subject credentials.
package atlassian

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Auth decorates an outgoing request with the caller's credentials.
type Auth func(*http.Request)

// Bearer returns an Auth that sets an OAuth access token or a personal access token.
func Bearer(token string) Auth {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

// Sentinel errors returned by API calls. Use errors.Is.
var (
	ErrUnauthorized = errors.New("atlassian: credentials rejected")
	ErrNotFound     = errors.New("atlassian: not found or no permission")
	ErrBadRequest   = errors.New("atlassian: bad request")
	ErrUpstream     = errors.New("atlassian: upstream error")
)

// APIError carries the HTTP status and a short, secret-free message.
type APIError struct {
	Status  int
	Message string
	kind    error
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%v (HTTP %d)", e.kind, e.Status)
	}
	return fmt.Sprintf("%v (HTTP %d): %s", e.kind, e.Status, e.Message)
}

func (e *APIError) Unwrap() error { return e.kind }

// Client performs JSON calls against one Atlassian base URL.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// DoJSON sends a request with optional JSON body and decodes a JSON response into out.
func (c *Client) DoJSON(ctx context.Context, auth Auth, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != nil {
		auth(req)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return newAPIError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out); err != nil {
		return fmt.Errorf("%w: decode response: %v", ErrUpstream, err)
	}
	return nil
}

func newAPIError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	e := &APIError{Status: resp.StatusCode, Message: extractMessage(raw)}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		e.kind = ErrUnauthorized
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound:
		e.kind = ErrNotFound
	case resp.StatusCode == http.StatusBadRequest:
		e.kind = ErrBadRequest
	default:
		e.kind = ErrUpstream
	}
	return e
}

// extractMessage pulls a human message out of Confluence/Jira error bodies.
// HTML pages (proxies, login redirects) are dropped.
func extractMessage(raw []byte) string {
	var body struct {
		Message       string   `json:"message"`
		ErrorMessages []string `json:"errorMessages"`
	}
	if json.Unmarshal(raw, &body) == nil {
		if body.Message != "" {
			return truncate(body.Message, 300)
		}
		if len(body.ErrorMessages) > 0 {
			return truncate(strings.Join(body.ErrorMessages, "; "), 300)
		}
	}
	return ""
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
