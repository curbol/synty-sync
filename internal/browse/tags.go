package browse

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/curbol/synty-sync/internal/assetindex"
	"github.com/curbol/synty-sync/internal/tagstore"
)

// tagView is one palette entry for the client: a tag's id, color, and how many
// assets (fingerprints) carry it.
type tagView struct {
	ID    string `json:"id"`
	Color string `json:"color"`
	Count int    `json:"count"`
}

// paletteView is the tag palette the client renders slivers and filters from, plus
// whether tagging is enabled at all.
type paletteView struct {
	Enabled bool      `json:"enabled"`
	Tags    []tagView `json:"tags"`
}

func (s *server) paletteLocked() paletteView {
	counts := s.store.Counts()
	defs := s.store.Tags()
	tv := make([]tagView, len(defs))
	for i, d := range defs {
		tv[i] = tagView{ID: d.ID, Color: d.Color, Count: counts[d.ID]}
	}
	return paletteView{Enabled: s.tagsEnabled, Tags: tv}
}

// resolveTags fills each card's Tags with the union of tag ids over its
// fingerprints, so a grouped card shows every tag any of its copies carries.
func (s *server) resolveTags(dtos []assetDTO) {
	s.tagsMu.RLock()
	defer s.tagsMu.RUnlock()
	for i := range dtos {
		dtos[i].Tags = s.unionTagsLocked(dtos[i].Fingerprints)
	}
}

func (s *server) unionTagsLocked(fps []string) []string {
	set := map[string]bool{}
	for _, fp := range fps {
		for _, id := range s.store.TagsFor(fp) {
			set[id] = true
		}
	}
	return sortedSet(set)
}

// filterByTags keeps cards matching the requested tags against the card's union tag
// set: AND requires all, OR (the default) requires any.
func filterByTags(dtos []assetDTO, tags []string, mode string) []assetDTO {
	if len(tags) == 0 {
		return dtos
	}
	and := mode == "and"
	out := dtos[:0]
	for _, d := range dtos {
		have := make(map[string]bool, len(d.Tags))
		for _, t := range d.Tags {
			have[t] = true
		}
		if matchTags(have, tags, and) {
			out = append(out, d)
		}
	}
	return out
}

func matchTags(have map[string]bool, want []string, and bool) bool {
	if and {
		for _, t := range want {
			if !have[t] {
				return false
			}
		}
		return true
	}
	for _, t := range want {
		if have[t] {
			return true
		}
	}
	return false
}

func (s *server) handleTags(w http.ResponseWriter, r *http.Request) {
	s.tagsMu.RLock()
	defer s.tagsMu.RUnlock()
	writeJSON(w, s.paletteLocked())
}

func (s *server) handleTagCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w) {
		return
	}
	var req struct {
		ID    string `json:"id"`
		Color string `json:"color"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ID == "" {
		writeErr(w, http.StatusBadRequest, "missing tag id")
		return
	}
	color := req.Color
	if color == "" {
		color = tagstore.DefaultColor(req.ID)
	}
	s.tagsMu.Lock()
	defer s.tagsMu.Unlock()
	if err := s.store.Define(req.ID, color); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.persistLocked(w) {
		return
	}
	writeJSON(w, s.paletteLocked())
}

func (s *server) handleTagPatch(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w) {
		return
	}
	var req struct {
		ID    string `json:"id"`
		NewID string `json:"newId"`
		Color string `json:"color"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ID == "" {
		writeErr(w, http.StatusBadRequest, "missing tag id")
		return
	}
	s.tagsMu.Lock()
	defer s.tagsMu.Unlock()
	target := req.ID
	if req.NewID != "" && req.NewID != req.ID {
		if err := s.store.Rename(req.ID, req.NewID); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		target = req.NewID
	}
	if req.Color != "" {
		if err := s.store.Define(target, req.Color); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if !s.persistLocked(w) {
		return
	}
	writeJSON(w, s.paletteLocked())
}

func (s *server) handleTagDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w) {
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ID == "" {
		writeErr(w, http.StatusBadRequest, "missing tag id")
		return
	}
	s.tagsMu.Lock()
	defer s.tagsMu.Unlock()
	s.store.Delete(req.ID)
	if !s.persistLocked(w) {
		return
	}
	writeJSON(w, s.paletteLocked())
}

