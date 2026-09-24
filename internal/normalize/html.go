package normalize

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/PuerkitoBio/goquery"
	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// BodyOptions control body normalization for one item.
type BodyOptions struct {
	Base      *url.URL // resolves relative URLs; the page URL or its <base href>
	LeadImage string   // prepended when the body has no image of its own (§6.9)
	KeepVideo bool     // keep <video> elements next to the fallback link (§6.4)
}

// Body turns extracted article HTML into feed-safe HTML (§6): lazy images
// fixed, embeds replaced with links, URLs made absolute, invisible characters
// stripped, sanitized, and tidied.
func Body(raw string, o BodyOptions) (string, error) {
	doc, err := parseFragment(raw)
	if err != nil {
		return "", err
	}
	body := doc.Find("body")

	// Webflow's empty CMS bindings and anything a reader can't use.
	body.Find(".w-dyn-bind-empty, .w-condition-invisible, script, style, noscript, template").Remove()

	fixLazyImages(body)
	flattenCode(body)
	replaceEmbeds(body, o.KeepVideo)
	absolutize(body, o.Base)
	stripInvisibleText(body.Get(0))

	h, err := body.Html()
	if err != nil {
		return "", err
	}
	h = policy(o.KeepVideo).Sanitize(h)

	if doc, err = parseFragment(h); err != nil {
		return "", err
	}
	body = doc.Find("body")
	tidy(body.Get(0))
	if o.LeadImage != "" && body.Find("img").Length() == 0 {
		body.PrependHtml(fmt.Sprintf(`<p><img src="%s" alt=""/></p>`, html.EscapeString(o.LeadImage)))
	}
	h, err = body.Html()
	return strings.TrimSpace(h), err
}

func parseFragment(h string) (*goquery.Document, error) {
	return goquery.NewDocumentFromReader(strings.NewReader("<html><body>" + h + "</body></html>"))
}

// fixLazyImages promotes data-src/data-srcset when the real attribute is
// missing or a placeholder, and drops loading hints (§6.3).
func fixLazyImages(s *goquery.Selection) {
	s.Find("img, source, video, iframe").Each(func(_ int, el *goquery.Selection) {
		for _, pair := range [][2]string{{"src", "data-src"}, {"srcset", "data-srcset"}} {
			lazy, ok := el.Attr(pair[1])
			if !ok || strings.TrimSpace(lazy) == "" {
				continue
			}
			if cur, _ := el.Attr(pair[0]); isPlaceholder(cur) {
				el.SetAttr(pair[0], lazy)
			}
			el.RemoveAttr(pair[1])
		}
		el.RemoveAttr("loading")
		el.RemoveAttr("decoding")
	})
}

func isPlaceholder(v string) bool {
	v = strings.TrimSpace(v)
	return v == "" || strings.HasPrefix(v, "data:") || strings.Contains(v, "placeholder") || v == "about:blank"
}

// flattenCode replaces each <pre>'s content with its plain text in a single
// <code>. Syntax-highlighting markup (per-token spans, per-line divs) is
// stripped by the sanitizer anyway, and flattening keeps the line breaks.
func flattenCode(s *goquery.Selection) {
	s.Find("pre").Each(func(_ int, pre *goquery.Selection) {
		var b strings.Builder
		for _, n := range pre.Nodes {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				codeText(&b, c)
			}
		}
		text := strings.TrimRight(b.String(), "\n")
		pre.SetHtml("<code>" + html.EscapeString(text) + "</code>")
	})
}

// codeText writes n's text, turning <br> and the start and end of block-level
// line wrappers into newlines, since highlighters often mark lines that way.
func codeText(b *strings.Builder, n *html.Node) {
	switch n.Type {
	case html.TextNode:
		b.WriteString(n.Data)
		return
	case html.ElementNode:
		if n.DataAtom == atom.Br {
			b.WriteByte('\n')
			return
		}
	}
	block := n.Type == html.ElementNode && (blockElements[n.DataAtom] || n.DataAtom == atom.Li || n.DataAtom == atom.Tr)
	// A block wrapper is a line of its own: break before it as well as after.
	lineBreak := func() {
		if s := b.String(); block && s != "" && !strings.HasSuffix(s, "\n") {
			b.WriteByte('\n')
		}
	}
	lineBreak()
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		codeText(b, c)
	}
	lineBreak()
}

