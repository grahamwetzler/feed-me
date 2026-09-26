# Feed Me!

Too many blogs these days have no RSS feed. Feed Me! was built to fill that gap.

It's a small Go service that turns those sites into full-text RSS and Atom feeds. You describe each site in a YAML file under `sites/`, which says where to discover posts and how to pull out the title, date, author and body. Feed Me! crawls politely, keeps each item's timestamp stable once it's been published, and serves feeds that pass the W3C validator.

## Supported sites

| Site | Config | Feeds |
| --- | --- | --- |
| [Claude Blog](https://claude.com/blog) | [`claude-blog.yaml`](sites/claude-blog.yaml) | `feeds/claude-blog.xml`, `feeds/claude-blog.atom` |
| [claude.dev Blog](https://claude.dev/) | [`claude-dev.yaml`](sites/claude-dev.yaml) | `feeds/claude-dev.xml`, `feeds/claude-dev.atom` |
| [SELECT Blog](https://select.dev/posts) | [`select-dev.yaml`](sites/select-dev.yaml) | `feeds/select-dev.xml`, `feeds/select-dev.atom` |
| [Snowflake Blog](https://www.snowflake.com/en/blog/) (including the engineering blog) | [`snowflake-blog.yaml`](sites/snowflake-blog.yaml) | `feeds/snowflake-blog.xml`, `feeds/snowflake-blog.atom` |

To add one, see [Adding a site](docs/adding-a-site.md).
