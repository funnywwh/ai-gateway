package webaccess

import (
	"html"
	"strings"
	"unicode"
)

// The extractor is deliberately small. Its job is "turn a page into text a model can read",
// not "render a page": it keeps the title, drops the parts that are code rather than prose,
// turns block boundaries into newlines and decodes entities. Reader-mode extraction (main
// content vs navigation vs comments) is out of scope — see the M73 design doc's 不做 list.
//
// Not depending on golang.org/x/net/html is a decision, not an oversight: the gateway's
// dependency set is tiny and the module cache does not carry that package, so a hand-written
// scanner is what keeps `make build` working exactly as it did before.

// extracted is the readable half of one HTML document.
type extracted struct {
	Title string
	Text  string
}

// skippedElements hold code, styling or embedded documents. Their text content is not prose:
// a script's source or a stylesheet's rules read like garbage to a model and can be huge.
// `head` is deliberately NOT here — the title and the description meta live in it.
var skippedElements = map[string]bool{
	"script": true, "style": true, "noscript": true, "template": true, "svg": true,
	"iframe": true, "object": true, "embed": true, "canvas": true,
	"audio": true, "video": true,
}

// blockElements end a line. Without this every page collapses into one wall of text, and a
// model cannot tell a heading from a paragraph.
var blockElements = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true, "br": true,
	"dd": true, "div": true, "dl": true, "dt": true, "fieldset": true, "figcaption": true,
	"figure": true, "footer": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true,
	"h6": true, "header": true, "hr": true, "li": true, "main": true, "nav": true, "ol": true,
	"p": true, "pre": true, "section": true, "table": true, "tbody": true, "td": true,
	"tfoot": true, "th": true, "thead": true, "tr": true, "ul": true,
}

// extractHTML scans one document. A malformed page is normal (the web is full of them), so the
// scanner never fails: it takes what it can parse and returns the text it collected.
func extractHTML(body []byte) extracted {
	raw := string(body)
	var (
		out       strings.Builder
		title     strings.Builder
		inTitle   bool
		skip      string // the skipped element currently open, "" when reading normally
		depth     int    // nested open tags of the skipped element
		lower     = strings.ToLower(raw)
		lastSpace = true
	)
	// The scan walks the original string with a case-folded shadow copy only for tag matching:
	// text must keep its original case.
	for i := 0; i < len(raw); {
		if raw[i] != '<' {
			start := i
			for i < len(raw) && raw[i] != '<' {
				i++
			}
			if skip != "" {
				continue
			}
			text := decodeEntities(raw[start:i])
			if inTitle {
				title.WriteString(text)
				continue
			}
			out.WriteString(collapseSpaces(text, &lastSpace))
			continue
		}

		// A tag: read up to the matching '>' while honouring quoted attribute values, since
		// attributes may contain '>' (a meta description often does).
		end := tagEnd(raw, i)
		if end < 0 {
			break
		}
		inner := raw[i+1 : end]
		i = end + 1
		switch {
		case strings.HasPrefix(inner, "!--"):
			// The comment opened at the '<' this tag started with: i is already past the
			// '>' of "<!--…>", but a comment has no '>' to stop at, so the scan resumes
			// after "-->" instead.
			commentStart := i - len(inner) - 2
			commentEnd := strings.Index(lower[commentStart:], "-->")
			if commentEnd < 0 {
				// An unterminated comment: nothing after it is trustworthy.
				return extracted{Title: tidyTitle(title.String()), Text: tidyText(out.String())}
			}
			i = commentStart + commentEnd + len("-->")
			continue
		case strings.HasPrefix(inner, "!"), strings.HasPrefix(inner, "?"):
			// Doctype and processing instructions.
			continue
		}

		name, closing, selfClosing := parseTag(inner)
		if name == "" {
			continue
		}
		switch {
		case skip != "":
			// Only the same element name can close the skipped subtree; other tags are
			// irrelevant while inside it.
			if name == skip {
				if closing {
					if depth <= 1 {
						skip, depth = "", 0
					} else {
						depth--
					}
				} else if !selfClosing {
					depth++
				}
			}
			continue
		case closing:
			if blockElements[name] {
				out.WriteString("\n")
				lastSpace = true
			}
			if name == "title" {
				inTitle = false
			}
			continue
		}
		if skippedElements[name] {
			if !selfClosing {
				skip, depth = name, 1
			}
			continue
		}
		switch {
		case name == "title":
			inTitle = true
		case name == "meta":
			if description := metaDescription(inner); description != "" && strings.TrimSpace(out.String()) == "" {
				out.WriteString(description)
				out.WriteString("\n\n")
				lastSpace = true
			}
		case name == "br":
			out.WriteString("\n")
			lastSpace = true
		case name == "li":
			// A list item keeps its marker: without it a bulleted list reads as loose
			// sentences and the model loses the grouping.
			out.WriteString("\n- ")
			lastSpace = true
		case blockElements[name]:
			out.WriteString("\n")
			lastSpace = true
		}
	}

	return extracted{Title: tidyTitle(title.String()), Text: tidyText(out.String())}
}

// tagEnd finds the '>' that closes a tag starting at index i, skipping quoted attribute
// values. It returns -1 when the document is truncated mid-tag.
func tagEnd(raw string, i int) int {
	var quote byte
	for j := i + 1; j < len(raw); j++ {
		switch ch := raw[j]; {
		case quote != 0:
			if ch == quote {
				quote = 0
			}
		case ch == '"' || ch == '\'':
			quote = ch
		case ch == '>':
			return j
		}
	}
	return -1
}

