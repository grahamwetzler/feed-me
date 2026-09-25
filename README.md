# Feed Me!

Too many blogs these days have no RSS feed. Feed Me! was built to fill that gap.

It's a small Go service that turns those sites into full-text RSS and Atom feeds. You describe each site in a YAML file under `sites/`, which says where to discover posts and how to pull out the title, date, author and body. Feed Me! crawls politely, keeps each item's timestamp stable once it's been published, and serves feeds that pass the W3C validator.
