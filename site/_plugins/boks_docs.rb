# Generates the site's pages from the repository's Markdown.
#
# The documents in docs/ (and README.md) are the source of truth and are edited there. This
# plugin reads them at build time and renders them; it never writes to them, and the site
# holds no copy of them. Four things happen on the way through:
#
#   1. GitHub's alert syntax (`> [!WARNING]`) becomes a blockquote carrying a CSS class,
#      because kramdown renders the marker as literal text otherwise. The words are the
#      document's own; only the marker line is rewritten, to the word GitHub renders.
#   2. Links between documents (`docs/verification.md`, `security-model.md#network`) are
#      rewritten to the site's URLs. A link to a path the site does not publish — LICENSE,
#      images/ — goes to the file on GitHub instead of breaking.
#   3. Every table is wrapped in a scroll container, so a wide table scrolls inside the page
#      rather than stretching it. The parity matrix has six columns of prose.
#   4. Headings become a per-page table of contents, and each section becomes an entry in
#      the search index the site ships as a static JSON file.
#
# Nothing here rewords a document. If a transformation cannot be applied, the build fails
# rather than publishing something that reads differently from the file in git.
module BoksDocs
  ALERT_TITLES = {
    "NOTE" => "Note",
    "TIP" => "Tip",
    "IMPORTANT" => "Important",
    "WARNING" => "Warning",
    "CAUTION" => "Caution",
  }.freeze

  # Documents are hand-written Markdown; a `](` inside a fenced code block is a shell
  # snippet, not a link. Transformations skip fenced blocks entirely.
  def self.transform(markdown, source_path, urls, repo, branch)
    lines = markdown.split("\n", -1)
    out = []
    in_fence = false
    fence = nil

    i = 0
    while i < lines.length
      line = lines[i]

      if in_fence
        in_fence = false if line.strip.start_with?(fence)
        out << line
        i += 1
        next
      end

      if (m = line.match(/\A\s{0,3}(`{3,}|~{3,})/))
        in_fence = true
        fence = m[1]
        out << line
        i += 1
        next
      end

      if (m = line.match(/\A>\s*\[!(#{ALERT_TITLES.keys.join('|')})\]\s*\z/))
        kind = m[1]
        body = []
        j = i + 1
        while j < lines.length && lines[j].start_with?(">")
          body << lines[j]
          j += 1
        end
        out << "> **#{ALERT_TITLES[kind]}**"
        out << ">"
        # An alert can quote a code block — the check-6 failure in verification.md does —
        # and a `](` in there is output, not a link.
        quoted_fence = nil
        out.concat(body.map do |b|
          inner = b.sub(/\A>\s?/, "")
          if quoted_fence
            quoted_fence = nil if inner.strip.start_with?(quoted_fence)
            next b
          end
          if (f = inner.match(/\A\s{0,3}(`{3,}|~{3,})/))
            quoted_fence = f[1]
            next b
          end
          rewrite_links(b, source_path, urls, repo, branch)
        end)
        out << "{: .alert .alert-#{kind.downcase}}"
        i = j
        next
      end

      out << rewrite_links(line, source_path, urls, repo, branch)
      i += 1
    end

    out.join("\n")
  end

  # `[text](target)` where target is a relative path in the repository.
  def self.rewrite_links(line, source_path, urls, repo, branch)
    line.gsub(/\]\(([^)\s]+)\)/) do
      target = Regexp.last_match(1)
      "](#{resolve(target, source_path, urls, repo, branch)})"
    end
  end

  def self.resolve(target, source_path, urls, repo, branch)
    return target if target.match?(%r{\A([a-z][a-z0-9+.-]*:|//|#)})

    path, _, frag = target.partition("#")
    return target if path.empty?

    dir = File.dirname(source_path)
    resolved = dir == "." ? path : File.expand_path(path, "/#{dir}").sub(%r{\A/}, "")
    resolved = resolved.chomp("/")

    if (url = urls[resolved])
      frag.empty? ? url : "#{url}##{frag}"
    else
      kind = target.end_with?("/") ? "tree" : "blob"
      "#{repo}/#{kind}/#{branch}/#{resolved}#{frag.empty? ? '' : "##{frag}"}"
    end
  end

  # A wide table scrolls inside its own box. tabindex makes that box reachable from the
  # keyboard, which is the difference between "scrollable" and "scrollable with a mouse".
  def self.wrap_tables(html)
    html.gsub(%r{<table>.*?</table>}m) do |table|
      %(<div class="table-wrap" tabindex="0" role="region" aria-label="Table, scrolls horizontally">#{table}</div>)
    end
  end

  def self.headings(html)
    html.scan(%r{<h([23])\s+id="([^"]+)"[^>]*>(.*?)</h\1>}m).map do |level, id, text|
      { "level" => level.to_i, "id" => id, "text" => strip_tags(text) }
    end
  end

  def self.strip_tags(html)
    html.gsub(/<[^>]+>/, " ")
        .gsub("&lt;", "<").gsub("&gt;", ">").gsub("&quot;", '"')
        .gsub("&#39;", "'").gsub("&amp;", "&")
        .gsub(/\s+/, " ").strip
  end

  # One search entry per section, so a hit lands on the heading it was found under rather
  # than at the top of an 1800-line document.
  def self.search_entries(html, title, url)
    parts = html.split(%r{(?=<h2\s)})
    parts.map do |part|
      head = part.match(%r{\A<h2\s+id="([^"]+)"[^>]*>(.*?)</h2>}m)
      text = strip_tags(head ? part.sub(head[0], "") : part)
      next nil if text.empty? && head.nil?

      {
        "title" => title,
        "url" => url,
        "section" => head ? strip_tags(head[2]) : nil,
        "anchor" => head ? head[1] : nil,
        "text" => text[0, 1800],
      }
    end.compact
  end
end

class BoksDocsGenerator < Jekyll::Generator
  safe false
  priority :high

  def generate(site)
    entries = site.config["boks_docs"] || []
    root = File.expand_path("..", site.source)
    repo = site.config["repo"]
    branch = site.config["repo_branch"]

    urls = entries.to_h { |e| [e["source"], e["url"]] }

    converter = site.find_converter_instance(Jekyll::Converters::Markdown)
    index = []

    nav = entries.map do |entry|
      source = entry["source"]
      path = File.join(root, source)
      unless File.file?(path)
        raise Jekyll::Errors::FatalException,
              "site: boks_docs lists #{source}, which does not exist. " \
              "Fix the path in site/_config.yml, or remove the entry."
      end

      markdown = File.read(path)
      title = entry["title"] || markdown[/^#\s+(.+)$/, 1] || source
      html = BoksDocs.wrap_tables(
        converter.convert(BoksDocs.transform(markdown, source, urls, repo, branch)),
      )

      index.concat(BoksDocs.search_entries(html, title, entry["url"]))

      page = Jekyll::PageWithoutAFile.new(site, site.source, entry["url"], "index.html")
      page.content = html
      page.data.merge!(
        "layout" => "doc",
        "title" => title,
        "nav_label" => entry["nav"] || title,
        "source" => source,
        "toc" => BoksDocs.headings(html),
        "is_home" => entry["url"] == "/",
      )
      site.pages << page

      { "url" => entry["url"], "label" => entry["nav"] || title, "toc" => page.data["toc"] }
    end

    site.config["nav"] = nav

    search = Jekyll::PageWithoutAFile.new(site, site.source, "", "search-index.json")
    search.content = JSON.generate(index)
    search.data["layout"] = nil
    search.data["sitemap"] = false
    site.pages << search

    Jekyll.logger.info "Boks docs:", "#{nav.length} pages, #{index.length} search sections"
  end
end
