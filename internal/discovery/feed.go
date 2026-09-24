package discovery

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/net/html/charset"

	"rss-er/internal/fetch"
	"rss-er/internal/normalize"
)

const (
	nsAtom = "http://www.w3.org/2005/Atom"
	nsDC   = "http://purl.org/dc/elements/1.1/"
)

// xmlLink covers both RSS <link>text</link> and Atom <link href rel/>.
type xmlLink struct {
	XMLName xml.Name
	Href    string `xml:"href,attr"`
	Rel     string `xml:"rel,attr"`
	Base    string `xml:"http://www.w3.org/XML/1998/namespace base,attr"`
	Text    string `xml:",chardata"`
}

// atomText is an Atom text construct (RFC 4287 §3.1), whose type says how to
// read it: plain text, escaped HTML, or inline XHTML.
type atomText struct {
	Type  string `xml:"type,attr"`
	Text  string `xml:",chardata"`
	Inner string `xml:",innerxml"`
}

func (t atomText) String() string {
	switch t.Type {
	case "html":
		return normalize.PlainText(t.Text)
	case "xhtml":
		return normalize.PlainText(t.Inner)
	}
	return t.Text
}

type rssItem struct {
	Title       string    `xml:"title"`
	Links       []xmlLink `xml:"link"`
	Description string    `xml:"description"`
	PubDate     string    `xml:"pubDate"`
	GUID        struct {
		Value       string `xml:",chardata"`
		IsPermaLink string `xml:"isPermaLink,attr"`
	} `xml:"guid"`
	Author     string   `xml:"author"`
	Creator    string   `xml:"http://purl.org/dc/elements/1.1/ creator"`
	DCDate     string   `xml:"http://purl.org/dc/elements/1.1/ date"`
	Updated    string   `xml:"http://www.w3.org/2005/Atom updated"`
	Categories []string `xml:"category"`
}

type atomEntry struct {
	Base      string    `xml:"http://www.w3.org/XML/1998/namespace base,attr"`
	Title     atomText  `xml:"title"`
	Links     []xmlLink `xml:"link"`
	Summary   atomText  `xml:"summary"`
	Published string    `xml:"published"`
	Updated   string    `xml:"updated"`
	Authors   []struct {
		Name string `xml:"name"`
	} `xml:"author"`
	Categories []struct {
		Term string `xml:"term,attr"`
	} `xml:"category"`
}

type feedDoc struct {
	XMLName xml.Name
	Base    string `xml:"http://www.w3.org/XML/1998/namespace base,attr"` // Atom xml:base
	Channel struct {
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
	Items   []rssItem   `xml:"item"`  // RSS 1.0 (RDF) puts items at the root
	Entries []atomEntry `xml:"entry"` // Atom
}

// fromFeed reads an RSS 2.0, RSS 1.0 or Atom feed. Item metadata becomes hints
// (published, updated, title, description, author, category).
func fromFeed(ctx context.Context, f fetch.Fetcher, feedURL string) ([]Candidate, error) {
	resp, err := f.Fetch(ctx, fetch.Request{URL: feedURL})
	if err != nil {
		return nil, err
	}
	base, _ := url.Parse(resp.URL)
	return parseFeed(resp.Body, base)
}

func parseFeed(body []byte, base *url.URL) ([]Candidate, error) {
	dec := xml.NewDecoder(bytes.NewReader(body))
	dec.CharsetReader = charset.NewReaderLabel
	dec.Strict = false // real-world feeds contain stray entities
	var doc feedDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parsing feed XML: %w", err)
	}

	var out []Candidate
	switch doc.XMLName.Local {
	case "rss", "RDF":
		items := append(doc.Channel.Items, doc.Items...)
		for _, it := range items {
			link := ""
			for _, l := range it.Links {
				if l.XMLName.Space != nsAtom && strings.TrimSpace(l.Text) != "" {
					link = l.Text
					break
				}
			}
			if link == "" && it.GUID.IsPermaLink != "false" {
				link = it.GUID.Value
			}
			if strings.TrimSpace(link) == "" {
				continue // resolving "" would yield the feed's own URL
			}
			h := hints{}
			h.set("title", it.Title)
			h.set("description", it.Description)
			h.set("published", first(it.PubDate, it.DCDate))
			h.set("updated", it.Updated)
			h.set("author", first(it.Creator, it.Author))
			h.set("category", strings.Join(it.Categories, "\n"))
			out = append(out, Candidate{URL: resolve(base, link), Hints: h})
		}
	case "feed":
		feedBase := withBase(base, doc.Base)
		for _, e := range doc.Entries {
			link := ""
			entryBase := withBase(feedBase, e.Base)
			linkBase := entryBase
			for _, l := range e.Links {
				if (l.Rel == "" || l.Rel == "alternate") && strings.TrimSpace(l.Href) != "" {
					link, linkBase = l.Href, withBase(entryBase, l.Base)
					break
				}
			}
			if strings.TrimSpace(link) == "" {
				continue
			}
			h := hints{}
			h.set("title", e.Title.String())
			h.set("description", e.Summary.String())
			h.set("published", first(e.Published, e.Updated))
			h.set("updated", e.Updated)
			if len(e.Authors) > 0 {
				h.set("author", e.Authors[0].Name)
			}
			var cats []string
			for _, c := range e.Categories {
				cats = append(cats, c.Term)
			}
			h.set("category", strings.Join(cats, "\n"))
			out = append(out, Candidate{URL: resolve(linkBase, link), Hints: h})
		}
	default:
		return nil, fmt.Errorf("root element is <%s>, want <rss>, <rdf:RDF> or <feed>", doc.XMLName.Local)
	}
	return out, nil
}

// withBase applies an xml:base attribute, itself relative to the enclosing base.
func withBase(base *url.URL, xmlBase string) *url.URL {
	if strings.TrimSpace(xmlBase) == "" {
		return base
	}
	b, err := url.Parse(strings.TrimSpace(xmlBase))
	if err != nil {
		return base
	}
	return base.ResolveReference(b)
}

type hints map[string]string

// set stores a cleaned, non-empty hint. Categories keep their newline separators.
func (h hints) set(k, v string) {
	if k == "category" {
		var parts []string
		for _, p := range strings.Split(v, "\n") {
			if p = normalize.CleanText(p); p != "" {
				parts = append(parts, p)
			}
		}
		v = strings.Join(parts, "\n")
	} else {
		v = normalize.CleanText(v)
	}
	if v != "" {
		h[k] = v
	}
}

func first(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
