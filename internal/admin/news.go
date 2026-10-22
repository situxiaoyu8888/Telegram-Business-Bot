package admin

import (
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/news"
)

// WithNews enables the news pages.
func (h *Handler) WithNews(s *news.Service) *Handler { h.news = s; return h }

func (h *Handler) registerNews(api *gin.RouterGroup, can func(...string) gin.HandlerFunc) {
	// Article, feed and chat forms pick categories from this list.
	api.GET("/news-categories", can("news", "news_categories", "news_feeds", "news_chats"), h.newsCategories)
	api.POST("/news-categories", can("news_categories"), h.saveNewsCategory)
	api.DELETE("/news-categories/:id", can("news_categories"), h.deleteNewsCategory)
	api.GET("/news", can("news"), h.newsArticles)
	api.POST("/news", can("news"), h.saveNewsArticle)
	api.POST("/news/:id/publish", can("news"), h.publishNewsArticle)
	api.DELETE("/news/:id", can("news"), h.deleteNewsArticle)
	api.GET("/news-feeds", can("news_feeds"), h.newsFeeds)
	api.POST("/news-feeds", can("news_feeds"), h.saveNewsFeed)
	api.POST("/news-feeds/:id/fetch", can("news_feeds"), h.fetchNewsFeed)
	api.DELETE("/news-feeds/:id", can("news_feeds"), h.deleteNewsFeed)
	api.GET("/news-chats", can("news_chats"), h.newsChats)
	api.POST("/news-chats", can("news_chats"), h.saveNewsChat)
}

func bad(c *gin.Context, msg string) { c.JSON(http.StatusBadRequest, gin.H{"error": msg}) }

func failed(c *gin.Context, msg string) { c.JSON(http.StatusInternalServerError, gin.H{"error": msg}) }

type newsCategoryRow struct {
	model.NewsCategory
	Subscribers int64 `json:"subscribers"`
	Articles    int64 `json:"articles"`
}

func (h *Handler) newsCategories(c *gin.Context) {
	cats, err := h.news.Categories(false)
	if err != nil {
		failed(c, "分类加载失败")
		return
	}
	var subs, arts []struct {
		CategoryID uint64
		N          int64
	}
	h.st.DB.Model(&model.NewsSubscription{}).Select("category_id, COUNT(*) AS n").Group("category_id").Scan(&subs)
	h.st.DB.Model(&model.NewsArticle{}).Select("category_id, COUNT(*) AS n").Where("status = ?", news.StatusPublished).Group("category_id").Scan(&arts)
	count := func(list []struct {
		CategoryID uint64
		N          int64
	}, id uint64) int64 {
		for _, x := range list {
			if x.CategoryID == id {
				return x.N
			}
		}
		return 0
	}
	rows := make([]newsCategoryRow, 0, len(cats)+1)
	for _, cat := range cats {
		rows = append(rows, newsCategoryRow{cat, count(subs, cat.ID), count(arts, cat.ID)})
	}
	c.JSON(http.StatusOK, rows)
}

