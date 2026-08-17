// Package web serves the local pack-selection page for `synty-sync select`. It
// lists every owned pack with its thumbnail and a checkbox, and on Save returns
// the chosen set so the caller can persist the manifest.
package web

import (
	"context"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"

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

// Serve runs the selection page at addr until the user clicks Save (or ctx is
// cancelled), returning the chosen set of enabled slugs.
func Serve(ctx context.Context, addr string, packs []model.Pack, enabled map[string]bool) (map[string]bool, error) {
	rows := make([]row, 0, len(packs))
	known := make(map[string]bool, len(packs))
	for _, p := range packs {
		rows = append(rows, row{Slug: p.Slug, Name: p.DisplayName, IconURL: p.IconURL, Enabled: enabled[p.Slug]})
		known[p.Slug] = true
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	result := make(chan map[string]bool, 1)
	mux := http.NewServeMux()
	// The root pattern is anchored with {$} so it matches only "/" and does not
	// swallow a non-POST /save, which must fail rather than render the page.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		count := 0
		for _, e := range enabled {
			if e {
				count++
			}
		}
		_ = page.Execute(w, pageData{Rows: rows, Count: count})
	})
	// POST only: this endpoint persists the whole pack selection, and any page the
	// user visits while select is open can reach localhost with a GET.
	mux.HandleFunc("POST /save", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// A slug this page never offered comes from a stale tab. The returned set is
		// the user's whole selection, so passing one through would make an empty
		// submission read as a deliberate choice of packs that no longer exist.
		chosen := map[string]bool{}
		for _, slug := range r.Form["pack"] {
			if known[slug] {
				chosen[slug] = true
			}
		}
		fmt.Fprintf(w, "Saved %d packs. You can close this tab.", len(chosen))
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

func shutdown(srv *http.Server) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
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
