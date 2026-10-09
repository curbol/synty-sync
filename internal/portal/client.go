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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// ParseMediaType reports a malformed *parameter* alongside the media type it did
	// read, and that type is a perfectly good verdict: "text/html; charset" is still a
	// document. Only a header it could make nothing of is not one.
	mt, _, err := mime.ParseMediaType(header)
	if err != nil && mt == "" {
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

// Client talks to the Sky Pilot portal with an authenticated cookie. New fills in the
// one field a zero value gets wrong; a struct literal is fine too, since every request
// normalizes it anyway. A Client is used through a pointer and builds its own transport
// once, so it must not be copied after first use.
type Client struct {
	HTTP       *http.Client
	BaseURL    string // e.g. https://syntystore.com (no trailing slash)
	CustomerID string
	Cookie     Credential
	UserAgent  string
	Limits     Limits

	// own is the client built for a caller that supplied none, cached so that every
	// request shares one connection pool rather than handshaking afresh.
	ownOnce sync.Once
	own     *http.Client
}

// New returns a Client for baseURL. A nil httpClient means this Client builds its own
// on first use, carrying Limits.HeaderTimeout; a supplied one is used as given and owns
// that bound itself.
func New(httpClient *http.Client, baseURL, customerID, cookie string) *Client {
	return &Client{
		HTTP:       httpClient,
		BaseURL:    strings.TrimRight(baseURL, "/"),
		CustomerID: customerID,
		Cookie:     Credential(cookie),
	}
}

// Credential is a Cookie header: the user's live session. It goes into a request and
// nowhere else. The Client holding it is a struct that a %v in a log line or a wrapped
// error walks field by field, so every way of rendering one prints a placeholder;
// string(c) is the header itself.
type Credential string

const redacted = "[redacted]"

// Format covers every verb, not only the ones a String method reaches: %#v goes to
// GoString, and a verb that does not fit a string (%d) prints the value inside its
// bad-verb marker.
func (Credential) Format(f fmt.State, _ rune) { io.WriteString(f, redacted) }

// MarshalText closes the encoders, which walk an exported field without consulting
// Format.
func (Credential) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// base is the BaseURL every request builds on. Request URLs are assembled by
// concatenation, so a trailing slash would produce "//apps/downloads/..." — normalized
// here rather than only in New, because the tests and any future caller build the
// struct directly and a doc comment is not a guarantee.
func (c *Client) base() string {
	return strings.TrimRight(c.BaseURL, "/")
}

// httpClient is the client requests go through, so a zero-value Client is usable
// rather than a nil-pointer panic on its first use.
//
// The fallback is not http.DefaultClient: its transport has no response-header
// timeout, and that is the only bound on a download's header phase. Resolve cannot
// use a context deadline there (it would cap a multi-gigabyte transfer) and the
// stall guard only starts once the headers arrive, so a Client against a server that
// answers the handshake and then goes quiet would hang for good.
//
// Built once, not per call: a transport per request means a fresh TLS handshake every
// time and one idle connection stranded per transport, which is what drainClose and
// the connection-reuse guard exist to avoid. Building it here rather than in New is
// what makes Limits.HeaderTimeout mean something: New cannot read a Limits the caller
// sets afterwards, and pre-filling HTTP there left the field with no reader at all.
func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	c.ownOnce.Do(func() {
		c.own = &http.Client{Transport: boundedTransport(c.limits().HeaderTimeout)}
	})
	return c.own
}

// boundedTransport clones the default transport (so proxy settings from the
// environment still apply) and gives it a response-header timeout. There is no
// whole-request timeout: asset downloads are large.
func boundedTransport(headerTimeout time.Duration) *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = headerTimeout
	return tr
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
		req.Header.Set("Cookie", string(c.Cookie))
	}
	return c.httpClient().Do(req)
}

// Limits bounds a page fetch. Every field is optional: a zero one takes the default
// below. They belong to the Client rather than the package so two clients in one
// process — a test's and a real one — cannot reach into each other's policy.
type Limits struct {
	// Attempts is how many times a page fetch is tried before giving up.
	Attempts int
	// Backoff is the first wait between attempts; each later one doubles it.
	Backoff time.Duration
	// PageTimeout bounds one attempt end to end. It covers page fetches only: a
	// download carries no whole-request deadline, since a pack runs to gigabytes.
	PageTimeout time.Duration
	// MaxPageBytes bounds the read: a library page is HTML, never megabytes.
	MaxPageBytes int64
	// StallTimeout bounds the silence inside a download body. A pack runs to
	// gigabytes, so a deadline on the whole transfer would kill a legitimately slow
	// one; this bounds how long the server may deliver nothing at all.
	StallTimeout time.Duration
	// HeaderTimeout bounds the wait for response headers. It is the only thing
	// standing between a download and an unbounded hang: Resolve cannot take a
	// deadline (that would cap the transfer) and the stall guard is not installed
	// until the headers arrive, so a server that completes TLS and then says nothing
	// blocks forever without it. Applied to the transport the Client builds for itself,
	// read once on the first request; a caller that supplies its own http.Client owns
	// this bound itself.
	HeaderTimeout time.Duration
}

