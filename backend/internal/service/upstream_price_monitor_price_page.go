package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"golang.org/x/net/html"
)

const x5m5xPricingPageURL = "https://api.x5m5x.com/pricing/"
const x5m5xPricingPageMaxBytes = 2 << 20

type UpstreamPricePageFetcher interface {
	FetchPerRequestPrices(context.Context) (map[string]domain.UpstreamPriceVector, error)
}

type UpstreamTokenDisplayPrice struct {
	// ModelName preserves the canonical spelling advertised in the row's title
	// attribute. The result map itself remains keyed by a lower-case name for
	// case-insensitive lookups.
	ModelName          string
	OfficialInput      *float64
	OfficialOutput     *float64
	OfficialCacheWrite *float64
	OfficialCacheRead  *float64
	SellingInput       *float64
	SellingOutput      *float64
	SellingCacheWrite  *float64
	SellingCacheRead   *float64
}

type UpstreamTokenPricePageFetcher interface {
	FetchTokenPrices(context.Context) (map[string]UpstreamTokenDisplayPrice, error)
}

type x5m5xPricePageFetcher struct{ client *http.Client }

func NewX5M5XPricePageFetcher() UpstreamPricePageFetcher {
	return newX5M5XPricePageFetcher()
}

func NewX5M5XTokenPricePageFetcher() UpstreamTokenPricePageFetcher {
	return newX5M5XPricePageFetcher()
}

func newX5M5XPricePageFetcher() *x5m5xPricePageFetcher {
	return &x5m5xPricePageFetcher{client: &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || !strings.EqualFold(req.URL.Hostname(), "api.x5m5x.com") {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}}
}

func (f *x5m5xPricePageFetcher) FetchPerRequestPrices(ctx context.Context) (map[string]domain.UpstreamPriceVector, error) {
	doc, err := f.fetchDocument(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]domain.UpstreamPriceVector)
	var walk func(*html.Node) error
	walk = func(node *html.Node) error {
		if node.Type == html.ElementNode && node.Data == "tr" && hasHTMLClass(node, "request-table-row") {
			name := strings.TrimSpace(firstDescendantText(node, "th", "m-name"))
			prices := descendantPriceTexts(node, "request-cost")
			if name == "" || len(prices) != 3 {
				return nil
			}
			key := strings.ToLower(name)
			if _, duplicate := out[key]; duplicate {
				return fmt.Errorf("duplicate x5m5x per-request model %q", name)
			}
			small, middle, large := prices[0], prices[1], prices[2]
			out[key] = domain.UpstreamPriceVector{
				PerRequestLTE256K: &small, PerRequest256K512K: &middle, PerRequestGT512K: &large,
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(doc); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("x5m5x pricing page contained no per-request models")
	}
	return out, nil
}

func (f *x5m5xPricePageFetcher) FetchTokenPrices(ctx context.Context) (map[string]UpstreamTokenDisplayPrice, error) {
	doc, err := f.fetchDocument(ctx)
	if err != nil {
		return nil, err
	}
	return parseX5M5XTokenPrices(doc)
}

func (f *x5m5xPricePageFetcher) fetchDocument(ctx context.Context) (*html.Node, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, x5m5xPricingPageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/html")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch x5m5x pricing page: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch x5m5x pricing page: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, x5m5xPricingPageMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read x5m5x pricing page: %w", err)
	}
	if len(body) > x5m5xPricingPageMaxBytes {
		return nil, errors.New("x5m5x pricing page is too large")
	}
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("parse x5m5x pricing page: %w", err)
	}
	return doc, nil
}

