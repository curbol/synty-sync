// Package portal reads the Synty store's Sky Pilot download portal: parsing the
// "Your Library" list and per-pack item pages, and resolving signed download URLs.
package portal

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/curbol/synty-sync/internal/model"
)

// variantKeywords anchor the token/variant split. The heading text has no reliable
// delimiter between token and variant (goquery concatenates the `token<br><span>`
// shape with no space, and one real row fuses them with an underscore), so we find
// the rightmost keyword: the variant is always the last segment before " | ".
// Synty labels the source variants inconsistently (some packs use "Source_Sprites"
// / "Source_Files" with an underscore), so both forms are keywords and the result
// is canonicalized below.
var variantKeywords = []string{"Godot", "Unity", "Unreal", "SourceFiles", "SourceSprites", "Source_Files", "Source_Sprites"}

// canonicalVariant folds Synty's inconsistent source-variant spellings to one token
// so the filter matches them uniformly (Fantasy Warrior HUD ships "Source_Sprites",
// Dark Fantasy HUD ships "SourceSprites").
func canonicalVariant(v string) string {
	switch v {
	case "Source_Sprites":
		return "SourceSprites"
	case "Source_Files":
		return "SourceFiles"
	}
	return v
}

var (
	libAnchorRe  = regexp.MustCompile(`/customers/(\d+)/orders/(\d+)/order_items/(\d+)`)
	downloadIDRe = regexp.MustCompile(`/apps/downloads/downloads/(\d+)`)
	sizeRe       = regexp.MustCompile(`\(([\d.]+)\s*(KB|MB|GB)\)`)
	wsRe         = regexp.MustCompile(`\s+`)
)

// HasLibrarySentinel reports whether a page is an authenticated Sky Pilot page,
// distinguishing it from a logged-out shell. The marker is the "Search My Products"
// box (.sky-pilot-search-input): it is present on every authenticated library page
// INCLUDING the empty page past the last one, whereas the "Your Library" heading is
// absent on that overflow page (confirmed against the live store), so the heading
// would misread the legitimate terminator as an expired session.
func HasLibrarySentinel(html []byte) (bool, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(html))
	if err != nil {
		return false, err
	}
	return doc.Find(".sky-pilot-search-input").Length() > 0, nil
}

// ParseLibraryPage extracts the packs listed on one "Your Library" page. An
// authenticated page with zero packs (the pagination terminator) returns an empty
// slice and no error.
func ParseLibraryPage(html []byte) ([]model.Pack, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(html))
	if err != nil {
		return nil, err
	}
	var packs []model.Pack
	var parseErr error
	doc.Find("a.sky-pilot-list-item").EachWithBreak(func(i int, s *goquery.Selection) bool {
		href, _ := s.Attr("href")
		name := collapse(s.Text())
		m := libAnchorRe.FindStringSubmatch(href)
		if m == nil {
			// The href carries the customer id, so the anchor is identified by its
			// index and (non-identifying) link text instead of by URL.
			parseErr = fmt.Errorf("library anchor %d (%q) has no order_item url", i, name)
			return false
		}
		orderID, _ := strconv.Atoi(m[2])
		orderItemID, _ := strconv.Atoi(m[3])
		icon, _ := s.Find("img").First().Attr("src")
		packs = append(packs, model.Pack{
			Slug:        model.Slug(name),
			DisplayName: name,
			OrderID:     orderID,
			OrderItemID: orderItemID,
			ItemURL:     href,
			IconURL:     normalizeURL(icon),
		})
		return true
	})
	if parseErr != nil {
		return nil, parseErr
	}
	return packs, nil
}