var (
	youTubeEmbed = regexp.MustCompile(`^https?://(?:www\.)?youtube(?:-nocookie)?\.com/embed/([A-Za-z0-9_-]{6,})`)
	vimeoEmbed   = regexp.MustCompile(`^https?://player\.vimeo\.com/video/(\d+)`)
)

// replaceEmbeds swaps iframes and videos for a linked poster image and a text
// link, since readers strip them inconsistently (§6.4).
func replaceEmbeds(s *goquery.Selection, keepVideo bool) {
	s.Find("iframe").Each(func(_ int, f *goquery.Selection) {
		src := strings.TrimSpace(f.AttrOr("src", ""))
		if strings.HasPrefix(src, "//") {
			src = "https:" + src
		}
		title := strings.TrimSpace(f.AttrOr("title", ""))
		var link, poster, label string
		switch {
		case youTubeEmbed.MatchString(src):
			id := youTubeEmbed.FindStringSubmatch(src)[1]
			link, poster, label = "https://www.youtube.com/watch?v="+id, "https://i.ytimg.com/vi/"+id+"/hqdefault.jpg", "▶ Watch video"
		case vimeoEmbed.MatchString(src):
			link, label = "https://vimeo.com/"+vimeoEmbed.FindStringSubmatch(src)[1], "▶ Watch video"
		case src != "":
			link, label = src, "Open embedded content"
		default:
			f.Remove()
			return
		}
		f.ReplaceWithHtml(embedHTML(link, poster, label, title))
	})
	s.Find("video").Each(func(_ int, v *goquery.Selection) {
		src := v.AttrOr("src", v.Find("source[src]").First().AttrOr("src", ""))
		if src == "" {
			if !keepVideo {
				v.Remove()
			}
			return
		}
		h := embedHTML(src, v.AttrOr("poster", ""), "▶ Watch video", v.AttrOr("title", ""))
		if keepVideo {
			v.AfterHtml(h)
		} else {
			v.ReplaceWithHtml(h)
		}
	})
}

func embedHTML(link, poster, label, title string) string {
	var b strings.Builder
	b.WriteString("<p>")
	if poster != "" {
		fmt.Fprintf(&b, `<a href="%s"><img src="%s" alt="%s"/></a><br/>`, html.EscapeString(link), html.EscapeString(poster), html.EscapeString(title))
	}
	text := label
	if title != "" {
		text += ": " + title
	}
	fmt.Fprintf(&b, `<a href="%s">%s</a></p>`, html.EscapeString(link), html.EscapeString(text))
	return b.String()
}

// absolutize resolves every URL-bearing attribute against base (§6.2).
func absolutize(s *goquery.Selection, base *url.URL) {
	if base == nil {
		return
	}
	resolve := func(v string) string {
		v = strings.TrimSpace(v)
		// In-page anchors resolve too: the ids they target don't survive
		// sanitizing, so point them at that section of the live page instead.
		if v == "" || strings.HasPrefix(v, "data:") {
			return v
		}
		u, err := base.Parse(v)
		if err != nil {
			return v
		}
		return u.String()
	}
	s.Find("[href], [src], [poster], [cite]").Each(func(_ int, el *goquery.Selection) {
		for _, a := range []string{"href", "src", "poster", "cite"} {
			if v, ok := el.Attr(a); ok {
				el.SetAttr(a, resolve(v))
			}
		}
	})
	s.Find("[srcset]").Each(func(_ int, el *goquery.Selection) {
		el.SetAttr("srcset", rewriteSrcset(el.AttrOr("srcset", ""), resolve))
	})
}

