# Feed Me! — Planning Document

*Drafted 2026-09-23 · decisions from review folded in the same day (see §12)*

A Go service that turns websites with no feed, or only a poor one, into spec-compliant, full-text RSS feeds. Each site gets a declarative config. The first two targets are the Claude blog (`https://claude.com/blog`) and the SELECT blog (`https://select.dev/posts`).

---

## 1. Goals

1. **Config-driven.** You can add a new site by writing a YAML file, with no Go code. Code changes should only be needed for new extraction *capabilities*, not for new sites.
2. **Spec-compliant output.** Feeds pass the W3C Feed Validator with no errors or warnings. RSS 2.0 is the primary format and Atom 1.0 is secondary.
3. **Full text.** Every item has the whole article body as sanitized HTML with absolute URLs, not a teaser.
4. **Accurate, stable timestamps.** Use the site's own publish and modified dates when it has them. Once an item's timestamp is emitted, it never changes. This keeps feed readers from re-sorting or re-notifying.
5. **Polite and cheap.** Respect `robots.txt`, use conditional GETs, rate-limit per host, and fetch only the article pages that are new or changed.
6. **Easy to run.** One static binary shipped as a Docker image. It runs as a long-lived container behind the user's reverse proxy, with a SQLite database on a mounted volume (§8.1).

### Non-goals (for v1)

- Rendering JavaScript-only sites with a headless browser. The design leaves room for it (§6.5), but it won't be built until a target needs it.
- A web UI for writing configs.
- Authenticated or paywalled content.
- Scraping with an LLM. Extraction stays deterministic and testable.

---

## 2. Reconnaissance

### 2.1 The Claude blog

I inspected the site on 2026-09-23:

| Aspect | Finding | Implication |
|---|---|---|
| Platform | Webflow (`data-wf-site`, `w-richtext`, `w-dyn-*` classes). Server-rendered HTML. | Plain HTTP fetch is enough. No JS rendering needed. |
| Index page | `/blog` lists about 23 posts. Pagination uses Webflow's hashed query params (`?d7430fcd_page=2`, `?b7eea976_page=2`) and there are two separate collection lists on the page. | Paginating the index is fragile because the hash IDs can change on a redesign. Prefer the sitemap for discovery. |
| Sitemap | `https://claude.com/sitemap.xml` contains **243** `https://claude.com/blog/<slug>` URLs, plus localized `/ja/blog`, `/de/blog` and so on. | Use the sitemap as the discovery source and filter with the regex `^https://claude\.com/blog/[^/?#]+$`. |
| Sitemap `<lastmod>` | Nearly every entry is `2026-09-22T15:4x–15:5x Z`, which looks like a site-wide republish timestamp. | Don't use it as a publish date. At most, treat it as a hint that a page should be re-fetched. |
| Post dates | JSON-LD `BlogPosting` has `"datePublished": "Sep 17, 2026"` and `"dateModified": "Sep 17, 2026"`. These are **not ISO 8601** and have **no time of day**. The visible page also shows "September 17, 2026" under a "Date" label. | The date parser needs configurable layouts (`Jan 2, 2006`). Date-only values need a time policy (§5). |
| Title / summary / image | JSON-LD `headline`, `description` and `image`. Also `og:*` meta tags. JSON-LD strings contain HTML entities (`&#39;`), and `image` is empty on some posts (as is `og:image`). | Use JSON-LD first and fall back to meta tags. The extractor HTML-unescapes JSON-LD strings. |
| Author | Not in JSON-LD, and I didn't confirm a byline in the markup. | Author is optional. Use the channel-level default `Anthropic`. |
| Body | `div.u-rich-text-blog.w-richtext` inside `div.blog_post_content_wrap`. There's also an empty, hidden CMS slot. *Corrected during M1 (2026-09-24):* the slot carries `w-condition-invisible` **either on itself or on a wrapper div**, and some posts (the `is-cc` layout) split the article across **two** rich-text blocks, with a carousel between them. | Selector: `.blog_post_content_wrap > .u-rich-text-blog.w-richtext:not(.w-condition-invisible)`, and `content` joins **every** match in document order (§4.2). The body contains `figure.w-richtext-figure-type-image` and `-type-video` (embeds). |
| Images | Hosted on `cdn.prod.website-files.com`. They may use `srcset` or `loading="lazy"`. | Rewrite and keep `srcset`, and make URLs absolute. |

**Proposed config for this site** is in §4.3.

### 2.2 The SELECT blog (select.dev)

I inspected the site on 2026-09-23. It's different from the Claude blog: it **has** a feed, but the feed is poor. That makes it a useful second target, because it shows Feed Me!'s value beyond sites with no feed at all.

