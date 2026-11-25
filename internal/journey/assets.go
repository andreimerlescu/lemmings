package journey

import (
	"bytes"
	stdhtml "html"
	"mime"
	"net/url"
	"path"
	"strings"

	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
)

const (
	maxExtractedAssets = 256
	maxAssetScanBytes  = 4 << 20
)

// AssetRef describes a single same-origin subresource discovered in HTML or CSS.
// Class is one of: style, script, image, font, audio, video, data, other.
type AssetRef struct {
	URL   string `json:"url"`
	Class string `json:"class"`
}

// ExtractAssets scans htmlBody with a pure-stdlib linear HTML scanner and
// returns up to 256 deduplicated, order-preserving same-origin subresource URLs.
//
// It respects the first <base href> element, extracts assets from <link>,
// <script>, <img>, <source>, <video>, <audio>, <embed>, <object>, <iframe>,
// inline <style> blocks, and style="" attributes, strips URL fragments,
// enforces http/https + discovery.SameOrigin against baseURL, and skips
// Concert internal /_concert/ paths.
func ExtractAssets(baseURL *url.URL, htmlBody []byte) []AssetRef {
	originStr, ok := validAssetBase(baseURL)
	if !ok || len(htmlBody) == 0 {
		return nil
	}
	if len(htmlBody) > maxAssetScanBytes {
		htmlBody = htmlBody[:maxAssetScanBytes]
	}

	effectiveBase := findEffectiveBase(baseURL, htmlBody)

	var out []AssetRef
	seen := make(map[string]struct{}, 32)
	add := func(raw, contentType, fallbackClass string) {
		addAssetRef(effectiveBase, originStr, raw, contentType, fallbackClass, &out, seen)
	}

	inAudio := false
	inVideo := false

	n := len(htmlBody)
	i := 0
	for i < n && len(out) < maxExtractedAssets {
		lt := bytes.IndexByte(htmlBody[i:], '<')
		if lt < 0 {
			break
		}
		i += lt
		if i+1 >= n {
			break
		}
		next := htmlBody[i+1]

		// Comment: <!-- ... -->
		if next == '!' {
			if i+4 <= n && htmlBody[i+2] == '-' && htmlBody[i+3] == '-' {
				end := bytes.Index(htmlBody[i+4:], []byte("-->"))
				if end < 0 {
					break
				}
				i += 4 + end + 3
				continue
			}
			// Doctype or other declaration: <! ... >
			gt := bytes.IndexByte(htmlBody[i+2:], '>')
			if gt < 0 {
				break
			}
			i += 2 + gt + 1
			continue
		}

		// Processing instruction: <? ... >
		if next == '?' {
			gt := bytes.IndexByte(htmlBody[i+2:], '>')
			if gt < 0 {
				break
			}
			i += 2 + gt + 1
			continue
		}

		// Closing tag: </tag ...>
		if next == '/' {
			tagName, end := scanClosingTag(htmlBody, i+2)
			switch tagName {
			case "audio":
				inAudio = false
			case "video":
				inVideo = false
			}
			i = end
			continue
		}

		// Not an ASCII tag start letter — treat as literal '<'.
		if !isASCIIAlpha(next) {
			i++
			continue
		}

		tagName, attrs, selfClosing, tagEnd := scanOpeningTag(htmlBody, i+1)
		i = tagEnd

		// Inline style="" attribute on any element.
		if styleVal, hasStyle := attrs["style"]; hasStyle && styleVal != "" {
			extractCSSInto(effectiveBase, originStr, []byte(styleVal), &out, seen)
			if len(out) >= maxExtractedAssets {
				break
			}
		}

		switch tagName {
		case "link":
			if href := attrs["href"]; href != "" {
				if okRel, fallback := linkRelClass(attrs["rel"], attrs["as"]); okRel {
					add(href, attrs["type"], fallback)
				}
			}
		case "script":
			if src := attrs["src"]; src != "" {
				add(src, "", "script")
			}
			if !selfClosing {
				i = skipRawTextElement(htmlBody, i, "script")
			}
		case "style":
			if !selfClosing {
				cssEnd, afterClose := findRawTextEnd(htmlBody, i, "style")
				if cssEnd > i {
					extractCSSInto(effectiveBase, originStr, htmlBody[i:cssEnd], &out, seen)
				}
				i = afterClose
			}
		case "img":
			if src := attrs["src"]; src != "" {
				add(src, "", "image")
			}
			if dataSrc := attrs["data-src"]; dataSrc != "" {
				add(dataSrc, "", "image")
			}
			if srcset := attrs["srcset"]; srcset != "" {
				for _, cand := range parseSrcset(srcset) {
					add(cand, "", "image")
				}
			}
		case "source":
			fallback := ""
			if inVideo {
				fallback = "video"
			} else if inAudio {
				fallback = "audio"
			}
			if src := attrs["src"]; src != "" {
				add(src, attrs["type"], fallback)
			}
			if srcset := attrs["srcset"]; srcset != "" {
				for _, cand := range parseSrcset(srcset) {
					add(cand, attrs["type"], "image")
				}
			}
		case "video":
			if !selfClosing {
				inVideo = true
			}
			if src := attrs["src"]; src != "" {
				add(src, "", "video")
			}
			if poster := attrs["poster"]; poster != "" {
				add(poster, "", "image")
			}
		case "audio":
			if !selfClosing {
				inAudio = true
			}
			if src := attrs["src"]; src != "" {
				add(src, "", "audio")
			}
		case "embed":
			if src := attrs["src"]; src != "" {
				add(src, attrs["type"], "")
			}
		case "object":
			if data := attrs["data"]; data != "" {
				add(data, attrs["type"], "")
			}
		case "iframe":
			if src := attrs["src"]; src != "" {
				add(src, "", "")
			}
		}
	}

	return out
}