// rewriteSrcset applies fn to each candidate URL, keeping its descriptor.
// It tokenizes as the HTML spec does: a URL runs to the next whitespace (so it
// may contain commas), and only trailing commas end a descriptor-less candidate.
func rewriteSrcset(v string, fn func(string) string) string {
	var out []string
	isSpace := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' }
	for i := 0; i < len(v); {
		for i < len(v) && (isSpace(v[i]) || v[i] == ',') {
			i++
		}
		start := i
		for i < len(v) && !isSpace(v[i]) {
			i++
		}
		u := v[start:i]
		if u == "" {
			break
		}
		var desc string
		if trimmed := strings.TrimRight(u, ","); trimmed != u {
			u = trimmed // "a.jpg," has no descriptor
		} else {
			start, depth := i, 0
			for ; i < len(v) && (v[i] != ',' || depth > 0); i++ {
				switch v[i] {
				case '(':
					depth++
				case ')':
					depth--
				}
			}
			desc = strings.Join(strings.Fields(v[start:i]), " ")
		}
		cand := fn(u)
		if desc != "" {
			cand += " " + desc
		}
		out = append(out, cand)
	}
	return strings.Join(out, ", ")
}

func stripInvisibleText(n *html.Node) {
	if n == nil {
		return
	}
	if n.Type == html.TextNode {
		n.Data = StripInvisible(n.Data)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		stripInvisibleText(c)
	}
}

// policy is the sanitizer allowlist (§6.5): structure, text semantics, tables,
// figures, code, images and links. class, style, id and handlers never pass.
func policy(keepVideo bool) *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowStandardURLs()
	p.RequireNoFollowOnLinks(false) // AllowStandardURLs turns it on; it means nothing in a feed
	p.RequireParseableURLs(true)
	p.AllowElements(
		"p", "br", "hr", "h1", "h2", "h3", "h4", "h5", "h6", "blockquote", "pre", "code",
		"em", "strong", "b", "i", "u", "s", "del", "ins", "sub", "sup", "small", "mark",
		"abbr", "cite", "q", "kbd", "samp", "var", "ul", "ol", "li", "dl", "dt", "dd",
		"figure", "figcaption", "table", "thead", "tbody", "tfoot", "tr", "caption", "div", "span",
	)
	p.AllowAttrs("href", "title").OnElements("a")
	// No width/height: frameworks emit layout hints there (Next.js writes 2000x2000
	// for a 2976x1892 image) that distort images once the site's CSS is gone.
	p.AllowAttrs("src", "srcset", "alt", "title").OnElements("img")
	p.AllowAttrs("colspan", "rowspan", "scope").OnElements("td", "th")
	p.AllowAttrs("start", "reversed").OnElements("ol")
	p.AllowAttrs("cite").OnElements("blockquote", "q")
	if keepVideo {
		p.AllowAttrs("src", "poster", "width", "height").OnElements("video")
		p.AllowAttrs("controls").Matching(regexp.MustCompile(`^(controls)?$`)).OnElements("video")
		p.AllowAttrs("src", "type").OnElements("source")
	}
	// Interactive and graphic elements whose text is noise without them.
	p.SkipElementsContent("svg", "button", "form", "select", "textarea", "canvas", "object", "embed", "template", "noscript")
	return p
}

// listContainers can't hold text at all, so whitespace-only text in them is noise.
var listContainers = map[atom.Atom]bool{
	atom.Ul: true, atom.Ol: true, atom.Dl: true, atom.Table: true,
	atom.Thead: true, atom.Tbody: true, atom.Tfoot: true, atom.Tr: true,
}

// blockContainers usually hold blocks, where whitespace-only text is noise
// only between blocks or at the edges: between inline elements it is a space.
var blockContainers = map[atom.Atom]bool{
	atom.Body: true, atom.Div: true, atom.Figure: true, atom.Blockquote: true,
}

var spaceRun = regexp.MustCompile(`[ \t\r\n\f]+`)

// tidy removes empty wrappers, unwraps single-child divs and spans, and
// collapses whitespace outside <pre> (§6.6).
func tidy(n *html.Node) {
	var next *html.Node
	for c := n.FirstChild; c != nil; c = next {
		next = c.NextSibling
		switch c.Type {
		case html.TextNode:
			if inPre(c) {
				continue
			}
			c.Data = spaceRun.ReplaceAllString(c.Data, " ")
			// Whitespace between inline elements separates words; it is only
			// noise at the container's edges or next to a block.
			if strings.TrimSpace(c.Data) == "" && (listContainers[n.DataAtom] ||
				blockContainers[n.DataAtom] && isBlockEdge(c.PrevSibling) && isBlockEdge(c.NextSibling)) {
				n.RemoveChild(c)
			}
		case html.ElementNode:
			tidy(c)
			switch c.DataAtom {
			case atom.Div, atom.Span:
				switch {
				case isEmpty(c):
					n.RemoveChild(c)
				case c.DataAtom == atom.Div && hasInline(c):
					// Inline content directly in a div would run into its neighbors' once
					// unwrapped, so each run of inline content becomes a paragraph.
					// Converting the div itself to <p> would be invalid if it also
					// holds blocks, such as <h2>.
					wrapInlineRuns(c)
					unwrap(c)
				default:
					unwrap(c) // no attributes survive sanitizing, so it is only a wrapper
				}
			case atom.P, atom.Figure, atom.A, atom.Li, atom.Figcaption, atom.Em, atom.Strong:
				if isEmpty(c) {
					n.RemoveChild(c)
				}
			}
		}
	}
}

