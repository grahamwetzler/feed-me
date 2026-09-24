package normalize

import (
	"net/url"
	"strings"
	"testing"
)

func body(t *testing.T, in string, o BodyOptions) string {
	t.Helper()
	if o.Base == nil {
		o.Base, _ = url.Parse("https://example.com/blog/post")
	}
	out, err := Body(in, o)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBody(t *testing.T) {
	tests := []struct {
		name, in, want string
		o              BodyOptions
	}{
		{
			name: "absolute URLs and srcset",
			in:   `<p><a href="../other">x</a> <img src="/a.png" srcset="/a.png 1x, b@2x.png 2x" alt="A"></p>`,
			want: `<p><a href="https://example.com/other">x</a> <img src="https://example.com/a.png" srcset="https://example.com/a.png 1x, https://example.com/blog/b@2x.png 2x" alt="A"/></p>`,
		},
		{
			name: "srcset URL with commas",
			in:   `<p><img src="/a.jpg" srcset="/i.jpg?crop=10,20 2x, /j.jpg,"></p>`,
			want: `<p><img src="https://example.com/a.jpg" srcset="https://example.com/i.jpg?crop=10,20 2x, https://example.com/j.jpg"/></p>`,
		},
		{
			name: "code lines marked by markup",
			in:   `<pre><div>first</div><div>second</div></pre><pre>a<br>b</pre>`,
			want: "<pre><code>first\nsecond</code></pre><pre><code>a\nb</code></pre>",
		},
		{
			name: "space between inline elements kept",
			in:   `<div><em>Hello</em> <strong>world</strong></div>`,
			want: `<p><em>Hello</em> <strong>world</strong></p>`,
		},
		{
			name: "divs of inline elements stay separate",
			in:   `<div><em>one</em></div><div><em>two</em></div>`,
			want: `<p><em>one</em></p><p><em>two</em></p>`,
		},
		{
			name: "lazy images promoted, loading dropped",
			in:   `<p><img src="data:image/gif;base64,R0lGOD" data-src="/real.jpg" data-srcset="/r.jpg 2x" loading="lazy" alt=""></p>`,
			want: `<p><img src="https://example.com/real.jpg" alt="" srcset="https://example.com/r.jpg 2x"/></p>`,
		},
		{
			name: "attributes and wrappers stripped",
			in:   `<div class="w-richtext" style="x"><figure style="max-width:2720pxpx" class="f"><div><img src="https://cdn.example/i.png" onload="evil()"></div><figcaption><em>Cap</em></figcaption></figure></div>`,
			want: `<figure><img src="https://cdn.example/i.png"/><figcaption><em>Cap</em></figcaption></figure>`,
		},
		{
			name: "webflow empty bindings removed",
			in:   `<p>Keep</p><div class="w-dyn-bind-empty"></div><p class="w-condition-invisible">Hidden</p>`,
			want: `<p>Keep</p>`,
		},
		{
			name: "youtube iframe becomes poster link",
			in:   `<figure class="w-richtext-figure-type-video"><div><iframe src="https://www.youtube.com/embed/tI1uzSJuSxc" title="Claude in Slack"></iframe></div></figure>`,
			want: `<figure><p><a href="https://www.youtube.com/watch?v=tI1uzSJuSxc"><img src="https://i.ytimg.com/vi/tI1uzSJuSxc/hqdefault.jpg" alt="Claude in Slack"/></a><br/><a href="https://www.youtube.com/watch?v=tI1uzSJuSxc">▶ Watch video: Claude in Slack</a></p></figure>`,
		},
		{
			name: "vimeo and other iframes become links",
			in:   `<iframe src="https://player.vimeo.com/video/12345?h=x"></iframe><iframe src="//widgets.example/embed" title="Chart"></iframe>`,
			want: `<p><a href="https://vimeo.com/12345">▶ Watch video</a></p><p><a href="https://widgets.example/embed">Open embedded content: Chart</a></p>`,
		},
		{
			name: "video replaced unless kept",
			in:   `<video poster="/p.jpg"><source src="/v.mp4"></video>`,
			want: `<p><a href="https://example.com/v.mp4"><img src="https://example.com/p.jpg" alt=""/></a><br/><a href="https://example.com/v.mp4">▶ Watch video</a></p>`,
		},
		{
			name: "code flattened, whitespace kept",
			in:   "<pre class=\"prism\"><code><div class=\"token-line\"><span class=\"token keyword\">SELECT</span>  1\n</div><div class=\"token-line\">  FROM t</div></code></pre>",
			want: "<pre><code>SELECT  1\n  FROM t</code></pre>",
		},
		{
			name: "whitespace collapsed outside pre, empty nodes dropped",
			in:   "<p>a \n\n  b</p>\n\n<p> </p><ul>\n <li>x</li>\n</ul><p><a href=\"/x\"></a></p>",
			want: "<p>a b</p><ul><li>x</li></ul>",
		},
		{
			name: "text-bearing div becomes a paragraph",
			in:   `<div>one</div><div>two</div>`,
			want: `<p>one</p><p>two</p>`,
		},
		{
			name: "div mixing text and blocks never nests a block in a p",
			in:   `<div>Sunday, 8:00pm<h2>Install</h2>Connect <em>tools</em>.<ul><li>x</li></ul></div>`,
			want: `<p>Sunday, 8:00pm</p><h2>Install</h2><p>Connect <em>tools</em>.</p><ul><li>x</li></ul>`,
		},
		{
			name: "in-page anchors point at the live page",
			in:   `<p><a href="#sb-sun">Install</a></p>`,
			want: `<p><a href="https://example.com/blog/post#sb-sun">Install</a></p>`,
		},
		{
			name: "dangerous content removed",
			in:   `<p onclick="x()">t<script>alert(1)</script><a href="javascript:alert(1)">j</a><button>Copy</button><svg><text>svg</text></svg></p>`,
			want: `<p>tj</p>`,
		},
		{
			name: "invisible characters stripped from text",
			in:   "<p>Title\u200b\u200c\ufeff\u200d here \U0001F469\u200d\U0001F4BB</p>",
			want: "<p>Title here \U0001F469\u200d\U0001F4BB</p>",
		},
		{
			name: "lead image prepended when body has none",
			in:   `<p>text</p>`,
			o:    BodyOptions{LeadImage: "https://cdn.example/lead.png"},
			want: `<p><img src="https://cdn.example/lead.png" alt=""/></p><p>text</p>`,
		},
		{
			name: "lead image skipped when body has one",
			in:   `<p><img src="https://cdn.example/a.png"></p>`,
			o:    BodyOptions{LeadImage: "https://cdn.example/lead.png"},
			want: `<p><img src="https://cdn.example/a.png"/></p>`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := body(t, tt.in, tt.o); got != tt.want {
				t.Errorf("\n got: %s\nwant: %s", got, tt.want)
			}
		})
	}
}

func TestPlainTextAndExcerpt(t *testing.T) {
	txt := PlainText("<h2>Intro</h2><p>First para.</p><ul><li>one</li><li>two</li></ul>")
	if txt != "Intro First para. one two" {
		t.Errorf("PlainText = %q", txt)
	}
	long := strings.Repeat("word ", 100)
	ex := Excerpt(long, 50)
	if !strings.HasSuffix(ex, "…") || len([]rune(ex)) > 51 || strings.Contains(ex, "wor…") {
		t.Errorf("Excerpt = %q", ex)
	}
	if Excerpt("short", 50) != "short" {
		t.Error("short text changed")
	}
}
