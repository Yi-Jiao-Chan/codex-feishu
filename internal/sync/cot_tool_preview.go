package sync

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"unicode"

	"codex-feishu/internal/cards"
	"codex-feishu/internal/codexclient"
	"codex-feishu/internal/cot"
)

const cotPreviewReadLimit = 128 * 1024
const cotRedacted = "[已脱敏]"
const cotPreviewOmitted = "…（内容已截断）"
const cotSecretName = `[-a-z0-9_.]*(?:api[_-]?key|token|secret|password|passwd|credential|private[_-]?key|access[_-]?key)[-a-z0-9_.]*`
const cotStringValue = `"""(?s:.*?)(?:"""|$)|'''(?s:.*?)(?:'''|$)|\x60(?:\\.|[^\x60\\])*(?:\x60|$)|"(?:\\.|[^"\\])*(?:"|$)|'(?:\\.|[^'\\])*(?:'|$)|\\"(?s:.*?)(?:\\"|$)|\\'(?s:.*?)(?:\\'|$)`
const cotQuotedValue = `(?:\[已脱敏\]|[rRuUbBfF]{0,2}(?:` + cotStringValue + `)|(?:\\[^\n]|[^\\\s,;|&)\]}"'\x60])+)+`

// These are display redactions for common credential forms, not a general DLP
// detector. Unknown binary/resource payloads never enter this text projection.
var (
	cotANSI        = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*(?:\x07|$))`)
	cotPrivateKey  = regexp.MustCompile(`(?s)-----BEGIN [^-]*PRIVATE KEY-----.*?(?:-----END [^-]*PRIVATE KEY-----|$)`)
	cotAuthHeader  = regexp.MustCompile(`(?im)(["']?(?:authorization|proxy-authorization|cookie|set-cookie)["']?\s*[:=]\s*)([^\r\n]+)`)
	cotSecretValue = regexp.MustCompile(`(?i)(["']?` + cotSecretName + `["']?\s*[:=]\s*|--?` + cotSecretName + `[ \t]+|\b(?:token|secret|password|passwd|api[_-]?key)[ \t]+)(` + cotQuotedValue + `)`)
	cotYAMLSecret  = regexp.MustCompile(`(?i)^([ \t]*)(["']?` + cotSecretName + `["']?[ \t]*:)[ \t]*[|>][-+0-9]*[^\n]*\n?$`)
	cotBasicAuth   = regexp.MustCompile(`(?i)((?:^|[ \t])(?:-[uU](?:[ \t]+|=)?|--(?:user|proxy-user|oauth2-bearer)(?:[ \t]+|=)))(` + cotQuotedValue + `)`)
	cotLiteral     = regexp.MustCompile(`^(?:` + cotQuotedValue + `)`)
	cotBearer      = regexp.MustCompile(`(?i)\b(?:Bearer|Basic)\s+[a-z0-9._~+/=-]+`)
	cotKnownToken  = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9_]{16,}|github_pat_[A-Za-z0-9_]{16,}|xox[baprs]-[A-Za-z0-9-]{12,}|(?:AKIA|ASIA)[A-Z0-9]{16,}|eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+)\b`)
	cotURLUserInfo = regexp.MustCompile(`(://)[^/\s@]+@`)
	cotHomePath    = regexp.MustCompile(`(?:/[A-Za-z0-9_.-]+)?/(?:home|Users)/[^/\s"'<>]+|[A-Za-z]:\\Users\\[^\\\s"'<>]+`)
	cotDataURL     = regexp.MustCompile(`data:[^\s"'<>]+`)
	cotBinaryText  = regexp.MustCompile(`[A-Za-z0-9+/]{256,}={0,2}`)
)

