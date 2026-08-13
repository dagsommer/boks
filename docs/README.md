# The documents, and the site made from them

The Markdown in this directory is the source of truth. It is written to be read here, in
the repository, and it is also published at <https://dagsommer.github.io/boks/> — the same
files, rendered. The site holds no copy of any document, so there is nothing to keep in
sync and nothing that can drift: edit the file, and the site follows.

## Adding a document

1. Write it in `docs/`, as a normal Markdown file with a single `# ` heading at the top.
   That heading becomes the page title.
2. Add an entry to `boks_docs` in [`site/_config.yml`](../site/_config.yml):

   ```yaml
     - source: docs/your-document.md
       url: /your-document/
       nav: Short label
   ```

   Order in that list is the order in the sidebar. A document that is not listed is not
   published — and a listed file that does not exist **fails the build**, rather than
   disappearing from the site quietly.

Nothing else is needed. No front matter, no navigation keys inside the document, no
duplicated copy under `site/`.

## What the build does to a document

[`site/_plugins/boks_docs.rb`](../site/_plugins/boks_docs.rb) reads each listed file and
renders it. It changes four things, and nothing else:

- **GitHub's alert syntax** (`> [!WARNING]`, `> [!CAUTION]`) becomes a blockquote with a
  class, so it renders as an alert instead of printing `[!WARNING]` as text. The marker
  line becomes the word GitHub itself renders; the body is untouched.
- **Links between documents** — `docs/verification.md`, `security-model.md#network` — are
  rewritten to the site's URLs. A link to a path the site does not publish (`LICENSE`,
  `images/`) is rewritten to that file on GitHub rather than left broken. The visible link
  text is never changed.
- **Tables** are wrapped in a container that scrolls sideways, so a wide table scrolls
  inside the page rather than stretching it.
- **Headings** become the per-page contents list in the sidebar and the entries in the
  search index, which is one static JSON file scored in the reader's browser. There is no
  search service, and the site makes no third-party requests of any kind — no analytics, no
  CDN, no remote fonts.

The words in a document are the words on the site. If you find a place where the rendered
page says something different from the file, that is a bug in the plugin.

## Building it locally

The site needs Ruby, Jekyll and one plugin gem. With Docker:

```bash
docker run --rm -v "$PWD":/src -w /src ruby:3.3 sh -c \
  'gem install --no-document jekyll:4.4.1 kramdown-parser-gfm:1.1.0 &&
   jekyll build --source site --destination site/_site --baseurl ""'
```

With a local Ruby:

```bash
cd site
bundle install
bundle exec jekyll build --baseurl ""     # or: bundle exec jekyll serve --baseurl ""
python3 -m http.server -d _site 4000      # if you built rather than served
```

`--baseurl ""` is for local viewing only. The published site lives under `/boks/`, which
the workflow passes in.

## Publishing

[`.github/workflows/pages.yml`](../.github/workflows/pages.yml) builds the site on every
push to `main` that touches `README.md`, `docs/`, `site/` or the workflow, and deploys it.
Pull requests touching those paths build the site without publishing it, which is what
catches a `_config.yml` entry pointing at a file that was renamed.

The build job holds no credential; a separate job holds `pages: write` and only ever hands
GitHub the artifact the build produced. That split is deliberate — see the comment at the
top of the workflow, and the same reasoning in `images.yml`.