// ExtractCSSAssets extracts same-origin asset references from url(...) and
// @import rules in cssBody, resolving against cssURL. It skips data:, blob:,
// about:, and javascript: URLs, strips fragments, enforces discovery.SameOrigin,
// skips /_concert/ paths, deduplicates preserving order, and caps at 256 refs.
func ExtractCSSAssets(cssURL *url.URL, cssBody []byte) []AssetRef {
	originStr, ok := validAssetBase(cssURL)
	if !ok || len(cssBody) == 0 {
		return nil
	}
	if len(cssBody) > maxAssetScanBytes {
		cssBody = cssBody[:maxAssetScanBytes]
	}

	var out []AssetRef
	seen := make(map[string]struct{}, 16)
	extractCSSInto(cssURL, originStr, cssBody, &out, seen)
	return out
}

// ClassifyAsset maps an asset to one of style, script, image, font, audio,
// video, data, or other, preferring the MIME contentType when specific and
// falling back to the URL's file extension.
func ClassifyAsset(rawURL, contentType string) string {
	if cls := classifyByMIME(contentType); cls != "" {
		return cls
	}
	return classifyByExtension(rawURL)
}

func classifyByMIME(contentType string) string {
	ct := strings.TrimSpace(contentType)
	if ct == "" {
		return ""
	}
	media, _, err := mime.ParseMediaType(ct)
	if err != nil || media == "" {
		if semi := strings.IndexByte(ct, ';'); semi >= 0 {
			ct = ct[:semi]
		}
		media = strings.ToLower(strings.TrimSpace(ct))
	} else {
		media = strings.ToLower(strings.TrimSpace(media))
	}

	switch {
	case media == "text/css":
		return "style"
	case media == "application/javascript",
		media == "text/javascript",
		media == "application/x-javascript",
		media == "application/ecmascript",
		media == "text/ecmascript",
		media == "application/wasm",
		media == "application/mjs":
		return "script"
	case strings.HasPrefix(media, "image/"):
		return "image"
	case strings.HasPrefix(media, "font/"),
		media == "application/font-woff",
		media == "application/font-woff2",
		media == "application/x-font-woff",
		media == "application/x-font-ttf",
		media == "application/x-font-truetype",
		media == "application/x-font-opentype",
		media == "application/vnd.ms-fontobject",
		media == "application/font-sfnt":
		return "font"
	case strings.HasPrefix(media, "audio/"):
		return "audio"
	case strings.HasPrefix(media, "video/"),
		media == "application/vnd.apple.mpegurl",
		media == "application/x-mpegurl",
		media == "application/dash+xml":
		return "video"
	case media == "application/json",
		media == "application/manifest+json",
		media == "application/ld+json",
		media == "application/xml",
		media == "text/xml",
		media == "text/csv",
		media == "text/vtt",
		media == "application/graphql",
		strings.HasSuffix(media, "+json"),
		strings.HasSuffix(media, "+xml"):
		return "data"
	default:
		return ""
	}
}