func cotToolPreviews(item codexclient.Item, public *cot.Item) {
	var input, output string
	switch item.Type {
	case "commandExecution":
		input = item.Command
		if item.AggregatedOutput != nil {
			output = *item.AggregatedOutput
			if strings.TrimSpace(output) == "" && public.Status == "completed" {
				public.ResultSummary = "命令未返回输出"
			}
		}
	case "mcpToolCall", "dynamicToolCall":
		input = cotJSONPreview(item.Arguments)
		if input == "{}" || input == "[]" {
			input = ""
		}
		var parts []string
		if item.Type == "mcpToolCall" {
			if item.Error != nil && item.Error.Message != "" {
				parts = append(parts, item.Error.Message)
			}
			if item.Result != nil {
				for _, content := range item.Result.Content {
					if content.Type == "text" && content.Text != "" {
						parts = append(parts, content.Text)
					}
				}
				if len(parts) == 0 {
					parts = append(parts, cotJSONPreview(item.Result.StructuredContent))
				}
			}
		} else {
			for _, content := range item.ContentItems {
				if content.Type == "inputText" && content.Text != "" {
					parts = append(parts, content.Text)
				}
			}
		}
		output = strings.Join(parts, "\n\n")
	case "fileChange":
		var parts []string
		for _, change := range item.Changes {
			if strings.TrimSpace(change.Diff) == "" {
				continue
			}
			name := progressFileName(change.Path)
			if name == "" {
				name = "文件变更"
			}
			parts = append(parts, name+"\n"+change.Diff)
		}
		output = strings.Join(parts, "\n\n")
	}
	public.InputPreview = cotSafePreview(input, item.CWD, cot.MaxToolInputRunes)
	public.OutputPreview = cotSafePreview(output, item.CWD, cot.MaxToolOutputRunes)
}

// Decode JSON before redaction so escaped keys and credential values cannot
// evade key-based masking. Oversized/malformed inputs are never copied raw.
func cotJSONPreview(raw json.RawMessage) string {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ""
	}
	if len(raw) > cotPreviewReadLimit {
		return "内容过长，已省略"
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if !json.Valid(raw) || decoder.Decode(&value) != nil {
		return ""
	}
	encoded, err := json.MarshalIndent(cotRedactJSON(value, 0), "", "  ")
	if err != nil {
		return ""
	}
	return string(encoded)
}

func cotRedactJSON(value any, depth int) any {
	if depth >= 20 {
		return "嵌套内容已省略"
	}
	switch value := value.(type) {
	case map[string]any:
		if cotNonTextObject(value) {
			return "[非文本内容已省略]"
		}
		result := make(map[string]any, len(value))
		for key, child := range value {
			publicKey := cotRedactText(key)
			if cotSensitiveKey(key) {
				result[publicKey] = cotRedacted
			} else if key == "_meta" || cotNonTextKey(key) {
				result[publicKey] = "[非文本内容已省略]"
			} else {
				result[publicKey] = cotRedactJSON(child, depth+1)
			}
		}
		return result
	case []any:
		result := make([]any, len(value))
		for i, child := range value {
			result[i] = cotRedactJSON(child, depth+1)
		}
		return result
	case string:
		// Strings may themselves contain JSON returned by a tool.
		trimmed := strings.TrimSpace(value)
		if json.Valid([]byte(value)) && (strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")) {
			var nested any
			decoder := json.NewDecoder(strings.NewReader(value))
			decoder.UseNumber()
			if decoder.Decode(&nested) == nil {
				encoded, _ := json.Marshal(cotRedactJSON(nested, depth+1))
				return string(encoded)
			}
		}
		return cotRedactText(value)
	default:
		return value
	}
}

func cotNonTextObject(value map[string]any) bool {
	for key, child := range value {
		label, ok := child.(string)
		if !ok {
			continue
		}
		switch strings.ToLower(key) {
		case "type":
			switch strings.ToLower(label) {
			case "image", "inputimage", "image_url", "audio", "inputaudio", "audio_url", "video", "inputvideo", "resource", "resource_link", "blob":
				return true
			}
		case "mimetype", "mime_type":
			for _, prefix := range []string{"image/", "audio/", "video/", "application/octet-stream"} {
				if strings.HasPrefix(strings.ToLower(label), prefix) {
					return true
				}
			}
		case "encoding":
			if strings.EqualFold(label, "base64") {
				return true
			}
		}
	}
	return false
}

func cotNonTextKey(key string) bool {
	switch strings.ToLower(strings.ReplaceAll(key, "_", "")) {
	case "base64", "imagedata", "audiodata", "imageurl", "audiourl", "blob":
		return true
	}
	return false
}

func cotSensitiveKey(key string) bool {
	key = strings.ToLower(strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return r
		}
		return -1
	}, key))
	// Token counts and budgets are useful execution data, not credentials.
	switch key {
	case "tokencount", "inputtokens", "outputtokens", "totaltokens", "cachedtokens", "maxtokens", "maxinputtokens", "maxoutputtokens", "tokenlimit", "tokenbudget", "tokenusage":
		return false
	}
	for _, fragment := range []string{"password", "passwd", "secret", "token", "apikey", "accesskey", "privatekey", "credential", "authorization", "cookie"} {
		if strings.Contains(key, fragment) {
			return true
		}
	}
	return false
}

