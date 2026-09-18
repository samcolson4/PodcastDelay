// Package feed renders a subscription's released episodes as RSS 2.0
// with iTunes extensions, using text/template rather than
// encoding/xml's struct-tag marshalling, which mishandles namespaced
// elements like <itunes:duration> (see docs/IMPLEMENTATION.md §3.7).
package feed

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"text/template"
	"time"
)

// Channel is the show-level data rendered into <channel>.
type Channel struct {
	Title       string
	Description string
	Link        string
	Language    string
	Author      string
	ImageURL    string
	Explicit    bool
	ItunesType  string
	Categories  []string
	Owner       string
	OwnerEmail  string
}

// Item is one rendered <item>. PubDate is the *virtual* release date —
// the whole trick this package exists for — with OriginalPubDate
// preserved in the description text instead.
type Item struct {
	GUID            string // already prefixed by the caller, see §3.4
	Title           string
	Description     string
	Link            string
	PubDate         time.Time
	OriginalPubDate *time.Time
	EnclosureURL    string
	EnclosureType   string
	EnclosureLength int64
	Duration        string
	EpisodeNumber   *int
	Season          *int
	EpisodeType     string
	Explicit        *bool
	ImageURL        string
}

// Feed is everything needed to render one subscription's public feed.
type Feed struct {
	SelfURL      string // absolute atom:link rel="self"
	Title        string // may be the title_override
	Channel      Channel
	Items        []Item
	LastBuild    time.Time // bucketed to the last release event, not "now" (§3.8)
	GeneratorTag string
}

const rfc1123Z = time.RFC1123Z

func esc(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func cdata(s string) string {
	// A literal "]]>" would terminate the CDATA section early; split
	// it defensively even though real feeds essentially never contain it.
	s = strings.ReplaceAll(s, "]]>", "]]]]><![CDATA[>")
	return "<![CDATA[" + s + "]]>"
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func describeWithOriginal(description string, original *time.Time) string {
	if original == nil {
		return description
	}
	note := fmt.Sprintf("Originally published %s.", original.Format("2 January 2006"))
	if description == "" {
		return note
	}
	return description + "\n\n" + note
}

var funcMap = template.FuncMap{
	"esc":                  esc,
	"cdata":                cdata,
	"yesNo":                yesNo,
	"rfc1123z":             func(t time.Time) string { return t.Format(rfc1123Z) },
	"describeWithOriginal": describeWithOriginal,
	"derefInt":             func(p *int) int { return *p },
	"derefBool":            func(p *bool) bool { return *p },
}

var tmpl = template.Must(template.New("feed").Funcs(funcMap).Parse(feedTemplate))

// Render writes the RSS document for f to w.
func Render(w io.Writer, f Feed) error {
	return tmpl.Execute(w, f)
}

// RenderString is a convenience wrapper around Render.
func RenderString(f Feed) (string, error) {
	var buf bytes.Buffer
	if err := Render(&buf, f); err != nil {
		return "", err
	}
	return buf.String(), nil
}

const feedTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"
  xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd"
  xmlns:atom="http://www.w3.org/2005/Atom">
<channel>
  <title>{{esc .Title}}</title>
  <link>{{esc .Channel.Link}}</link>
  <atom:link href="{{esc .SelfURL}}" rel="self" type="application/rss+xml"/>
  <description>{{cdata .Channel.Description}}</description>
  {{- if .Channel.Language}}
  <language>{{esc .Channel.Language}}</language>
  {{- end}}
  <lastBuildDate>{{rfc1123z .LastBuild}}</lastBuildDate>
  {{- if .GeneratorTag}}
  <generator>{{esc .GeneratorTag}}</generator>
  {{- end}}
  {{- if .Channel.Author}}
  <itunes:author>{{esc .Channel.Author}}</itunes:author>
  {{- end}}
  <itunes:explicit>{{yesNo .Channel.Explicit}}</itunes:explicit>
  {{- if .Channel.ItunesType}}
  <itunes:type>{{esc .Channel.ItunesType}}</itunes:type>
  {{- end}}
  {{- if .Channel.ImageURL}}
  <itunes:image href="{{esc .Channel.ImageURL}}"/>
  <image>
    <url>{{esc .Channel.ImageURL}}</url>
    <title>{{esc .Title}}</title>
    <link>{{esc .Channel.Link}}</link>
  </image>
  {{- end}}
  {{- if .Channel.Owner}}
  <itunes:owner>
    <itunes:name>{{esc .Channel.Owner}}</itunes:name>
    {{- if .Channel.OwnerEmail}}
    <itunes:email>{{esc .Channel.OwnerEmail}}</itunes:email>
    {{- end}}
  </itunes:owner>
  {{- end}}
  {{- range .Channel.Categories}}
  <itunes:category text="{{esc .}}"/>
  {{- end}}
  {{- range .Items}}
  <item>
    <title>{{esc .Title}}</title>
    <link>{{esc .Link}}</link>
    <guid isPermaLink="false">{{esc .GUID}}</guid>
    <pubDate>{{rfc1123z .PubDate}}</pubDate>
    <description>{{cdata (describeWithOriginal .Description .OriginalPubDate)}}</description>
    <enclosure url="{{esc .EnclosureURL}}" type="{{esc .EnclosureType}}" length="{{.EnclosureLength}}"/>
    {{- if .Duration}}
    <itunes:duration>{{esc .Duration}}</itunes:duration>
    {{- end}}
    {{- if .EpisodeNumber}}
    <itunes:episode>{{derefInt .EpisodeNumber}}</itunes:episode>
    {{- end}}
    {{- if .Season}}
    <itunes:season>{{derefInt .Season}}</itunes:season>
    {{- end}}
    {{- if .EpisodeType}}
    <itunes:episodeType>{{esc .EpisodeType}}</itunes:episodeType>
    {{- end}}
    {{- if .Explicit}}
    <itunes:explicit>{{yesNo (derefBool .Explicit)}}</itunes:explicit>
    {{- end}}
    {{- if .ImageURL}}
    <itunes:image href="{{esc .ImageURL}}"/>
    {{- end}}
  </item>
  {{- end}}
</channel>
</rss>
`