func classifyByExtension(rawURL string) string {
	p := strings.TrimSpace(rawURL)
	if u, err := url.Parse(p); err == nil {
		p = u.Path
	} else if idx := strings.IndexAny(p, "?#"); idx >= 0 {
		p = p[:idx]
	}

	switch strings.ToLower(path.Ext(p)) {
	case ".css", ".less", ".scss", ".sass":
		return "style"
	case ".js", ".mjs", ".cjs", ".wasm", ".ts":
		return "script"
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".svg", ".ico", ".bmp", ".tif", ".tiff", ".jfif", ".apng":
		return "image"
	case ".woff", ".woff2", ".ttf", ".otf", ".eot":
		return "font"
	case ".mp3", ".wav", ".ogg", ".oga", ".flac", ".aac", ".m4a", ".opus":
		return "audio"
	case ".mp4", ".m4v", ".webm", ".ogv", ".mov", ".m3u8", ".mpd":
		return "video"
	case ".json", ".webmanifest", ".xml", ".csv", ".vtt", ".map", ".txt":
		return "data"
	default:
		return "other"
	}
}

func validAssetBase(baseURL *url.URL) (string, bool) {
	if baseURL == nil || baseURL.Host == "" || baseURL.User != nil {
		return "", false
	}
	scheme := strings.ToLower(baseURL.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	return scheme + "://" + baseURL.Host, true
}

func addAssetRef(
	effectiveBase *url.URL,
	originStr string,
	raw string,
	contentType string,
	fallbackClass string,
	out *[]AssetRef,
	seen map[string]struct{},
) {
	if len(*out) >= maxExtractedAssets {
		return
	}
	resolved, ok := resolveAssetURL(effectiveBase, originStr, raw)
	if !ok {
		return
	}
	if _, dup := seen[resolved]; dup {
		return
	}
	seen[resolved] = struct{}{}

	cls := ClassifyAsset(resolved, contentType)
	if cls == "other" && fallbackClass != "" {
		cls = fallbackClass
	}
	*out = append(*out, AssetRef{URL: resolved, Class: cls})
}

func resolveAssetURL(effectiveBase *url.URL, originStr, raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "#") {
		return "", false
	}
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "data:") ||
		strings.HasPrefix(lower, "blob:") ||
		strings.HasPrefix(lower, "about:") ||
		strings.HasPrefix(lower, "javascript:") ||
		strings.HasPrefix(lower, "mailto:") ||
		strings.HasPrefix(lower, "tel:") ||
		strings.HasPrefix(lower, "vbscript:") {
		return "", false
	}

	ref, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	if ref.Scheme != "" {
		s := strings.ToLower(ref.Scheme)
		if s != "http" && s != "https" {
			return "", false
		}
	}

	resolved := effectiveBase.ResolveReference(ref)
	resolved.Fragment = ""
	resolved.RawFragment = ""

	scheme := strings.ToLower(resolved.Scheme)
	if (scheme != "http" && scheme != "https") || resolved.Host == "" || resolved.User != nil {
		return "", false
	}
	out := resolved.String()
	if !discovery.SameOrigin(originStr, out) {
		return "", false
	}
	if isConcertInternalPath(resolved.Path) {
		return "", false
	}
	return out, true
}

func isConcertInternalPath(p string) bool {
	if p == "/_concert" || strings.HasPrefix(p, "/_concert/") {
		return true
	}
	cleaned := path.Clean("/" + p)
	return cleaned == "/_concert" || strings.HasPrefix(cleaned, "/_concert/")
}

func linkRelClass(rel, asAttr string) (bool, string) {
	if rel == "" {
		return false, ""
	}
	for _, tok := range strings.Fields(strings.ToLower(rel)) {
		switch tok {
		case "stylesheet":
			return true, "style"
		case "icon", "apple-touch-icon":
			return true, "image"
		case "modulepreload":
			return true, "script"
		case "manifest":
			return true, "data"
		case "preload":
			return true, preloadAsClass(asAttr)
		}
	}
	return false, ""
}

func preloadAsClass(asAttr string) string {
	switch strings.ToLower(strings.TrimSpace(asAttr)) {
	case "style":
		return "style"
	case "script", "worker":
		return "script"
	case "image":
		return "image"
	case "font":
		return "font"
	case "audio":
		return "audio"
	case "video", "track":
		return "video"
	case "fetch":
		return "data"
	default:
		return ""
	}
}