// ParseItemPage extracts the downloadable files on one pack's item page. Versionless
// rows (icons) are skipped. packSlug stamps each FileEntry's owning pack.
//
// unknown holds the label of every row that carries a version and a download id but
// no variant keyword this build recognizes. Such a row is skipped rather than failed,
// because a future engine must not abort a run — but the skip is how a renamed
// keyword drops a file the lockfile already tracks, so the caller is told rather than
// left to notice the entry missing.
func ParseItemPage(html []byte, packSlug string) (files []model.FileEntry, unknown []string, err error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(html))
	if err != nil {
		return nil, nil, err
	}
	rows := doc.Find("div.sky-pilot-list-item")
	if rows.Length() == 0 {
		// Every owned pack's item page carries at least an icon row, so zero rows
		// means the markup moved, not that the pack is empty. Failing here keeps a
		// selector change from rewriting the pack's lockfile entry as empty.
		return nil, nil, fmt.Errorf("item page for %q: no file rows found", packSlug)
	}
	var parseErr error
	versioned := 0
	rows.EachWithBreak(func(_ int, row *goquery.Selection) bool {
		heading := row.Find(".sky-pilot-file-heading")
		sizeText := strings.TrimSpace(row.Find(".sky-pilot-file-size").Text())
		heading.Find(".sky-pilot-file-size").Remove()
		label := collapse(heading.Text())

		if !strings.Contains(label, " | ") {
			return true // versionless icon row
		}
		versioned++
		href, ok := row.Find(".sky-pilot-actions a[href]").First().Attr("href")
		if !ok {
			href, _ = row.Find("a[href*='/apps/downloads/downloads/']").First().Attr("href")
		}
		idMatch := downloadIDRe.FindStringSubmatch(href)
		if idMatch == nil {
			parseErr = fmt.Errorf("file row %q without a download id", label)
			return false
		}
		fileID, _ := strconv.Atoi(idMatch[1])

		token, variant, version, ok, splitErr := splitLabel(label)
		if splitErr != nil {
			// A row that already has a valid download id but is otherwise malformed
			// (empty version, or a recognized variant keyword with no token) is
			// structural breakage, not a skippable variant — fail loud.
			parseErr = fmt.Errorf("file row %q: %w", label, splitErr)
			return false
		}
		if !ok {
			// Unrecognized variant keyword (e.g. Synty's "Ureal" typo, or a future
			// engine). Skip rather than abort: such variants are never in the
			// Godot/SourceFiles filter.
			unknown = append(unknown, label)
			return true
		}
		size, _ := parseSize(sizeText)
		files = append(files, model.FileEntry{
			PackSlug:     packSlug,
			FileToken:    token,
			Variant:      model.Variant(canonicalVariant(variant)),
			Version:      version,
			FileID:       fileID,
			SizeBytes:    size,
			DownloadHref: html2href(href),
			Archived:     strings.Contains(strings.ToUpper(version), "ARCHIVED"),
		})
		return true
	})
	if parseErr != nil {
		return nil, nil, parseErr
	}
	if versioned == 0 {
		// The zero-rows guard above only covers the row selector. If the file-heading
		// class moves, every row parses to an empty label and is skipped as an icon
		// row, and this pack would silently rebuild its lockfile entry as empty.
		return nil, nil, fmt.Errorf("item page for %q: %d rows, none carrying a version label", packSlug, rows.Length())
	}
	return files, unknown, nil
}

// splitLabel turns "POLYGON_PirateGodot_4_5_1 | v1_0_1" (or the underscore-fused
// "INTERFACE_Dark_Fantasy_HUD_SourceSprites | v3") into token, variant, version.
// ok is false with a nil error only for the benign "no recognized variant keyword"
// case (a safe skip); a non-nil error marks structural breakage a caller must not
// silently drop.
func splitLabel(label string) (token, variant, version string, ok bool, err error) {
	bar := strings.Index(label, " | ")
	if bar < 0 {
		return "", "", "", false, nil
	}
	left := strings.TrimSpace(label[:bar])
	version = strings.TrimSpace(label[bar+len(" | "):])
	if version == "" {
		return "", "", "", false, fmt.Errorf("empty version")
	}
	cut := -1
	for _, kw := range variantKeywords {
		if i := strings.LastIndex(left, kw); i > cut {
			cut = i
		}
	}
	if cut < 0 {
		return "", "", "", false, nil // no recognized variant keyword — safe skip
	}
	token = strings.Trim(left[:cut], " _")
	variant = strings.TrimSpace(left[cut:])
	if token == "" || variant == "" {
		return "", "", "", false, fmt.Errorf("variant %q with empty token", variant)
	}
	return token, variant, version, true, nil
}

// parseSize converts a rounded portal size label ("2.6 MB") to an approximate byte
// count using 1024-based units (the store rounds, so this is a ballpark only).
func parseSize(s string) (int64, bool) {
	m := sizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	mult := map[string]float64{"KB": 1 << 10, "MB": 1 << 20, "GB": 1 << 30}[m[2]]
	return int64(n * mult), true
}

func collapse(s string) string { return strings.TrimSpace(wsRe.ReplaceAllString(s, " ")) }

// html2href returns the href as given; goquery already decodes HTML entities in
// attribute values, so "&amp;" arrives as "&".
func html2href(h string) string { return h }

// normalizeURL upgrades a protocol-relative URL ("//cdn...") to https.
func normalizeURL(u string) string {
	if strings.HasPrefix(u, "//") {
		return "https:" + u
	}
	return u
}
