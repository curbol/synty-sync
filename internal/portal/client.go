package portal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/curbol/synty-sync/internal/model"
)

// ErrExpiredSession is returned when the library page lacks the logged-in
// sentinel (a logout shell / login redirect), so the caller can refuse to
// overwrite the lockfile and ask for a fresh session.
var ErrExpiredSession = errors.New("expired or missing session: the library page is not logged in")

const shopParam = "synty-store.myshopify.com"

// Client talks to the Sky Pilot portal with an authenticated cookie.
type Client struct {
	HTTP       *http.Client
	BaseURL    string // e.g. https://syntystore.com (no trailing slash)
	CustomerID string
	Cookie     string
	UserAgent  string
}

func (c *Client) ua() string {
	if c.UserAgent != "" {
		return c.UserAgent
	}
	return "synty-sync/1.0"
}

func (c *Client) get(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.ua())
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	if c.Cookie != "" {
		req.Header.Set("Cookie", c.Cookie)
	}
	return c.HTTP.Do(req)
}

const httpAttempts = 4

var httpBackoff = 500 * time.Millisecond // var so tests can shorten it

// getBody fetches a page, retrying transient failures (network errors, 5xx) with
// bounded exponential backoff. A 4xx fails fast (retrying won't help). The store
// occasionally returns a one-off 500, which would otherwise abort the whole run.
func (c *Client) getBody(ctx context.Context, rawURL string) ([]byte, error) {
	var lastErr error
	for i := 0; i < httpAttempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(httpBackoff << (i - 1)):
			}
		}
		resp, err := c.get(ctx, rawURL)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode >= 500 {
			resp.Body.Close()
			lastErr = fmt.Errorf("GET %s: status %d", rawURL, resp.StatusCode)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: status %d", rawURL, resp.StatusCode)
		}
		return body, nil
	}
	return nil, fmt.Errorf("GET %s failed after %d attempts: %w", rawURL, httpAttempts, lastErr)
}

// withShop appends the shop app-proxy param (harmless to a test server).
func withShop(rawURL string) string {
	if strings.Contains(rawURL, "shop=") {
		return rawURL
	}
	sep := "?"
	if strings.Contains(rawURL, "?") {
		sep = "&"
	}
	return rawURL + sep + "shop=" + shopParam
}

// Enumerate walks the library pages until a zero-row page (the terminator) and
// returns all owned packs. The terminator is a page with no order_item anchors; a
// short page (fewer than the page size) is not the terminator. The logged-in
// sentinel only disambiguates page 1's zero-row case: with the Sky Pilot library
// UI it is an empty library, without it the session is expired. (Pages past the
// last one are a bare overflow shell with neither the UI nor anchors, so the
// sentinel cannot serve as a per-page terminator marker.)
func (c *Client) Enumerate(ctx context.Context) ([]model.Pack, error) {
	var all []model.Pack
	for page := 1; ; page++ {
		u := withShop(fmt.Sprintf("%s/apps/downloads/orders/%s?line_items_page=%d", c.BaseURL, c.CustomerID, page))
		body, err := c.getBody(ctx, u)
		if err != nil {
			return nil, err
		}
		packs, err := ParseLibraryPage(body)
		if err != nil {
			return nil, fmt.Errorf("page %d: %w", page, err)
		}
		if len(packs) == 0 {
			if page == 1 {
				ok, err := HasLibrarySentinel(body)
				if err != nil {
					return nil, err
				}
				if !ok {
					return nil, ErrExpiredSession
				}
			}
			return all, nil // empty library (page 1 + sentinel) or terminator
		}
		all = append(all, packs...)
	}
}

// ItemFiles fetches and parses one pack's item page.
func (c *Client) ItemFiles(ctx context.Context, pack model.Pack) ([]model.FileEntry, error) {
	body, err := c.getBody(ctx, withShop(c.BaseURL+pack.ItemURL))
	if err != nil {
		return nil, err
	}
	return ParseItemPage(body, pack.Slug)
}

// Resolve issues the download request, follows the 302 to the signed CDN URL, and
// returns the open body plus the filename. The signed URL sets Content-Disposition
// to a bare "attachment", so the filename is taken from the final URL path
// basename, with the Content-Disposition filename as a fallback. The caller must
// close the returned body.
func (c *Client) Resolve(ctx context.Context, file model.FileEntry) (body io.ReadCloser, filename string, err error) {
	resp, err := c.get(ctx, withShop(c.BaseURL+file.DownloadHref))
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, "", fmt.Errorf("download %s: status %d", file.Key(), resp.StatusCode)
	}
	filename = filenameFromURL(resp.Request.URL)
	if filename == "" {
		filename = filenameFromDisposition(resp.Header.Get("Content-Disposition"))
	}
	if filename == "" {
		resp.Body.Close()
		return nil, "", fmt.Errorf("download %s: could not determine filename", file.Key())
	}
	return resp.Body, filename, nil
}

func filenameFromURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	base := path.Base(u.Path)
	if base == "." || base == "/" || base == "" {
		return ""
	}
	return base
}

func filenameFromDisposition(cd string) string {
	if cd == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(cd)
	if err != nil {
		return ""
	}
	return params["filename"]
}