// findEffectiveBase locates the first <base href="..."> element in htmlBody
// (ignoring comments, <script>, and <style> blocks) and resolves it against
// baseURL. If absent or unparseable, it returns baseURL unchanged.
func findEffectiveBase(baseURL *url.URL, htmlBody []byte) *url.URL {
	if !containsFoldASCII(htmlBody, "<base") {
		return baseURL
	}
	n := len(htmlBody)
	i := 0
	for i < n {
		lt := bytes.IndexByte(htmlBody[i:], '<')
		if lt < 0 {
			break
		}
		i += lt
		if i+1 >= n {
			break
		}
		next := htmlBody[i+1]
		if next == '!' {
			if i+4 <= n && htmlBody[i+2] == '-' && htmlBody[i+3] == '-' {
				end := bytes.Index(htmlBody[i+4:], []byte("-->"))
				if end < 0 {
					break
				}
				i += 4 + end + 3
				continue
			}
			gt := bytes.IndexByte(htmlBody[i+2:], '>')
			if gt < 0 {
				break
			}
			i += 2 + gt + 1
			continue
		}
		if next == '?' {
			gt := bytes.IndexByte(htmlBody[i+2:], '>')
			if gt < 0 {
				break
			}
			i += 2 + gt + 1
			continue
		}
		if next == '/' {
			_, end := scanClosingTag(htmlBody, i+2)
			i = end
			continue
		}
		if !isASCIIAlpha(next) {
			i++
			continue
		}

		tagName, attrs, selfClosing, tagEnd := scanOpeningTag(htmlBody, i+1)
		i = tagEnd

		switch tagName {
		case "base":
			if href := strings.TrimSpace(attrs["href"]); href != "" {
				if ref, err := url.Parse(href); err == nil {
					return baseURL.ResolveReference(ref)
				}
			}
		case "script":
			if !selfClosing {
				i = skipRawTextElement(htmlBody, i, "script")
			}
		case "style":
			if !selfClosing {
				i = skipRawTextElement(htmlBody, i, "style")
			}
		}
	}
	return baseURL
}

