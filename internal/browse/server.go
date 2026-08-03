// Package browse serves a local web UI to search and preview a game-asset library.
// It queries an assetindex.Index and streams asset bytes and thumbnails; the
// frontend (embedded here) renders results, 3D previews (three.js), and copy-path.
package browse

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/curbol/synty-sync/internal/assetindex"
	"github.com/curbol/synty-sync/internal/web"
)

//go:embed assets
var assetsFS embed.FS

const defaultLimit = 200
const maxLimit = 500

type server struct {
	ix     *assetindex.Index
	facets facets
	static http.Handler
}

type facets struct {
	Categories []facetValue `json:"categories"`
	Vendors    []facetValue `json:"vendors"`
	Variants   []facetValue `json:"variants"`
}

type facetValue struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// assetDTO is the client-facing view of an asset: everything the UI needs, without
// the internal Source locator (the id is the only handle the client gets).
type assetDTO struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	RelPath  string `json:"relPath"`
	CopyPath string `json:"copyPath"`
	Category string `json:"category"`
	Ext      string `json:"ext"`
	Vendor   string `json:"vendor"`
	Pack     string `json:"pack"`
	Variant  string `json:"variant"`
	Size     int64  `json:"size"`
	Thumb    string `json:"thumb"`
}

func toDTO(a assetindex.Asset) assetDTO {
	return assetDTO{
		ID: a.ID, Name: a.Name, RelPath: a.RelPath, CopyPath: a.CopyPath,
		Category: string(a.Category), Ext: a.Ext, Vendor: a.Vendor, Pack: a.Pack,
		Variant: a.Variant, Size: a.Size, Thumb: string(a.Thumb),
	}
}

// newServer wires an index and its precomputed facets to the embedded frontend.
func newServer(ix *assetindex.Index) (*server, error) {
	static, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		return nil, err
	}
	return &server{ix: ix, facets: buildFacets(ix), static: http.FileServerFS(static)}, nil
}

// handler builds the route mux; shared by Serve and tests.
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.index)
	mux.Handle("/static/", http.StripPrefix("/static/", s.static))
	mux.HandleFunc("/api/assets", s.handleAssets)
	mux.HandleFunc("/api/content", s.handleContent)
	mux.HandleFunc("/api/thumb", s.handleThumb)
	return mux
}

// Serve runs the browse UI at addr until ctx is cancelled (Ctrl-C).
func Serve(ctx context.Context, addr string, ix *assetindex.Index) error {
	s, err := newServer(ix)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	srv := &http.Server{Handler: s.handler()}
	go srv.Serve(ln)
	url := "http://" + ln.Addr().String()
	fmt.Printf("browse %d assets at %s  (Ctrl-C to stop)\n", len(ix.Assets), url)
	web.OpenBrowser(url)

	<-ctx.Done()
	shutdownCtx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = srv.Shutdown(shutdownCtx)
	return nil
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

func (s *server) handleAssets(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	typ := r.URL.Query().Get("type")
	vendor := r.URL.Query().Get("vendor")
	variant := r.URL.Query().Get("variant")
	hasVariant := r.URL.Query().Has("variant")
	offset := atoiDefault(r.URL.Query().Get("offset"), 0)
	limit := atoiDefault(r.URL.Query().Get("limit"), defaultLimit)
	if limit <= 0 || limit > maxLimit {
		limit = defaultLimit
	}

	var matched []assetindex.Asset
	for _, a := range s.ix.Assets {
		if typ != "" && string(a.Category) != typ {
			continue
		}
		if vendor != "" && a.Vendor != vendor {
			continue
		}
		if hasVariant && a.Variant != variant {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(a.Name), q) {
			continue
		}
		matched = append(matched, a)
	}

	total := len(matched)
	lo := min(offset, total)
	hi := min(lo+limit, total)
	items := make([]assetDTO, 0, hi-lo)
	for _, a := range matched[lo:hi] {
		items = append(items, toDTO(a))
	}

	writeJSON(w, map[string]any{
		"total":  total,
		"offset": lo,
		"items":  items,
		"facets": s.facets,
	})
}

func (s *server) handleContent(w http.ResponseWriter, r *http.Request) {
	a, ok := s.ix.Lookup(r.URL.Query().Get("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	rc, size, err := s.ix.Open(a)
	if err != nil {
		http.Error(w, "cannot open asset", http.StatusNotFound)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", contentType(a.Ext))
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	io.Copy(w, rc)
}

func (s *server) handleThumb(w http.ResponseWriter, r *http.Request) {
	a, ok := s.ix.Lookup(r.URL.Query().Get("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	rc, size, err := s.ix.OpenThumbnail(a)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	io.Copy(w, rc)
}

// contentType maps an extension to a response type. Model formats are served as
// application/octet-stream so nothing mangles the binary the three.js loaders read
// as an ArrayBuffer; browser-native images get their real image type.
func contentType(ext string) string {
	switch ext {
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "svg":
		return "image/svg+xml"
	case "bmp":
		return "image/bmp"
	}
	return "application/octet-stream"
}

func buildFacets(ix *assetindex.Index) facets {
	return facets{
		Categories: sortedFacet(ix.Categories()),
		Vendors:    sortedFacet(ix.Vendors()),
		Variants:   sortedFacet(ix.Variants()),
	}
}

// sortedFacet turns a value->count map into a slice sorted by descending count then
// value, for a stable filter UI.
func sortedFacet(m map[string]int) []facetValue {
	out := make([]facetValue, 0, len(m))
	for v, c := range m {
		out = append(out, facetValue{Value: v, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Value < out[j].Value
	})
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
