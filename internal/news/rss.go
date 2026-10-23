package news

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
)

// Fetcher downloads a feed.
type Fetcher func(ctx context.Context, url string) ([]byte, error)

const maxFeedBytes = 4 << 20

// AllowPrivateFeeds lets feeds live on loopback or private networks (local tests only); otherwise
// a console account could use feed URLs to reach internal services.
var AllowPrivateFeeds bool

var errPrivateFeed = errors.New("订阅源地址指向本机或内网，已拒绝")

// publicOnly refuses connections to loopback, private, link-local and other non-public
// addresses. It runs on the resolved address, so DNS names and redirects are covered too.
func publicOnly(_, address string, _ syscall.RawConn) error {
	if AllowPrivateFeeds {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return errPrivateFeed
	}
	ip = ip.Unmap()
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return errPrivateFeed
	}
	for _, p := range nonPublic {
		if p.Contains(ip) {
			return errPrivateFeed
		}
	}
	return nil
}

// nonPublic are further ranges no feed lives on: carrier-grade NAT (cloud metadata such as
// 100.100.100.200), "this network", benchmarking, protocol assignments, reserved, and NAT64.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
}

var feedClient = &http.Client{
	Timeout: 20 * time.Second,
	Transport: &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, Control: publicOnly}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
	},
	CheckRedirect: func(_ *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("订阅源重定向次数过多")
		}
		return nil
	},
}