| Aspect | Finding | Implication |
|---|---|---|
| Blog location | `/posts`. `/blog` returns 404. The site also has a `/changelog`, which is out of scope. | The feed covers `https://select.dev/posts/<slug>` only. |
| Platform | Next.js (App Router, `self.__next_f` flight data), with content from the Sanity CMS. Pages are server-rendered. | Plain HTTP fetch is enough. No JS rendering needed. |
| Existing feeds | The `<head>` advertises `/posts/rss.xml` (blog, **103 items**, the full history), `/changelog/rss.xml` and `/rss.xml` (everything, 288 items). | Use `/posts/rss.xml` for **discovery** only (new `feed` strategy, §3.1). |
| Feed problems | **Summary only:** `<description>` is a one-line teaser, and there's no `content:encoded`. **Dirty titles:** each `<title>` ends with about 1,100 invisible zero-width characters (U+200B/C/D and U+FEFF). This looks like Sanity/Vercel "stega" metadata, which encodes the CMS source into text. Descriptions have the same problem. | Don't reuse their titles or descriptions. Take them from the page's meta tags, which are clean. Also strip invisible characters from all text as a safety net (§6). |
| Feed dates | `pubDate` has real times, for example `Mon, 21 Sep 2026 15:12:00 GMT`. These match `publishedAt: 2026-09-21T15:12:00.000Z` in the page's CMS data. | Use the feed's `pubDate` (the `listing` source) as the publish date. It's exact, so the date-only policy in §5 isn't needed here. |
| Sitemap | `sitemap.xml` is an index pointing to `sitemap-0.xml`, which has 184 changelog URLs and **no** `/posts/` URLs. | The sitemap is useless for discovery on this site. |
| `robots.txt` | Disallows only `*.json` and `*.js`. | Fetching post pages is allowed. |
| Title / summary / image / author | Clean `og:title`, `og:description` and `og:image` (hosted on `cdn.sanity.io`), plus `article:author` (for example "Niall Woodward"). There's no JSON-LD. | Use meta tags. This site gets real per-post authors. |
| Visible date | There's no `<time>` element. The byline paragraph shows "Monday, September 21, 2026", with only Tailwind utility classes on it. | This is only a fallback source, with layout `Monday, January 2, 2006`. |
| Body | The article is the single `div` whose class list includes the Tailwind token `prose-p:!my-0`. I checked this on 3 posts: exactly one match each, holding the full text (10–17k characters), code blocks (`pre`), images and headings. The table of contents is a separate `nav` outside the div, so it isn't included. There are no `<article>` or `<main>` elements. | Selector: `div[class~="prose-p:!my-0"]`. This is **fragile**, because a redesign will change Tailwind classes. The extraction-health check (§8) matters most here. |
| Images | `img` tags with `src` and `srcset` pointing at `cdn.sanity.io`, with `loading="lazy"`. | These are already absolute. Keep `srcset` and drop `loading`. |

**Proposed config for this site** is in §4.4.

---

## 3. Architecture

```
            ┌─────────────┐
 config/*.yaml ──▶│ Config load │──▶ validated SiteConfig[]
            └─────────────┘
                   │
    ┌──────────────▼──────────────┐
    │ Scheduler (per-site interval)│
    └──────────────┬──────────────┘
                   ▼
 ┌──────────┐  ┌───────────┐  ┌───────────┐  ┌────────────┐  ┌──────────┐
 │Discovery │─▶│  Fetcher  │─▶│ Extractor │─▶│ Normalizer │─▶│  Store   │
 │(sitemap/ │  │(HTTP cache│  │(JSON-LD,  │  │(sanitize,  │  │ (SQLite) │
 │ index/   │  │ ETag, rate│  │ meta, CSS │  │ abs URLs,  │  │ items +  │
 │ feed)    │  │ limit,    │  │ selectors)│  │ dates, tz) │  │ state    │
 └──────────┘  │ robots)   │  └───────────┘  └────────────┘  └────┬─────┘
               └───────────┘                                      │
                                                          ┌───────▼───────┐
                                                          │ Feed renderer │
                                                          │ RSS 2.0 / Atom│
                                                          └───────┬───────┘
                                                 static files ◀──┴──▶ HTTP server
```

### 3.1 Pipeline stages

1. **Discovery** produces candidate article URLs, each with optional hints (a listing date, a title). Strategies are pluggable:
   - `sitemap`: parses `sitemap.xml` and sitemap indexes (including gzipped ones) and filters with a URL regex.
   - `index`: fetches listing page(s), selects links with a CSS selector, and follows a "next" selector up to `max_pages`.
   - `feed`: parses an existing RSS or Atom feed and uses it as a list of article URLs. Each item's `pubDate`/`published`, `updated`, `title`, `description`, `author` and `category` values are captured as hints, which `listing:<field>` sources can use. This is for sites like select.dev whose feed is summary-only or broken. The article pages are still fetched for full text.
   - `links`: a static list of URLs, mostly for testing.
   - More than one strategy can run, and the results are unioned and deduplicated by canonical URL.
