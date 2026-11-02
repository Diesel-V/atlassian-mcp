package confluence

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"atlassian-mcp/internal/atlassian"
)

const searchJSON = `{"results":[
 {"content":{"id":"101","type":"page","title":"Регламент выкаток",
   "space":{"key":"TECHOPS","name":"TechOps"},
   "history":{"createdBy":{"username":"a.ivanov","displayName":"Алексей Иванов"}},
   "version":{"when":"2026-10-01T10:00:00.000+03:00","number":7},
   "_links":{"webui":"/display/TECHOPS/Reglament"}},
  "title":"Регламент @@@hl@@@выкаток@@@endhl@@@",
  "excerpt":"Выкатки в @@@hl@@@нерабочее@@@endhl@@@\nвремя запрещены",
  "url":"/display/TECHOPS/Reglament","lastModified":"2026-10-01T10:00:00.000+03:00"},
 {"user":{"username":"x"},"title":"Some user","url":"/display/~x"}
],"size":2}`

const pageJSON = `{"id":"101","type":"page","title":"Регламент выкаток",
 "space":{"key":"TECHOPS","name":"TechOps"},
 "history":{"createdBy":{"username":"a.ivanov","displayName":"Алексей Иванов"}},
 "version":{"when":"2026-10-01T10:00:00.000+03:00","number":7},
 "body":{"view":{"value":"<h1>Правила</h1><p>Выкатки <strong>только</strong> в рабочее время.</p>"}},
 "_links":{"webui":"/pages/viewpage.action?pageId=101"}}`

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, srv.Client(), 20, 30)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSearch(t *testing.T) {
	var gotQuery string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/search" || r.Header.Get("Authorization") != "Bearer user-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		gotQuery = r.URL.Query().Get("cql")
		if r.URL.Query().Get("limit") != "5" {
			t.Errorf("limit = %s", r.URL.Query().Get("limit"))
		}
		_, _ = io.WriteString(w, searchJSON)
	})

	res, err := c.Search(context.Background(), atlassian.Bearer("user-token"), SearchParams{Query: "регламент выкаток", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != `text ~ "регламент" AND text ~ "выкаток" AND type in ("page", "blogpost")` {
		t.Errorf("cql = %s", gotQuery)
	}
	if len(res) != 1 {
		t.Fatalf("results = %+v", res)
	}
	r := res[0]
	if r.ID != "101" || r.Space != "TECHOPS" || r.Author != "Алексей Иванов" ||
		r.Excerpt != "Выкатки в нерабочее время запрещены" || !strings.HasSuffix(r.URL, "/display/TECHOPS/Reglament") ||
		!strings.HasPrefix(r.URL, "http") {
		t.Errorf("result = %+v", r)
	}
}

func TestSearchLimitIsCapped(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("limit"); got != "20" {
			t.Errorf("limit = %s, want cap 20", got)
		}
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	if _, err := c.Search(context.Background(), nil, SearchParams{Query: "x", Limit: 500}); err != nil {
		t.Fatal(err)
	}
}

func TestSearchValidationHappensBeforeRequest(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("request must not be sent")
	})
	if _, err := c.Search(context.Background(), nil, SearchParams{}); !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("err = %v", err)
	}
}

func TestGetPage(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/rest/api/content/101":
			_, _ = io.WriteString(w, pageJSON)
		case r.URL.Path == "/rest/api/content" && r.URL.Query().Get("spaceKey") == "TECHOPS" &&
			r.URL.Query().Get("title") == "Регламент выкаток":
			_, _ = io.WriteString(w, `{"results":[`+pageJSON+`]}`)
		case r.URL.Path == "/rest/api/content":
			_, _ = io.WriteString(w, `{"results":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ctx := context.Background()

	p, err := c.GetPage(ctx, nil, "101")
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "Регламент выкаток" || p.Version != 7 || p.Author != "Алексей Иванов" || !strings.Contains(p.Body, "# Правила") ||
		!strings.Contains(p.Body, "**только**") {
		t.Errorf("page = %+v", p)
	}
	if len([]rune(p.Body)) > 30 || !p.Truncated {
		t.Errorf("body not truncated to 30 runes: %d %v", len([]rune(p.Body)), p.Truncated)
	}

	byTitle, err := c.GetPage(ctx, nil, c.baseURL.String()+"/display/TECHOPS/%D0%A0%D0%B5%D0%B3%D0%BB%D0%B0%D0%BC%D0%B5%D0%BD%D1%82+%D0%B2%D1%8B%D0%BA%D0%B0%D1%82%D0%BE%D0%BA")
	if err != nil || byTitle.ID != "101" {
		t.Fatalf("by display URL: %+v %v", byTitle, err)
	}

	if _, err := c.GetPage(ctx, nil, c.baseURL.String()+"/display/TECHOPS/Missing"); !errors.Is(err, atlassian.ErrNotFound) {
		t.Errorf("missing page: err = %v", err)
	}
	if _, err := c.GetPage(ctx, nil, "999"); !errors.Is(err, atlassian.ErrNotFound) {
		t.Errorf("404: err = %v", err)
	}
}

func TestParsePageRef(t *testing.T) {
	c, _ := NewClient("https://confluence.example.com", http.DefaultClient, 10, 100)
	ok := map[string]PageRef{
		"12345": {ID: "12345"},
		"https://confluence.example.com/pages/viewpage.action?pageId=987":            {ID: "987"},
		"https://CONFLUENCE.example.com/spaces/DBA/pages/555/Backup+plan":            {ID: "555"},
		"https://confluence.example.com/display/DBA/Backup+plan":                     {SpaceKey: "DBA", Title: "Backup plan"},
		"https://confluence.example.com/display/~s.sidorov/%D0%9F%D0%BB%D0%B0%D0%BD": {SpaceKey: "~s.sidorov", Title: "План"},
	}
	for in, want := range ok {
		got, err := c.ParsePageRef(in)
		if err != nil || got != want {
			t.Errorf("ParsePageRef(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"", "abc", "https://evil.example.org/pages/viewpage.action?pageId=1",
		"https://confluence.example.com/questions/1", "/display/DBA/x",
	} {
		if _, err := c.ParsePageRef(bad); !errors.Is(err, ErrInvalidParams) {
			t.Errorf("ParsePageRef(%q) err = %v", bad, err)
		}
	}
}

func TestWhoAmI(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/user/current" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") == "" {
			_, _ = io.WriteString(w, `{"type":"anonymous"}`)
			return
		}
		_, _ = io.WriteString(w, `{"username":"i.ivanov","displayName":"Иван"}`)
	})
	u, err := c.WhoAmI(context.Background(), atlassian.Bearer("t"))
	if err != nil || u != "i.ivanov" {
		t.Fatalf("WhoAmI = %q, %v", u, err)
	}
	if _, err := c.WhoAmI(context.Background(), nil); !errors.Is(err, atlassian.ErrUnauthorized) {
		t.Fatalf("anonymous: err = %v", err)
	}
}