func (h *Handler) saveNewsCategory(c *gin.Context) {
	var b model.NewsCategory
	if c.ShouldBindJSON(&b) != nil {
		bad(c, "参数错误")
		return
	}
	b.Name = strings.TrimSpace(b.Name)
	if b.Name == "" || utf8.RuneCountInString(b.Name) > 32 {
		bad(c, "请填写 32 字以内的分类名称")
		return
	}
	fields := map[string]any{"name": b.Name, "sort": b.Sort, "enabled": b.Enabled}
	if b.ID == 0 {
		row := model.NewsCategory{Name: b.Name, Sort: b.Sort, Enabled: b.Enabled}
		if h.st.DB.Create(&row).Error != nil {
			failed(c, "保存失败")
			return
		}
		c.JSON(http.StatusOK, row)
		return
	}
	if h.st.DB.Model(&model.NewsCategory{}).Where("id = ?", b.ID).Updates(fields).Error != nil {
		failed(c, "保存失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) deleteNewsCategory(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var n int64
	h.st.DB.Model(&model.NewsArticle{}).Where("category_id = ?", id).Count(&n)
	if n > 0 {
		bad(c, "该分类下还有新闻，请先移走或删除，或改为停用")
		return
	}
	h.st.DB.Where("category_id = ?", id).Delete(&model.NewsSubscription{})
	if h.st.DB.Delete(&model.NewsCategory{}, id).Error != nil {
		failed(c, "删除失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

type newsArticleRow struct {
	model.NewsArticle
	Sent    int64 `json:"sent"`
	Failed  int64 `json:"failed"`
	Pending int64 `json:"pending"`
}

func (h *Handler) newsArticles(c *gin.Context) {
	list := []model.NewsArticle{}
	if h.st.DB.Order("id desc").Limit(500).Find(&list).Error != nil {
		failed(c, "新闻加载失败")
		return
	}
	ids := make([]uint64, 0, len(list))
	for _, a := range list {
		ids = append(ids, a.ID)
	}
	var stats []struct {
		ArticleID uint64
		Status    string
		N         int64
	}
	if len(ids) > 0 {
		h.st.DB.Model(&model.NewsDelivery{}).Select("article_id, status, COUNT(*) AS n").Where("article_id IN ?", ids).Group("article_id, status").Scan(&stats)
	}
	rows := make([]newsArticleRow, 0, len(list))
	for _, a := range list {
		row := newsArticleRow{NewsArticle: a}
		for _, s := range stats {
			if s.ArticleID != a.ID {
				continue
			}
			switch s.Status {
			case "sent":
				row.Sent += s.N
			case "failed":
				row.Failed += s.N
			default:
				row.Pending += s.N
			}
		}
		rows = append(rows, row)
	}
	c.JSON(http.StatusOK, rows)
}

func cleanURL(s string, max int) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", true
	}
	return s, news.ValidFeedURL(s) && len(s) <= max
}

// saveNewsArticle creates or edits an article. Mode "draft" keeps it unpublished, "publish"
// publishes it now and "schedule" publishes it at publish_at. Editing a published article
// changes its text but does not push it again.
func (h *Handler) saveNewsArticle(c *gin.Context) {
	var b struct {
		ID         uint64 `json:"id"`
		CategoryID uint64 `json:"category_id"`
		Title      string `json:"title"`
		Content    string `json:"content"`
		ImageURL   string `json:"image_url"`
		LinkURL    string `json:"link_url"`
		Mode       string `json:"mode"`
		PublishAt  string `json:"publish_at"`
		Push       bool   `json:"push"`
	}
	if c.ShouldBindJSON(&b) != nil {
		bad(c, "参数错误")
		return
	}
	b.Title, b.Content = strings.TrimSpace(b.Title), strings.TrimSpace(b.Content)
	if b.Title == "" || utf8.RuneCountInString(b.Title) > 120 {
		bad(c, "请填写 120 字以内的标题")
		return
	}
	if utf8.RuneCountInString(b.Content) > 3000 {
		bad(c, "正文最多 3000 字")
		return
	}
	var cat model.NewsCategory
	if h.st.DB.First(&cat, b.CategoryID).Error != nil {
		bad(c, "请选择新闻分类")
		return
	}
	img, ok1 := cleanURL(b.ImageURL, 512)
	link, ok2 := cleanURL(b.LinkURL, 1024)
	if !ok1 || !ok2 {
		bad(c, "图片和原文链接需为 http(s) 地址")
		return
	}
	fields := map[string]any{"category_id": cat.ID, "title": b.Title, "content": b.Content, "image_url": img, "link_url": link, "push": b.Push}
	var cur model.NewsArticle
	if b.ID != 0 {
		if h.st.DB.First(&cur, b.ID).Error != nil {
			bad(c, "新闻不存在")
			return
		}
	}
	if cur.Status != news.StatusPublished {
		now := time.Now()
		switch b.Mode {
		case "draft":
			fields["status"], fields["publish_at"] = news.StatusDraft, nil
		case "publish":
			fields["status"], fields["published_at"], fields["publish_at"] = news.StatusPublished, now, nil
		case "schedule":
			at, err := time.ParseInLocation("2006-01-02T15:04", b.PublishAt, time.Local)
			if err != nil || !at.After(now) {
				bad(c, "请选择未来的发布时间")
				return
			}
			fields["status"], fields["publish_at"] = news.StatusScheduled, at
		default:
			bad(c, "请选择发布方式")
			return
		}
	}
	if b.ID == 0 {
		row := model.NewsArticle{Source: news.SourceManual, CreatedBy: operator(c), Status: news.StatusDraft}
		if h.st.DB.Create(&row).Error != nil {
			failed(c, "保存失败")
			return
		}
		b.ID = row.ID
	}
	if h.st.DB.Model(&model.NewsArticle{}).Where("id = ?", b.ID).Updates(fields).Error != nil {
		failed(c, "保存失败")
		return
	}
	h.queueNews()
	c.JSON(http.StatusOK, gin.H{"ok": true, "id": b.ID})
}

// queueNews queues deliveries right away instead of waiting for the worker's next round.
func (h *Handler) queueNews() {
	if err := h.news.Fanout(); err != nil && h.logger != nil {
		h.logger.Warn("news fan-out failed", zap.Error(err))
	}
}

func (h *Handler) publishNewsArticle(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	if err := h.news.Publish(id); err != nil {
		bad(c, err.Error())
		return
	}
	h.queueNews()
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) deleteNewsArticle(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	// Messages already delivered stay in Telegram; queued ones are dropped.
	h.st.DB.Where("article_id = ?", id).Delete(&model.NewsDelivery{})
	if h.st.DB.Delete(&model.NewsArticle{}, id).Error != nil {
		failed(c, "删除失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) newsFeeds(c *gin.Context) {
	rows := []model.NewsFeed{}
	if h.st.DB.Order("id desc").Find(&rows).Error != nil {
		failed(c, "订阅源加载失败")
		return
	}
	c.JSON(http.StatusOK, rows)
}

func (h *Handler) saveNewsFeed(c *gin.Context) {
	var b model.NewsFeed
	if c.ShouldBindJSON(&b) != nil {
		bad(c, "参数错误")
		return
	}
	b.Name, b.URL = strings.TrimSpace(b.Name), strings.TrimSpace(b.URL)
	if b.Name == "" || utf8.RuneCountInString(b.Name) > 64 {
		bad(c, "请填写 64 字以内的名称")
		return
	}
	if !news.ValidFeedURL(b.URL) || len(b.URL) > 1024 {
		bad(c, "请填写 http(s) 开头的 RSS/Atom 地址")
		return
	}
	if h.st.DB.First(&model.NewsCategory{}, b.CategoryID).Error != nil {
		bad(c, "请选择新闻分类")
		return
	}
	if b.Interval < 5 || b.Interval > 1440 {
		bad(c, "抓取间隔需在 5–1440 分钟之间")
		return
	}
	fields := map[string]any{"name": b.Name, "url": b.URL, "category_id": b.CategoryID, "auto_publish": b.AutoPublish, "interval": b.Interval, "enabled": b.Enabled}
	if b.ID == 0 {
		row := model.NewsFeed{Name: b.Name, URL: b.URL, CategoryID: b.CategoryID, AutoPublish: b.AutoPublish, Interval: b.Interval, Enabled: b.Enabled}
		if h.st.DB.Create(&row).Error != nil {
			failed(c, "保存失败")
			return
		}
		c.JSON(http.StatusOK, row)
		return
	}
	if h.st.DB.Model(&model.NewsFeed{}).Where("id = ?", b.ID).Updates(fields).Error != nil {
		failed(c, "保存失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) fetchNewsFeed(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var f model.NewsFeed
	if h.st.DB.First(&f, id).Error != nil {
		bad(c, "订阅源不存在")
		return
	}
	n, err := h.news.FetchFeed(c.Request.Context(), f)
	if err != nil {
		bad(c, "抓取失败："+err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "added": n})
}

func (h *Handler) deleteNewsFeed(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	if h.st.DB.Delete(&model.NewsFeed{}, id).Error != nil {
		failed(c, "删除失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

type newsChatRow struct {
	model.NewsChat
	BotName string `json:"bot_name"`
}

func (h *Handler) newsChats(c *gin.Context) {
	rows := []newsChatRow{}
	err := h.st.DB.Table("news_chats c").Select("c.*, b.name AS bot_name").Joins("LEFT JOIN business_bots b ON b.id = c.bot_id").Order("c.id desc").Scan(&rows).Error
	if err != nil {
		failed(c, "群和频道加载失败")
		return
	}
	c.JSON(http.StatusOK, rows)
}

func (h *Handler) saveNewsChat(c *gin.Context) {
	var b model.NewsChat
	if c.ShouldBindJSON(&b) != nil {
		bad(c, "参数错误")
		return
	}
	var cur model.NewsChat
	if h.st.DB.First(&cur, b.ID).Error != nil {
		bad(c, "群或频道不存在")
		return
	}
	var cats []string
	for _, p := range strings.Split(b.PushCategories, ",") {
		if n, err := strconv.ParseUint(strings.TrimSpace(p), 10, 64); err == nil && n > 0 {
			cats = append(cats, strconv.FormatUint(n, 10))
		}
	}
	if b.SourceEnabled {
		if cur.Type != "channel" {
			bad(c, "只有频道可以作为新闻来源")
			return
		}
		if h.st.DB.First(&model.NewsCategory{}, b.SourceCategoryID).Error != nil {
			bad(c, "请选择频道消息归入的分类")
			return
		}
	}
	fields := map[string]any{"push_enabled": b.PushEnabled, "push_categories": strings.Join(cats, ","), "source_enabled": b.SourceEnabled, "source_category_id": b.SourceCategoryID, "auto_publish": b.AutoPublish}
	if h.st.DB.Model(&model.NewsChat{}).Where("id = ?", b.ID).Updates(fields).Error != nil {
		failed(c, "保存失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
