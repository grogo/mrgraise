package main

import (
	"fmt"
	htmlLib "html"
	"regexp"
	"strings"

	"github.com/gomarkdown/markdown"
	"github.com/gomarkdown/markdown/html"
	"github.com/gomarkdown/markdown/parser"
)

// filterParagraphs removes lines that start with any of the given phrases.
func filterParagraphs(text string, phrases []string) string {
	assert(len(phrases) > 0, "phrases must not be empty")

	lines := strings.Split(text, "\n")

	// Build regex pattern from phrases
	escaped := make([]string, len(phrases))
	for i, p := range phrases {
		escaped[i] = regexp.QuoteMeta(p)
	}
	pattern := regexp.MustCompile(`(?i)^(?:` + strings.Join(escaped, "|") + `)`)

	var filtered []string
	for _, line := range lines {
		if !pattern.MatchString(strings.TrimSpace(line)) {
			filtered = append(filtered, line)
		}
	}

	return strings.Join(filtered, "\n\n")
}

// removeNumbering strips leading numbering (e.g., "1. ") and bullet markers from lines.
func removeNumbering(text string) string {
	numPattern := regexp.MustCompile(`(?m)^\d+\.(?:\s+|$)`)
	text = numPattern.ReplaceAllString(text, "")

	bulletPattern := regexp.MustCompile("(?m)^[-*\u2022](?:\\s+|$)")
	text = bulletPattern.ReplaceAllString(text, "")

	return text
}

// stripMarkdown converts markdown to HTML, then strips HTML tags to get plain text.
func stripMarkdown(text string) string {
	extensions := parser.CommonExtensions
	p := parser.NewWithExtensions(extensions)
	doc := p.Parse([]byte(text))

	htmlFlags := html.CommonFlags
	renderer := html.NewRenderer(html.RendererOptions{Flags: htmlFlags})
	htmlBytes := markdown.Render(doc, renderer)

	// Strip HTML tags, then unescape HTML entities (&ldquo; &gt; etc.)
	tagPattern := regexp.MustCompile(`<[^>]+>`)
	text = tagPattern.ReplaceAllString(string(htmlBytes), "")
	return htmlLib.UnescapeString(text)
}

// extractImpressionText returns text after the "IMPRESSION:" line.
func extractImpressionText(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.ToUpper(line), "IMPRESSION:") {
			return strings.Join(lines[i+1:], "\n")
		}
	}
	return text
}

// numberParagraphs extracts impression text and numbers each non-empty line.
func numberParagraphs(text string) string {
	text = extractImpressionText(text)
	lines := strings.Split(strings.TrimSpace(text), "\n")

	var numbered []string
	i := 1
	for _, line := range lines {
		if len(strings.TrimSpace(line)) > 1 {
			numbered = append(numbered, fmt.Sprintf("%d. %s", i, strings.TrimSpace(line)))
			i++
		}
	}

	return strings.Join(numbered, "\n\n")
}

// extractErrorsBeforeImpression extracts content between <errors> and </errors>
// tags, but only from text before "IMPRESSION:".
func extractErrorsBeforeImpression(text string) string {
	relevantText := text
	if idx := strings.Index(text, "IMPRESSION:"); idx != -1 {
		relevantText = text[:idx]
	}

	pattern := regexp.MustCompile(`(?is)<errors>\s*(.*?)\s*</errors>`)
	match := pattern.FindStringSubmatch(relevantText)
	if len(match) >= 2 {
		return strings.TrimSpace(match[1])
	}
	return ""
}

// assert panics with the given message if the condition is false.
func assert(condition bool, msg string) {
	if !condition {
		panic("assertion failed: " + msg)
	}
}
