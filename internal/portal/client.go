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
	"github.com/curbol/synty-sync/internal/retry"
)

// ErrExpiredSession is returned when the library page lacks the logged-in
// sentinel (a logout shell / login redirect), so the caller can refuse to
// overwrite the lockfile and ask for a fresh session.
var ErrExpiredSession = errors.New("expired or missing session: the library page is not logged in")

// StatusError is a non-success HTTP response. Callers classify it with StatusOf to
// decide whether to retry (5xx / transient) or fail fast (4xx). Op is a short,
// PII-free context string (never the resolved URL, which carries the customer id).
type StatusError struct {
	Status int
	Op     string
}

func (e *StatusError) Error() string { return fmt.Sprintf("%s: status %d", e.Op, e.Status) }

// StatusOf returns the HTTP status carried by err when it is (or wraps) a
// StatusError, and whether one was found.
func StatusOf(err error) (int, bool) {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status, true
	}
	return 0, false
}

// ErrNotAPackage means the download endpoint answered with a document instead of
// package bytes. An expired session and a CDN refusal both take that shape, and both
// arrive with a 200 the status check alone would wave through — after which the body
// is streamed into the cache, hashed, and recorded in the lockfile as the pack's
// verified content, where it stays until someone opens the file by hand.
var ErrNotAPackage = errors.New("the download response is a document, not package bytes")

// documentMediaType reports whether a Content-Type is one no archive can have. It
// judges only what it recognizes: an absent or unparseable header is not a verdict,
// because the caller sniffs the delivered bytes as well.
func documentMediaType(header string) (string, bool) {
	if header == "" {
		return "", false
	}
	mt, _, err := mime.ParseMediaType(header)
	if err != nil {
		return header, false
	}
	mt = strings.ToLower(mt)
	if strings.HasPrefix(mt, "text/") || strings.HasSuffix(mt, "+xml") || strings.HasSuffix(mt, "+json") {
		return mt, true
	}
	switch mt {
	case "application/json", "application/xml":
		return mt, true
	}
	return mt, false
}

const shopParam = "synty-store.myshopify.com"

// Client talks to the Sky Pilot portal with an authenticated cookie. Build one with
// New: it supplies the two things a zero value gets wrong, a non-nil HTTP client and
// a BaseURL without a trailing slash (the request URLs are built by concatenation).
type Client struct {
	HTTP       *http.Client
	BaseURL    string // e.g. https://syntystore.com (no trailing slash)
	CustomerID string
	Cookie     string
	UserAgent  string
	Limits     Limits
}

// New returns a Client for baseURL. A nil httpClient means http.DefaultClient.
func New(httpClient *http.Client, baseURL, customerID, cookie string) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		HTTP:       httpClient,
		BaseURL:    strings.TrimRight(baseURL, "/"),
		CustomerID: customerID,
		Cookie:     cookie,
	}
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

// Limits bounds a page fetch. Every field is optional: a zero one takes the default
// below. They belong to the Client rather than the package so two clients in one
// process — a test's and a real one — cannot reach into each other's policy.
type Limits struct {
	// Attempts is how many times a page fetch is tried before giving up.
	Attempts int
	// Backoff is the first wait between attempts; each later one doubles it.
	Backoff time.Duration
	// PageTimeout bounds one attempt end to end. The client sets no whole-request
	// timeout (file downloads are large) and only a response-header timeout, which a
	// server can satisfy and then stall the body forever.
	PageTimeout time.Duration
	// MaxPageBytes bounds the read: a library page is HTML, never megabytes.
	MaxPageBytes int64
}

const (
	defaultAttempts     = 4
	defaultBackoff      = 500 * time.Millisecond
	defaultPageTimeout  = 60 * time.Second
	defaultMaxPageBytes = int64(8 << 20)
)

// limits fills in whatever the caller left zero.
func (c *Client) limits() Limits {
	l := c.Limits
	if l.Attempts <= 0 {
		l.Attempts = defaultAttempts
	}
	if l.Backoff <= 0 {
		l.Backoff = defaultBackoff
	}
	if l.PageTimeout <= 0 {
		l.PageTimeout = defaultPageTimeout
	}
	if l.MaxPageBytes <= 0 {
		l.MaxPageBytes = defaultMaxPageBytes
	}
	return l
}

// getBody fetches a page, retrying transient failures (network errors, 5xx) with
// bounded exponential backoff. A 4xx fails fast (retrying won't help). The store
// occasionally returns a one-off 500, which would otherwise abort the whole run.
func (c *Client) getBody(ctx context.Context, rawURL string) ([]byte, error) {
	lim := c.limits()
	var body []byte
	err := retry.Do(ctx, lim.Attempts, lim.Backoff, func() error {
		attemptCtx, cancel := context.WithTimeout(ctx, lim.PageTimeout)
		defer cancel()
		resp, err := c.get(attemptCtx, rawURL)
		if err != nil {
			return c.redactErr(rawURL, err) // network error, retryable
		}
		defer drainClose(resp)
		b, err := io.ReadAll(io.LimitReader(resp.Body, lim.MaxPageBytes+1))
		if err != nil {
			return c.redactErr(rawURL, err)
		}
		if int64(len(b)) > lim.MaxPageBytes {
			// Erroring beats truncating: a shortened library page still parses as a
			// valid short page and would quietly end enumeration early.
			return retry.Stop(fmt.Errorf("GET %s: page exceeds %d bytes", c.redact(rawURL), lim.MaxPageBytes))
		}
		if resp.StatusCode != http.StatusOK {
			se := &StatusError{Status: resp.StatusCode, Op: "GET " + c.redact(rawURL)}
			if transientStatus(resp.StatusCode) {
				return se
			}
			return retry.Stop(se)
		}
		body = b
		return nil
	})
	return body, err
}