const (
	defaultAttempts      = 4
	defaultBackoff       = 500 * time.Millisecond
	defaultPageTimeout   = 60 * time.Second
	defaultMaxPageBytes  = int64(8 << 20)
	defaultStallTimeout  = 2 * time.Minute
	defaultHeaderTimeout = 60 * time.Second
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
	if l.StallTimeout <= 0 {
		l.StallTimeout = defaultStallTimeout
	}
	if l.HeaderTimeout <= 0 {
		l.HeaderTimeout = defaultHeaderTimeout
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
				// A rate limit that names its own wait is worth honoring: the backoff
				// budget here is a few seconds, and a store asking for thirty would
				// otherwise exhaust every attempt well inside the window it set.
				if d, ok := retryAfter(resp.Header.Get("Retry-After")); ok {
					return retry.After(se, d)
				}
				return se
			}
			return retry.Stop(se)
		}
		body = b
		return nil
	})
	return body, err
}

// retryAfter reads a Retry-After header in either of its forms, delta-seconds or an
// HTTP-date. A date already in the past means "now", not a negative wait.
func retryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0), true
	}
	return 0, false
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
//
// The bound here is on bytes, which is not the same as a bound on time: a server that
// sends a few and then goes quiet without closing blocks the read for as long as the
// request context allows. Callers whose context carries a deadline get one from it;
// the download leg deliberately has none, so it uses drainCloseBounded instead.
func drainClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainReadLimit))
	resp.Body.Close()
}

