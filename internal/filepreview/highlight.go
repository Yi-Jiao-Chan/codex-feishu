package filepreview

import (
	"html"
	"strings"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"
)

// Large documents remain readable in full without spending their entire file
// allowance on syntax analysis. A Markdown document shares this budget across
// its fenced blocks. Unknown languages and lexer failures retain plain text.
const highlightBytes = 256 << 10

// Fence metadata is a language name, not a filename. Avoid the registry's
// filename-glob fallback for unknown, user-supplied language strings.
var fenceLanguages = func() map[string]chroma.Lexer {
	languages := make(map[string]chroma.Lexer)
	for _, lexer := range lexers.GlobalLexerRegistry.Lexers {
		for _, alias := range lexer.Config().Aliases {
			languages[strings.ToLower(alias)] = lexer
		}
	}
	for _, lexer := range lexers.GlobalLexerRegistry.Lexers {
		languages[strings.ToLower(lexer.Config().Name)] = lexer
	}
	return languages
}()

type codeHighlighter struct{ remaining int }

func (h *codeHighlighter) tokens(source string, lexer chroma.Lexer) (tokens []chroma.Token) {
	plain := []chroma.Token{{Type: chroma.Text, Value: source}}
	tokens = plain
	if lexer == nil || len(source) > h.remaining {
		return tokens
	}
	h.remaining -= len(source)
	// Chroma iterators may panic on a tokenisation error. Rendering must still
	// return the original escaped text, never a partial or unavailable document.
	defer func() {
		if recover() != nil {
			tokens = plain
		}
	}()
	parsed, err := chroma.Tokenise(chroma.Coalesce(lexer), &chroma.TokeniseOptions{State: "root"}, source)
	if err != nil {
		return tokens
	}
	var original strings.Builder
	for _, token := range parsed {
		original.WriteString(token.Value)
	}
	if original.String() != source {
		return tokens
	}
	return parsed
}

func tokenHTML(tokens []chroma.Token) string {
	var output strings.Builder
	for _, token := range tokens {
		class := ""
		for kind := token.Type; kind != 0; kind = kind.Parent() {
			if value, ok := chroma.StandardTypes[kind]; ok {
				class = value
				break
			}
		}
		if class != "" {
			output.WriteString(`<span class="` + class + `">`)
		}
		output.WriteString(html.EscapeString(token.Value))
		if class != "" {
			output.WriteString("</span>")
		}
	}
	return output.String()
}

func highlightCSS() string {
	var css strings.Builder
	formatter := chromahtml.New(chromahtml.WithClasses(true), chromahtml.WithCSSComments(false))
	_ = formatter.WriteCSS(&css, styles.Get("github"))
	return css.String()
}

func (h *codeHighlighter) RegisterFuncs(register renderer.NodeRendererFuncRegisterer) {
	register.Register(ast.KindFencedCodeBlock, h.renderFence)
}

func (h *codeHighlighter) renderFence(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	fence := node.(*ast.FencedCodeBlock)
	var content strings.Builder
	for i := 0; i < fence.Lines().Len(); i++ {
		line := fence.Lines().At(i)
		content.Write(line.Value(source))
	}
	language := string(fence.Language(source))
	var lexer chroma.Lexer
	if len(language) <= 64 && content.Len() <= h.remaining {
		lexer = fenceLanguages[strings.ToLower(language)]
	}
	_, _ = w.WriteString(`<pre class="chroma"><code`)
	if language != "" && len(language) <= 64 {
		_, _ = w.WriteString(` class="language-` + html.EscapeString(language) + `"`)
	}
	_, _ = w.WriteString(">")
	_, _ = w.WriteString(tokenHTML(h.tokens(content.String(), lexer)))
	_, _ = w.WriteString("</code></pre>\n")
	return ast.WalkSkipChildren, nil
}