// transientStatus reports a status worth another attempt: a 5xx, or the two 4xx
// codes that are about timing rather than the request being wrong — 429 (back off
// and it clears) and 408 (the server says the request did not finish in time).
func transientStatus(code int) bool {
	return code >= 500 || code == http.StatusTooManyRequests || code == http.StatusRequestTimeout
}

// drainReadLimit bounds the drain of a body whose content is being discarded, so a
// server that streams without end cannot stall the close.
const drainReadLimit = 1 << 20

// drainClose reads off any unconsumed body before closing, so the transport can
// return the connection to the pool instead of tearing it down. A retried request
// would otherwise pay a fresh handshake on every attempt.
func drainClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainReadLimit))
	resp.Body.Close()
}

// redact removes the customer id from a string bound for an error message; the id
// is account PII and must never reach stderr.
func (c *Client) redact(s string) string {
	if c.CustomerID == "" {
		return s
	}
	return strings.ReplaceAll(s, c.CustomerID, "<redacted>")
}

// redactErr strips the request URL (which carries the customer id) out of a
// net/http transport error, replacing it with the redacted URL.
func (c *Client) redactErr(rawURL string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("GET %s: %w", c.redact(rawURL), ue.Err)
	}
	return err
}

// transportCause drops the URL from a net/http transport error entirely, keeping
// only the underlying cause. Used where the URL cannot be made safe by redaction:
// a download href carries the account email as well as the customer id.
func transportCause(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
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

// maxLibraryPages backstops the pagination walk. At 15 packs per page this is far
// past any real library; it only bounds a store that stops advancing.
const maxLibraryPages = 500

// Enumerate walks the library pages until a zero-row page (the terminator) and
// returns all owned packs. The terminator is a page with no order_item anchors; a
// short page (fewer than the page size) is not the terminator. Every zero-row page
// is checked for the logged-in sentinel, which is present on authenticated library
// pages including the empty one past the last: with it, the walk ended legitimately
// (or the library is empty); without it, the session expired mid-walk and a
// truncated pack list must not be mistaken for the whole library.
func (c *Client) Enumerate(ctx context.Context) ([]model.Pack, error) {
	var all []model.Pack
	seen := map[int]bool{}
	for page := 1; page <= maxLibraryPages; page++ {
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
			ok, err := HasLibrarySentinel(body)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, ErrExpiredSession
			}
			return all, nil // empty library or terminator
		}
		added := 0
		for _, p := range packs {
			if seen[p.OrderItemID] {
				continue
			}
			seen[p.OrderItemID] = true
			all = append(all, p)
			added++
		}
		if added == 0 {
			// A paginator that clamps an out-of-range page to the last one would
			// otherwise re-serve the same packs forever.
			return nil, fmt.Errorf("page %d repeated the previous page's packs; pagination is not advancing", page)
		}
	}
	return nil, fmt.Errorf("library pagination exceeded %d pages", maxLibraryPages)
}

// ItemFiles fetches and parses one pack's item page.
func (c *Client) ItemFiles(ctx context.Context, pack model.Pack) ([]model.FileEntry, error) {
	body, err := c.getBody(ctx, withShop(c.BaseURL+pack.ItemURL))
	if err != nil {
		return nil, err
	}
	return ParseItemPage(body, pack.Slug)
}

// Resolve issues the download request, follows the 302 to the signed CDN URL, checks
// the response is not a document, and returns the open body plus the filename. The signed URL sets Content-Disposition
// to a bare "attachment", so the filename is taken from the final URL path
// basename, with the Content-Disposition filename as a fallback. The caller must
// close the returned body.
func (c *Client) Resolve(ctx context.Context, file model.FileEntry) (body io.ReadCloser, filename string, err error) {
	resp, err := c.get(ctx, withShop(c.BaseURL+file.DownloadHref))
	if err != nil {
		// The href carries the account email and customer id, so the URL never
		// reaches the message; the file key identifies the request instead.
		return nil, "", fmt.Errorf("download %s: %w", file.Key(), transportCause(err))
	}
	if resp.StatusCode != http.StatusOK {
		drainClose(resp)
		return nil, "", &StatusError{Status: resp.StatusCode, Op: "download " + file.Key()}
	}
	if mt, isDoc := documentMediaType(resp.Header.Get("Content-Type")); isDoc {
		drainClose(resp)
		return nil, "", fmt.Errorf("download %s: %w (Content-Type %s)", file.Key(), ErrNotAPackage, mt)
	}
	filename = filenameFromURL(resp.Request.URL)
	if filename == "" {
		filename = filenameFromDisposition(resp.Header.Get("Content-Disposition"))
	}
	if filename == "" {
		drainClose(resp)
		return nil, "", fmt.Errorf("download %s: could not determine filename", file.Key())
	}
	return resp.Body, filename, nil
}

func filenameFromURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	return cleanBase(u.Path)
}

func filenameFromDisposition(cd string) string {
	if cd == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(cd)
	if err != nil {
		return ""
	}
	return cleanBase(params["filename"])
}

// cleanBase reduces a path or a Content-Disposition filename to its bare last
// element, so neither source can carry directory components (".." or a nested
// path) into the cache's write path. A degenerate result is dropped.
func cleanBase(name string) string {
	base := path.Base(name)
	if base == "." || base == ".." || base == "/" || base == "" {
		return ""
	}
	return base
}
