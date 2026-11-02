package confluence

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"

	"atlassian-mcp/internal/atlassian"
)

// Client talks to the Confluence Data Center REST API.
type Client struct {
	api             *atlassian.Client
	baseURL         *url.URL
	maxResults      int
	maxContentChars int
}

// NewClient creates a client for baseURL (e.g. https://confluence.example.com).
func NewClient(baseURL string, httpClient *http.Client, maxResults, maxContentChars int) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, err
	}
	return &Client{
		api:             &atlassian.Client{BaseURL: u.String(), HTTP: httpClient},
		baseURL:         u,
		maxResults:      maxResults,
		maxContentChars: maxContentChars,
	}, nil
}

// SearchResult is one hit.
type SearchResult struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	Title        string `json:"title"`
	Space        string `json:"space,omitempty"`
	SpaceName    string `json:"space_name,omitempty"`
	Author       string `json:"author,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	Excerpt      string `json:"excerpt,omitempty"`
	URL          string `json:"url"`
}

// Page is a page with its body converted to Markdown.
type Page struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Space        string `json:"space,omitempty"`
	SpaceName    string `json:"space_name,omitempty"`
	Author       string `json:"author,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	Version      int    `json:"version,omitempty"`
	URL          string `json:"url"`
	Body         string `json:"body"`
	Truncated    bool   `json:"truncated,omitempty"`
}

type apiUser struct {
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
}

type apiContent struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title"`
	Space *struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	} `json:"space"`
	History *struct {
		CreatedBy apiUser `json:"createdBy"`
	} `json:"history"`
	Version *struct {
		When   string `json:"when"`
		Number int    `json:"number"`
	} `json:"version"`
	Body *struct {
		View struct {
			Value string `json:"value"`
		} `json:"view"`
	} `json:"body"`
	Links struct {
		WebUI string `json:"webui"`
	} `json:"_links"`
}

