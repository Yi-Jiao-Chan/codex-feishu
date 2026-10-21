package codexclient

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func visualizationFixture(payload string) string {
	return visualizationStart + visualizationData + payload + visualizationEnd
}

func TestRewriteVisualizationsValidPathsAndFallback(t *testing.T) {
	for _, path := range []string{"/workspace/chart.html", "/workspace/chart.HTM", "/workspace/图 (一).xhtml"} {
		body := "前文\n\n" + visualizationFixture(`{"path":"`+path+`"}`) + "\n\n后文"
		got, err := RewriteVisualizations(body, func(actual string) (string, error) {
			if actual != path {
				t.Fatalf("renderer path = %q, want %q", actual, path)
			}
			return "[查看可视化](https://preview.invalid/f/chart)", nil
		})
		want := "前文\n\n[查看可视化](https://preview.invalid/f/chart)\n\n后文"
		if err != nil || got != want {
			t.Fatalf("display = %q, error %v", got, err)
		}
		got, err = RewriteVisualizations(body, nil)
		if err != nil || got != "前文\n\n"+VisualizationFallback+"\n\n后文" {
			t.Fatalf("fallback = %q, error %v", got, err)
		}
		got, err = RewriteVisualizations(body, func(string) (string, error) { return "", nil })
		if err != nil || got != "前文\n\n\n\n后文" {
			t.Fatalf("empty renderer did not remove envelope: %q, error %v", got, err)
		}
	}
}

func TestRewriteVisualizationsRejectsInvalidEnvelopes(t *testing.T) {
	cases := map[string]string{
		"relative":            `{"path":"chart.html"}`,
		"url":                 `{"path":"https://example.invalid/chart.html"}`,
		"file URL":            `{"path":"file:///workspace/chart.html"}`,
		"traversal":           `{"path":"/workspace/../chart.html"}`,
		"backslash":           `{"path":"/workspace/sub\\chart.html"}`,
		"nul":                 `{"path":"/workspace/cha\u0000rt.html"}`,
		"newline":             `{"path":"/workspace/cha\nrt.html"}`,
		"delete":              `{"path":"/workspace/cha\u007frt.html"}`,
		"extension":           `{"path":"/workspace/chart.md"}`,
		"query":               `{"path":"/workspace/chart.html?mode=1"}`,
		"fragment":            `{"path":"/workspace/chart.html#view"}`,
		"empty":               `{"path":""}`,
		"null path":           `{"path":null}`,
		"number path":         `{"path":4}`,
		"wrong case":          `{"Path":"/workspace/chart.html"}`,
		"duplicate":           `{"path":"/workspace/chart.html","path":"/workspace/other.html"}`,
		"extra field":         `{"path":"/workspace/chart.html","title":"chart"}`,
		"unknown field first": `{"title":"chart","path":"/workspace/chart.html"}`,
		"empty object":        `{}`,
		"array":               `[{"path":"/workspace/chart.html"}]`,
		"null":                `null`,
		"trailing object":     `{"path":"/workspace/chart.html"}{}`,
		"trailing comma":      `{"path":"/workspace/chart.html",}`,
		"incomplete JSON":     `{"path":"/workspace/chart.html"`,
		"invalid UTF8":        "{\"path\":\"/workspace/\xff.html\"}",
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			body := "before " + visualizationFixture(payload) + " after"
			got, err := RewriteVisualizations(body, func(string) (string, error) {
				t.Fatal("invalid envelope reached renderer")
				return "unexpected", nil
			})
			if err != nil || got != "before "+VisualizationFallback+" after" {
				t.Fatalf("invalid envelope = %q, error %v", got, err)
			}
		})
	}
}

func TestRewriteVisualizationsPreservesLiteralMarkdownAndUnknownMarkers(t *testing.T) {
	visual := visualizationFixture(`{"path":"/workspace/chart.html"}`)
	cases := map[string]string{
		"ordinary":         "ordinary [file](/workspace/report.md)",
		"unknown":          "\ue200other\ue202{}\ue201",
		"similar name":     "\ue200visualize_other\ue202{}\ue201",
		"dotted name":      "\ue200visualize.other\ue202{}\ue201",
		"inline":           "`" + visual + "`",
		"wide inline":      "``literal ` " + visual + "``",
		"multiline inline": "`first\n" + visual + "\nlast`",
		"fenced":           "```text\n" + visual + "\n```",
		"unclosed fence":   "~~~text\n" + visual,
		"quoted fence":     "> ```text\n> " + visual + "\n> ```",
		"list fence":       "- item\n\n  ```text\n  " + visual + "\n  ```",
		"fence info":       "~~~" + visual + "\ntext\n~~~",
		"indented":         "    " + visual + "\n",
		"tab indented":     "\t" + visual + "\n",
		"escaped":          "\\" + visual,
		"odd escapes":      "\\\\\\" + visual,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := RewriteVisualizations(body, func(string) (string, error) {
				t.Fatal("literal or unknown marker reached renderer")
				return "unexpected", nil
			})
			if err != nil || got != body {
				t.Fatalf("literal text changed: %q, error %v", got, err)
			}
		})
	}
	// The same marker in code and prose must be handled by source position.
	body := "`" + visual + "`\n\n" + visual + "\n\n\\\\" + visual
	got, err := RewriteVisualizations(body, nil)
	want := "`" + visual + "`\n\n" + VisualizationFallback + "\n\n\\\\" + VisualizationFallback
	if err != nil || got != want {
		t.Fatalf("mixed code and prose = %q, error %v", got, err)
	}
}