2. **Fetcher** is a shared `http.Client` wrapper that provides:
   - Per-host token-bucket rate limiting (default 1 req/s, configurable).
   - `robots.txt` checks, cached per host.
   - Conditional GET using stored `ETag`/`Last-Modified`. A `304` short-circuits extraction.
   - Retries with jittered backoff on 429 and 5xx, honoring `Retry-After`.
   - An honest User-Agent: `feed-me/<version> (+<contact URL>)`.
   - Response size cap and timeouts.
3. **Extractor** works out each field from an ordered list of *sources*. The first source that returns a non-empty value wins:
   - `jsonld:<path>`: for example `jsonld:BlogPosting.datePublished`. It handles `@graph` arrays and multiple scripts.
   - `meta:<name|property>`: for example `meta:article:published_time`.
   - `css:<selector>` with an optional `attr`. Without `attr`, it takes the text for scalar fields and the inner HTML for `content`.
   - `time:<selector>`: reads `<time datetime>`.
   - `url:<regex>`: pulls a value out of the URL, such as `/2026/09/17/`.
   - `listing:<field>`: uses a hint captured during discovery, for example `listing:published` from a `feed` item's `pubDate`.
4. **Normalizer** takes the raw content HTML and prepares it for the feed (§6). It also parses dates (§5).
5. **Store** is SQLite, using the pure-Go `modernc.org/sqlite` driver so there's no CGO and cross-compiling stays easy. It persists items, HTTP cache validators, and timestamps assigned on first sight.
6. **Renderer** builds RSS 2.0 (and optionally Atom) from the N most recent stored items, sorted by publish date descending.

### 3.2 Incremental strategy

- **Backfill (first run):** discover everything, then fetch every article at the rate limit. For the Claude blog that's 243 pages, about 4 minutes at 1 req/s, and it only happens once. The sitemap has no dates, so fetching each page is the only reliable way to find the 50 newest. If discovery already supplies dates (a `feed` source, as with select.dev), feed-me fetches only the newest `max_items` pages instead. Every item stays in SQLite afterward (a few MB at most), so old posts are never fetched again. The feed only ever shows the newest `max_items` (50).
- **Steady state:** run discovery on each tick and fetch only:
  - URLs not yet in the store.
  - Items published within the last `refresh_window` (default 14 days), to pick up edits. These use conditional GETs, so they're cheap when nothing changed.
  - Anything whose sitemap `lastmod` moved forward, if `trust_lastmod: true`. The default is false.
- An item that disappears from discovery is **kept**. Feeds shouldn't lose history because of a sitemap hiccup. The item is marked `missing_since` and can be pruned after a configurable age.

---

## 4. Configuration

### 4.1 Files

- `feed-me.yaml` holds global settings: store path, output directory, listen address, default rate limits and User-Agent.
- `sites/<id>.yaml` holds one file per site. The `id` becomes the feed path, for example `/feeds/claude-blog.xml`.

YAML is parsed with `gopkg.in/yaml.v3` using strict decoding, so unknown keys are errors. That catches typos early. The repo also ships a JSON Schema for editor autocompletion.

### 4.2 Site schema (v1)

```yaml
id: string                  # required, [a-z0-9-]+
version: 1                  # schema version, for future migrations

channel:
  title: string             # required
  link: url                 # required: the human-facing site URL
  description: string       # required (RSS requires <description>)
  language: en-us           # optional
  image: url                # optional channel image
  author: string            # default author when items lack one
  ttl: 60                   # minutes; emitted as <ttl>

schedule:
  interval: 1h
  refresh_window: 336h      # re-check recently published items for edits
  max_items: 50             # items in the rendered feed
  timezone: America/Los_Angeles   # used for date-only / zone-less timestamps

fetch:
  rate: 1/s
  timeout: 20s
  headers: {}               # extra request headers if a site needs them
  respect_robots: true

discovery:
  - type: sitemap | index | feed | links
    url: url
    include: [regex, ...]   # URL must match at least one
    exclude: [regex, ...]
    # index-only:
    link_selector: css
    next_selector: css
    max_pages: 5

item:
  canonical: [source, ...]  # default: [meta:og:url, css:link[rel=canonical]@href, request URL]
  title:     [source, ...]
  summary:   [source, ...]
  author:    [source, ...]
  image:     [source, ...]
  categories: [source, ...] # multi-valued
  published:
    sources: [source, ...]
    layouts: [Go time layouts, ...]    # tried in order after RFC3339
  updated:
    sources: [source, ...]
    layouts: [...]
  content:
    selector: css           # required; the inner HTML of every match, in document order, becomes the body
    exclude: [css, ...]     # nodes removed before serialization
    keep_first_image: true  # hoist the lead image if the body lacks one
```