func cotRedactText(value string) string {
	value = cotANSI.ReplaceAllString(value, "")
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) || unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, value)
	value = cotPrivateKey.ReplaceAllString(value, cotRedacted)
	value = cotRedactYAML(value)
	value = cotSecretValue.ReplaceAllStringFunc(value, func(match string) string {
		parts := cotSecretValue.FindStringSubmatch(match)
		key := strings.TrimRight(parts[1], " \t\r\n=:")
		if !cotSensitiveKey(key) {
			return match
		}
		return parts[1] + cotMaskLiteral(parts[2])
	})
	value = cotBasicAuth.ReplaceAllStringFunc(value, func(match string) string {
		parts := cotBasicAuth.FindStringSubmatch(match)
		if !strings.Contains(parts[2], ":") && !strings.Contains(strings.ToLower(parts[1]), "--oauth2-bearer") {
			return match
		}
		return parts[1] + cotMaskLiteral(parts[2])
	})
	value = cotAuthHeader.ReplaceAllStringFunc(value, func(match string) string {
		parts := cotAuthHeader.FindStringSubmatch(match)
		if strings.HasPrefix(parts[2], `"`) || strings.HasPrefix(parts[2], "'") {
			literal := cotLiteral.FindString(parts[2])
			return parts[1] + cotMaskLiteral(literal) + parts[2][len(literal):]
		}
		return parts[1] + cotRedacted
	})
	value = cotBearer.ReplaceAllString(value, cotRedacted)
	value = cotKnownToken.ReplaceAllString(value, cotRedacted)
	value = cotURLUserInfo.ReplaceAllString(value, "${1}"+cotRedacted+"@")
	value = cotDataURL.ReplaceAllString(value, "[二进制内容已省略]")
	return cotBinaryText.ReplaceAllString(value, "[二进制内容已省略]")
}

func cotMaskLiteral(value string) string {
	for _, quote := range []string{`"""`, "'''", `"`, "'", "`"} {
		if strings.HasPrefix(value, quote) {
			return quote + cotRedacted + quote
		}
	}
	return cotRedacted
}

func cotRedactYAML(value string) string {
	lines := strings.SplitAfter(value, "\n")
	var out strings.Builder
	for i := 0; i < len(lines); i++ {
		parts := cotYAMLSecret.FindStringSubmatch(lines[i])
		if parts == nil {
			out.WriteString(lines[i])
			continue
		}
		out.WriteString(parts[1] + parts[2] + " " + cotRedacted + "\n")
		// Only this key's block belongs to the secret. Keep sibling keys at
		// the same indentation, including ordinary counters and file lists.
		for i+1 < len(lines) {
			next := lines[i+1]
			indent := len(next) - len(strings.TrimLeft(next, " \t"))
			if strings.TrimSpace(next) != "" && indent <= len(parts[1]) {
				break
			}
			i++
		}
	}
	return out.String()
}

func cotSafePreview(value, cwd string, limit int) string {
	if len(value) > cotPreviewReadLimit {
		return "内容过长，已省略"
	}
	if json.Valid([]byte(value)) && strings.TrimSpace(value) != "null" {
		value = cotJSONPreview(json.RawMessage(value))
	} else {
		value = cotRedactText(value)
	}
	if cwd = strings.TrimRight(cwd, "/\\"); len(cwd) > 1 {
		value = strings.ReplaceAll(value, cwd+"/", "")
		value = strings.ReplaceAll(value, cwd+`\`, "")
	}
	value = cotHomePath.ReplaceAllString(value, "~")
	value = strings.Trim(cards.Safe(value), "\n")
	if strings.TrimSpace(value) == "" {
		return ""
	}
	runes := []rune(value)
	if len(runes) > limit {
		// Reserve an explicit marker, and retain the tail where errors and test
		// summaries usually occur. Redaction always precedes this truncation.
		budget := limit - len([]rune(cotPreviewOmitted)) - 2
		head := budget * 2 / 3
		value = strings.Trim(string(runes[:head]), "\n") + "\n" + cotPreviewOmitted + "\n" + strings.Trim(string(runes[len(runes)-(budget-head):]), "\n")
	}
	return value
}
