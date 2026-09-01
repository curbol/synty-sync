// Package web serves the local pack-selection page for `synty-sync select`. It
// lists every owned pack with its thumbnail and a checkbox, and on Save returns
// the chosen set so the caller can persist the manifest.
package web

import (
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

// Serve runs the selection page on ln until the user clicks Save (or ctx is
// cancelled), returning the chosen set of enabled slugs. It takes a bound listener
// rather than an address so the caller decides where the page lives and a test can
// hand it an ephemeral port. Serve closes ln before it returns.
func Serve(ctx context.Context, ln net.Listener, packs []model.Pack, enabled map[string]bool) (map[string]bool, error) {
	rows := make([]row, 0, len(packs))
	known := make(map[string]bool, len(packs))
	for _, p := range packs {
		rows = append(rows, row{Slug: p.Slug, Name: p.DisplayName, IconURL: p.IconURL, Enabled: enabled[p.Slug]})
		known[p.Slug] = true
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
		ln.Close()
		return nil, err
	}

	result := make(chan map[string]bool, 1)
	mux := http.NewServeMux()
	// The root pattern is anchored with {$} so it matches only "/" and does not
	// swallow a non-POST /save, which must fail rather than render the page.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if !localRequest(r, ln.Addr()) {
			http.Error(w, "unexpected Host", http.StatusMisdirectedRequest)
			return
		}
		count := 0
		for _, e := range enabled {
			if e {
				count++
			}
		}
		_ = page.Execute(w, pageData{Rows: rows, Count: count, Token: token})
	})
	// POST only: this endpoint persists the whole pack selection, and any page the
	// user visits while select is open can reach localhost with a GET.
	mux.HandleFunc("POST /save", func(w http.ResponseWriter, r *http.Request) {
		if !localRequest(r, ln.Addr()) {
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
		// The caller decides whether this selection is written — it refuses an empty
		// submission, and the save itself can fail — so the page reports only what it
		// knows, and the terminal reports the outcome.
		fmt.Fprintf(w, "Got your selection (%d packs). Return to the terminal.", len(chosen))
		result <- chosen
	})

	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	url := "http://" + ln.Addr().String()
	fmt.Fprintf(os.Stderr, "select packs at %s  (Ctrl-C to cancel)\n", url)
	OpenBrowser(url)

	select {
	case chosen := <-result:
		shutdown(srv)
		return chosen, nil
	case <-ctx.Done():
		shutdown(srv)
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
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		return false
	}
	boundHost, boundPort, err := net.SplitHostPort(bound.String())
	if err != nil || port != boundPort {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	// A wildcard bind has no single address to match, so only loopback is accepted.
	if ip.IsLoopback() {
		return true
	}
	boundIP := net.ParseIP(boundHost)
	return boundIP != nil && !boundIP.IsUnspecified() && boundIP.Equal(ip)
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