func TestRewriteVisualizationsIncompleteBoundaries(t *testing.T) {
	unknown := "\ue200other\ue202{}\ue201"
	visual := visualizationFixture(`{"path":"/workspace/chart.html"}`)
	cases := map[string]struct{ body, want string }{
		"header only":          {visualizationStart, VisualizationFallback},
		"payload tail":         {visualizationStart + visualizationData + `{"path":"/workspace/`, VisualizationFallback},
		"next line":            {visualizationStart + visualizationData + `{"path":` + "\n正常正文", VisualizationFallback + "\n正常正文"},
		"CRLF":                 {visualizationStart + visualizationData + "{\r\n正常正文", VisualizationFallback + "\r\n正常正文"},
		"next marker":          {visualizationStart + visualizationData + "{" + unknown, VisualizationFallback + unknown},
		"next visualization":   {visualizationStart + visualizationData + "{" + visual, VisualizationFallback + VisualizationFallback},
		"unknown on next line": {visualizationStart + visualizationData + "{\n正常正文\n" + unknown, VisualizationFallback + "\n正常正文\n" + unknown},
		"missing separator":    {visualizationStart + `{"path":"/workspace/chart.html"}` + visualizationEnd, VisualizationFallback},
		"formatted JSON":       {visualizationFixture("{\n\"path\": \"/workspace/chart.html\"\n}"), VisualizationFallback},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := RewriteVisualizations(tc.body, nil)
			if err != nil || got != tc.want {
				t.Fatalf("bounded fallback = %q, want %q, error %v", got, tc.want, err)
			}
		})
	}
}

func TestRewriteVisualizationsRendererFailureIsAtomicAndIdempotent(t *testing.T) {
	first := visualizationFixture(`{"path":"/workspace/first.html"}`)
	second := visualizationFixture(`{"path":"/workspace/second.html"}`)
	body := first + "\n\n" + second
	failure := errors.New("renderer failure")
	var paths []string
	got, err := RewriteVisualizations(body, func(path string) (string, error) {
		paths = append(paths, path)
		if len(paths) == 2 {
			return "", failure
		}
		return "[static preview](https://preview.invalid/f/first)", nil
	})
	if got != body || err != failure || !reflect.DeepEqual(paths, []string{"/workspace/first.html", "/workspace/second.html"}) {
		t.Fatalf("renderer failure returned partial output or changed error: %q, %v, %v", got, err, paths)
	}
	got, err = RewriteVisualizations(body, nil)
	if err != nil || strings.Count(got, VisualizationFallback) != 2 {
		t.Fatalf("fallback conversion = %q, error %v", got, err)
	}
	again, err := RewriteVisualizations(got, nil)
	if err != nil || again != got {
		t.Fatal("repeated display preparation changed the result")
	}
}

func TestRewriteVisualizationsDamagedEnvelopeDoesNotConsumeLaterProseOrCode(t *testing.T) {
	header := visualizationStart + visualizationData + "{"
	cases := map[string]struct{ body, want string }{
		"fenced closing marker": {
			header + "\n\n正常正文\n\n```text\n" + visualizationEnd + "\n```",
			VisualizationFallback + "\n\n正常正文\n\n```text\n" + visualizationEnd + "\n```",
		},
		"inline closing marker": {
			header + "\n\n正常正文\n\n`" + visualizationEnd + "`",
			VisualizationFallback + "\n\n正常正文\n\n`" + visualizationEnd + "`",
		},
		"indented closing marker": {
			header + "\n\n正常正文\n\n    " + visualizationEnd + "\n",
			VisualizationFallback + "\n\n正常正文\n\n    " + visualizationEnd + "\n",
		},
		"same line inline closing marker": {
			header + "` " + visualizationEnd + " ` 后文",
			VisualizationFallback + "` " + visualizationEnd + " ` 后文",
		},
		"multiline inline closing marker": {
			header + "``first\n" + visualizationEnd + "\nlast`` 后文",
			VisualizationFallback + "``first\n" + visualizationEnd + "\nlast`` 后文",
		},
		"missing separator across prose": {
			visualizationStart + "{\n\n正常正文\n\n" + visualizationEnd + "后文",
			VisualizationFallback + "\n\n正常正文\n\n" + visualizationEnd + "后文",
		},
		"malformed payload across prose": {
			header + "\n\n正常正文\n\n" + visualizationEnd + "后文",
			VisualizationFallback + "\n\n正常正文\n\n" + visualizationEnd + "后文",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := RewriteVisualizations(tc.body, func(string) (string, error) {
				t.Fatal("damaged envelope reached renderer")
				return "unexpected", nil
			})
			if err != nil || got != tc.want {
				t.Fatalf("damaged envelope = %q, want %q, error %v", got, tc.want, err)
			}
		})
	}
	// Formatting whitespace is allowed in a complete, unambiguous JSON object.
	body := visualizationFixture("{\n\n  \"path\": \"/workspace/chart.html\"\n}") + "\n后文"
	got, err := RewriteVisualizations(body, func(path string) (string, error) {
		if path != "/workspace/chart.html" {
			t.Fatalf("multiline path = %q", path)
		}
		return "preview", nil
	})
	if err != nil || got != "preview\n后文" {
		t.Fatalf("formatted JSON no longer renders: %q, error %v", got, err)
	}
}

