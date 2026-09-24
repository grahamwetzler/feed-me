package feed

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

func sample() (Channel, []Item) {
	ch := Channel{
		Title: "Claude Blog", Link: "https://claude.com/blog", Description: "News & guides",
		Language: "en-us", TTL: 60, SelfURL: "https://rss.example.com/feeds/claude-blog.xml",
		LastBuild: time.Date(2026, 9, 24, 16, 0, 0, 0, time.UTC), Generator: "rss-er/test",
	}
	items := []Item{
		{
			Title: "Second <post>", Link: "https://claude.com/blog/b", GUID: "https://claude.com/blog/b", PermaLink: true,
			Description: "Summary & more", ContentHTML: `<p>Body with ]]> inside and a bad char` + "\x01" + `</p>`,
			Author: "Anthropic", Categories: []string{"News"}, Image: "https://cdn.example/b.png?w=1",
			Published: time.Date(2026, 9, 17, 7, 0, 0, 0, time.FixedZone("PDT", -7*3600)),
		},
		{
			Title: "First", Link: "https://claude.com/blog/a", GUID: "5f0c7d0e-1111-5222-8333-444455556666",
			Description: "S", ContentHTML: "<p>a</p>", Published: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	return ch, items
}

func TestRSS(t *testing.T) {
	ch, items := sample()
	out, err := RSS(ch, items)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/" xmlns:atom="http://www.w3.org/2005/Atom" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:media="http://search.yahoo.com/mrss/">`,
		`<atom:link href="https://rss.example.com/feeds/claude-blog.xml" rel="self" type="application/rss+xml"></atom:link>`,
		`<pubDate>Thu, 17 Sep 2026 14:00:00 +0000</pubDate>`, // newest item, in UTC
		`<lastBuildDate>Thu, 24 Sep 2026 16:00:00 +0000</lastBuildDate>`,
		`<title>Second &lt;post&gt;</title>`,
		`<content:encoded><![CDATA[<p>Body with ]]]]><![CDATA[> inside and a bad char</p>]]></content:encoded>`,
		`<dc:creator>Anthropic</dc:creator>`,
		`<guid isPermaLink="true">https://claude.com/blog/b</guid>`,
		`<guid isPermaLink="false">5f0c7d0e-1111-5222-8333-444455556666</guid>`,
		`<media:content url="https://cdn.example/b.png?w=1" medium="image"></media:content>`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s\n%s", want, s)
		}
	}
	if strings.Contains(s, "\x01") || strings.Contains(s, "<enclosure") {
		t.Error("control character or unrequested enclosure in output")
	}
	if probs := Check(out); len(probs) > 0 {
		t.Errorf("Check: %v", probs)
	}
	var v struct {
		Items []struct {
			Content string `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
		} `xml:"channel>item"`
	}
	if err := xml.Unmarshal(out, &v); err != nil || v.Items[0].Content != "<p>Body with ]]> inside and a bad char</p>" {
		t.Errorf("content does not round-trip: %v %q", err, v.Items)
	}
}

func TestEnclosure(t *testing.T) {
	ch, items := sample()
	items[0].Enclosure = true
	out, _ := RSS(ch, items)
	if !strings.Contains(string(out), `<enclosure url="https://cdn.example/b.png?w=1" length="0" type="image/png"></enclosure>`) {
		t.Errorf("no enclosure:\n%s", out)
	}
}

func TestCheckFindsProblems(t *testing.T) {
	ch, items := sample()
	items[0], items[1] = items[1], items[0] // oldest first
	items[1].GUID = items[0].GUID
	items[1].PermaLink = false
	ch.SelfURL = ""
	out, _ := RSS(ch, items)
	probs := strings.Join(Check(out), "\n")
	for _, want := range []string{"not sorted newest first", "duplicate guid", `atom:link rel=self: "" is not an absolute`} {
		if !strings.Contains(probs, want) {
			t.Errorf("missing %q in:\n%s", want, probs)
		}
	}
	if p := Check([]byte("<rss><channel>")); len(p) == 0 || !strings.Contains(p[0], "not well-formed") {
		t.Errorf("malformed: %v", p)
	}
}