// drainCloseBounded is drainClose for a request that carries no deadline of its own.
// Cancelling the request is what breaks the read, which is the same lever the stall
// guard pulls on the success path; on an error path the deferred cancel fires straight
// after anyway, so calling it early costs nothing.
func drainCloseBounded(resp *http.Response, cancel context.CancelFunc, window time.Duration) {
	stop := time.AfterFunc(window, cancel)
	defer stop.Stop()
	drainClose(resp)
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
		u := withShop(fmt.Sprintf("%s/apps/downloads/orders/%s?line_items_page=%d", c.base(), c.CustomerID, page))
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
			if err := checkSlugCollisions(all); err != nil {
				return nil, err
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

// checkSlugCollisions refuses a library in which two packs key to one slug. The slug
// is the pack's identity in both committed files, so a collision does not merely
// shadow one pack: the lockfile records whichever pack is written last and loses the
// other's files, and the manifest grows two entries that share a key, where enabling
// either enables both. Neither is detectable from the file afterwards.
//
// Refusing rather than disambiguating is the deliberate choice. A generated
// discriminator would have to change the slug of both packs, including the one that
// held the name alone until the second arrived, which silently drops that pack's
// enabled flag and rebuilds its lockfile record under a key nothing recorded.
// Declining to write is recoverable; rewriting identity behind the user is not.
func checkSlugCollisions(packs []model.Pack) error {
	bySlug := map[string][]model.Pack{}
	for _, p := range packs {
		bySlug[p.Slug] = append(bySlug[p.Slug], p)
	}
	slugs := make([]string, 0, len(bySlug))
	for slug, group := range bySlug {
		if len(group) > 1 {
			slugs = append(slugs, slug)
		}
	}
	if len(slugs) == 0 {
		return nil
	}
	sort.Strings(slugs)
	var b strings.Builder
	b.WriteString("two packs share one slug, which the lockfile and manifest key by:")
	for _, slug := range slugs {
		for _, p := range bySlug[slug] {
			fmt.Fprintf(&b, "\n  %s: %q (order item %d)", slug, p.DisplayName, p.OrderItemID)
		}
	}
	return errors.New(b.String())
}

// ItemFiles fetches and parses one pack's item page, returning its files and the
// labels of any rows whose variant this build does not recognize (see ParseItemPage).
//
// It can return no files and no error, when every versioned row on the page carries a
// variant keyword this build does not know. A caller that rebuilds a pack's record
// from the result has to treat that as a failure of its own — an empty file list
// rewrites the pack's lockfile entry as empty and discards everything it recorded.
func (c *Client) ItemFiles(ctx context.Context, pack model.Pack) (files []model.FileEntry, unknown []string, err error) {
	body, err := c.getBody(ctx, withShop(c.base()+pack.ItemURL))
	if err != nil {
		return nil, nil, err
	}
	files, unknown, err = ParseItemPage(body, pack.Slug)
	if err != nil {
		// A session that expires between the enumeration walk and these fetches serves
		// a logout shell, which carries none of the selectors the parser needs and so
		// arrives here as "the markup moved". They are the same bytes to the parser and
		// a different thing to do about it, and only the sentinel tells them apart.
		if ok, sentinelErr := HasLibrarySentinel(body); sentinelErr == nil && !ok {
			return nil, nil, ErrExpiredSession
		}
		return nil, nil, err
	}
	return files, unknown, nil
}

// Resolve issues the download request, follows the 302 to the signed CDN URL, checks
// the response is not a document, and returns the open body plus the filename. The signed URL sets Content-Disposition
// to a bare "attachment", so the filename is taken from the final URL path
// basename, with the Content-Disposition filename as a fallback. The caller must
// close the returned body.
func (c *Client) Resolve(ctx context.Context, file model.FileEntry) (body io.ReadCloser, filename string, err error) {
	// Only a body handed back to the caller keeps the request alive, so every path
	// that returns without one releases it here instead.
	reqCtx, cancel := context.WithCancel(ctx)
	defer func() {
		if err != nil {
			cancel()
		}
	}()
	resp, err := c.get(reqCtx, withShop(c.base()+file.DownloadHref))
	if err != nil {
		// The href carries the account email and customer id, so the URL never
		// reaches the message; the file key identifies the request instead.
		return nil, "", fmt.Errorf("download %s: %w", file.Key(), transportCause(err))
	}
	// reqCtx carries no deadline, because a pack runs to gigabytes, and the stall guard
	// is only installed on the way out. So every path that discards a body here bounds
	// the discard itself: the refusals below are the ordinary ones — a 403 is an expired
	// CloudFront signature the syncer re-signs on the next attempt, a document is an
	// expired session — and a server that sends a few bytes of one and then goes quiet
	// would otherwise block the read for good, with no retry and no output.
	stall := c.limits().StallTimeout
	if resp.StatusCode != http.StatusOK {
		drainCloseBounded(resp, cancel, stall)
		return nil, "", &StatusError{Status: resp.StatusCode, Op: "download " + file.Key()}
	}
	if mt, isDoc := documentMediaType(resp.Header.Get("Content-Type")); isDoc {
		drainCloseBounded(resp, cancel, stall)
		return nil, "", fmt.Errorf("download %s: %w (Content-Type %s)", file.Key(), ErrNotAPackage, mt)
	}
	filename = filenameFromURL(resp.Request.URL)
	if filename == "" {
		filename = filenameFromDisposition(resp.Header.Get("Content-Disposition"))
	}
	if filename == "" {
		drainCloseBounded(resp, cancel, stall)
		return nil, "", fmt.Errorf("download %s: could not determine filename", file.Key())
	}
	return newStallGuard(resp.Body, stall, cancel), filename, nil
}

// ErrStalled marks a transfer that stopped delivering bytes.
var ErrStalled = errors.New("the transfer stalled")

// stallGuard fails a body that goes quiet. By the time it is installed the response
// headers have already satisfied ResponseHeaderTimeout, and a download deliberately
// carries no whole-request deadline, so without it a server that stops mid-body
// blocks the read forever: the attempt never returns, and the retry that would
// resolve a fresh signed URL never runs.
type stallGuard struct {
	body    io.ReadCloser
	window  time.Duration
	timer   *time.Timer
	cancel  context.CancelFunc
	stalled atomic.Bool
}

func newStallGuard(body io.ReadCloser, window time.Duration, cancel context.CancelFunc) *stallGuard {
	g := &stallGuard{body: body, window: window, cancel: cancel}
	g.timer = time.AfterFunc(window, func() {
		g.stalled.Store(true)
		cancel()
	})
	return g
}

func (g *stallGuard) Read(p []byte) (int, error) {
	n, err := g.body.Read(p)
	if n > 0 {
		g.timer.Reset(g.window)
	}
	// Cancelling the request is how the read is broken out of, but "context
	// canceled" reads as an interrupt the user caused. Name the real cause instead.
	if err != nil && err != io.EOF && g.stalled.Load() {
		return n, fmt.Errorf("%w: no bytes for %s", ErrStalled, g.window)
	}
	return n, err
}

func (g *stallGuard) Close() error {
	g.timer.Stop()
	err := g.body.Close()
	g.cancel()
	return err
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
