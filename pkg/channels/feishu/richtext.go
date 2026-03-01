//go:build amd64 || arm64 || riscv64 || mips64 || ppc64

package feishu

import (
	"encoding/json"
	"regexp"
	"strings"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// Feishu post (rich text) message format:
// {
//   "zh_cn": {
//     "title": "",
//     "content": [
//       [ { "tag": "text", "text": "hello " }, { "tag": "a", "text": "link", "href": "..." } ],
//       [ { "tag": "text", "text": "line 2" } ]
//     ]
//   }
// }
//
// Supported tags: text, a, at, img
// Text supports: bold, italic, underline, strikethrough via "style" array

var (
	reCodeBlock   = regexp.MustCompile("(?s)```\\w*\n?(.*?)```")
	reInlineCode  = regexp.MustCompile("`([^`]+)`")
	reMdLink      = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	reMdBoldStar  = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reMdBoldUnder = regexp.MustCompile(`__(.+?)__`)
	reMdItalic    = regexp.MustCompile(`(?:^|[^*])\*([^*]+)\*(?:[^*]|$)`)
	reMdStrike    = regexp.MustCompile(`~~(.+?)~~`)
	reMdHeading   = regexp.MustCompile(`^#{1,6}\s+(.+)$`)
)

// postElement represents a single element in a Feishu post content line.
type postElement map[string]any

// markdownToFeishuPost converts Markdown text to Feishu post JSON content string.
// Returns the JSON string for MsgTypePost, or an error.
func markdownToFeishuPost(text string) (string, error) {
	lines := strings.Split(text, "\n")
	var contentLines [][]postElement

	i := 0
	for i < len(lines) {
		line := lines[i]

		// Check for code block start
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			var codeLines []string
			i++ // skip opening ```
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
				codeLines = append(codeLines, lines[i])
				i++
			}
			if i < len(lines) {
				i++ // skip closing ```
			}
			codeText := strings.Join(codeLines, "\n")
			// Feishu post doesn't have a native code block tag, use text with code_block style
			contentLines = append(contentLines, []postElement{
				{"tag": "text", "text": codeText, "style": []string{"code_block"}},
			})
			continue
		}

		// Heading: strip # prefix and render as bold text
		if match := reMdHeading.FindStringSubmatch(line); match != nil {
			contentLines = append(contentLines, []postElement{
				{"tag": "text", "text": match[1], "style": []string{"bold"}},
			})
			i++
			continue
		}

		// Empty line
		if strings.TrimSpace(line) == "" {
			contentLines = append(contentLines, []postElement{
				{"tag": "text", "text": ""},
			})
			i++
			continue
		}

		// Normal line — parse inline elements
		elements := parseInlineElements(line)
		if len(elements) > 0 {
			contentLines = append(contentLines, elements)
		}
		i++
	}

	post := map[string]any{
		"zh_cn": map[string]any{
			"content": contentLines,
		},
	}

	data, err := json.Marshal(post)
	return string(data), err
}

// parseInlineElements converts a single line of Markdown into Feishu post elements.
func parseInlineElements(line string) []postElement {
	// Strip list markers
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
		line = "• " + line[2:]
	}
	if len(line) > 2 && line[0] >= '0' && line[0] <= '9' && line[1] == '.' {
		// numbered list, keep as-is
	}

	var elements []postElement

	// We process the line left to right, extracting special elements
	remaining := line

	for remaining != "" {
		// Find the earliest match among: link, bold, italic, strikethrough, inline code
		type match struct {
			start, end int
			typ        string
			groups     []string
		}

		var earliest *match

		if loc := reMdLink.FindStringSubmatchIndex(remaining); loc != nil {
			earliest = &match{start: loc[0], end: loc[1], typ: "link",
				groups: []string{remaining[loc[2]:loc[3]], remaining[loc[4]:loc[5]]}}
		}

		if loc := reInlineCode.FindStringSubmatchIndex(remaining); loc != nil {
			if earliest == nil || loc[0] < earliest.start {
				earliest = &match{start: loc[0], end: loc[1], typ: "code",
					groups: []string{remaining[loc[2]:loc[3]]}}
			}
		}

		if loc := reMdBoldStar.FindStringSubmatchIndex(remaining); loc != nil {
			if earliest == nil || loc[0] < earliest.start {
				earliest = &match{start: loc[0], end: loc[1], typ: "bold",
					groups: []string{remaining[loc[2]:loc[3]]}}
			}
		}

		if loc := reMdBoldUnder.FindStringSubmatchIndex(remaining); loc != nil {
			if earliest == nil || loc[0] < earliest.start {
				earliest = &match{start: loc[0], end: loc[1], typ: "bold",
					groups: []string{remaining[loc[2]:loc[3]]}}
			}
		}

		if loc := reMdStrike.FindStringSubmatchIndex(remaining); loc != nil {
			if earliest == nil || loc[0] < earliest.start {
				earliest = &match{start: loc[0], end: loc[1], typ: "strike",
					groups: []string{remaining[loc[2]:loc[3]]}}
			}
		}

		if earliest == nil {
			// No more special elements, add the rest as plain text
			if remaining != "" {
				elements = append(elements, postElement{"tag": "text", "text": remaining})
			}
			break
		}

		// Add text before the match
		if earliest.start > 0 {
			elements = append(elements, postElement{"tag": "text", "text": remaining[:earliest.start]})
		}

		// Add the matched element
		switch earliest.typ {
		case "link":
			elements = append(elements, postElement{
				"tag":  "a",
				"text": earliest.groups[0],
				"href": earliest.groups[1],
			})
		case "code":
			elements = append(elements, postElement{
				"tag": "text", "text": earliest.groups[0],
				"style": []string{"code_block"},
			})
		case "bold":
			elements = append(elements, postElement{
				"tag": "text", "text": earliest.groups[0],
				"style": []string{"bold"},
			})
		case "strike":
			elements = append(elements, postElement{
				"tag": "text", "text": earliest.groups[0],
				"style": []string{"lineThrough"},
			})
		}

		remaining = remaining[earliest.end:]
	}

	return elements
}

// buildFeishuContent converts content to Feishu message format.
// It tries to render as post (rich text) first. If conversion fails,
// falls back to plain text format.
// Returns (msgType, jsonPayload, error).
func buildFeishuContent(content string) (string, string, error) {
	postPayload, err := markdownToFeishuPost(content)
	if err == nil {
		return larkim.MsgTypePost, postPayload, nil
	}
	// Fallback to plain text
	payload, err := json.Marshal(map[string]string{"text": content})
	if err != nil {
		return "", "", err
	}
	return larkim.MsgTypeText, string(payload), nil
}