// handleAssign toggles a tag across a set of fingerprints (a card's whole group),
// so the card's union display matches what was written. It returns the set's
// resulting union tags plus the full palette (so a just-created tag's color is
// known to the client without a second request).
func (s *server) handleAssign(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w) {
		return
	}
	var req struct {
		Fingerprints []string `json:"fingerprints"`
		Tag          string   `json:"tag"`
		On           bool     `json:"on"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Tag == "" {
		writeErr(w, http.StatusBadRequest, "missing tag")
		return
	}
	if len(req.Fingerprints) == 0 {
		writeErr(w, http.StatusBadRequest, "missing fingerprints")
		return
	}
	s.tagsMu.Lock()
	defer s.tagsMu.Unlock()
	for _, fp := range req.Fingerprints {
		if req.On {
			s.store.Assign(fp, req.Tag)
		} else {
			s.store.Unassign(fp, req.Tag)
		}
	}
	if !s.persistLocked(w) {
		return
	}
	writeJSON(w, map[string]any{
		"tags":    s.unionTagsLocked(req.Fingerprints),
		"palette": s.paletteLocked(),
	})
}

// resolveRelated fills each card's Related with the union of link-related
// fingerprints over its own fingerprints, minus its own set.
func (s *server) resolveRelated(dtos []assetDTO) {
	s.tagsMu.RLock()
	defer s.tagsMu.RUnlock()
	for i := range dtos {
		own := make(map[string]bool, len(dtos[i].Fingerprints))
		for _, fp := range dtos[i].Fingerprints {
			own[fp] = true
		}
		rel := map[string]bool{}
		for _, fp := range dtos[i].Fingerprints {
			for _, r := range s.store.Related(fp) {
				if !own[r] {
					rel[r] = true
				}
			}
		}
		if len(rel) > 0 {
			dtos[i].Related = sortedSet(rel)
		}
	}
}

// expandRelated pulls companion cards into a tag-filtered result: any card from the
// pre-tag-filter set (preTag) that shares a link group with a match. It relaxes only
// the tag filter, so other facets and the text search still apply (a companion must
// have survived them to be in preTag). Matches keep their order; companions follow.
func expandRelated(filtered, preTag []assetDTO) []assetDTO {
	if len(filtered) == 0 {
		return filtered
	}
	want := map[string]bool{}
	for _, d := range filtered {
		for _, fp := range d.Related {
			want[fp] = true
		}
	}
	if len(want) == 0 {
		return filtered
	}
	present := make(map[string]bool, len(filtered))
	for _, d := range filtered {
		present[d.ID] = true
	}
	out := filtered
	for _, d := range preTag {
		if present[d.ID] {
			continue
		}
		for _, fp := range d.Fingerprints {
			if want[fp] {
				out = append(out, d)
				break
			}
		}
	}
	return out
}

// handleLink links or unlinks a set of fingerprints as one travel-together group,
// mirroring handleAssign's shape. Linking needs at least two fingerprints.
func (s *server) handleLink(w http.ResponseWriter, r *http.Request) {
	if !s.requireEnabled(w) {
		return
	}
	var req struct {
		Fingerprints []string `json:"fingerprints"`
		On           bool     `json:"on"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Fingerprints) == 0 {
		writeErr(w, http.StatusBadRequest, "missing fingerprints")
		return
	}
	if req.On && len(req.Fingerprints) < 2 {
		writeErr(w, http.StatusBadRequest, "need at least two fingerprints to link")
		return
	}
	s.tagsMu.Lock()
	defer s.tagsMu.Unlock()
	if req.On {
		s.store.Link(req.Fingerprints)
	} else {
		s.store.Unlink(req.Fingerprints)
	}
	if !s.persistLocked(w) {
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleRelated returns the cards linked to the given fingerprints (a card's whole
// fingerprint set, passed as repeated ?fingerprint= params). It searches the whole
// library, not the current page or facet filter, so companions surface regardless of
// how the grid is filtered.
func (s *server) handleRelated(w http.ResponseWriter, r *http.Request) {
	fps := r.URL.Query()["fingerprint"]
	own := make(map[string]bool, len(fps))
	for _, fp := range fps {
		own[fp] = true
	}
	s.tagsMu.RLock()
	related := map[string]bool{}
	for _, fp := range fps {
		for _, rfp := range s.store.Related(fp) {
			if !own[rfp] {
				related[rfp] = true
			}
		}
	}
	s.tagsMu.RUnlock()

	var assets []assetindex.Asset
	seen := map[string]bool{}
	for rfp := range related {
		for _, a := range s.byFP[rfp] {
			if seen[a.ID] {
				continue
			}
			seen[a.ID] = true
			assets = append(assets, a)
		}
	}
	grouped := groupItems(assets)
	s.resolveTags(grouped)
	sortItems(grouped, "")
	writeJSON(w, map[string]any{"items": grouped})
}

func (s *server) requireEnabled(w http.ResponseWriter) bool {
	if !s.tagsEnabled {
		writeErr(w, http.StatusConflict, "tagging is disabled: no synty-sync.toml found near the browse root")
		return false
	}
	return true
}

// persistLocked writes the store; the caller must hold the write lock.
func (s *server) persistLocked(w http.ResponseWriter) bool {
	if err := tagstore.Save(s.tagsPath, s.store); err != nil {
		// Handlers mutate the store and then persist, so a failed save would otherwise
		// leave memory claiming more than disk holds: the UI keeps reporting the tag
		// until a restart silently takes it away again.
		if reloaded, lerr := tagstore.Load(s.tagsPath); lerr == nil {
			s.store = reloaded
		}
		writeErr(w, http.StatusInternalServerError, "could not save tags: "+err.Error())
		return false
	}
	return true
}

// maxTagBodyBytes bounds a write request. The payloads are a handful of
// fingerprints and a tag name; anything larger is a mistake or an attack.
const maxTagBodyBytes = 1 << 20

// decodeJSON reads a write request's body. browse has no session by design, so any
// page the user has open can reach it on localhost; requiring a JSON content-type
// forces a CORS preflight that this server does not answer, which is what keeps a
// drive-by fetch from writing to the committed tag store.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeErr(w, http.StatusUnsupportedMediaType, "expected application/json")
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTagBodyBytes)).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
