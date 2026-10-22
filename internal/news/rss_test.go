package news

import (
	"strings"
	"testing"
)

const rssSample = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:media="http://search.yahoo.com/mrss/" xmlns:content="http://purl.org/rss/1.0/modules/content/">
<channel><title>Demo</title>
<item><title>泰国 &amp; 缅甸 新闻</title><link>https://example.com/a</link><guid>a-1</guid>
<description><![CDATA[<p>第一段<br>第二行</p><img src="https://example.com/a.jpg">]]></description>
<pubDate>Mon, 02 Jan 2006 15:04:05 +0700</pubDate></item>
<item><title>Second</title><link>https://example.com/b</link><media:content url="https://example.com/b.png" medium="image"/></item>
</channel></rss>`

const atomSample = `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom"><title>Atom</title>
<entry><title>Atom entry</title><link rel="alternate" href="https://example.com/x"/><id>tag:x</id>
<summary type="html">&lt;b&gt;Bold&lt;/b&gt; text</summary><updated>2024-05-01T10:00:00Z</updated></entry>
</feed>`

func TestParseRSSAndAtom(t *testing.T) {
	items, err := ParseFeed([]byte(rssSample))
	if err != nil || len(items) != 2 {
		t.Fatal(items, err)
	}
	a := items[0]
	if a.Title != "泰国 & 缅甸 新闻" || a.GUID != "a-1" || a.Summary != "第一段\n第二行" || a.ImageURL != "https://example.com/a.jpg" || a.Published.IsZero() {
		t.Fatalf("%+v", a)
	}
	if items[1].ImageURL != "https://example.com/b.png" {
		t.Fatalf("%+v", items[1])
	}
	items, err = ParseFeed([]byte(atomSample))
	if err != nil || len(items) != 1 || items[0].Link != "https://example.com/x" || items[0].Summary != "Bold text" || items[0].GUID != "tag:x" {
		t.Fatal(items, err)
	}
	if _, err := ParseFeed([]byte("<html><body>nope</body></html>")); err == nil {
		t.Fatal("HTML accepted as a feed")
	}
	if _, err := ParseFeed([]byte(`<?xml version="1.0" encoding="GBK"?><rss><channel></channel></rss>`)); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatal("non-UTF-8 feed not reported", err)
	}
}

func TestHelpers(t *testing.T) {
	if !coversCategory("", 3) || !coversCategory("1, 3", 3) || coversCategory("1,2", 3) {
		t.Fatal("coversCategory")
	}
	if title, body := splitTitle("标题\n正文第一行\n第二行"); title != "标题" || body != "正文第一行\n第二行" {
		t.Fatal(title, body)
	}
	if httpURL("javascript:alert(1)", 100) != "" || httpURL("https://a.b/c", 100) == "" {
		t.Fatal("httpURL")
	}
}