func containsFoldASCII(haystack []byte, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	if len(haystack) < len(needle) {
		return false
	}
	first := toLowerASCII(needle[0])
	limit := len(haystack) - len(needle)
	for i := 0; i <= limit; i++ {
		if toLowerASCII(haystack[i]) != first {
			continue
		}
		match := true
		for j := 1; j < len(needle); j++ {
			if toLowerASCII(haystack[i+j]) != toLowerASCII(needle[j]) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func scanClosingTag(b []byte, start int) (string, int) {
	n := len(b)
	i := start
	for i < n && isTagNameByte(b[i]) {
		i++
	}
	name := strings.ToLower(string(b[start:i]))
	gt := bytes.IndexByte(b[i:], '>')
	if gt < 0 {
		return name, n
	}
	return name, i + gt + 1
}

// scanOpeningTag parses an opening HTML tag starting right after '<'.
// It returns the lowercased tag name, a map of lowercased attribute names to
// HTML-unescaped values, whether the tag ended with '/>', and the byte offset
// immediately after '>'.
func scanOpeningTag(b []byte, start int) (string, map[string]string, bool, int) {
	n := len(b)
	i := start
	for i < n && isTagNameByte(b[i]) {
		i++
	}
	tagName := strings.ToLower(string(b[start:i]))

	var attrs map[string]string
	selfClosing := false

	for i < n {
		// Skip whitespace.
		for i < n && isASCIIWhitespace(b[i]) {
			i++
		}
		if i >= n {
			break
		}
		if b[i] == '>' {
			i++
			break
		}
		if b[i] == '/' {
			if i+1 < n && b[i+1] == '>' {
				selfClosing = true
				i += 2
				break
			}
			i++
			continue
		}

		// Read attribute name.
		nameStart := i
		for i < n && !isASCIIWhitespace(b[i]) && b[i] != '=' && b[i] != '>' && b[i] != '/' {
			i++
		}
		attrName := strings.ToLower(string(b[nameStart:i]))
		if attrName == "" {
			i++
			continue
		}

		// Skip whitespace before optional '='.
		for i < n && isASCIIWhitespace(b[i]) {
			i++
		}

		var val string
		if i < n && b[i] == '=' {
			i++ // consume '='
			for i < n && isASCIIWhitespace(b[i]) {
				i++
			}
			if i < n && (b[i] == '"' || b[i] == '\'') {
				quote := b[i]
				i++
				valStart := i
				for i < n && b[i] != quote {
					i++
				}
				val = string(b[valStart:i])
				if i < n && b[i] == quote {
					i++
				}
			} else {
				valStart := i
				for i < n && !isASCIIWhitespace(b[i]) && b[i] != '>' {
					if b[i] == '/' && i+1 < n && b[i+1] == '>' {
						break
					}
					i++
				}
				val = string(b[valStart:i])
			}
			if strings.ContainsRune(val, '&') {
				val = stdhtml.UnescapeString(val)
			}
		}

		if isRelevantAttr(tagName, attrName) {
			if attrs == nil {
				attrs = make(map[string]string, 4)
			}
			if _, exists := attrs[attrName]; !exists {
				attrs[attrName] = val
			}
		}
	}

	return tagName, attrs, selfClosing, i
}

func isRelevantAttr(tag, attr string) bool {
	if attr == "style" {
		return true
	}
	switch tag {
	case "base":
		return attr == "href"
	case "link":
		return attr == "rel" || attr == "href" || attr == "as" || attr == "type"
	case "script":
		return attr == "src" || attr == "type"
	case "img":
		return attr == "src" || attr == "data-src" || attr == "srcset"
	case "source":
		return attr == "src" || attr == "srcset" || attr == "type"
	case "video":
		return attr == "src" || attr == "poster"
	case "audio", "iframe":
		return attr == "src"
	case "embed":
		return attr == "src" || attr == "type"
	case "object":
		return attr == "data" || attr == "type"
	default:
		return false
	}
}

func skipRawTextElement(b []byte, start int, tag string) int {
	_, after := findRawTextEnd(b, start, tag)
	return after
}

// findRawTextEnd finds the matching </tag...> closing tag for raw-text HTML
// elements (<script>, <style>) starting at start. It returns the end offset of
// the inner text and the byte offset immediately after the closing '>'.
func findRawTextEnd(b []byte, start int, tag string) (contentEnd, afterClose int) {
	n := len(b)
	tlen := len(tag)
	for i := start; i < n; i++ {
		if b[i] != '<' || i+2+tlen > n || b[i+1] != '/' {
			continue
		}
		match := true
		for j := 0; j < tlen; j++ {
			if toLowerASCII(b[i+2+j]) != tag[j] {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		afterName := i + 2 + tlen
		if afterName < n && !isASCIIWhitespace(b[afterName]) && b[afterName] != '>' && b[afterName] != '/' {
			continue
		}
		gt := bytes.IndexByte(b[afterName:], '>')
		if gt < 0 {
			return i, n
		}
		return i, afterName + gt + 1
	}
	return n, n
}

// parseSrcset implements the WHATWG HTML srcset candidate URL parser.
func parseSrcset(srcset string) []string {
	var out []string
	n := len(srcset)
	i := 0
	for i < n {
		// Skip leading whitespace and commas.
		for i < n && (isASCIIWhitespace(srcset[i]) || srcset[i] == ',') {
			i++
		}
		if i >= n {
			break
		}

		// Collect URL token up to whitespace.
		urlStart := i
		for i < n && !isASCIIWhitespace(srcset[i]) {
			i++
		}
		raw := srcset[urlStart:i]

		// If the token ends with a comma, strip trailing commas — no descriptor follows.
		if strings.HasSuffix(raw, ",") {
			raw = strings.TrimRight(raw, ",")
			if raw != "" {
				out = append(out, raw)
			}
			continue
		}
		if raw != "" {
			out = append(out, raw)
		}

		// Skip optional descriptor up to the next comma outside parentheses.
		parenDepth := 0
		for i < n {
			c := srcset[i]
			if c == '(' {
				parenDepth++
			} else if c == ')' && parenDepth > 0 {
				parenDepth--
			} else if c == ',' && parenDepth == 0 {
				i++
				break
			}
			i++
		}
	}
	return out
}

func extractCSSInto(
	cssURL *url.URL,
	originStr string,
	cssBody []byte,
	out *[]AssetRef,
	seen map[string]struct{},
) {
	n := len(cssBody)
	i := 0
	for i < n && len(*out) < maxExtractedAssets {
		c := cssBody[i]

		// Skip CSS comments: /* ... */
		if c == '/' && i+1 < n && cssBody[i+1] == '*' {
			end := bytes.Index(cssBody[i+2:], []byte("*/"))
			if end < 0 {
				return
			}
			i += 2 + end + 2
			continue
		}

		// Skip standalone string literals so url(...) inside content: "..." is ignored.
		if c == '"' || c == '\'' {
			_, nextIdx := scanCSSString(cssBody, i)
			i = nextIdx
			continue
		}

		// @import "..." | @import '...' | @import url(...)
		if c == '@' && (i == 0 || !isCSSIdentByte(cssBody[i-1])) && hasFoldPrefixAt(cssBody, i, "@import") {
			after := i + len("@import")
			if after < n && (isASCIIWhitespace(cssBody[after]) || cssBody[after] == '"' || cssBody[after] == '\'' || cssBody[after] == '/') {
				j := skipCSSSpaceAndComments(cssBody, after)
				if j < n && (cssBody[j] == '"' || cssBody[j] == '\'') {
					val, nextIdx := scanCSSString(cssBody, j)
					addAssetRef(cssURL, originStr, val, "", "style", out, seen)
					i = nextIdx
					continue
				}
				if j < n && hasFoldPrefixAt(cssBody, j, "url(") {
					val, nextIdx, ok := scanCSSURL(cssBody, j+4)
					if ok {
						addAssetRef(cssURL, originStr, val, "", "style", out, seen)
					}
					i = nextIdx
					continue
				}
			}
			i = after
			continue
		}

		// url(...)
		if (c == 'u' || c == 'U') && (i == 0 || !isCSSIdentByte(cssBody[i-1])) && hasFoldPrefixAt(cssBody, i, "url(") {
			val, nextIdx, ok := scanCSSURL(cssBody, i+4)
			if ok {
				addAssetRef(cssURL, originStr, val, "", "", out, seen)
			}
			i = nextIdx
			continue
		}

		i++
	}
}

func skipCSSSpaceAndComments(b []byte, i int) int {
	n := len(b)
	for i < n {
		if isASCIIWhitespace(b[i]) {
			i++
			continue
		}
		if b[i] == '/' && i+1 < n && b[i+1] == '*' {
			end := bytes.Index(b[i+2:], []byte("*/"))
			if end < 0 {
				return n
			}
			i += 2 + end + 2
			continue
		}
		break
	}
	return i
}

// scanCSSString reads a quoted CSS string starting at the opening quote at start.
func scanCSSString(b []byte, start int) (string, int) {
	n := len(b)
	if start >= n {
		return "", n
	}
	quote := b[start]
	i := start + 1
	var buf strings.Builder
	for i < n {
		c := b[i]
		if c == '\\' {
			if i+1 >= n {
				i++
				break
			}
			next := b[i+1]
			if next == '\n' {
				i += 2
				continue
			}
			if next == '\r' {
				if i+2 < n && b[i+2] == '\n' {
					i += 3
				} else {
					i += 2
				}
				continue
			}
			buf.WriteByte(next)
			i += 2
			continue
		}
		if c == quote {
			return buf.String(), i + 1
		}
		if c == '\n' || c == '\r' {
			// Unterminated string literal.
			return buf.String(), i
		}
		buf.WriteByte(c)
		i++
	}
	return buf.String(), n
}

// scanCSSURL reads the argument inside url(...) starting immediately after "url(".
func scanCSSURL(b []byte, start int) (string, int, bool) {
	n := len(b)
	i := skipCSSSpaceAndComments(b, start)
	if i >= n {
		return "", n, false
	}

	// Quoted URL inside url("...") or url('...').
	if b[i] == '"' || b[i] == '\'' {
		val, nextIdx := scanCSSString(b, i)
		j := skipCSSSpaceAndComments(b, nextIdx)
		if j < n && b[j] == ')' {
			return val, j + 1, true
		}
		return val, j, val != ""
	}

	// Unquoted URL inside url(...).
	var buf strings.Builder
	for i < n {
		c := b[i]
		if c == ')' {
			return strings.TrimSpace(buf.String()), i + 1, true
		}
		if c == '\\' && i+1 < n {
			buf.WriteByte(b[i+1])
			i += 2
			continue
		}
		if c == '\n' || c == '\r' || c == ';' || c == '{' || c == '}' {
			// Malformed unclosed url(...).
			return "", i, false
		}
		buf.WriteByte(c)
		i++
	}
	return "", n, false
}

func hasFoldPrefixAt(b []byte, offset int, prefix string) bool {
	if offset+len(prefix) > len(b) {
		return false
	}
	for j := 0; j < len(prefix); j++ {
		if toLowerASCII(b[offset+j]) != toLowerASCII(prefix[j]) {
			return false
		}
	}
	return true
}

func isASCIIAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isASCIIWhitespace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

func isTagNameByte(c byte) bool {
	return isASCIIAlpha(c) || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == ':'
}

func isCSSIdentByte(c byte) bool {
	return isASCIIAlpha(c) || (c >= '0' && c <= '9') || c == '-' || c == '_'
}

func toLowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}
