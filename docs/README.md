# The documents, and the site made from them

The Markdown in this directory is the source of truth. It is written to be read here, in
the repository, and the user-facing pages are also published at
<https://dagsommer.github.io/boks/> — the same files, rendered. The site holds no copy of
any document, so there is nothing to keep in sync and nothing that can drift: edit the
file, and the site follows.

## How the documents are organised

The order is task-oriented, after the reference product's own documentation: what you do
first, then what you do next, then what you look up. It is not the order the documents were
written in.

| | |
|---|---|
| **Guide** | [get-started.md](get-started.md), [install.md](install.md), [walkthrough.md](walkthrough.md), [usage.md](usage.md), [agents.md](agents.md) |
| **Reference** | [architecture.md](architecture.md), [security-model.md](security-model.md), [cli.md](cli.md), [troubleshooting.md](troubleshooting.md), [faq.md](faq.md) |
| **Project** | [roadmap.md](roadmap.md), [release-notes.md](release-notes.md) |
<<<<<<< HEAD
| **Engineering record** (repo only) | [verification.md](verification.md), [docker-sandbox-parity.md](docker-sandbox-parity.md), [windows.md](windows.md), [windows-e2e.md](windows-e2e.md), [upstream-libkrun-virtio-net.md](upstream-libkrun-virtio-net.md) |
=======
| **Evidence** | [verification.md](verification.md), [verify-linux-prompt.md](verify-linux-prompt.md), [docker-sandbox-parity.md](docker-sandbox-parity.md), [windows.md](windows.md), [upstream-libkrun-virtio-net.md](upstream-libkrun-virtio-net.md) |
>>>>>>> worktree-agent-a34894461e82d6ab8

The first three groups are the site: the pages a *user* of Boks needs. The engineering
record stays in the repository for contributors and is deliberately not published — the
site is a product's front door, not a lab notebook — but it is not diminished by that: it
is what every claim in the other three groups is answerable to. Published pages that cite
those documents still link to them, because the build rewrites a link to an unpublished doc
into a link to the file on GitHub; every claim remains one click from its evidence.

Two of these are not written the way the rest are, and both say so at the top of the file:

- **[cli.md](cli.md) is generated** from the cobra command tree by
  [`internal/cli/reference.go`](../internal/cli/reference.go). Do not edit it; edit the
  command. See [Generated documents](#generated-documents).
- **[release-notes.md](release-notes.md)** is generated per release from conventional
  commits by [`scripts/release-notes.sh`](../scripts/release-notes.sh), except for its first
  entry, which is hand-written and explains why.

## The landing page is not a document

`/` on the site is [`site/index.html`](../site/index.html) with the `home` layout, not a
rendered Markdown file. That is the one place the site has design of its own.

It still carries no words that are not somewhere else. The plugin quotes two things out of
the file named by `home_source` in [`site/_config.yml`](../site/_config.yml) — currently
`README.md` — and puts them in the hero:

- the **lede**: everything between the first `# ` heading and the first alert;
- the **experimental warning**: the first `> [!WARNING]` block.

Neither is copied into `site/`. **If the warning cannot be found, the build fails**, with a
message saying so, because a landing page that has quietly lost its warning is exactly the
failure worth being loud about. Two assertions in
[`pages.yml`](../.github/workflows/pages.yml) then check that it is still rendered as a
warning and still inside the hero rather than moved below the content.

Everything else the landing page says — the platform cards, the feature grid, the diagram —
is prose in `site/index.html`, and it is held to two rules: no claim the documents do not
already support, and every sentence is for a visitor deciding whether to use Boks, never
about how the project works internally. Three of its statements have greps in CI, because
they are the ones a redesign would be most tempted to round off.

## Adding a document

1. Write it in `docs/`, as a normal Markdown file with a single `# ` heading at the top.
   That heading becomes the page title.
2. Add an entry to `boks_docs` in [`site/_config.yml`](../site/_config.yml):

   ```yaml
     - source: docs/your-document.md
       url: /your-document/
       nav: Short label
   ```

   Order in that list is the order in the sidebar, and an entry with only a `section:` key is
   a heading in it. A document that is not listed is not published — and a listed file that
   does not exist **fails the build**, rather than disappearing from the site quietly.

Nothing else is needed. No front matter, no navigation keys inside the document, no
duplicated copy under `site/`.

## Generated documents

```bash
make docs          # write docs/cli.md from the command tree
make docs-check    # exit 1 if it is out of date, write nothing
```

`go test ./internal/cli/` fails while `docs/cli.md` is stale, which is what makes running
`make docs` non-optional rather than a thing to remember. The same check runs in
[`pages.yml`](../.github/workflows/pages.yml), so a hand-edited `docs/cli.md` cannot be
published; note that a flag renamed in `internal/cli` without a documentation change does not
trigger that workflow, and it is the Go test that catches that case.

The generator walks cobra directly rather than calling `github.com/spf13/cobra/doc`, which
would pull go-md2man and blackfriday into `go.mod` for a man-page renderer this page does not
use. The reasoning is at the top of [`internal/cli/reference.go`](../internal/cli/reference.go).

Release notes:

```bash
make release-notes TAG=v0.1.1                  # print it
make release-notes TAG=v0.1.1 INSERT=--insert  # write it into docs/release-notes.md
```

How a release is cut, in full, is at the top of
[`scripts/release-notes.sh`](../scripts/release-notes.sh).

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
  CDN, no remote fonts. A build check fails if any page grows one.

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

Then, on the output:

```bash
sh site/check-anchors.sh                  # every cross-document link lands on a real heading
```

That one exists because it is the failure nothing else notices: a heading is renamed, a link
from another document keeps working, and it silently lands at the top of a long page.

## Publishing

[`.github/workflows/pages.yml`](../.github/workflows/pages.yml) builds the site on every
push to `main` that touches `README.md`, `docs/`, `site/`, `internal/cli/`, `cmd/gen-docs/`
or the workflow, and deploys it. Pull requests touching those paths build the site without
publishing it, which is what catches a `_config.yml` entry pointing at a file that was
renamed.

The build job holds no credential; a separate job holds `pages: write` and only ever hands
GitHub the artifact the build produced. That split is deliberate — see the comment at the
top of the workflow, and the same reasoning in `images.yml`.