func parseX5M5XTokenPrices(doc *html.Node) (map[string]UpstreamTokenDisplayPrice, error) {
	out := make(map[string]UpstreamTokenDisplayPrice)
	var walk func(*html.Node) error
	walk = func(node *html.Node) error {
		if node == nil {
			return nil
		}
		if node.Type == html.ElementNode && node.Data == "tr" && hasHTMLClass(node, "token-model") {
			name := x5m5xTokenModelName(node)
			key := strings.ToLower(strings.TrimSpace(name))
			if key == "" {
				return errors.New("x5m5x token price row omitted model name")
			}
			if _, duplicate := out[key]; duplicate {
				return fmt.Errorf("duplicate x5m5x token model %q", name)
			}
			value := UpstreamTokenDisplayPrice{ModelName: name}

			inputCell := firstDescendantByAttribute(node, "td", "data-label", "输入")
			if inputCell == nil {
				return fmt.Errorf("parse %s input prices: missing input cell", name)
			}
			official, selling, parseErr := parseX5M5XTokenValue(inputCell)
			if parseErr != nil {
				return fmt.Errorf("parse %s input prices: %w", name, parseErr)
			}
			value.OfficialInput, value.SellingInput = official, selling

			outputCell := firstDescendantByAttribute(node, "td", "data-label", "输出")
			if outputCell == nil {
				return fmt.Errorf("parse %s output prices: missing output cell", name)
			}
			official, selling, parseErr = parseX5M5XTokenValue(outputCell)
			if parseErr != nil {
				return fmt.Errorf("parse %s output prices: %w", name, parseErr)
			}
			value.OfficialOutput, value.SellingOutput = official, selling

			cacheCell := firstDescendantByAttribute(node, "td", "data-label", "缓存")
			if cacheCell == nil {
				return fmt.Errorf("parse %s cache prices: missing cache cell", name)
			}
			seenCacheDimensions := make(map[string]bool, 2)
			for _, dimension := range descendantsByTag(cacheCell, "div") {
				labelNode := directChildByTag(dimension, "em")
				if labelNode == nil {
					continue
				}
				label := strings.TrimSpace(htmlNodeText(labelNode))
				if label != "读" && label != "写" {
					continue
				}
				if seenCacheDimensions[label] {
					return fmt.Errorf("parse %s cache prices: duplicate cache-%s dimension", name, label)
				}
				seenCacheDimensions[label] = true
				official, selling, parseErr = parseX5M5XTokenValue(dimension)
				if parseErr != nil {
					return fmt.Errorf("parse %s cache-%s prices: %w", name, label, parseErr)
				}
				if label == "读" {
					value.OfficialCacheRead, value.SellingCacheRead = official, selling
				} else {
					value.OfficialCacheWrite, value.SellingCacheWrite = official, selling
				}
			}
			if !seenCacheDimensions["读"] || !seenCacheDimensions["写"] {
				return fmt.Errorf("parse %s cache prices: expected read and write dimensions", name)
			}
			out[key] = value
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(doc); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("x5m5x pricing page contained no token models")
	}
	return out, nil
}

func x5m5xTokenModelName(row *html.Node) string {
	nameCell := firstDescendantByClass(row, "token-name")
	if title, found := htmlAttribute(nameCell, "title"); found {
		if title = strings.TrimSpace(title); title != "" {
			return title
		}
	}
	if dataModel, found := htmlAttribute(row, "data-model"); found {
		if dataModel = strings.TrimSpace(dataModel); dataModel != "" {
			return dataModel
		}
	}
	return strings.TrimSpace(directNodeText(nameCell))
}

func parseX5M5XTokenValue(node *html.Node) (official, selling *float64, err error) {
	valueNode := firstDescendantByClass(node, "token-value")
	if valueNode == nil {
		return nil, nil, errors.New("missing token-value")
	}
	officialNode := firstDescendantByTag(valueNode, "b")
	if officialNode == nil {
		return nil, nil, errors.New("missing official price")
	}
	sellingNode := firstDescendantByTag(valueNode, "strong")
	if sellingNode == nil {
		return nil, nil, errors.New("missing upstream selling price")
	}
	official, err = parseX5M5XOptionalPrice(htmlNodeText(officialNode))
	if err != nil {
		return nil, nil, fmt.Errorf("official price: %w", err)
	}
	selling, err = parseX5M5XOptionalPrice(htmlNodeText(sellingNode))
	if err != nil {
		return nil, nil, fmt.Errorf("upstream selling price: %w", err)
	}
	return official, selling, nil
}

func parseX5M5XOptionalPrice(raw string) (*float64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "—" || raw == "–" || raw == "-" || raw == "未核实" {
		return nil, nil
	}
	if raw == "" {
		return nil, errors.New("empty price")
	}
	raw = strings.NewReplacer("¥", "", "￥", "", ",", "").Replace(raw)
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return nil, fmt.Errorf("invalid price %q", raw)
	}
	return x5m5xFloat64Ptr(value), nil
}

func x5m5xFloat64Ptr(value float64) *float64 { return &value }

func hasHTMLClass(node *html.Node, className string) bool {
	for _, attr := range node.Attr {
		if attr.Key == "class" {
			for _, value := range strings.Fields(attr.Val) {
				if value == className {
					return true
				}
			}
		}
	}
	return false
}

func firstDescendantText(node *html.Node, tag, className string) string {
	if node.Type == html.ElementNode && node.Data == tag && (className == "" || hasHTMLClass(node, className)) {
		return htmlNodeText(node)
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if value := firstDescendantText(child, tag, className); value != "" {
			return value
		}
	}
	return ""
}

func firstDescendantByAttribute(node *html.Node, tag, key, value string) *html.Node {
	if node == nil {
		return nil
	}
	attribute, found := htmlAttribute(node, key)
	if node.Type == html.ElementNode && node.Data == tag && found && attribute == value {
		return node
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if found := firstDescendantByAttribute(child, tag, key, value); found != nil {
			return found
		}
	}
	return nil
}

func firstDescendantByClass(node *html.Node, className string) *html.Node {
	if node == nil {
		return nil
	}
	if node.Type == html.ElementNode && hasHTMLClass(node, className) {
		return node
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if found := firstDescendantByClass(child, className); found != nil {
			return found
		}
	}
	return nil
}

func firstDescendantByTag(node *html.Node, tag string) *html.Node {
	if node == nil {
		return nil
	}
	if node.Type == html.ElementNode && node.Data == tag {
		return node
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if found := firstDescendantByTag(child, tag); found != nil {
			return found
		}
	}
	return nil
}

func directChildByTag(node *html.Node, tag string) *html.Node {
	if node == nil {
		return nil
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode && child.Data == tag {
			return child
		}
	}
	return nil
}

// directNodeText deliberately excludes nested model-note markup when the
// upstream row lacks its preferred title/data-model attributes.
func directNodeText(node *html.Node) string {
	if node == nil {
		return ""
	}
	var builder strings.Builder
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.TextNode {
			_, _ = builder.WriteString(child.Data)
		}
	}
	return builder.String()
}

func descendantsByTag(node *html.Node, tag string) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.ElementNode && current.Data == tag {
			out = append(out, current)
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	if node != nil {
		walk(node)
	}
	return out
}

func descendantPriceTexts(node *html.Node, className string) []float64 {
	var values []float64
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.ElementNode && hasHTMLClass(current, className) {
			raw := strings.NewReplacer("¥", "", "￥", "", ",", "").Replace(strings.TrimSpace(htmlNodeText(current)))
			if value, err := strconv.ParseFloat(raw, 64); err == nil && value >= 0 {
				values = append(values, value)
			}
			return
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return values
}

func htmlNodeText(node *html.Node) string {
	var builder strings.Builder
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.TextNode {
			_, _ = builder.WriteString(current.Data)
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return strings.TrimSpace(builder.String())
}
