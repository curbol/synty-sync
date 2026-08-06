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

// assetDTO is the client-facing view of an asset: the representative's display
// fields plus every identical copy (same file across variants/packs) so the UI can
// show one card and expose all paths. Count/Copies are 1/[self] when ungrouped.
type assetDTO struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	RelPath  string    `json:"relPath"`
	CopyPath string    `json:"copyPath"`
	Category string    `json:"category"`
	Ext      string    `json:"ext"`
	Vendor   string    `json:"vendor"`
	Pack     string    `json:"pack"`
	Variant  string    `json:"variant"`
	Size     int64     `json:"size"`
	Thumb    string    `json:"thumb"`
	Count    int       `json:"count"`
	Copies   []copyDTO `json:"copies"`
}

// copyDTO is one occurrence of an asset (its variant/pack and the path to copy).
type copyDTO struct {
	ID       string `json:"id"`
	Variant  string `json:"variant"`
	Vendor   string `json:"vendor"`
	Pack     string `json:"pack"`
	CopyPath string `json:"copyPath"`
}

func copyOf(a assetindex.Asset) copyDTO {
	return copyDTO{ID: a.ID, Variant: a.Variant, Vendor: a.Vendor, Pack: a.Pack, CopyPath: a.CopyPath}
}

func toDTO(a assetindex.Asset) assetDTO {
	return assetDTO{
		ID: a.ID, Name: a.Name, RelPath: a.RelPath, CopyPath: a.CopyPath,
		Category: string(a.Category), Ext: a.Ext, Vendor: a.Vendor, Pack: a.Pack,
		Variant: a.Variant, Size: a.Size, Thumb: string(a.Thumb),
		Count: 1, Copies: []copyDTO{copyOf(a)},
	}
}

// thumbRank ranks thumbnail kinds so a group picks the copy with the best preview
// (a Unity copy with a preview.png beats a SourceFiles copy with only geometry).
func thumbRank(t assetindex.ThumbKind) int {
	switch t {
	case assetindex.ThumbImage, assetindex.ThumbPreview:
		return 4
	case assetindex.ThumbGLB:
		return 3
	case assetindex.ThumbFBX:
		return 2
	}
	return 0
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
	query := r.URL.Query()
	q := strings.ToLower(strings.TrimSpace(query.Get("q")))
	types := valueSet(query["type"])
	vendors := valueSet(query["vendor"])
	variants := valueSet(query["variant"])
	offset := atoiDefault(query.Get("offset"), 0)
	limit := atoiDefault(query.Get("limit"), defaultLimit)
	if limit <= 0 || limit > maxLimit {
		limit = defaultLimit
	}

	var matched []assetindex.Asset
	for _, a := range s.ix.Assets {
		if types != nil && !types[string(a.Category)] {
			continue
		}
		if vendors != nil && !vendors[a.Vendor] {
			continue
		}
		if variants != nil && !variants[a.Variant] {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(a.Name), q) {
			continue
		}
		matched = append(matched, a)
	}

	// Group identical copies (same file name + size) into one entry unless disabled,
	// so the same asset shipped across variants/packs shows once with all its paths.
	grouped := groupItems(matched)
	if query.Get("group") == "0" {
		grouped = make([]assetDTO, len(matched))
		for i, a := range matched {
			grouped[i] = toDTO(a)
		}
	}
	sortItems(grouped, query.Get("sort"))

	total := len(grouped)
	lo := min(offset, total)
	hi := min(lo+limit, total)

	writeJSON(w, map[string]any{
		"total":  total,
		"offset": lo,
		"items":  grouped[lo:hi],
		"facets": s.facets,
	})
}

// sortItems orders results: "path" keeps assets grouped by their location
// (vendor/pack/folder); the default is case-insensitive by name.
func sortItems(items []assetDTO, mode string) {
	switch mode {
	case "path":
		sort.Slice(items, func(i, j int) bool { return items[i].RelPath < items[j].RelPath })
	default:
		sort.Slice(items, func(i, j int) bool {
			ni, nj := strings.ToLower(items[i].Name), strings.ToLower(items[j].Name)
			if ni != nj {
				return ni < nj
			}
			return items[i].RelPath < items[j].RelPath
		})
	}
}

// groupNameKey folds file names that differ only by separators/case, so the same
// file collapses even when a variant renamed it. Synty's Unity export inserts an
// underscore before a trailing number (SPR_..._Gem09.png -> ..._Gem_09.png), which
// otherwise leaves the identical sprite showing as two cards. Pairing this with the
// byte size in the group key keeps genuinely different files apart.
func groupNameKey(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// groupItems collapses assets that are the same file (normalized name + size) into
// one DTO, keeping first-seen order, choosing the best-thumbnail copy as the
// representative, and listing every copy.
func groupItems(assets []assetindex.Asset) []assetDTO {
	type group struct {
		rep    assetindex.Asset
		copies []assetindex.Asset
	}
	byKey := map[string]*group{}
	var order []string
	for _, a := range assets {
		key := groupNameKey(a.Name) + "\x00" + strconv.FormatInt(a.Size, 10)
		g := byKey[key]
		if g == nil {
			g = &group{rep: a}
			byKey[key] = g
			order = append(order, key)
		} else if thumbRank(a.Thumb) > thumbRank(g.rep.Thumb) {
			g.rep = a
		}
		g.copies = append(g.copies, a)
	}
	out := make([]assetDTO, 0, len(order))
	for _, key := range order {
		g := byKey[key]
		d := toDTO(g.rep)
		d.Count = len(g.copies)
		d.Copies = make([]copyDTO, len(g.copies))
		for i, c := range g.copies {
			d.Copies[i] = copyOf(c)
		}
		out = append(out, d)
	}
	return out
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

// sortedFacet turns a value->count map into a slice sorted alphabetically by value
// (case-insensitive), so a specific option is easy to find in the filter UI.
func sortedFacet(m map[string]int) []facetValue {
	out := make([]facetValue, 0, len(m))
	for v, c := range m {
		out = append(out, facetValue{Value: v, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		li, lj := strings.ToLower(out[i].Value), strings.ToLower(out[j].Value)
		if li != lj {
			return li < lj
		}
		return out[i].Value < out[j].Value
	})
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// valueSet turns the repeated values of a filter param into a membership set, or nil
// when the param was absent (meaning no filter on that facet). A present-but-empty
// value ("", e.g. variant=) stays in the set and selects the loose/unknown bucket.
func valueSet(vals []string) map[string]bool {
	if vals == nil {
		return nil
	}
	set := make(map[string]bool, len(vals))
	for _, v := range vals {
		set[v] = true
	}
	return set
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