A source's string form is `kind:expr[@attr]`, for example `css:article img@src`. Validation happens at load time. Every CSS selector must compile with `cascadia`, every regex must compile, every Go time layout must round-trip, and every required field must be present. The binary exits non-zero with file:line errors.

### 4.3 `sites/claude-blog.yaml`

```yaml
id: claude-blog
version: 1

channel:
  title: Claude Blog
  link: https://claude.com/blog
  description: Product news, customer stories, and guides from the Claude team at Anthropic.
  language: en-us
  author: Anthropic
  ttl: 60

schedule:
  interval: 1h
  max_items: 50
  timezone: America/Los_Angeles

discovery:
  - type: sitemap
    url: https://claude.com/sitemap.xml
    include: ['^https://claude\.com/blog/[^/?#]+$']   # English only: excludes /ja/blog, /de/blog, etc.

item:
  title:   [jsonld:BlogPosting.headline, meta:og:title, css:h1]
  summary: [jsonld:BlogPosting.description, meta:description]
  image:   [jsonld:BlogPosting.image, meta:og:image]
  published:
    sources: [jsonld:BlogPosting.datePublished]
    layouts: ['Jan 2, 2006', 'January 2, 2006']
  updated:
    sources: [jsonld:BlogPosting.dateModified]
    layouts: ['Jan 2, 2006', 'January 2, 2006']
  content:
    selector: '.blog_post_content_wrap > .u-rich-text-blog.w-richtext:not(.w-condition-invisible)'
    exclude: ['script', 'style', 'noscript']
```

### 4.4 `sites/select-dev.yaml`

```yaml
id: select-dev
version: 1

channel:
  title: SELECT Blog
  link: https://select.dev/posts
  description: Articles on Snowflake and Databricks cost optimization, performance, and data engineering from the SELECT team.
  language: en-us
  author: SELECT
  ttl: 60

schedule:
  interval: 1h
  max_items: 50
  timezone: UTC            # feed dates are already exact and in GMT

discovery:
  - type: feed
    url: https://select.dev/posts/rss.xml
    include: ['^https://select\.dev/posts/[^/?#]+$']

item:
  # Their feed's titles and descriptions carry invisible stega characters, so
  # prefer the page's clean meta tags. listing:* comes last, as a fallback only.
  title:   [meta:og:title, listing:title]
  summary: [meta:og:description, meta:description, listing:description]
  author:  [meta:article:author]
  image:   [meta:og:image]
  published:
    # Exact timestamps come from their feed. The visible byline is date-only.
    sources: [listing:published, 'css:p.uppercase > span:last-child']
    layouts: ['Monday, January 2, 2006']
  content:
    selector: 'div[class~="prose-p:!my-0"]'
    exclude: ['script', 'style', 'noscript', 'button']
```