// isEmpty is true for elements with no text and no media or breaks inside.
func isEmpty(n *html.Node) bool {
	if n.Type == html.TextNode {
		return strings.TrimSpace(n.Data) == ""
	}
	switch n.DataAtom {
	case atom.Img, atom.Video, atom.Br, atom.Hr, atom.Table:
		return false
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if !isEmpty(c) {
			return false
		}
	}
	return true
}

// blockElements may not appear inside a <p>.
var blockElements = map[atom.Atom]bool{
	atom.P: true, atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true,
	atom.Ul: true, atom.Ol: true, atom.Dl: true, atom.Pre: true, atom.Blockquote: true, atom.Figure: true,
	atom.Figcaption: true, atom.Table: true, atom.Hr: true, atom.Div: true,
}

// wrapInlineRuns puts each run of n's inline children (text and inline
// elements) in its own <p>, leaving block children as they are.
func wrapInlineRuns(n *html.Node) {
	var run *html.Node
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		if c.Type == html.ElementNode && blockElements[c.DataAtom] {
			run = nil
		} else if run != nil || !isEmpty(c) {
			if run == nil {
				run = &html.Node{Type: html.ElementNode, Data: "p", DataAtom: atom.P}
				n.InsertBefore(run, c)
			}
			n.RemoveChild(c)
			run.AppendChild(c)
		}
		c = next
	}
}

// hasInline reports whether n has inline text (loose or inside inline
// elements) directly inside it, which would run into its neighbors' text if
// unwrapped. Media alone doesn't count: a lone image needs no paragraph.
func hasInline(n *html.Node) bool {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if !(c.Type == html.ElementNode && blockElements[c.DataAtom]) && hasText(c) {
			return true
		}
	}
	return false
}

func hasText(n *html.Node) bool {
	if n.Type == html.TextNode {
		return strings.TrimSpace(n.Data) != ""
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if hasText(c) {
			return true
		}
	}
	return false
}

// isBlockEdge is true at a container's edge (nil) or next to a block element.
func isBlockEdge(n *html.Node) bool {
	return n == nil || n.Type == html.ElementNode && blockElements[n.DataAtom]
}

func unwrap(n *html.Node) {
	p := n.Parent
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		n.RemoveChild(c)
		p.InsertBefore(c, n)
		c = next
	}
	p.RemoveChild(n)
}

func inPre(n *html.Node) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.DataAtom == atom.Pre {
			return true
		}
	}
	return false
}

// PlainText returns the text of an HTML fragment with whitespace collapsed.
func PlainText(h string) string {
	doc, err := parseFragment(h)
	if err != nil {
		return ""
	}
	// Separate block elements so words from adjacent paragraphs don't run together.
	doc.Find("p, li, h1, h2, h3, h4, h5, h6, pre, blockquote, figcaption, td, th, br, div").Each(func(_ int, s *goquery.Selection) {
		s.AppendHtml(" ")
	})
	return CleanText(doc.Find("body").Text())
}

// Excerpt is the first ~n characters of text, cut at a word boundary (§6.8).
func Excerpt(text string, n int) string {
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	head := string([]rune(text)[:n])
	// Cut at a word only past halfway, so an early space can't leave a stub.
	if i := strings.LastIndexByte(head, ' '); i > 0 && utf8.RuneCountInString(head[:i]) > n/2 {
		return strings.TrimRight(head[:i], " ,;:.-") + "…"
	}
	return head + "…"
}
