package webaccess

import (
	"strings"
	"testing"
)

// TestExtractHTMLKeepsProseAndDropsCode pins the extractor's whole contract in one document:
// narration survives, code and styling do not, block boundaries become line breaks, entities
// are decoded, and the title is read out of the head.
func TestExtractHTMLKeepsProseAndDropsCode(t *testing.T) {
	page := `<!doctype html>
<html><head>
<meta charset="utf-8">
<title>  DeepSeek &amp; 价格  </title>
<style>body { color: red; }</style>
<script>var x = "<p>not prose</p>";</script>
<meta name="description" content="官方价格页">
</head>
<body>
<!-- <script>hidden</script> -->
<h1>模型价格</h1>
<p>输入 &lt;token&gt; ￥0.5 / 百万。</p>
<ul><li>缓存命中 ￥0.1</li><li>输出 ￥1.2</li></ul>
<div>最后更新：2026-09-21&nbsp;10:00</div>
<noscript>请开启 JavaScript</noscript>
<table><tr><td>上下文</td><td>1M</td></tr></table>
</body></html>`

	got := extractHTML([]byte(page))
	if got.Title != "DeepSeek & 价格" {
		t.Errorf("title = %q", got.Title)
	}
	for _, want := range []string{
		"模型价格",
		"输入 <token> ￥0.5 / 百万。",
		"- 缓存命中 ￥0.1", // the list item is on its own line
		"最后更新：2026-09-21 10:00",
		"上下文",
		"1M",
	} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("text must contain %q:\n%s", want, got.Text)
		}
	}
	for _, unwanted := range []string{"color: red", "var x", "not prose", "请开启 JavaScript", "hidden"} {
		if strings.Contains(got.Text, unwanted) {
			t.Errorf("text must not contain %q:\n%s", unwanted, got.Text)
		}
	}
	// Block elements end a line: the heading, each list item and each row are separate lines.
	lines := strings.Split(got.Text, "\n")
	if len(lines) < 5 {
		t.Errorf("expected one line per block element, got %d lines:\n%s", len(lines), got.Text)
	}
}

// TestExtractHTMLHandlesMalformedPages: the web is full of unterminated tags and stray '<'.
// The scanner must return what it read rather than fail or loop.
func TestExtractHTMLHandlesMalformedPages(t *testing.T) {
	for name, page := range map[string]string{
		"unterminated comment": `hello <!-- never closed`,
		"truncated tag":        `hello <a href="http://example.com`,
		"stray angle bracket":  `3 < 4 and 5 > 2`,
		"nested skip":          `<noscript><style>x{}</style>keep</noscript>after`,
		"void element":         `<p>one<img src="x.png">two<hr>three`,
		"self closing script":  `<script/>visible`,
	} {
		got := extractHTML([]byte(page))
		if strings.Contains(got.Text, "color") {
			t.Errorf("%s: text = %q", name, got.Text)
		}
	}
	if got := extractHTML([]byte(`<p>one<img src="x">two`)); !strings.Contains(got.Text, "one") || !strings.Contains(got.Text, "two") {
		t.Errorf("a void element must not swallow the rest of the page: %q", got.Text)
	}
	if got := extractHTML([]byte(`<noscript><style>x{}</style>keep</noscript>after`)); strings.Contains(got.Text, "keep") || !strings.Contains(got.Text, "after") {
		t.Errorf("nested skipped elements must be closed correctly: %q", got.Text)
	}
}

// TestExtractHTMLUsesDescriptionWhenThereIsNoBody: a page whose body is script-rendered still
// has a meta description, and that is often the only readable text in it.
func TestExtractHTMLUsesDescriptionWhenThereIsNoBody(t *testing.T) {
	page := `<html><head><meta property="og:description" content="一句话说明这个页面"></head><body><script>render()</script></body></html>`
	got := extractHTML([]byte(page))
	if !strings.Contains(got.Text, "一句话说明这个页面") {
		t.Fatalf("text = %q", got.Text)
	}
}

// TestDeclaredCharsetAndUTF8Names covers the one encoding decision this package makes: UTF-8
// only, with the declaration read from the document when the header is silent.
func TestDeclaredCharsetAndUTF8Names(t *testing.T) {
	cases := map[string]string{
		`<meta charset="utf-8">`: "utf-8",
		`<META CHARSET='GBK'>`:   "gbk",
		`<meta http-equiv="Content-Type" content="text/html; charset=gb2312">`: "gb2312",
		`<html><head></head>`: " ",
	}
	for page, want := range cases {
		got := declaredCharset(page)
		if strings.TrimSpace(want) == "" {
			continue
		}
		if got != want {
			t.Errorf("declaredCharset(%q) = %q, want %q", page, got, want)
		}
	}
	for name, want := range map[string]bool{
		"": true, "utf-8": true, "UTF8": true, "us-ascii": true, "gbk": false, "gb2312": false,
	} {
		if got := isUTF8Name(name); got != want {
			t.Errorf("isUTF8Name(%q) = %v, want %v", name, got, want)
		}
	}
}