// Search runs a CQL search and returns at most p.Limit (capped) results.
func (c *Client) Search(ctx context.Context, auth atlassian.Auth, p SearchParams) ([]SearchResult, error) {
	cql, err := BuildCQL(p)
	if err != nil {
		return nil, err
	}
	limit := p.Limit
	if limit <= 0 || limit > c.maxResults {
		limit = c.maxResults
	}
	q := url.Values{
		"cql":    {cql},
		"limit":  {strconv.Itoa(limit)},
		"expand": {"content.space,content.history,content.version"},
	}
	var resp struct {
		Results []struct {
			Content      apiContent `json:"content"`
			Title        string     `json:"title"`
			Excerpt      string     `json:"excerpt"`
			URL          string     `json:"url"`
			LastModified string     `json:"lastModified"`
		} `json:"results"`
	}
	if err := c.api.DoJSON(ctx, auth, http.MethodGet, "/rest/api/search?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	out := make([]SearchResult, 0, len(resp.Results))
	for _, r := range resp.Results {
		ct := r.Content
		if ct.ID == "" {
			continue // attachments, users and other non-content hits
		}
		res := SearchResult{
			ID:           ct.ID,
			Type:         ct.Type,
			Title:        firstNonEmpty(ct.Title, cleanExcerpt(r.Title)),
			Excerpt:      cleanExcerpt(r.Excerpt),
			URL:          c.absolute(firstNonEmpty(ct.Links.WebUI, r.URL)),
			LastModified: r.LastModified,
		}
		if ct.Space != nil {
			res.Space, res.SpaceName = ct.Space.Key, ct.Space.Name
		}
		if ct.History != nil {
			res.Author = ct.History.CreatedBy.DisplayName
		}
		if res.LastModified == "" && ct.Version != nil {
			res.LastModified = ct.Version.When
		}
		out = append(out, res)
	}
	return out, nil
}

// GetPage loads a page by ID or by a Confluence URL.
func (c *Client) GetPage(ctx context.Context, auth atlassian.Auth, ref string) (*Page, error) {
	pr, err := c.ParsePageRef(ref)
	if err != nil {
		return nil, err
	}
	const expand = "body.view,space,version,history"
	var ct apiContent
	if pr.ID != "" {
		err = c.api.DoJSON(ctx, auth, http.MethodGet,
			"/rest/api/content/"+url.PathEscape(pr.ID)+"?expand="+expand, nil, &ct)
	} else {
		var list struct {
			Results []apiContent `json:"results"`
		}
		q := url.Values{"spaceKey": {pr.SpaceKey}, "title": {pr.Title}, "expand": {expand}}
		err = c.api.DoJSON(ctx, auth, http.MethodGet, "/rest/api/content?"+q.Encode(), nil, &list)
		if err == nil {
			if len(list.Results) == 0 {
				return nil, fmt.Errorf("%w: page %q in space %s", atlassian.ErrNotFound, pr.Title, pr.SpaceKey)
			}
			ct = list.Results[0]
		}
	}
	if err != nil {
		return nil, err
	}

	page := &Page{ID: ct.ID, Title: ct.Title, URL: c.absolute(ct.Links.WebUI)}
	if ct.Space != nil {
		page.Space, page.SpaceName = ct.Space.Key, ct.Space.Name
	}
	if ct.History != nil {
		page.Author = ct.History.CreatedBy.DisplayName
	}
	if ct.Version != nil {
		page.LastModified, page.Version = ct.Version.When, ct.Version.Number
	}
	if ct.Body != nil {
		md, err := htmltomarkdown.ConvertString(ct.Body.View.Value)
		if err != nil {
			md = ct.Body.View.Value
		}
		page.Body, page.Truncated = truncateRunes(strings.TrimSpace(md), c.maxContentChars)
	}
	return page, nil
}

// WhoAmI returns the username the credentials belong to.
func (c *Client) WhoAmI(ctx context.Context, auth atlassian.Auth) (string, error) {
	var u apiUser
	if err := c.api.DoJSON(ctx, auth, http.MethodGet, "/rest/api/user/current", nil, &u); err != nil {
		return "", err
	}
	if u.Username == "" {
		return "", fmt.Errorf("%w: anonymous user", atlassian.ErrUnauthorized)
	}
	return u.Username, nil
}

// PageRef identifies a page either by ID or by space and title.
type PageRef struct {
	ID       string
	SpaceKey string
	Title    string
}

var (
	numericRe     = regexp.MustCompile(`^\d{1,20}$`)
	spacesPagesRe = regexp.MustCompile(`/spaces/[^/]+/pages/(\d{1,20})(?:/|$)`)
	displayRe     = regexp.MustCompile(`/display/([^/]+)/([^/?#]+)`)
)

// ParsePageRef accepts a numeric ID or a URL of this Confluence instance.
func (c *Client) ParsePageRef(ref string) (PageRef, error) {
	ref = strings.TrimSpace(ref)
	if numericRe.MatchString(ref) {
		return PageRef{ID: ref}, nil
	}
	u, err := url.Parse(ref)
	if err != nil || u.Host == "" {
		return PageRef{}, fmt.Errorf("%w: expected a page ID or URL", ErrInvalidParams)
	}
	if !strings.EqualFold(u.Host, c.baseURL.Host) {
		return PageRef{}, fmt.Errorf("%w: URL is not on %s", ErrInvalidParams, c.baseURL.Host)
	}
	if id := u.Query().Get("pageId"); numericRe.MatchString(id) {
		return PageRef{ID: id}, nil
	}
	if m := spacesPagesRe.FindStringSubmatch(u.Path); m != nil {
		return PageRef{ID: m[1]}, nil
	}
	if m := displayRe.FindStringSubmatch(u.EscapedPath()); m != nil {
		space, err1 := url.PathUnescape(m[1])
		title, err2 := url.QueryUnescape(m[2]) // "+" means space in display URLs
		if err1 == nil && err2 == nil && spaceKeyRe.MatchString(space) && title != "" {
			return PageRef{SpaceKey: space, Title: title}, nil
		}
	}
	return PageRef{}, fmt.Errorf("%w: unsupported Confluence URL", ErrInvalidParams)
}

func (c *Client) absolute(path string) string {
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	return c.baseURL.String() + path
}

var excerptMarkers = strings.NewReplacer("@@@hl@@@", "", "@@@endhl@@@", "", "\n", " ")

func cleanExcerpt(s string) string {
	return strings.Join(strings.Fields(excerptMarkers.Replace(s)), " ")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncateRunes(s string, n int) (string, bool) {
	r := []rune(s)
	if len(r) <= n {
		return s, false
	}
	return string(r[:n]), true
}
