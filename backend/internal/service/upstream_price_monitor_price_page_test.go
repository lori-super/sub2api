package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"
)

func TestX5M5XPerRequestHTMLHelpers(t *testing.T) {
	doc, err := html.Parse(strings.NewReader(`<table><tr class="request-table-row">
		<th class="m-name" title="MiniMax-M3">MiniMax-M3</th>
		<td class="request-cost">¥0.0010</td><td class="request-cost">¥0.0015</td><td class="request-cost">¥0.0020</td>
	</tr></table>`))
	require.NoError(t, err)
	require.Equal(t, "MiniMax-M3", firstDescendantText(doc, "th", "m-name"))
	require.Equal(t, []float64{0.001, 0.0015, 0.002}, descendantPriceTexts(doc, "request-cost"))
}

func TestParseX5M5XTokenPricesPreservesDimensionSpecificAndUnverifiedPrices(t *testing.T) {
	doc, err := html.Parse(strings.NewReader(`<table><tbody>
	<tr class="token-model" data-model="deepseek-v4-flash-0731">
	  <th class="token-name">deepseek-v4-flash-0731</th>
	  <td data-label="输入"><div class="token-value"><span>官网参考</span><b>¥1.6</b><span>到手</span><strong>¥0.16</strong></div></td>
	  <td data-label="输出"><div class="token-value"><span>官网参考</span><b>¥4.7</b><span>到手</span><strong>¥0.47</strong></div></td>
	  <td data-label="缓存"><div class="cache-values">
	    <div><em>读</em><div class="token-value"><span>官网参考</span><b>¥0.1</b><span>到手</span><strong>¥0.03</strong></div></div>
	    <div><em>写</em><div class="token-value"><span>官网参考</span><b>未核实</b><span>到手</span><strong>—</strong></div></div>
	  </div></td>
	</tr>
	<tr class="token-model" data-model="minimax-m3">
	  <th class="token-name" title="MiniMax-M3">MiniMax-M3<small class="model-note">temporary note</small></th>
	  <td data-label="输入"><div class="token-value"><b>¥9.8</b><strong>¥0.98</strong></div></td>
	  <td data-label="输出"><div class="token-value"><b>¥30.9</b><strong>¥3.09</strong></div></td>
	  <td data-label="缓存"><div class="cache-values">
	    <div><em>读</em><div class="token-value"><b>¥1.9</b><strong>¥0.19</strong></div></div>
	    <div><em>写</em><div class="token-value"><b>未核实</b><strong>¥0</strong></div></div>
	  </div></td>
	</tr></tbody></table>`))
	require.NoError(t, err)

	prices, err := parseX5M5XTokenPrices(doc)
	require.NoError(t, err)
	require.Len(t, prices, 2)
	deepseek := prices["deepseek-v4-flash-0731"]
	require.InDelta(t, 1.6, *deepseek.OfficialInput, 1e-12)
	require.InDelta(t, 0.16, *deepseek.SellingInput, 1e-12)
	require.InDelta(t, 0.03, *deepseek.SellingCacheRead, 1e-12)
	require.Nil(t, deepseek.OfficialCacheWrite)
	require.Nil(t, deepseek.SellingCacheWrite)
	minimax := prices["minimax-m3"]
	require.Equal(t, "MiniMax-M3", minimax.ModelName)
	require.Nil(t, minimax.OfficialCacheWrite)
	require.NotNil(t, minimax.SellingCacheWrite, "an explicit zero must not be treated as unverified")
	require.Zero(t, *minimax.SellingCacheWrite)
}

func TestParseX5M5XTokenPricesRejectsCaseInsensitiveDuplicates(t *testing.T) {
	doc, err := html.Parse(strings.NewReader(`<table><tbody>
	` + completeX5M5XTokenRow("MiniMax-M3", "MiniMax-M3") + `
	` + completeX5M5XTokenRow("minimax-m3", "minimax-m3") + `
	</tbody></table>`))
	require.NoError(t, err)

	_, err = parseX5M5XTokenPrices(doc)
	require.ErrorContains(t, err, "duplicate x5m5x token model")
}

func TestParseX5M5XTokenPricesRejectsEmptyOrMalformedResults(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		doc, err := html.Parse(strings.NewReader(`<html><body><table></table></body></html>`))
		require.NoError(t, err)
		_, err = parseX5M5XTokenPrices(doc)
		require.ErrorContains(t, err, "contained no token models")
	})

	t.Run("missing selling value", func(t *testing.T) {
		doc, err := html.Parse(strings.NewReader(`<table><tbody><tr class="token-model" data-model="broken">
			<th class="token-name" title="Broken">Broken</th>
			<td data-label="输入"><div class="token-value"><b>¥1</b></div></td>
			<td data-label="输出"><div class="token-value"><b>¥2</b><strong>¥0.2</strong></div></td>
			<td data-label="缓存"><div><em>读</em><div class="token-value"><b>¥0.1</b><strong>¥0.01</strong></div></div><div><em>写</em><div class="token-value"><b>未核实</b><strong>—</strong></div></div></td>
		</tr></tbody></table>`))
		require.NoError(t, err)
		_, err = parseX5M5XTokenPrices(doc)
		require.ErrorContains(t, err, "missing upstream selling price")
	})

	t.Run("missing cache dimension", func(t *testing.T) {
		doc, err := html.Parse(strings.NewReader(`<table><tbody><tr class="token-model" data-model="broken">
			<th class="token-name">broken</th>
			<td data-label="输入"><div class="token-value"><b>¥1</b><strong>¥0.1</strong></div></td>
			<td data-label="输出"><div class="token-value"><b>¥2</b><strong>¥0.2</strong></div></td>
			<td data-label="缓存"><div><em>读</em><div class="token-value"><b>¥0.1</b><strong>¥0.01</strong></div></div></td>
		</tr></tbody></table>`))
		require.NoError(t, err)
		_, err = parseX5M5XTokenPrices(doc)
		require.ErrorContains(t, err, "expected read and write dimensions")
	})
}

func TestParseX5M5XOptionalPriceDistinguishesUnknownAndZero(t *testing.T) {
	for _, placeholder := range []string{"未核实", "—", "-"} {
		value, err := parseX5M5XOptionalPrice(placeholder)
		require.NoError(t, err)
		require.Nil(t, value)
	}
	zero, err := parseX5M5XOptionalPrice("¥0")
	require.NoError(t, err)
	require.NotNil(t, zero)
	require.Zero(t, *zero)
	_, err = parseX5M5XOptionalPrice("")
	require.Error(t, err)
	_, err = parseX5M5XOptionalPrice("$1")
	require.Error(t, err)
}

func completeX5M5XTokenRow(dataModel, title string) string {
	return `<tr class="token-model" data-model="` + dataModel + `">
		<th class="token-name" title="` + title + `">` + title + `</th>
		<td data-label="输入"><div class="token-value"><b>¥1</b><strong>¥0.1</strong></div></td>
		<td data-label="输出"><div class="token-value"><b>¥2</b><strong>¥0.2</strong></div></td>
		<td data-label="缓存"><div><em>读</em><div class="token-value"><b>¥0.1</b><strong>¥0.01</strong></div></div><div><em>写</em><div class="token-value"><b>未核实</b><strong>—</strong></div></div></td>
	</tr>`
}
