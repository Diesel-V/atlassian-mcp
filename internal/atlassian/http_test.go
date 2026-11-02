package atlassian

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDoJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/ok":
			if r.Method == http.MethodPost {
				b, _ := io.ReadAll(r.Body)
				if r.Header.Get("Content-Type") != "application/json" || string(b) != `{"q":"x"}` {
					w.WriteHeader(http.StatusTeapot)
					return
				}
			}
			_, _ = io.WriteString(w, `{"name":"alice"}`)
		case "/bad":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"errorMessages":["Field 'x' does not exist"]}`)
		case "/forbidden":
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<html>login</html>`)
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"No content found with id 1"}`)
		case "/boom":
			w.WriteHeader(http.StatusBadGateway)
		case "/garbage":
			_, _ = io.WriteString(w, `not json`)
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	ctx := context.Background()
	auth := Bearer("tok")

	var out struct{ Name string }
	if err := c.DoJSON(ctx, auth, http.MethodGet, "/ok", nil, &out); err != nil || out.Name != "alice" {
		t.Fatalf("GET: %v %+v", err, out)
	}
	if err := c.DoJSON(ctx, auth, http.MethodPost, "/ok", map[string]string{"q": "x"}, &out); err != nil {
		t.Fatalf("POST: %v", err)
	}

	tests := []struct {
		path    string
		auth    Auth
		want    error
		message string
	}{
		{"/ok", Bearer("wrong"), ErrUnauthorized, ""},
		{"/bad", auth, ErrBadRequest, "Field 'x' does not exist"},
		{"/forbidden", auth, ErrNotFound, ""},
		{"/missing", auth, ErrNotFound, "No content found with id 1"},
		{"/boom", auth, ErrUpstream, ""},
		{"/garbage", auth, ErrUpstream, ""},
	}
	for _, tt := range tests {
		err := c.DoJSON(ctx, tt.auth, http.MethodGet, tt.path, nil, &out)
		if !errors.Is(err, tt.want) {
			t.Errorf("%s: err = %v, want %v", tt.path, err, tt.want)
			continue
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Message != tt.message {
			t.Errorf("%s: message = %q, want %q", tt.path, apiErr.Message, tt.message)
		}
		if strings.Contains(err.Error(), "<html>") {
			t.Errorf("%s: HTML leaked into error: %v", tt.path, err)
		}
	}
}

func TestDoJSONNetworkError(t *testing.T) {
	c := &Client{BaseURL: "http://127.0.0.1:1", HTTP: http.DefaultClient}
	if err := c.DoJSON(context.Background(), nil, http.MethodGet, "/", nil, nil); !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v", err)
	}
}
