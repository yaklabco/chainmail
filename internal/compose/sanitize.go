package compose

import (
	"bytes"
	"slices"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
)

// textStyles are the CSS properties a style attribute may keep: what text
// formatting needs, and nothing that positions or loads anything. Each value
// is checked by bluemonday's handler for that property.
var textStyles = []string{
	"color", "background-color",
	"font-family", "font-size", "font-style", "font-variant", "font-weight",
	"letter-spacing", "line-height", "text-align", "text-decoration",
	"text-indent", "text-transform", "vertical-align", "white-space", "word-spacing",
	"margin", "margin-top", "margin-right", "margin-bottom", "margin-left",
	"padding", "padding-top", "padding-right", "padding-bottom", "padding-left",
	"border", "border-top", "border-right", "border-bottom", "border-left",
	"border-color", "border-style", "border-width", "border-collapse",
}

// sanitizePolicy is bluemonday's policy for user content, plus classes and
// style attributes holding textStyles. It drops script and style elements,
// on* attributes, and URLs other than relative ones and mailto:, http:,
// https: and cid:. A bluemonday.Policy is safe for concurrent use once built.
var sanitizePolicy = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowStyling()
	p.AllowAttrs("style").Globally()
	p.AllowStyles(textStyles...).Globally()
	// cid: is how a message refers to its own parts, such as inline images.
	p.AllowURLSchemes("cid")
	return p
}()

// SanitizeHTML returns s with everything unsafe to show removed: scripts,
// style elements, event handler attributes, javascript: and other unsafe
// URLs, and styles beyond text formatting. Images keep only cid: and https:
// sources, so showing the result loads nothing over plain HTTP and nothing
// relative to wherever it is shown.
func SanitizeHTML(s string) string {
	return imageSources(sanitizePolicy.Sanitize(s))
}

// imageSources drops the src of every img whose source is not cid: or
// https:. bluemonday applies one URL scheme list to every element, so this
// narrows it for images afterwards. Its input is bluemonday's output, so
// everything but the img tags it changes is copied byte for byte.
func imageSources(s string) string {
	z := html.NewTokenizer(strings.NewReader(s))
	var out bytes.Buffer
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			// io.EOF: the tokenizer reads from a string, so nothing else can fail.
			return out.String()
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			out.Write(z.Raw())
			continue
		}
		// Token unescapes in place, which changes what Raw returned.
		raw := bytes.Clone(z.Raw())
		tok := z.Token()
		if tok.Data != "img" || !slices.ContainsFunc(tok.Attr, badImageSource) {
			out.Write(raw)
			continue
		}
		tok.Attr = slices.DeleteFunc(tok.Attr, badImageSource)
		out.WriteString(tok.String())
	}
}

// badImageSource reports whether a is an img src other than cid: or https:.
func badImageSource(a html.Attribute) bool {
	if a.Key != "src" {
		return false
	}
	v := strings.ToLower(strings.TrimSpace(a.Val))
	return !strings.HasPrefix(v, "cid:") && !strings.HasPrefix(v, "https:")
}
