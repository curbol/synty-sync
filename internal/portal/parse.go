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
	"github.com/curbol/hexed-haven/tools/synty/internal/model"
)

// variantKeywords anchor the token/variant split. The heading text has no reliable
// delimiter between token and variant (goquery concatenates the `token<br><span>`
// shape with no space, and one real row fuses them with an underscore), so we find
// the rightmost keyword: the variant is always the last segment before " | ".
var variantKeywords = []string{"Godot", "Unity", "Unreal", "SourceFiles", "SourceSprites"}

var (
	libAnchorRe  = regexp.MustCompile(`/customers/(\d+)/orders/(\d+)/order_items/(\d+)`)
	downloadIDRe = regexp.MustCompile(`/apps/downloads/downloads/(\d+)`)
	sizeRe       = regexp.MustCompile(`\(([\d.]+)\s*(KB|MB|GB)\)`)
	wsRe         = regexp.MustCompile(`\s+`)
)

// HasLibrarySentinel reports whether a page is an authenticated library page (the
// positive "Your Library" heading), distinguishing it from a logged-out shell.
// sky-pilot-files-list is NOT used: it appears on item pages too.
func HasLibrarySentinel(html []byte) (bool, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(html))
	if err != nil {
		return false, err
	}
	found := false
	doc.Find("h1, h2, h3").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		if strings.EqualFold(strings.TrimSpace(s.Text()), "Your Library") {
			found = true
			return false
		}
		return true
	})
	return found, nil
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
	doc.Find("a.sky-pilot-list-item").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		href, _ := s.Attr("href")
		m := libAnchorRe.FindStringSubmatch(href)
		if m == nil {
			parseErr = fmt.Errorf("library anchor without order_item url: %q", href)
			return false
		}
		orderID, _ := strconv.Atoi(m[2])
		orderItemID, _ := strconv.Atoi(m[3])
		name := collapse(s.Text())
		packs = append(packs, model.Pack{
			Slug:        model.Slug(name),
			DisplayName: name,
			OrderID:     orderID,
			OrderItemID: orderItemID,
			ItemURL:     href,
		})
		return true
	})
	return packs, parseErr
}

// ParseItemPage extracts the downloadable files on one pack's item page. Versionless
// rows (icons) are skipped. packSlug stamps each FileEntry's owning pack.
func ParseItemPage(html []byte, packSlug string) ([]model.FileEntry, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(html))
	if err != nil {
		return nil, err
	}
	var files []model.FileEntry
	var parseErr error
	doc.Find("div.sky-pilot-list-item").EachWithBreak(func(_ int, row *goquery.Selection) bool {
		heading := row.Find(".sky-pilot-file-heading")
		sizeText := strings.TrimSpace(row.Find(".sky-pilot-file-size").Text())
		heading.Find(".sky-pilot-file-size").Remove()
		label := collapse(heading.Text())

		if !strings.Contains(label, " | ") {
			return true // versionless icon row
		}
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

		token, variant, version, ok := splitLabel(label)
		if !ok {
			parseErr = fmt.Errorf("file row %q: cannot recover token/variant", label)
			return false
		}
		size, _ := parseSize(sizeText)
		files = append(files, model.FileEntry{
			PackSlug:     packSlug,
			FileToken:    token,
			Variant:      model.Variant(variant),
			Version:      version,
			FileID:       fileID,
			SizeBytes:    size,
			DownloadHref: html2href(href),
			Archived:     strings.Contains(strings.ToUpper(version), "ARCHIVED"),
		})
		return true
	})
	return files, parseErr
}

// splitLabel turns "POLYGON_PirateGodot_4_5_1 | v1_0_1" (or the underscore-fused
// "INTERFACE_Dark_Fantasy_HUD_SourceSprites | v3") into token, variant, version.
func splitLabel(label string) (token, variant, version string, ok bool) {
	bar := strings.Index(label, " | ")
	if bar < 0 {
		return "", "", "", false
	}
	left := strings.TrimSpace(label[:bar])
	version = strings.TrimSpace(label[bar+len(" | "):])
	if version == "" {
		return "", "", "", false
	}
	cut := -1
	for _, kw := range variantKeywords {
		if i := strings.LastIndex(left, kw); i > cut {
			cut = i
		}
	}
	if cut <= 0 {
		return "", "", "", false
	}
	token = strings.Trim(left[:cut], " _")
	variant = strings.TrimSpace(left[cut:])
	if token == "" || variant == "" {
		return "", "", "", false
	}
	return token, variant, version, true
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
