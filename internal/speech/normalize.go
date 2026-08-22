package speech

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const omittedMarker = "\x00MOHUDDLE_SCREEN_REFERENCE\x00"

var (
	fencePattern       = regexp.MustCompile(`^\s*(` + "```" + `|~~~)\s*([^\s]*)`)
	tableRulePattern   = regexp.MustCompile(`^\s*\|?\s*:?-{3,}:?\s*(\|\s*:?-{3,}:?\s*)+\|?\s*$`)
	markdownImage      = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	markdownLink       = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	inlineCode         = regexp.MustCompile("`{1,2}[^`\\n]+`{1,2}")
	bareURL            = regexp.MustCompile(`https?://[^\s<>()]+`)
	htmlComment        = regexp.MustCompile(`(?s)<!--.*?-->`)
	htmlTag            = regexp.MustCompile(`<[^>]+>`)
	headingPrefix      = regexp.MustCompile(`^\s{0,3}#{1,6}\s+`)
	listPrefix         = regexp.MustCompile(`^\s*(?:[-*+]\s+|\d+[.)]\s+)`)
	quotePrefix        = regexp.MustCompile(`^\s*>+\s?`)
	horizontalRule     = regexp.MustCompile(`^\s*(?:[-*_]\s*){3,}$`)
	stackStartPattern  = regexp.MustCompile(`(?i)^\s*(traceback \(most recent call last\):|panic:|exception in thread|fatal error:)`)
	stackFramePattern  = regexp.MustCompile(`^\s*(?:at\s+\S+\(|File\s+"[^"]+",\s+line\s+\d+|\S+\.go:\d+|\S+\.py:\d+)`)
	codeLinePattern    = regexp.MustCompile(`^\s*(?:package|import|func|type|class|def|const|var|let|SELECT|INSERT|UPDATE|DELETE)\b|^\s*[{}\[\]();]+\s*$`)
	markdownDecoration = regexp.MustCompile(`\*{1,3}|~{2}`)
)

// Normalize creates a speech-only copy. It retains natural prose, removes
// presentation markup, and replaces non-natural blocks with one combined cue.
func Normalize(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = htmlComment.ReplaceAllString(value, "")
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	if (strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")) && json.Valid([]byte(trimmed)) {
		return screenCue([]string{"structured data"})
	}

	lines := strings.Split(value, "\n")
	var output []string
	var omitted []string
	addOmitted := func(kind string) {
		omitted = appendIfMissing(omitted, kind)
		output = append(output, omittedMarker)
	}

	for index := 0; index < len(lines); {
		line := lines[index]
		if match := fencePattern.FindStringSubmatch(line); len(match) > 0 {
			kind := "code"
			language := strings.ToLower(match[2])
			if language == "json" || language == "yaml" || language == "yml" || language == "xml" {
				kind = "structured data"
			}
			fence := match[1]
			index++
			for index < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[index]), fence) {
				index++
			}
			if index < len(lines) {
				index++
			}
			addOmitted(kind)
			continue
		}
		if index+1 < len(lines) && strings.Contains(line, "|") && tableRulePattern.MatchString(lines[index+1]) {
			index += 2
			for index < len(lines) && strings.Contains(lines[index], "|") && strings.TrimSpace(lines[index]) != "" {
				index++
			}
			addOmitted("table")
			continue
		}
		if stackStartPattern.MatchString(line) || stackFramePattern.MatchString(line) {
			index++
			for index < len(lines) && strings.TrimSpace(lines[index]) != "" {
				index++
			}
			addOmitted("technical details")
			continue
		}
		if startsTechnicalBlock(lines, index) {
			index += 3
			for index < len(lines) && (isCodeLine(lines[index]) || strings.TrimSpace(lines[index]) == "") {
				index++
			}
			addOmitted("technical details")
			continue
		}
		if horizontalRule.MatchString(line) {
			index++
			continue
		}

		line = markdownImage.ReplaceAllStringFunc(line, func(string) string {
			omitted = appendIfMissing(omitted, "image")
			return omittedMarker
		})
		line = markdownLink.ReplaceAllString(line, "$1")
		line = inlineCode.ReplaceAllStringFunc(line, func(string) string {
			omitted = appendIfMissing(omitted, "code")
			return omittedMarker
		})
		line = bareURL.ReplaceAllStringFunc(line, func(string) string {
			omitted = appendIfMissing(omitted, "link")
			return omittedMarker
		})
		line = headingPrefix.ReplaceAllString(line, "")
		line = listPrefix.ReplaceAllString(line, "")
		line = quotePrefix.ReplaceAllString(line, "")
		line = htmlTag.ReplaceAllString(line, "")
		line = markdownDecoration.ReplaceAllString(line, "")
		output = append(output, strings.TrimSpace(line))
		index++
	}

	joined := strings.Join(output, "\n")
	if len(omitted) > 0 {
		cue := screenCue(omitted)
		joined = strings.Replace(joined, omittedMarker, cue, 1)
		joined = strings.ReplaceAll(joined, omittedMarker, "")
	}
	return collapseSpeechWhitespace(joined)
}

func startsTechnicalBlock(lines []string, index int) bool {
	if index+2 >= len(lines) {
		return false
	}
	return isCodeLine(lines[index]) && isCodeLine(lines[index+1]) && isCodeLine(lines[index+2])
}

func isCodeLine(line string) bool {
	if strings.TrimSpace(line) == "" {
		return false
	}
	spaces := len(line) - len(strings.TrimLeft(line, " \t"))
	return spaces >= 4 || codeLinePattern.MatchString(line)
}

func appendIfMissing(values []string, value string) []string {
	for _, current := range values {
		if current == value {
			return values
		}
	}
	return append(values, value)
}

func screenCue(kinds []string) string {
	if len(kinds) == 0 {
		return ""
	}
	// Stable ordering makes the cue and tests deterministic while remaining
	// content-specific.
	order := map[string]int{"code": 0, "table": 1, "structured data": 2, "technical details": 3, "link": 4, "image": 5}
	sort.SliceStable(kinds, func(i, j int) bool { return order[kinds[i]] < order[kinds[j]] })
	var description string
	switch len(kinds) {
	case 1:
		description = kinds[0]
	case 2:
		description = kinds[0] + " and " + kinds[1]
	default:
		description = strings.Join(kinds[:len(kinds)-1], ", ") + ", and " + kinds[len(kinds)-1]
	}
	return "Refer to the " + description + " on screen."
}

func collapseSpeechWhitespace(value string) string {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return ""
	}
	return strings.Join(fields, " ")
}

func Chunk(value string, limit int) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if limit < 1 {
		limit = DefaultChunkChars
	}
	var chunks []string
	for utf8.RuneCountInString(value) > limit {
		runes := []rune(value)
		cut := sentenceBoundary(runes, limit)
		if cut == 0 {
			cut = wordBoundary(runes, limit)
		}
		if cut == 0 {
			cut = limit
		}
		chunk := strings.TrimSpace(string(runes[:cut]))
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
		value = strings.TrimSpace(string(runes[cut:]))
	}
	if value != "" {
		chunks = append(chunks, value)
	}
	return chunks
}

func sentenceBoundary(value []rune, limit int) int {
	last := 0
	for index := 0; index < limit && index+1 < len(value); index++ {
		if strings.ContainsRune(".!?", value[index]) && unicode.IsSpace(value[index+1]) {
			last = index + 1
		}
	}
	return last
}

func wordBoundary(value []rune, limit int) int {
	for index := limit; index > 0; index-- {
		if unicode.IsSpace(value[index-1]) {
			return index - 1
		}
	}
	return 0
}
