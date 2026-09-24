// Package feed renders stored items as RSS 2.0 (§7.1) and checks feeds (§7.3).
package feed

import (
	"bytes"
	"encoding/xml"
	"mime"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

// Namespaces (§7.1).
const (
	NSContent = "http://purl.org/rss/1.0/modules/content/"
	NSAtom    = "http://www.w3.org/2005/Atom"
	NSDC      = "http://purl.org/dc/elements/1.1/"
	NSMedia   = "http://search.yahoo.com/mrss/"
	DocsURL   = "https://www.rssboard.org/rss-specification"
)

// Channel is the feed-level metadata.
type Channel struct {
	Title       string
	Link        string
	Description string
	Language    string
	Image       string
	TTL         int
	SelfURL     string    // public URL of this feed, for atom:link rel=self
	LastBuild   time.Time // last content change, not last run (§5.7)
	Generator   string
}

// Item is one rendered entry.
type Item struct {
	Title       string
	Link        string
	GUID        string
	PermaLink   bool
	Description string // plain text
	ContentHTML string
	Author      string
	Categories  []string
	Published   time.Time
	Updated     time.Time
	Image       string
	Enclosure   bool // also emit <enclosure> for Image (§6.9)
}

type rssDoc struct {
	XMLName   xml.Name   `xml:"rss"`
	Version   string     `xml:"version,attr"`
	NSContent string     `xml:"xmlns:content,attr"`
	NSAtom    string     `xml:"xmlns:atom,attr"`
	NSDC      string     `xml:"xmlns:dc,attr"`
	NSMedia   string     `xml:"xmlns:media,attr"`
	Channel   rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title         string    `xml:"title"`
	Link          string    `xml:"link"`
	Description   string    `xml:"description"`
	AtomLink      atomLink  `xml:"atom:link"`
	Language      string    `xml:"language,omitempty"`
	PubDate       string    `xml:"pubDate,omitempty"`
	LastBuildDate string    `xml:"lastBuildDate,omitempty"`
	Docs          string    `xml:"docs"`
	Generator     string    `xml:"generator,omitempty"`
	TTL           int       `xml:"ttl,omitempty"`
	Image         *rssImage `xml:"image"`
	Items         []rssItem `xml:"item"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
}

type rssImage struct {
	URL   string `xml:"url"`
	Title string `xml:"title"`
	Link  string `xml:"link"`
}

type rssItem struct {
	Title       string        `xml:"title"`
	Link        string        `xml:"link"`
	Description string        `xml:"description"`
	Content     cdata         `xml:"content:encoded"`
	Creator     string        `xml:"dc:creator,omitempty"`
	Categories  []string      `xml:"category"`
	PubDate     string        `xml:"pubDate"`
	GUID        rssGUID       `xml:"guid"`
	Enclosure   *rssEnclosure `xml:"enclosure"`
	Media       *mediaContent `xml:"media:content"`
}

// cdata is written as <![CDATA[...]]>; encoding/xml splits any "]]>" inside (§6.7).
type cdata struct {
	Text string `xml:",cdata"`
}

type rssGUID struct {
	IsPermaLink string `xml:"isPermaLink,attr"`
	Value       string `xml:",chardata"`
}

type rssEnclosure struct {
	URL    string `xml:"url,attr"`
	Length string `xml:"length,attr"`
	Type   string `xml:"type,attr"`
}

type mediaContent struct {
	URL    string `xml:"url,attr"`
	Medium string `xml:"medium,attr"`
}

// RSSDate formats a time for RSS: RFC 822 with a 4-digit year, in UTC (§5.6).
func RSSDate(t time.Time) string { return t.UTC().Format(time.RFC1123Z) }

// RSS renders an RSS 2.0 document. Items are written in the order given.
func RSS(ch Channel, items []Item) ([]byte, error) {
	c := rssChannel{
		Title:       clean(ch.Title),
		Link:        ch.Link,
		Description: clean(ch.Description),
		AtomLink:    atomLink{Href: ch.SelfURL, Rel: "self", Type: "application/rss+xml"},
		Language:    ch.Language,
		Docs:        DocsURL,
		Generator:   clean(ch.Generator),
		TTL:         ch.TTL,
	}
	if !ch.LastBuild.IsZero() {
		c.LastBuildDate = RSSDate(ch.LastBuild)
	}
	if ch.Image != "" {
		c.Image = &rssImage{URL: ch.Image, Title: c.Title, Link: ch.Link}
	}
	var newest time.Time
	for _, it := range items {
		if it.Published.After(newest) {
			newest = it.Published
		}
		ri := rssItem{
			Title:       clean(it.Title),
			Link:        it.Link,
			Description: clean(it.Description),
			Content:     cdata{clean(it.ContentHTML)},
			Creator:     clean(it.Author),
			PubDate:     RSSDate(it.Published),
			GUID:        rssGUID{IsPermaLink: "false", Value: it.GUID},
		}
		if it.PermaLink {
			ri.GUID.IsPermaLink = "true"
		}
		for _, cat := range it.Categories {
			ri.Categories = append(ri.Categories, clean(cat))
		}
		if it.Image != "" {
			ri.Media = &mediaContent{URL: it.Image, Medium: "image"}
			if it.Enclosure {
				// The length is unknown without a HEAD request; 0 is allowed with a warning.
				ri.Enclosure = &rssEnclosure{URL: it.Image, Length: "0", Type: imageType(it.Image)}
			}
		}
		c.Items = append(c.Items, ri)
	}
	if !newest.IsZero() {
		c.PubDate = RSSDate(newest)
	}

	doc := rssDoc{Version: "2.0", NSContent: NSContent, NSAtom: NSAtom, NSDC: NSDC, NSMedia: NSMedia, Channel: c}
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

func imageType(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	if t := mime.TypeByExtension(strings.ToLower(path.Ext(u))); strings.HasPrefix(t, "image/") {
		return t
	}
	return "image/jpeg"
}

// clean removes characters XML 1.0 does not allow (§6.7): control characters
// other than tab, newline and carriage return, surrogates, U+FFFE/U+FFFF and
// invalid UTF-8.
func clean(s string) string {
	valid := func(r rune) bool {
		return r == '\t' || r == '\n' || r == '\r' ||
			(r >= 0x20 && r <= 0xD7FF) || (r >= 0xE000 && r <= 0xFFFD) || (r >= 0x10000 && r <= 0x10FFFF)
	}
	ok := utf8.ValidString(s)
	for _, r := range s {
		if !ok || !valid(r) {
			return strings.Map(func(r rune) rune {
				if r == utf8.RuneError || !valid(r) {
					return -1
				}
				return r
			}, s)
		}
	}
	return s
}
