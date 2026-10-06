package webfetch

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

// 基础提取使用结构化 HTML 解析；不执行脚本、不读取外链或模拟浏览器样式。
// 这里只做必要的 Unicode 字符预算，不复制 basic 的文件行数截断与统计逻辑。
func extractWebText(ctx context.Context, body []byte, mediaType string) (string, string, error) {
	if mediaType != "text/html" {
		return "", cleanWebText(string(body)), nil
	}
	root, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("解析 HTML 失败: %w", err)
	}
	title, contentRoot := "", root
	stack := []*html.Node{root}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n.Type == html.ElementNode && n.Data == "body" {
			contentRoot = n
		}
		if n.Type == html.ElementNode && n.Data == "title" && title == "" {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.TextNode {
					title += c.Data
				}
			}
			title = strings.Join(strings.Fields(cleanWebText(title)), " ")
		}
		for c := n.LastChild; c != nil; c = c.PrevSibling {
			stack = append(stack, c)
		}
	}
	type visit struct {
		node *html.Node
		exit bool
	}
	visits := []visit{{node: contentRoot}}
	var text strings.Builder
	for len(visits) > 0 {
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
		v := visits[len(visits)-1]
		visits = visits[:len(visits)-1]
		n := v.node
		if n.Type == html.TextNode {
			text.WriteString(n.Data)
			continue
		}
		if n.Type == html.ElementNode && skipWebElement(n) {
			continue
		}
		block := n.Type == html.ElementNode && webBlockElement(n.Data)
		if block {
			text.WriteByte('\n')
		}
		if v.exit {
			continue
		}
		if block {
			visits = append(visits, visit{node: n, exit: true})
		}
		for c := n.LastChild; c != nil; c = c.PrevSibling {
			visits = append(visits, visit{node: c})
		}
	}
	lines := strings.Split(cleanWebText(text.String()), "\n")
	var normalized []string
	for _, line := range lines {
		if line = strings.Join(strings.Fields(line), " "); line != "" {
			normalized = append(normalized, line)
		}
	}
	return title, strings.Join(normalized, "\n"), nil
}

func skipWebElement(n *html.Node) bool {
	switch n.Data {
	case "head", "script", "style", "noscript", "iframe", "object", "embed", "template", "svg", "canvas":
		return true
	}
	for _, a := range n.Attr {
		if a.Key == "hidden" || (a.Key == "aria-hidden" && strings.EqualFold(a.Val, "true")) {
			return true
		}
	}
	return false
}

func webBlockElement(name string) bool {
	switch name {
	case "p", "div", "br", "hr", "main", "article", "section", "header", "footer", "nav", "aside", "h1", "h2", "h3", "h4", "h5", "h6", "li", "ul", "ol", "pre", "blockquote", "table", "tr", "td", "th", "dl", "dt", "dd":
		return true
	}
	return false
}

func cleanWebText(text string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return -1
		}
		return r
	}, text))
}

// 按 UTF-8 边界切片，不会在中文字符中间截断，也不建立整份 rune 副本。
func limitWebRunes(text string, maximum int) string {
	count := 0
	for offset := range text {
		if count == maximum {
			return text[:offset]
		}
		count++
	}
	return text
}
