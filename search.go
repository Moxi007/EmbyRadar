package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// SearchWeb 使用 DuckDuckGo Lite 进行简易网页搜索并返回前几个结果的摘要
// 适合给 AI 作为扩展上下文
func SearchWeb(query string) string {
	if query == "" {
		return "搜索词为空"
	}

	searchURL := "https://lite.duckduckgo.com/lite/"
	
	// 构建 POST 数据
	data := url.Values{}
	data.Set("q", query)

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	req, err := http.NewRequest("POST", searchURL, strings.NewReader(data.Encode()))
	if err != nil {
		log.Printf("[Search] 创建请求失败: %v", err)
		return fmt.Sprintf("搜索失败: %v", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[Search] 请求失败: %v", err)
		return fmt.Sprintf("搜索请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("搜索引擎返回错误状态码: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Sprintf("读取搜索结果失败: %v", err)
	}

	return parseDuckDuckGoLiteHTML(string(body))
}

// 解析 DuckDuckGo Lite 的 HTML，提取标题、链接和摘要
func parseDuckDuckGoLiteHTML(htmlContent string) string {
	doc, err := html.Parse(strings.NewReader(htmlContent))
	if err != nil {
		return "解析搜索结果 HTML 失败"
	}

	var results []string

	// 遍历 HTML 节点
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			// 提取标题
			for _, a := range n.Attr {
				if a.Key == "class" && strings.Contains(a.Val, "result-snippet") {
					// 这里的 class 叫 result-snippet 但它经常包着标题（在不同版本的 Lite 页面中可能有差异）
					// 为适应常见的表格布局，我们需要在 td 中找
				}
			}
		}

		if n.Type == html.ElementNode && n.Data == "tr" {
			// DDG Lite 使用 tr 包装每个结果
			// 标题和链接在包含 class="result-title" 的 a 标签里
			// 摘要在 class="result-snippet" 的 td 里
			// 新版的 DDG table 并没有特别好的 tr class，我们用另一种方式收集
			class := getAttr(n, "class")
			if class != "" {
			}
		}

		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}

	// 第二种更鲁棒平铺解析法：分别收集 class="result-snippet" 的 a 内容作为标题
	// class="result-snippet" 的 td 内容作为摘要
	var titles []string
	var snippets []string

	var collect func(*html.Node)
	collect = func(n *html.Node) {
		if n.Type == html.ElementNode {
			class := getAttr(n, "class")
			if n.Data == "a" && strings.Contains(class, "result-snippet") {
				titles = append(titles, getTextContent(n))
			} else if n.Data == "td" && strings.Contains(class, "result-snippet") {
				snippets = append(snippets, getTextContent(n))
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			collect(c)
		}
	}

	collect(doc)

	// 合并结果
	count := len(titles)
	if len(snippets) < count {
		count = len(snippets)
	}

	if count == 0 {
		return "未找到相关的搜索结果摘要。"
	}

	// 最多返回前 5 条结果
	maxResults := 5
	if count > maxResults {
		count = maxResults
	}

	for i := 0; i < count; i++ {
		t := strings.TrimSpace(titles[i])
		s := strings.TrimSpace(snippets[i])
		if t != "" && s != "" {
			resultStr := fmt.Sprintf("【标题】: %s\n【摘要】: %s\n", t, s)
			results = append(results, resultStr)
		}
	}

	if len(results) == 0 {
		return "搜到了结果网页但无法解析出文本摘要。"
	}

	return strings.Join(results, "\n")
}

func getAttr(n *html.Node, key string) string {
	for _, attr := range n.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

func getTextContent(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(getTextContent(c))
	}
	return b.String()
}