// parseTag splits "<name attr=…>", "</name>" or "<name/>" into its parts.
func parseTag(inner string) (name string, closing, selfClosing bool) {
	inner = strings.TrimSpace(inner)
	if strings.HasPrefix(inner, "/") {
		closing = true
		inner = strings.TrimSpace(inner[1:])
	}
	trimmedRight := strings.TrimRight(inner, "/ \t\r\n")
	selfClosing = trimmedRight != inner
	inner = trimmedRight
	end := strings.IndexAny(inner, " \t\r\n")
	if end < 0 {
		end = len(inner)
	}
	name = strings.ToLower(inner[:end])
	for _, r := range name {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' && r != ':' {
			// "<3" and other non-tags are not elements.
			return "", false, false
		}
	}
	// Void elements are self-closing by definition; treating them as open tags would make the
	// scanner swallow the rest of the page.
	if voidElements[name] {
		selfClosing = true
	}
	return name, closing, selfClosing
}

// voidElements never have content.
var voidElements = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true,
	"img": true, "input": true, "link": true, "meta": true, "param": true, "source": true,
	"track": true, "wbr": true,
}

// metaDescription reads a <meta name="description"> or og:description content attribute.
func metaDescription(inner string) string {
	attrs := parseAttributes(inner)
	key := strings.ToLower(attrs["name"])
	if key == "" {
		key = strings.ToLower(attrs["property"])
	}
	if key != "description" && key != "og:description" {
		return ""
	}
	return strings.TrimSpace(decodeEntities(attrs["content"]))
}

// parseAttributes reads the attributes of one tag into a lowercase-keyed map. Values may be
// quoted or bare.
func parseAttributes(inner string) map[string]string {
	out := map[string]string{}
	rest := inner
	if idx := strings.IndexAny(rest, " \t\r\n"); idx >= 0 {
		rest = rest[idx+1:]
	} else {
		return out
	}
	for len(rest) > 0 {
		rest = strings.TrimLeft(rest, " \t\r\n")
		if rest == "" {
			break
		}
		end := strings.IndexAny(rest, " \t\r\n=")
		if end < 0 {
			out[strings.ToLower(rest)] = ""
			break
		}
		name := strings.ToLower(rest[:end])
		rest = rest[end:]
		if !strings.HasPrefix(rest, "=") {
			out[name] = ""
			continue
		}
		rest = strings.TrimPrefix(rest, "=")
		rest = strings.TrimLeft(rest, " \t\r\n")
		if rest == "" {
			out[name] = ""
			break
		}
		var value string
		if rest[0] == '"' || rest[0] == '\'' {
			quote := rest[0]
			rest = rest[1:]
			if idx := strings.IndexByte(rest, quote); idx < 0 {
				value, rest = rest, ""
			} else {
				value, rest = rest[:idx], rest[idx+1:]
			}
		} else if idx := strings.IndexAny(rest, " \t\r\n"); idx < 0 {
			value, rest = rest, ""
		} else {
			value, rest = rest[:idx], rest[idx+1:]
		}
		if name != "" {
			out[name] = value
		}
	}
	return out
}

// declaredCharset reads the document's own charset declaration, which is the fallback when the
// HTTP header does not carry one. Only the first few KB are inspected: the declaration must be
// in the head to be honoured by browsers either way.
func declaredCharset(raw string) string {
	head := raw
	if len(head) > 4096 {
		head = head[:4096]
	}
	lower := strings.ToLower(head)
	if idx := strings.Index(lower, "charset="); idx >= 0 {
		rest := head[idx+len("charset="):]
		rest = strings.TrimLeft(rest, " \t\"'")
		end := strings.IndexAny(rest, "\"' \t\r\n;>/")
		if end < 0 {
			end = len(rest)
		}
		return strings.ToLower(strings.TrimSpace(rest[:end]))
	}
	return ""
}

// isUTF8Name reports whether a declared charset is one this package decodes.
func isUTF8Name(charset string) bool {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		return true
	}
	return false
}

// collapseSpaces folds runs of whitespace inside text into single spaces, and keeps the
// newlines the block elements introduced.
func collapseSpaces(text string, lastSpace *bool) string {
	var b strings.Builder
	for _, r := range text {
		switch {
		case r == '\n' || r == '\r':
			b.WriteByte('\n')
			*lastSpace = true
		case r == '\t' || unicode.IsSpace(r):
			if !*lastSpace {
				b.WriteByte(' ')
				*lastSpace = true
			}
		default:
			b.WriteRune(r)
			*lastSpace = false
		}
	}
	return b.String()
}

// tidyText trims each line and collapses the empty ones, so the model sees paragraphs rather
// than the page's indentation.
func tidyText(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, line := range lines {
		text := strings.TrimSpace(line)
		if text == "" {
			blank = len(out) > 0
			continue
		}
		if blank {
			out = append(out, "")
			blank = false
		}
		out = append(out, text)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// tidyTitle normalizes a page title to one line.
func tidyTitle(raw string) string {
	return strings.Join(strings.Fields(raw), " ")
}

// decodeEntities resolves character references with the standard library's own table, so the
// extractor inherits its (correct) handling of the named entities.
func decodeEntities(text string) string {
	if !strings.Contains(text, "&") {
		return text
	}
	return html.UnescapeString(text)
}
