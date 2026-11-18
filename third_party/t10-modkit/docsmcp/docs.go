// Package docsmcp serves a component's own docs to coding agents over MCP, at
// /docs/mcp, as contracts/docs/README.md in the t10 workspace describes. It
// serves only docs whose front matter says they're for people building on t10
// (audience consumer, not a record), because nothing guards the route.
package docsmcp

import (
	"bytes"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Doc is one served Markdown file.
type Doc struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Audience string `json:"audience"`
	Kind     string `json:"kind"`
	Status   string `json:"status"`
	Summary  string `json:"summary"`
	body     string
	sections []section
}

type section struct {
	heading string
	slug    string
	level   int
	start   int // offset of the heading line in body
	end     int // offset where the section, with its subsections, ends
	text    string
}

// load reads every served doc from the FS, keyed by ID: its path without .md.
func load(fsys fs.FS) (map[string]*Doc, error) {
	docs := map[string]*Doc{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == "records" || strings.HasPrefix(p, "records/") {
				return fs.SkipDir
			}
			return nil
		}
		if path.Ext(p) != ".md" {
			return nil
		}
		raw, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		doc, err := parse(strings.TrimSuffix(p, ".md"), raw)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if doc != nil {
			docs[doc.ID] = doc
		}
		return nil
	})
	return docs, err
}

var frontEnd = []byte("\n---\n")

// parse answers nil for a doc that isn't served: no front matter, or not for consumers.
func parse(id string, raw []byte) (*Doc, error) {
	raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(raw, []byte("---\n")) {
		return nil, nil
	}
	end := bytes.Index(raw[3:], frontEnd)
	if end < 0 {
		return nil, fmt.Errorf("front matter isn't closed")
	}
	doc := &Doc{ID: id}
	if err := yaml.Unmarshal(raw[4:end+3], doc); err != nil {
		return nil, fmt.Errorf("front matter: %w", err)
	}
	if doc.Audience != "consumer" || doc.Status == "record" || doc.Kind == "record" {
		return nil, nil
	}
	doc.body = strings.TrimLeft(string(raw[end+3+len(frontEnd):]), "\n")
	doc.sections = sections(doc.body)
	return doc, nil
}

// UnmarshalYAML keeps the front matter's fields, which are lower case.
func (d *Doc) UnmarshalYAML(n *yaml.Node) error {
	var f struct{ Title, Audience, Kind, Status, Summary string }
	if err := n.Decode(&f); err != nil {
		return err
	}
	d.Title, d.Audience, d.Kind, d.Status, d.Summary = f.Title, f.Audience, f.Kind, f.Status, f.Summary
	return nil
}

var heading = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*#*\s*$`)

// sections splits a body at its headings, skipping fenced code.
func sections(body string) []section {
	var out []section
	fenced := false
	offset := 0
	for _, line := range strings.SplitAfter(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
		} else if m := heading.FindStringSubmatch(strings.TrimRight(line, "\n")); m != nil && !fenced {
			out = append(out, section{heading: m[2], slug: slug(m[2]), level: len(m[1]), start: offset})
		}
		offset += len(line)
	}
	for i := range out {
		out[i].end = len(body)
		for j := i + 1; j < len(out); j++ {
			if out[j].level <= out[i].level {
				out[i].end = out[j].start
				break
			}
		}
		next := len(body)
		if i+1 < len(out) {
			next = out[i+1].start
		}
		out[i].text = body[out[i].start:next]
	}
	return out
}

var notSlug = regexp.MustCompile(`[^\p{L}\p{N} _-]`)

// slug is GitHub's anchor for a heading, so a link's anchor names a section.
func slug(h string) string {
	return strings.ReplaceAll(notSlug.ReplaceAllString(strings.ToLower(h), ""), " ", "-")
}

// find answers a section by its slug or its heading, ignoring case.
func (d *Doc) find(name string) (section, bool) {
	name = strings.TrimPrefix(strings.TrimSpace(name), "#")
	for _, s := range d.sections {
		if s.slug == slug(name) || strings.EqualFold(s.heading, name) {
			return s, true
		}
	}
	return section{}, false
}

// headings lists a doc's section headings, for a reader choosing one.
func (d *Doc) headings() []string {
	out := make([]string, 0, len(d.sections))
	for _, s := range d.sections {
		out = append(out, strings.Repeat("#", s.level)+" "+s.heading)
	}
	return out
}

// Hit is one search result: a section of a doc.
type Hit struct {
	ID      string  `json:"id"`
	Section string  `json:"section"`
	Title   string  `json:"title"`
	Snippet string  `json:"snippet"`
	Score   float64 `json:"score"`
}

var word = regexp.MustCompile(`[\p{L}\p{N}_./-]+`)

func terms(q string) []string {
	var out []string
	for _, w := range word.FindAllString(strings.ToLower(q), -1) {
		if len(w) > 1 || w > "\u007f" {
			out = append(out, w)
		}
	}
	return out
}

// search ranks sections by how many of the terms they hold, then by how often
// they appear, weighting the section's heading and its doc's title and summary. Every term must appear somewhere in
// the doc, so a longer query narrows the results.
func search(docs map[string]*Doc, q string, limit int) []Hit {
	ts := terms(q)
	if len(ts) == 0 {
		return nil
	}
	var hits []Hit
	for _, d := range docs {
		whole := strings.ToLower(d.Title + "\n" + d.Summary + "\n" + d.body)
		all := true
		for _, t := range ts {
			if !strings.Contains(whole, t) {
				all = false
				break
			}
		}
		if !all {
			continue
		}
		head := strings.ToLower(d.Title + " " + d.Summary)
		parts := d.sections
		if len(parts) == 0 {
			parts = []section{{text: d.body}}
		}
		for _, s := range parts {
			text := strings.ToLower(s.text)
			score := 0.0
			for _, t := range ts {
				n := float64(strings.Count(text, t)) + 3*float64(strings.Count(strings.ToLower(s.heading), t))
				if n > 0 {
					score += 100 // a section that has more of the words comes first
				}
				score += n + 2*float64(strings.Count(head, t))
			}
			if score == 0 {
				continue
			}
			hits = append(hits, Hit{ID: d.ID, Section: s.slug, Title: title(d, s), Snippet: snippet(s.text, ts), Score: score})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		if hits[i].ID != hits[j].ID {
			return hits[i].ID < hits[j].ID
		}
		return hits[i].Section < hits[j].Section
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

func title(d *Doc, s section) string {
	if s.heading == "" || s.heading == d.Title {
		return d.Title
	}
	return d.Title + " › " + s.heading
}

// snippet is up to about 240 characters around the first term found.
func snippet(text string, ts []string) string {
	lower := strings.ToLower(text)
	at := -1
	for _, t := range ts {
		if i := strings.Index(lower, t); i >= 0 && (at < 0 || i < at) {
			at = i
		}
	}
	start := max(at-80, 0)
	end := min(start+240, len(text))
	for start > 0 && !utf8Start(text[start]) {
		start--
	}
	for end < len(text) && !utf8Start(text[end]) {
		end++
	}
	out := strings.Join(strings.Fields(text[start:end]), " ")
	if start > 0 {
		out = "…" + out
	}
	if end < len(text) {
		out += "…"
	}
	return out
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