func TestParseVisualizationsUsesOriginalByteCoordinatesAndPaths(t *testing.T) {
	firstPath := "/workspace/[report](/authorized/report.md)/chart.html"
	first := visualizationFixture(`{"path":"` + firstPath + `"}`)
	invalid := visualizationFixture(`{"path":"relative.html"}`)
	last := visualizationFixture("{\n  \"path\": \"/workspace/图.xhtml\"\n}")
	prefix, between, suffix := "中文前文\n\n", "\n\n中间正文\n\n", "\n\n后文"
	body := prefix + first + between + invalid + between + last + suffix
	firstStart := len(prefix)
	invalidStart := firstStart + len(first) + len(between)
	lastStart := invalidStart + len(invalid) + len(between)
	want := []VisualizationSpan{
		{Start: firstStart, Stop: firstStart + len(first), Path: firstPath},
		{Start: invalidStart, Stop: invalidStart + len(invalid)},
		{Start: lastStart, Stop: lastStart + len(last), Path: "/workspace/图.xhtml"},
	}
	got := ParseVisualizations(body)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("spans = %#v, want %#v", got, want)
	}
	for n, original := range []string{first, invalid, last} {
		if body[got[n].Start:got[n].Stop] != original {
			t.Fatalf("span %d no longer addresses the original envelope", n)
		}
	}
}

func TestParseVisualizationsExcludesLiteralAndUnknownMarkers(t *testing.T) {
	visual := visualizationFixture(`{"path":"/workspace/chart.html"}`)
	literal := strings.Join([]string{
		"`" + visual + "`",
		"```text\n" + visual + "\n```",
		"    " + visual,
		"\\" + visual,
		"\ue200other\ue202{}\ue201",
		"\ue200visualize.other\ue202{}\ue201",
	}, "\n\n")
	if spans := ParseVisualizations(literal); len(spans) != 0 {
		t.Fatalf("literal examples produced spans: %#v", spans)
	}
	body := literal + "\n\n" + visual + "\n\n" + literal
	start := len(literal) + 2
	want := []VisualizationSpan{{Start: start, Stop: start + len(visual), Path: "/workspace/chart.html"}}
	if spans := ParseVisualizations(body); !reflect.DeepEqual(spans, want) {
		t.Fatalf("mixed literal/prose spans = %#v, want %#v", spans, want)
	}
}

func TestParseVisualizationsReportsConservativeFallbackBoundaries(t *testing.T) {
	incomplete := visualizationStart + visualizationData + "{"
	malformed := visualizationFixture(`{"path":"/workspace/chart.html","extra":true}`)
	good := visualizationFixture(`{"path":"/workspace/chart.html"}`)
	for name, tc := range map[string]struct {
		body string
		want []VisualizationSpan
	}{
		"code terminator": {
			body: incomplete + "\n\n正文\n\n```\n" + visualizationEnd + "\n```",
			want: []VisualizationSpan{{Start: 0, Stop: len(incomplete)}},
		},
		"inline boundary": {
			body: incomplete + "` " + visualizationEnd + " `",
			want: []VisualizationSpan{{Start: 0, Stop: len(incomplete)}},
		},
		"complete malformed": {
			body: malformed,
			want: []VisualizationSpan{{Start: 0, Stop: len(malformed)}},
		},
		"next envelope": {
			body: incomplete + good,
			want: []VisualizationSpan{
				{Start: 0, Stop: len(incomplete)},
				{Start: len(incomplete), Stop: len(incomplete) + len(good), Path: "/workspace/chart.html"},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if spans := ParseVisualizations(tc.body); !reflect.DeepEqual(spans, tc.want) {
				t.Fatalf("fallback boundaries = %#v, want %#v", spans, tc.want)
			}
		})
	}
}
