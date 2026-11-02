package metrics

import (
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestExposition(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounterVec("amcp_tool_calls_total", "Tool calls.", "tool", "status")
	h := r.NewHistogramVec("amcp_tool_duration_seconds", "Tool latency.", []float64{1, 0.1}, "tool")

	c.Inc("confluence_search", "ok")
	c.Inc("confluence_search", "ok")
	c.Inc("jira_search", `we"ird`)
	h.Observe(0.05, "confluence_search")
	h.Observe(0.5, "confluence_search")
	h.Observe(5, "confluence_search")

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	out := rec.Body.String()

	for _, want := range []string{
		"# TYPE amcp_tool_calls_total counter",
		`amcp_tool_calls_total{tool="confluence_search",status="ok"} 2`,
		`amcp_tool_calls_total{tool="jira_search",status="we\"ird"} 1`,
		"# TYPE amcp_tool_duration_seconds histogram",
		`amcp_tool_duration_seconds_bucket{tool="confluence_search",le="0.1"} 1`,
		`amcp_tool_duration_seconds_bucket{tool="confluence_search",le="1"} 2`,
		`amcp_tool_duration_seconds_bucket{tool="confluence_search",le="+Inf"} 3`,
		`amcp_tool_duration_seconds_sum{tool="confluence_search"} 5.55`,
		`amcp_tool_duration_seconds_count{tool="confluence_search"} 3`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("content type = %s", rec.Header().Get("Content-Type"))
	}
	if got := c.Value("confluence_search", "ok"); got != 2 {
		t.Errorf("Value = %v", got)
	}
}

func TestConcurrentUse(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounterVec("c", "c", "l")
	h := r.NewHistogramVec("h", "h", DefaultDurationBuckets, "l")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Inc("x")
			h.Observe(0.2, "x")
			r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
		}()
	}
	wg.Wait()
	if c.Value("x") != 50 {
		t.Fatalf("value = %v", c.Value("x"))
	}
}

func TestGaugeFunc(t *testing.T) {
	r := NewRegistry()
	var fail error
	var reported error
	r.NewGaugeFunc("amcp_connected_users", "Users with a grant.", "provider",
		func() (map[string]float64, error) {
			if fail != nil {
				return nil, fail
			}
			return map[string]float64{"jira": 2, "confluence": 5}, nil
		},
		func(err error) { reported = err })

	scrape := func() string {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		return rec.Body.String()
	}
	want := "# HELP amcp_connected_users Users with a grant.\n# TYPE amcp_connected_users gauge\n" +
		"amcp_connected_users{provider=\"confluence\"} 5\namcp_connected_users{provider=\"jira\"} 2\n"
	if got := scrape(); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}

	fail = errors.New("db down")
	got := scrape()
	if strings.Contains(got, "provider=") || !errors.Is(reported, fail) {
		t.Fatalf("on error: output %q, reported %v", got, reported)
	}
}
