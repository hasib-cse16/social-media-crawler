package meta

import (
	"bytes"
	"encoding/json"
	"html"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Meta's pages do not embed one tidy state blob the way TikTok's do. Their
// payloads are Relay responses spread over many <script> tags, and the pieces
// worth reading are frequently JSON that has itself been serialised *into* a
// JSON string, so the markup contains \"shortcode_media\":{\"id\":\"...\"}.
// The helpers here deal with both facts: unescape first, then pull out balanced
// objects by key rather than trying to model the whole document.

// unescapeJSONString turns the doubly-escaped fragments in a Meta page into
// plain JSON text. It is deliberately blunt — the result is only ever fed to
// the balanced-object scanner below, never parsed as a whole — so the goal is
// to make embedded objects findable, not to produce a valid document.
func unescapeJSONString(body []byte) []byte {
	s := string(body)
	if !strings.Contains(s, `\"`) {
		return body
	}
	r := strings.NewReplacer(
		`\"`, `"`,
		`\\/`, `/`,
		`\/`, `/`,
		`\n`, "\n",
		`\t`, "\t",
	)
	return []byte(r.Replace(s))
}

// findObject returns the balanced JSON object that follows the first occurrence
// of `"key":` in body, or nil when the key is absent. Brace counting is
// string-aware so a `}` inside a caption cannot end the object early.
func findObject(body []byte, key string) []byte {
	needle := []byte(`"` + key + `":`)
	from := 0
	for {
		idx := bytes.Index(body[from:], needle)
		if idx < 0 {
			return nil
		}
		start := from + idx + len(needle)
		for start < len(body) && (body[start] == ' ' || body[start] == '\n' || body[start] == '\t') {
			start++
		}
		if start < len(body) && body[start] == '{' {
			if obj := balancedObject(body[start:]); obj != nil {
				return obj
			}
		}
		from = start
		if from >= len(body) {
			return nil
		}
	}
}

// balancedObject reads one complete JSON object from the front of body.
func balancedObject(body []byte) []byte {
	if len(body) == 0 || body[0] != '{' {
		return nil
	}
	depth := 0
	inString := false
	escaped := false
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\':
			escaped = true
		case inString:
			if c == '"' {
				inString = false
			}
		case c == '"':
			inString = true
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return body[:i+1]
			}
		}
	}
	return nil // truncated
}

// numberAfterKey pulls a bare integer written as `"key":123` or `"key":"123"`.
// It is the fallback for counters that appear in Meta's Relay payloads without
// a surrounding object we can model.
func numberAfterKey(body []byte, key string) *uint64 {
	re := regexp.MustCompile(`"` + regexp.QuoteMeta(key) + `"\s*:\s*"?(\d{1,20})"?`)
	m := re.FindSubmatch(body)
	if m == nil {
		return nil
	}
	n, err := strconv.ParseUint(string(m[1]), 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

// metaContentRe extracts an OpenGraph/meta tag's content by property name.
// Attribute order is not stable across Meta's renderers, so both orders are
// matched rather than assuming property comes first.
func metaContent(body []byte, property string) string {
	p := regexp.QuoteMeta(property)
	patterns := []string{
		`<meta[^>]+(?:property|name)="` + p + `"[^>]+content="([^"]*)"`,
		`<meta[^>]+content="([^"]*)"[^>]+(?:property|name)="` + p + `"`,
	}
	for _, pattern := range patterns {
		if m := regexp.MustCompile(pattern).FindSubmatch(body); m != nil {
			return html.UnescapeString(string(m[1]))
		}
	}
	return ""
}

// humanCountRe matches the counters Meta writes into og:description, e.g.
// "1,234 likes, 56 comments" or "1.2M views".
var humanCountRe = regexp.MustCompile(`(?i)([0-9][0-9.,\x{00A0} ]*)\s*([KMB])?\s+(views|plays|likes|comments|shares)`)

// parseHumanCounts reads the counters out of an og:description string.
//
// These are the *rendered* figures, so anything Meta abbreviates ("1.2M") comes
// back rounded — 1.2M is returned as 1,200,000, not the true value. They are
// used only when the exact figures are not in the page payload, and the caller
// notes the loss of precision in its log line.
func parseHumanCounts(desc string) map[string]uint64 {
	out := map[string]uint64{}
	for _, m := range humanCountRe.FindAllStringSubmatch(desc, -1) {
		n, ok := parseHumanNumber(m[1], m[2])
		if !ok {
			continue
		}
		label := strings.ToLower(m[3])
		if label == "plays" {
			label = "views"
		}
		if _, seen := out[label]; !seen {
			out[label] = n
		}
	}
	return out
}

// parseHumanNumber converts "1,234" / "1.2" + "M" into an integer.
func parseHumanNumber(digits, suffix string) (uint64, bool) {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case ',', ' ', ' ':
			return -1
		}
		return r
	}, strings.TrimSpace(digits))
	if cleaned == "" {
		return 0, false
	}

	multiplier := 1.0
	switch strings.ToUpper(suffix) {
	case "K":
		multiplier = 1e3
	case "M":
		multiplier = 1e6
	case "B":
		multiplier = 1e9
	}

	if multiplier == 1 {
		// No suffix: the figure is exact, so keep it exact rather than routing
		// it through a float.
		cleaned = strings.ReplaceAll(cleaned, ".", "")
		n, err := strconv.ParseUint(cleaned, 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}

	f, err := strconv.ParseFloat(cleaned, 64)
	if err != nil || f < 0 || math.IsInf(f, 0) {
		return 0, false
	}
	return uint64(math.Round(f * multiplier)), true
}

// decodeInto unmarshals obj into v, reporting whether it worked. Payload shapes
// change without notice, so a decode failure is a reason to fall back to the
// next extraction strategy, not to fail the request.
func decodeInto(obj []byte, v any) bool {
	return obj != nil && json.Unmarshal(obj, v) == nil
}

// stringAfterKey pulls a string value written as `"key":"value"`. Escaped
// characters inside the value are left as they are; callers use it for ids and
// names, where escapes do not occur in practice.
func stringAfterKey(body []byte, key string) string {
	re := regexp.MustCompile(`"` + regexp.QuoteMeta(key) + `"\s*:\s*"([^"\\]{1,512})"`)
	m := re.FindSubmatch(body)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// truncate shortens a string for logging.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