func httpFetch(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; NewsBot/1.0)")
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml, text/xml, */*")
	resp, err := feedClient.Do(req)
	if err != nil {
		if errors.Is(err, errPrivateFeed) {
			return nil, errPrivateFeed
		}
		return nil, errors.New("无法连接订阅源")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("订阅源返回错误，请检查地址")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxFeedBytes {
		return nil, errors.New("订阅源内容超过 4 MB")
	}
	return body, nil
}

// ValidFeedURL checks that a feed address is an absolute http(s) URL.
func ValidFeedURL(s string) bool {
	u, err := url.Parse(strings.TrimSpace(s))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// FeedItem is one entry of an RSS or Atom feed.
type FeedItem struct {
	GUID      string
	Title     string
	Link      string
	Summary   string
	ImageURL  string
	Published time.Time
}

type rssDoc struct {
	XMLName xml.Name
	Items   []struct {
		Title       string `xml:"title"`
		Link        string `xml:"link"`
		GUID        string `xml:"guid"`
		Description string `xml:"description"`
		Encoded     string `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
		PubDate     string `xml:"pubDate"`
		Enclosure   []struct {
			URL  string `xml:"url,attr"`
			Type string `xml:"type,attr"`
		} `xml:"enclosure"`
		Media []struct {
			URL    string `xml:"url,attr"`
			Medium string `xml:"medium,attr"`
			Type   string `xml:"type,attr"`
		} `xml:"http://search.yahoo.com/mrss/ content"`
		Thumbnail []struct {
			URL string `xml:"url,attr"`
		} `xml:"http://search.yahoo.com/mrss/ thumbnail"`
	} `xml:"channel>item"`
	Entries []struct {
		Title string `xml:"title"`
		Links []struct {
			Href string `xml:"href,attr"`
			Rel  string `xml:"rel,attr"`
			Type string `xml:"type,attr"`
		} `xml:"link"`
		ID        string `xml:"id"`
		Summary   string `xml:"summary"`
		Content   string `xml:"content"`
		Published string `xml:"published"`
		Updated   string `xml:"updated"`
	} `xml:"entry"`
}

var (
	tagPattern   = regexp.MustCompile(`(?s)<[^>]*>`)
	imgPattern   = regexp.MustCompile(`(?i)<img[^>]+src=["']([^"']+)["']`)
	spacePattern = regexp.MustCompile(`[ \t\r\f\v]+`)
	linesPattern = regexp.MustCompile(`\n\s*\n+`)
)

// plainText turns feed HTML into plain text with paragraph breaks.
func plainText(s string) string {
	s = regexp.MustCompile(`(?i)<br\s*/?>|</p>|</div>|</li>`).ReplaceAllString(s, "\n")
	s = html.UnescapeString(tagPattern.ReplaceAllString(s, ""))
	s = spacePattern.ReplaceAllString(s, " ")
	s = linesPattern.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

func parseTime(v string) time.Time {
	v = strings.TrimSpace(v)
	for _, layout := range []string{time.RFC1123Z, time.RFC1123, time.RFC3339, "Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t
		}
	}
	return time.Time{}
}

// ParseFeed reads RSS 2.0 or Atom (UTF-8).
func ParseFeed(body []byte) ([]FeedItem, error) {
	var doc rssDoc
	dec := xml.NewDecoder(bytes.NewReader(body))
	dec.Strict = false
	dec.CharsetReader = func(label string, in io.Reader) (io.Reader, error) {
		if l := strings.ToLower(label); l == "utf-8" || l == "utf8" || l == "us-ascii" {
			return in, nil
		}
		return nil, fmt.Errorf("暂不支持 %s 编码的订阅源（仅支持 UTF-8）", label)
	}
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("无法解析订阅源：%w", err)
	}
	var out []FeedItem
	for _, it := range doc.Items {
		body := it.Encoded
		if strings.TrimSpace(body) == "" {
			body = it.Description
		}
		item := FeedItem{GUID: strings.TrimSpace(it.GUID), Title: plainText(it.Title), Link: strings.TrimSpace(it.Link), Summary: plainText(body), Published: parseTime(it.PubDate)}
		for _, e := range it.Enclosure {
			if strings.HasPrefix(e.Type, "image/") {
				item.ImageURL = e.URL
			}
		}
		for _, m := range it.Media {
			if item.ImageURL == "" && (m.Medium == "image" || strings.HasPrefix(m.Type, "image/")) {
				item.ImageURL = m.URL
			}
		}
		if item.ImageURL == "" && len(it.Thumbnail) > 0 {
			item.ImageURL = it.Thumbnail[0].URL
		}
		if item.ImageURL == "" {
			if m := imgPattern.FindStringSubmatch(body); m != nil {
				item.ImageURL = html.UnescapeString(m[1])
			}
		}
		out = append(out, item)
	}
	for _, e := range doc.Entries {
		item := FeedItem{GUID: strings.TrimSpace(e.ID), Title: plainText(e.Title), Published: parseTime(e.Published)}
		if item.Published.IsZero() {
			item.Published = parseTime(e.Updated)
		}
		for _, l := range e.Links {
			if l.Rel == "" || l.Rel == "alternate" {
				item.Link = strings.TrimSpace(l.Href)
			}
			if l.Rel == "enclosure" && strings.HasPrefix(l.Type, "image/") {
				item.ImageURL = l.Href
			}
		}
		body := e.Content
		if strings.TrimSpace(body) == "" {
			body = e.Summary
		}
		item.Summary = plainText(body)
		if item.ImageURL == "" {
			if m := imgPattern.FindStringSubmatch(body); m != nil {
				item.ImageURL = html.UnescapeString(m[1])
			}
		}
		out = append(out, item)
	}
	if doc.XMLName.Local != "rss" && doc.XMLName.Local != "feed" {
		return nil, errors.New("不是 RSS 或 Atom 订阅源")
	}
	return out, nil
}

// httpURL keeps only absolute http(s) URLs (feeds may carry relative or javascript: links).
func httpURL(s string, max int) string {
	s = strings.TrimSpace(s)
	if !ValidFeedURL(s) || len(s) > max {
		return ""
	}
	return s
}

// FetchFeed downloads a feed and stores its new items. The first fetch only archives what is
// already there (published without push) so adding a feed does not flood subscribers.
func (s *Service) FetchFeed(ctx context.Context, feed model.NewsFeed) (int, error) {
	body, err := s.fetch(ctx, feed.URL)
	var items []FeedItem
	if err == nil {
		items, err = ParseFeed(body)
	}
	now := s.now()
	if err != nil {
		s.st.DB.Model(&model.NewsFeed{}).Where("id = ?", feed.ID).Updates(map[string]any{"last_fetched_at": now, "last_error": truncate(err.Error(), 255)})
		return 0, err
	}
	first := feed.LastFetchedAt == nil
	if len(items) > 30 {
		items = items[:30]
	}
	added := 0
	// Oldest first, so the newest article ends up newest.
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		id := it.GUID
		if id == "" {
			id = it.Link
		}
		if id == "" {
			id = it.Title
		}
		if it.Title == "" || id == "" {
			continue
		}
		key := fmt.Sprintf("rss:%d:%s", feed.ID, truncate(id, 160))
		a := model.NewsArticle{CategoryID: feed.CategoryID, Title: truncate(it.Title, 255), Content: truncate(it.Summary, 3000), ImageURL: httpURL(it.ImageURL, 512), LinkURL: httpURL(it.Link, 1024),
			Source: SourceRSS, SourceID: feed.ID, ExternalKey: &key, Status: StatusPending, Push: true}
		switch {
		case first:
			a.Status, a.PublishedAt, a.DispatchedAt, a.Push = StatusPublished, publishedTime(it, now), &now, false
		case feed.AutoPublish:
			a.Status, a.PublishedAt = StatusPublished, &now
		}
		if err := s.st.DB.Create(&a).Error; err != nil {
			if store.IsDuplicate(err) {
				continue
			}
			return added, err
		}
		added++
	}
	s.st.DB.Model(&model.NewsFeed{}).Where("id = ?", feed.ID).Updates(map[string]any{"last_fetched_at": now, "last_error": ""})
	return added, nil
}

func publishedTime(it FeedItem, now time.Time) *time.Time {
	if !it.Published.IsZero() && it.Published.Before(now) {
		t := it.Published
		return &t
	}
	return &now
}

// FetchDue fetches every enabled feed whose interval has passed.
func (s *Service) FetchDue(ctx context.Context) {
	var feeds []model.NewsFeed
	if err := s.st.DB.Where("enabled = ?", true).Find(&feeds).Error; err != nil {
		s.logger.Error("load news feeds failed", zap.Error(err))
		return
	}
	now := s.now()
	for _, f := range feeds {
		interval := time.Duration(f.Interval) * time.Minute
		if interval < 5*time.Minute {
			interval = 5 * time.Minute
		}
		if f.LastFetchedAt != nil && now.Sub(*f.LastFetchedAt) < interval {
			continue
		}
		if n, err := s.FetchFeed(ctx, f); err != nil {
			s.logger.Warn("news feed fetch failed", zap.Uint64("feed_id", f.ID), zap.Error(err))
		} else if n > 0 {
			s.logger.Info("news feed fetched", zap.Uint64("feed_id", f.ID), zap.Int("new", n))
		}
	}
}