Discovery pulls all 103 posts from their feed. On the first run, Feed Me! fetches the 50 newest pages (their feed already gives dates, so it doesn't need to fetch everything to find the newest). About a minute at 1 req/s.

---

## 5. Timestamps

Getting dates right matters most for how a feed feels, so the rules are explicit.

1. **Parse order:** RFC 3339 / ISO 8601, then RFC 1123/822, then each configured layout. A value with no zone is interpreted in `schedule.timezone`.
2. **Date-only values** such as the Claude blog's `Sep 17, 2026` need a time of day. The policy is:
   - If we first saw the item **on that same calendar date** in the site's timezone, use the **first-seen time**. New posts get a realistic time and sort correctly relative to each other.
   - Otherwise, as during backfill, use **00:00 local**. Items from the same day are then tie-broken by discovery order, stored as a sub-second offset so the order is deterministic.
3. **Freeze on first emit.** The computed `published` value is stored and never recomputed, even if the policy or source changes later. The only exception is a real change to the source date: if the site's own `datePublished` changes to a different *day*, the stored value is updated and logged.
4. **Updated:** If `dateModified` is later than `published`, emit it as `<atom:updated>` in Atom and track it internally. RSS 2.0 has no per-item updated field, so it isn't emitted there. The `<guid>` does **not** change on edits, because that would duplicate items in readers.
5. **Fallback:** If no publish date can be extracted at all, use the first-seen time and log a warning. The item is never dropped for lacking a date. The `feed-me check` command (§8) surfaces these.
6. **Serialization:**
   - RSS uses RFC 822 with a 4-digit year and numeric zone: `Mon, 02 Jan 2006 15:04:05 -0700`. Go's `time.RFC1123Z` produces exactly this. Timestamps are written in UTC (`+0000`) for consistency.
   - Atom uses RFC 3339.
7. **Channel dates:** `<lastBuildDate>` is the time of the last *content change*, not the last run. That avoids pointless churn. `<pubDate>` is the newest item's publish date.

---

## 6. Full-text content normalization

The goal is body HTML that renders well in every reader and can't be used for XSS.

1. **Select and prune.** Take the inner HTML of `content.selector` and remove the `content.exclude` nodes.
2. **Absolutize URLs.** Resolve `href`, `src`, `srcset` (each candidate), `poster` and `data-src` against the article URL, honoring `<base href>`.
3. **Fix lazy loading.** Promote `data-src` and `data-srcset` to `src` and `srcset` when `src` is missing or a placeholder. Drop `loading` attributes.
4. **Embeds.** Readers strip `<iframe>` and `<video>` inconsistently. Replace YouTube and Vimeo iframes, and `<video>` elements, with a linked poster image plus a text link ("▶ Watch video"). Keep `<video>` too if `keep_video: true`.
5. **Sanitize.** Use `bluemonday` with a UGC-like policy that allows headings, lists, tables, `figure`/`figcaption`, `pre`/`code`, `img` (`src`, `srcset`, `alt`; *`width`/`height` dropped in M2: Next.js emits placeholder dimensions such as 2000×2000 for a 2976×1892 image, which distort images without the site's CSS*) and `a` (`href`, `title`). It strips `class`, `style`, `id` and event handlers.
6. **Tidy.** Remove empty wrapper `div`s, collapse whitespace outside `<pre>`, and drop Webflow-specific empty nodes such as `w-dyn-bind-empty`.
6a. **Strip invisible characters.** This applies to every text field (title, summary, author, categories) and to text nodes in the body. Remove U+200B–U+200D, U+2060–U+2064 and U+FEFF when they appear in runs of 2 or more, plus any lone U+FEFF. A lone U+200D (zero-width joiner) between emoji is kept, because emoji sequences such as 👩‍💻 need it. This catches CMS "stega" encoding like select.dev's (§2.2), which otherwise bloats titles and can break reader dedup and search.
7. **Encode.** The body goes into `<content:encoded>` inside CDATA. Any literal `]]>` in the content is split as `]]]]><![CDATA[>`. Invalid XML characters (control characters other than `\t`, `\n` and `\r`) are removed.
8. **Summary.** `<description>` gets the configured summary. If there isn't one, it gets the first ~300 characters of the body's plain text, entity-escaped.
9. **Lead image.** If `image` was extracted and the body has no images, prepend the image. Also emit an `<enclosure>`, which requires a `length` value. That comes from a HEAD request, or is `0` if unknown, which the validator accepts with a warning. Enclosures are off by default and can be enabled per site; `<media:content>` is used instead.

### 6.5 Future: JS-rendered sites

The fetcher is an interface: `Fetch(ctx, url) (*Response, error)`. A later `fetch.renderer: chromedp` option could add a headless-Chrome implementation behind a build tag, so the default binary stays small.

---

## 7. Feed output and spec compliance

### 7.1 RSS 2.0 (primary): `/feeds/<id>.xml`

Namespaces:
- `content` for `http://purl.org/rss/1.0/modules/content/`
- `atom` for `http://www.w3.org/2005/Atom`
- `dc` for `http://purl.org/dc/elements/1.1/`
- `media` for `http://search.yahoo.com/mrss/`

Channel elements:
- Required: `title`, `link`, `description`.
- Also emitted: `language`, `lastBuildDate`, `pubDate`, `ttl`, `generator`, `image` (when configured), and `docs` (`https://www.rssboard.org/rss-specification`).
- Always emitted: `<atom:link href="<public feed URL>" rel="self" type="application/rss+xml"/>`. The validator warns without it, so a `public_base_url` global setting is required.

Item elements:
- `title`, `link` (canonical URL), `description`, `content:encoded` and `pubDate`.
- `guid isPermaLink="true"` set to the canonical URL. If the canonical URL could ever change for a site, a per-site `guid: stable-hash` option switches to a UUIDv5 of the first-seen URL with `isPermaLink="false"`.
- `dc:creator` for the author name. RSS `<author>` requires an email address, so it isn't used.
- `category` elements and `media:content` for the lead image.

Other output details:
- Encoding is UTF-8 with an XML declaration.
- The HTTP response uses `Content-Type: application/rss+xml; charset=utf-8`.
- Serialization uses hand-written `encoding/xml` structs, not a feed library. Libraries like `gorilla/feeds` hide control over namespaces, `guid` attributes and `atom:link`, and the spec is small enough that owning the structs is simpler.

### 7.2 Atom 1.0 (secondary): `/feeds/<id>.atom`

- Includes `id`, `title`, `updated`, `author`, `link rel=self` and `link rel=alternate`.
- Entries include `id` (a `tag:` URI or the canonical URL), `published`, `updated`, `summary` and `content type="html"`.

### 7.3 Verification

- **Golden tests** render fixed store contents and compare the output byte-for-byte.
- **Schema checks in tests:** well-formed XML, required elements present, date formats matching the regex/round-trip, and no duplicate GUIDs.
- **W3C validation** as a `make validate` target that posts the rendered feed to `https://validator.w3.org/feed/check.cgi?output=soap12`. It fails on any error and on any warning not on an explicit allowlist. It runs locally and in CI (not per-commit, to be polite to the validator).
- **Manual reader check** at milestone M2: subscribe in 2–3 real readers (for example NetNewsWire, Feedbin and Miniflux) to confirm rendering, image loading and ordering.

---

## 8. CLI and deployment

```
feed-me run      [--config feed-me.yaml]   # long-running: scheduler + HTTP server
feed-me build    [--site id]              # one-shot: fetch + write static feeds to out_dir, exit
feed-me check    --site id [--url URL]    # dry-run extraction; prints fields + warnings as a table/JSON
feed-me validate [--site id]              # render + validate feed(s)
feed-me config lint                       # validate all configs, exit non-zero on error
```

- `check` is the main tool for writing a new site config. It fetches one or a few URLs and shows every extracted field, which source won, and the parsed date. This gives a tight loop while writing selectors.
- **HTTP server** (`run`):
  - Serves `/feeds/<id>.xml`, `/feeds/<id>.atom`, `/healthz` and an `/` index of feeds, with an OPML export at `/feeds.opml`.
  - Sets `ETag` and `Last-Modified` so readers' conditional GETs return `304`.
- **`build`** is a secondary mode for one-off runs and CI. It writes to files atomically (temp file plus rename).
- **Logging** uses structured `log/slog`, as JSON by default in the container so `docker logs` output is machine-readable.

### 8.1 Docker deployment (primary target)

The service runs as a long-lived `feed-me run` container behind the user's existing reverse proxy.

**Image:**
- A multi-stage `Dockerfile`: `golang:1.27` builds with `CGO_ENABLED=0` (possible because `modernc.org/sqlite` is pure Go), then the binary is copied into `gcr.io/distroless/static-debian12:nonroot`.
- Size is roughly 20 MB. It runs as a non-root user.
- CA certificates come with the distroless base, and the timezone database is embedded with the `time/tzdata` import, so `America/Los_Angeles` resolves inside the container.

**Volumes and config:**
- `/data` holds the SQLite DB (`/data/feed-me.db`). Mount it as a named volume or bind mount so state survives image upgrades. A bind-mounted directory must be writable by the image's nonroot user (`chown 65532:65532 ./data`).
- `/config` holds `feed-me.yaml` and `/sites` holds `*.yaml`, both mounted read-only as directories (a single-file mount misses an editor's save-by-rename). Changing either needs only a container restart, not an image rebuild. A copy of the configs is also baked into the image as a default.
- SQLite runs in WAL mode with `busy_timeout`. There is one writer (the scheduler), and HTTP handlers only read.
- **Schema migrations** run automatically at startup. The DB records a `schema_version`.

**Networking and the reverse proxy:**
- The server listens on `:8080` over plain HTTP. The proxy handles TLS.
- `public_base_url` is set in config or with the `FEED_ME_PUBLIC_BASE_URL` environment variable. It's used for `atom:link rel=self` and OPML links. The app **doesn't** try to infer it from `X-Forwarded-*` headers, so the self-link is always stable and correct.
- An optional `base_path` handles the case where the proxy mounts the app under a sub-path, for example `/rss/`.
- Feed responses include `Cache-Control: public, max-age=300`, `ETag` and `Last-Modified`, so the proxy (or a CDN in front of it) can cache them, and readers get `304`s.

**Health and lifecycle:**
- Distroless has no shell or `curl`, so the healthcheck is a subcommand: `HEALTHCHECK CMD ["/feed-me", "healthcheck"]`. It hits `http://127.0.0.1:8080/healthz`.
- `/healthz` returns 200 while the process is up. `/readyz` returns 200 only once every site has completed at least one successful run and every one of its feeds can be served. It is deliberately all-or-nothing: a site that stores no items, or whose feed keeps failing its checks, keeps `/readyz` at 503 and names the site, while the other feeds are still served. So route traffic and restarts on `/healthz` (as the `HEALTHCHECK` does), and use `/readyz` to find a broken site.
- `Last-Modified` is the time a feed's bytes last changed. It is stored with the feed's `ETag`, so a restart that renders the same bytes keeps it.
- On `SIGTERM`, it finishes the in-flight page fetch, stops the scheduler, drains HTTP connections (10s) and closes the DB cleanly.
- On startup, a site whose last run is older than its `interval` runs immediately. Otherwise it waits for its next tick. Restarts therefore don't cause a burst of fetches.

**`compose.yaml`** (shipped in the repo). The container config is `deploy/feed-me.yaml`, which puts the store on `/data` and logs JSON. `deploy/` and `sites/` are mounted over the baked-in copies. Shutdown can take up to 40s (30s for the page in flight, 10s for HTTP), so the stop grace period is raised from Docker's default of 10s; with plain `docker run`, pass `--stop-timeout 45`.

```yaml
services:
  feed-me:
    image: feed-me:latest
    build:
      context: .
      args:
        VERSION: ${VERSION:-dev}
    restart: unless-stopped
    stop_grace_period: 45s
    environment:
      FEED_ME_PUBLIC_BASE_URL: https://rss.example.com
    volumes:
      - feed-me-data:/data
      - ./deploy:/config:ro
      - ./sites:/sites:ro
    expose: ["8080"] # the reverse proxy joins this network; no host port needed
volumes:
  feed-me-data:
```

The resulting feed URL is `https://rss.example.com/feeds/claude-blog.xml`.
- **Observability:**
  - One log line per run with `site`, `discovered`, `fetched`, `not_modified`, `new_items`, `updated_items`, `errors` and `duration`.
  - If extraction starts failing, for example after a site redesign breaks a selector, the run logs a loud `WARN` and **keeps serving the last good feed**. It never publishes an empty feed.
  - A per-site "extraction health" check flags when more than X% of newly fetched pages are missing `content` or `published`.

---

## 9. Project layout

```
feed-me/
  cmd/feed-me/main.go
  internal/
    config/      # YAML load, strict decode, validation, source-spec parsing
    discovery/   # sitemap, index, links strategies
    fetch/       # client, rate limit, robots, conditional GET, retries
    extract/     # jsonld, meta, css, time, url sources; field resolution
    normalize/   # html cleanup, url absolutizing, sanitize, dates
    store/       # sqlite schema + migrations, queries
    feed/        # rss + atom structs, rendering
    server/      # http handlers
    scheduler/   # runs each site every interval, one at a time
    pipeline/    # orchestrates one site run
  sites/
    claude-blog.yaml
    select-dev.yaml
  testdata/
    claude-blog/ # saved HTML fixtures + sitemap + expected golden feed
    select-dev/  # saved posts/rss.xml (with stega chars intact) + post HTML + golden feed
  deploy/
    feed-me.yaml  # container config: store on /data, JSON logs
  Dockerfile
  compose.yaml
  PLAN.md
  README.md
  Makefile
  go.mod
```

**Dependencies**, kept deliberately few:

| Dependency | Purpose |
|---|---|
| `github.com/PuerkitoBio/goquery` (brings `andybalholm/cascadia`, `golang.org/x/net/html`) | HTML parsing and selection |
| `github.com/microcosm-cc/bluemonday` | Sanitization |
| `gopkg.in/yaml.v3` | Config parsing |
| `modernc.org/sqlite` | Store |
| `github.com/temoto/robotstxt` | `robots.txt` |
| `golang.org/x/time/rate` | Rate limiting |

Everything else comes from the standard library: `encoding/xml`, `net/http`, `log/slog` and `time`. Go version is 1.27.

### Store schema (sketch)

```sql
items(
  site_id TEXT, guid TEXT, url TEXT, title TEXT, summary TEXT, content_html TEXT,
  author TEXT, image_url TEXT, categories_json TEXT,
  published_at INTEGER, published_source TEXT,   -- 'jsonld' | 'first_seen' | ...
  updated_at INTEGER, first_seen_at INTEGER, last_fetched_at INTEGER,
  content_hash TEXT, missing_since INTEGER,
  PRIMARY KEY (site_id, guid)
);
http_cache(site_id TEXT, url TEXT, etag TEXT, last_modified TEXT, hints_hash TEXT, fetched_at INTEGER, status INTEGER,
           PRIMARY KEY (site_id, url));   -- per site; written only after the response is stored
site_state(site_id TEXT PRIMARY KEY, last_run_at INTEGER, last_change_at INTEGER, last_error TEXT);
```

`content_hash` is a SHA-256 of the normalized body. If a re-fetch produces a different hash, the item is marked updated and `last_change_at` is bumped. If the hash matches and the other stored fields (URL, canonical, author, image, categories, dates) are the same, nothing changes. A change to only those fields is stored without bumping `updated_at`.

Validators are saved only after a response has been fully processed, so a failed extraction is retried with a full fetch. `hints_hash` records the discovery hints an item was built from. When they change (for example, the feed corrects a date), the next fetch is unconditional, because a 304 would hide the change.

---

## 10. Testing strategy

- **Unit tests:**
  - Date parsing: every layout, zone handling, and the date-only policy, including the same-day first-seen branch and DST edges.
  - Source-spec parsing, URL absolutizing, `srcset` rewriting and CDATA escaping.
- **Fixture tests:** Saved copies of the Claude blog sitemap, the index and about 5 representative posts (plain text, images, video embed, code blocks, tables) live under `testdata/`. Extraction runs offline against them through an `httptest.Server` or a `RoundTripper` stub.
- **Golden feed tests:** fixtures go in and the full RSS/Atom comes out, compared against checked-in files. `go test ./... -update` regenerates them.
- **Live smoke test** (`-tags live`, not in default CI): run `check` against the real site and assert every field is non-empty. This catches redesigns.
- **Property-style checks** on the renderer: the output always parses as XML, GUIDs are unique, and items are sorted by date descending.

---

## 11. Milestones

| # | Deliverable | Done when |
|---|---|---|
| **M0** | Skeleton: `go mod init`, CLI scaffold, config load + strict validation + `config lint`, Claude blog YAML | `feed-me config lint` passes on `sites/claude-blog.yaml` and fails usefully on a broken copy |
| **M1** | Fetcher (rate limit, robots, conditional GET), `sitemap` and `feed` discovery, extractor (jsonld/meta/css/listing), invisible-character stripping, `check` command | `feed-me check --site claude-blog` prints correct title/date/summary/body for 5 fixture posts |
| **M2** | Normalizer + SQLite store + RSS renderer + `build` | `feed-me build` produces a feed with ≥50 full-text items that **passes the W3C validator with zero errors**; renders correctly in 2 readers |
| **M3** | Timestamp policy (first-seen, freezing), incremental refresh, update detection, last-good-feed protection | Running `build` hourly for 48h yields no reordering, no duplicate items, and edits are picked up |
| **M4** | `run` mode: scheduler + HTTP server with ETag/304, OPML, `/healthz` and `/readyz`, `healthcheck` subcommand; Atom output; Dockerfile + `compose.yaml` (§8.1) | Container running behind the reverse proxy, with a feed reader subscribed through the public URL; new posts arrive within one interval; state survives `docker compose down && up` |
| **M5** | Second site, select.dev (§2.2, §4.4), to prove the config abstraction. It uses `feed` discovery, `listing` dates and meta-only extraction, which are different paths from the Claude blog. | `sites/select-dev.yaml` added with **zero Go changes**; the feed passes the W3C validator; titles contain no zero-width characters; bodies include code blocks and images |

*Status (2026-09-24):* M0–M5 are implemented. M5 was checked against the live site: `feed-me validate --site select-dev --w3c` reports 0 errors for both the RSS and Atom feeds, and its only warning is `SelfDoesntMatchLocation`, because the feed is posted as raw data. No title has a zero-width character; the only ones are two lone U+200B the author put around links in post bodies, which §6a keeps. The 50 items contain 175 `<pre>` blocks and 230 images. No Go code is specific to select.dev. These exit criteria still need to be checked outside the code: subscribing in real readers (M2), 48 hours of hourly builds (M3), and the deployment behind the reverse proxy (M4).

---

## 12. Decisions and open questions

### Decided (2026-09-23)

| Topic | Decision |
|---|---|
| Hosting | Long-running Docker container (`feed-me run`) behind the existing reverse proxy. See §8.1. |
| Feed size | 50 items (`max_items: 50`). No full-history feed. |
| Storage | SQLite on a mounted `/data` volume, holding all state: items, HTTP cache and run state. |
| Languages | English only. Localized paths such as `/ja/blog` are excluded by the discovery regex. |
| Second site | select.dev's blog (`/posts`, not the changelog). It uses its existing summary-only feed for discovery and dates, and fetches pages for full text. See §2.2. |

### Still open

1. ~~**Categories:**~~ *Resolved in M1 (2026-09-24):* Claude blog post pages carry no categories of their own. The only tags are on the related-post cards after the article, so no `categories` sources are configured.
2. **select.dev changelog:** It's out of scope for now. If wanted later, it's a second config pointing at `/changelog/rss.xml` with the same approach, so no new code is needed.
