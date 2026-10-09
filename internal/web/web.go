// Package web serves the local pack-selection page for `synty-sync select`. It
// lists every owned pack with its thumbnail and a checkbox, and on Save returns
// the chosen set so the caller can persist the manifest.
package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/curbol/synty-sync/internal/model"
)

type row struct {
	Slug    string
	Name    string
	IconURL string
	Enabled bool
}

type pageData struct {
	Rows  []row
	Count int
	Token string
}

var page = template.Must(template.New("select").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>synty-sync · select packs</title>
<style>
 body{font:14px/1.4 system-ui,sans-serif;margin:0;background:#11131a;color:#e6e8ee}
 header{position:sticky;top:0;display:flex;gap:12px;align-items:center;padding:12px 16px;background:#181b24;border-bottom:1px solid #2a2e3a}
 header h1{font-size:15px;margin:0;flex:0 0 auto}
 #filter{flex:1;padding:6px 10px;background:#11131a;border:1px solid #2a2e3a;color:#e6e8ee;border-radius:6px}
 button{padding:6px 12px;background:#3b82f6;color:#fff;border:0;border-radius:6px;cursor:pointer}
 button.ghost{background:#2a2e3a}
 .grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(240px,1fr));gap:10px;padding:16px}
 label.card{display:flex;gap:10px;align-items:center;padding:8px;background:#181b24;border:1px solid #2a2e3a;border-radius:8px;cursor:pointer}
 label.card:has(input:checked){border-color:#3b82f6;background:#1c2436}
 img{width:48px;height:48px;object-fit:contain;background:#0c0e14;border-radius:6px;flex:0 0 auto}
 .name{flex:1;min-width:0}
 .count{opacity:.7}
</style></head>
<body>
<form method="post" action="/save">
 <input type="hidden" name="csrf" value="{{.Token}}">
 <header>
  <h1>synty-sync</h1>
  <input id="filter" placeholder="filter packs…" oninput="flt(this.value)">
  <span class="count"><span id="n">{{.Count}}</span> selected</span>
  <button type="button" class="ghost" onclick="all(true)">all</button>
  <button type="button" class="ghost" onclick="all(false)">none</button>
  <button type="submit">Save</button>
 </header>
 <div class="grid" id="grid">
 {{range .Rows}}
  <label class="card" data-name="{{.Name}}">
   <input type="checkbox" name="pack" value="{{.Slug}}" {{if .Enabled}}checked{{end}} onchange="tally()">
   {{if .IconURL}}<img src="{{.IconURL}}" loading="lazy" alt="">{{end}}
   <span class="name">{{.Name}}</span>
  </label>
 {{end}}
 </div>
</form>
<script>
 function tally(){document.getElementById('n').textContent=document.querySelectorAll('input[name=pack]:checked').length}
 function all(v){document.querySelectorAll('.card').forEach(c=>{if(c.style.display!=='none')c.querySelector('input').checked=v});tally()}
 function flt(q){q=q.toLowerCase();document.querySelectorAll('.card').forEach(c=>{c.style.display=c.dataset.name.toLowerCase().includes(q)?'':'none'})}
</script>
</body></html>`))

// msgAlreadySaved answers a save after the first. Two tabs on the page share the run's
// token, so the second one's form is perfectly current: telling it the form is foreign
// or stale would be false, and Serve has already taken a selection.
const msgAlreadySaved = "another tab already saved, and this run takes one selection; " +
	"run `synty-sync select` again to change it"

// handler is one run's selection page: the rendered rows, the token its form carries
// back, and the one selection it accepts.
type handler struct {
	mux   *http.ServeMux
	bound net.Addr
	token string

	// One save, enforced rather than assumed. Serve reads one selection and stops, so
	// a second accepted save would be answered as taken while it sat in a channel
	// nobody reads again.
	once   sync.Once
	result chan map[string]bool
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// Serve runs the selection page on ln until the user clicks Save (or ctx is
// cancelled), returning the chosen set of enabled slugs. It takes a bound listener
// rather than an address so the caller decides where the page lives and a test can
// hand it an ephemeral port. Serve closes ln before it returns.
//
// Whatever ln is bound to, both handlers answer only a request that came from this
// machine: the page is the account's whole library and the form rewrites a committed
// file, so it is not something to hand to the network even when asked to.
func Serve(ctx context.Context, ln net.Listener, packs []model.Pack, enabled map[string]bool) (map[string]bool, error) {
	h, err := newHandler(ln.Addr(), packs, enabled)
	if err != nil {
		ln.Close()
		return nil, err
	}
	return serveHandler(ctx, ln, h)
}

// newHandler builds the page for one run, answering only requests addressed to bound.
func newHandler(bound net.Addr, packs []model.Pack, enabled map[string]bool) (*handler, error) {
	rows := make([]row, 0, len(packs))
	known := make(map[string]bool, len(packs))
	// Counted off the rows rather than off enabled, so the number in the header is the
	// number of boxes actually ticked below it. An enabled set holding a slug that is
	// not in packs — a caller measuring against what was enabled before the library was
	// re-read, say — otherwise renders a count the page itself contradicts.
	checked := 0
	for _, p := range packs {
		on := enabled[p.Slug]
		rows = append(rows, row{Slug: p.Slug, Name: p.DisplayName, IconURL: p.IconURL, Enabled: on})
		known[p.Slug] = true
		if on {
			checked++
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })

	// A submission is the user's whole pack selection, written to a committed file.
	// A cross-origin form POST is a CORS "simple request" — no preflight, no
	// same-origin block — so the method guard below is not enough on its own: a page
	// in another tab could post a set of guessed slugs (they are derived from public
	// display names) and both discard the real selection and enable packs the user
	// never chose. Only a form this server rendered carries the token.
	token, err := newToken()
	if err != nil {
		return nil, err
	}

	h := &handler{
		mux:    http.NewServeMux(),
		bound:  bound,
		token:  token,
		result: make(chan map[string]bool, 1),
	}
	// The root pattern is anchored with {$} so it matches only "/" and does not
	// swallow a non-POST /save, which must fail rather than render the page.
	h.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if !localRequest(r, bound) {
			http.Error(w, "unexpected Host", http.StatusMisdirectedRequest)
			return
		}
		// Rendered into a buffer first, so a failure can still become an error status:
		// template.Must runs Parse, and html/template's escape analysis and the field
		// lookups wait for Execute, so a template edit that fails there would otherwise
		// serve a 200 with a blank page and say nothing anywhere.
		var buf bytes.Buffer
		if err := page.Execute(&buf, pageData{Rows: rows, Count: checked, Token: token}); err != nil {
			fmt.Fprintln(os.Stderr, "select: rendering the page failed:", err)
			http.Error(w, "rendering the selection page failed; see the terminal", http.StatusInternalServerError)
			return
		}
		_, _ = buf.WriteTo(w)
	})
	// POST only: this endpoint persists the whole pack selection, and any page the
	// user visits while select is open can reach localhost with a GET.
	h.mux.HandleFunc("POST /save", func(w http.ResponseWriter, r *http.Request) {
		if !localRequest(r, bound) {
			http.Error(w, "unexpected Host", http.StatusMisdirectedRequest)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// PostForm, not Form: a token supplied in the query string would let a link
		// stand in for the rendered page.
		if subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(token)) != 1 {
			http.Error(w, "this form did not come from the page synty-sync is serving", http.StatusForbidden)
			return
		}
		// The returned set is the user's whole selection, so a slug this page never
		// rendered must not ride along in it: a client that holds the token still has
		// to stay inside what it was offered, or a submission naming packs that do not
		// exist reads as a deliberate choice of them.
		chosen := map[string]bool{}
		for _, slug := range r.PostForm["pack"] {
			if known[slug] {
				chosen[slug] = true
			}
		}
		accepted := false
		h.once.Do(func() {
			accepted = true
			h.result <- chosen
		})
		if !accepted {
			http.Error(w, msgAlreadySaved, http.StatusConflict)
			return
		}
		// The caller decides whether this selection is written — it refuses an empty
		// submission, and the save itself can fail — so the page reports only what it
		// knows, and the terminal reports the outcome.
		fmt.Fprintf(w, "Got your selection (%d packs). Return to the terminal.", len(chosen))
	})
	return h, nil
}

// serveHandler serves h on ln until it accepts a selection or ctx ends.
func serveHandler(ctx context.Context, ln net.Listener, h *handler) (chosen map[string]bool, err error) {
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	// A cancelled run still drains the result once shutdown has waited for every
	// in-flight handler. A save accepted as ctx ended, whether already queued (both
	// cases below ready, and Go picks at random) or still reading its body, has told
	// the browser it was taken, so it wins over the interrupt.
	defer func() {
		shutdown(srv)
		if err == nil {
			return
		}
		select {
		case sel := <-h.result:
			chosen, err = sel, nil
		default:
		}
	}()

	url := "http://" + ln.Addr().String()
	fmt.Fprintf(os.Stderr, "select packs at %s  (Ctrl-C to cancel)\n", url)
	OpenBrowser(url)

	select {
	case chosen := <-h.result:
		return chosen, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// newToken returns the per-invocation secret the rendered form carries back.
func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating a form token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// localRequest reports whether a request addressed this server the way a browser on
// this machine would. A page that reached us by pointing its own name at a loopback
// address arrives with that name in Host; without this it would be same-origin with
// the selection page and free to read the whole pack list, which is the user's
// purchase history.
func localRequest(r *http.Request, bound net.Addr) bool {
	// The peer before the Host header, because Host is the client's to claim and the
	// peer is not. On a listener bound to a wildcard address the Host check alone
	// inverts: a remote client sending "Host: 127.0.0.1:<port>" reads as a local
	// browser and is let in, while the browser on the machine that bind was meant to
	// reach can only send that machine's real address and is turned away.
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	if ip := net.ParseIP(peer); ip == nil || !ip.IsLoopback() {
		return false
	}
	// A browser omits the port when it is the scheme's default, so a page bound to :80
	// arrives with a bare name rather than a host:port pair.
	host, port := strings.TrimSuffix(strings.TrimPrefix(r.Host, "["), "]"), "80"
	if h, p, err := net.SplitHostPort(r.Host); err == nil {
		host, port = h, p
	}
	boundHost, boundPort, err := net.SplitHostPort(bound.String())
	if err != nil || port != boundPort {
		return false
	}
	// A wildcard bind has no single address to match, so only loopback is accepted.
	if LoopbackHost(host) {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	boundIP := net.ParseIP(boundHost)
	return boundIP != nil && !boundIP.IsUnspecified() && boundIP.Equal(ip)
}

// LoopbackHost reports whether a host names this machine's loopback: the literal
// "localhost" in any casing, or an address that is one.
//
// Exported because main asks the same question of the --addr it is about to bind, and
// the two answers have to match: a name this accepts on the way in but main refuses on
// the way out cannot be reached at all, and one main binds but this refuses serves a
// page that answers every request with a 421.
func LoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// shutdownGrace bounds how long a shutdown waits for the response already being
// written. The handler sends its result before returning, so without a grace period
// the process can exit before the browser has the page.
const shutdownGrace = 2 * time.Second

func shutdown(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// OpenBrowser best-effort opens the default browser at url. Failure is ignored:
// the caller has already printed the URL for the user to open manually. It is a var
// so tests can stub the launch out rather than spawning a real browser.
var OpenBrowser = func(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd = "rundll32"
		args = []string{"url.dll,FileProtocolHandler"}
	default:
		cmd = "xdg-open"
	}
	_ = exec.Command(cmd, append(args, url)...).Start()
}
