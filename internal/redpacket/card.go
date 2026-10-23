package redpacket

import (
	"fmt"
	"html"
	"regexp"
	"strings"
	"unicode/utf16"

	"github.com/shopspring/decimal"

	"telegram-business-bot/internal/model"
)

var escaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// amount formats a ledger amount without trailing zeros.
func amount(v string) string {
	d, err := decimal.NewFromString(v)
	if err != nil {
		return v
	}
	return d.String()
}

// Open reports whether the packet can still be claimed at the time it was loaded.
func (s *Service) Open(p *model.RedPacket) bool {
	return p.Status == StatusActive && p.ClaimedCount < p.Count && s.now().Before(p.ExpiresAt)
}

// captionLimit is Telegram's caption length (visible characters, in UTF-16 units).
const captionLimit = 1024

var tags = regexp.MustCompile(`<[^>]*>`)

func visibleLen(h string) int {
	return len(utf16.Encode([]rune(html.UnescapeString(tags.ReplaceAllString(h, "")))))
}

// Card is the HTML message showing a packet and its latest claims. A packet with a cover is a
// photo whose caption is the card, so fewer claims are listed when the caption would be too long.
func (s *Service) Card(p *model.RedPacket, claims []model.RedPacketClaim) string {
	if p.Cover == "" {
		return s.card(p, claims, 15)
	}
	var text string
	for _, n := range []int{15, 10, 5, 0} {
		if text = s.card(p, claims, n); visibleLen(text) <= captionLimit {
			break
		}
	}
	return text
}

func (s *Service) card(p *model.RedPacket, claims []model.RedPacketClaim, show int) string {
	cur, unit := p.Currency, p.Currency
	if c, err := s.wallet.Currency(p.Currency); err == nil {
		cur, unit = c.Name, c.Name
		if c.Icon != "" {
			cur = c.Icon + " " + c.Name
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🧧 <b>%s 发了一个%s红包</b>\n", escaper.Replace(p.SenderName), TypeNames[p.Type])
	fmt.Fprintf(&b, "<i>%s</i>\n\n", escaper.Replace(p.Greeting))
	fmt.Fprintf(&b, "💰 共 <b>%s</b> %s，%d 个\n", amount(p.Total), escaper.Replace(cur), p.Count)
	switch p.Type {
	case TypeExclusive:
		target := "@" + p.TargetUsername
		if p.TargetTelegramID != 0 {
			target = fmt.Sprintf("Telegram ID %d", p.TargetTelegramID)
		}
		fmt.Fprintf(&b, "🎯 仅 %s 可领取\n", escaper.Replace(target))
	case TypePassword:
		fmt.Fprintf(&b, "🔑 口令：<code>%s</code>（点「抢红包」后输入口令，或在群里直接发送口令）\n", escaper.Replace(p.Password))
	}
	fmt.Fprintf(&b, "📦 已领取 %d/%d", p.ClaimedCount, p.Count)
	switch {
	case p.Status == StatusFinished || p.ClaimedCount >= p.Count:
		b.WriteString(" · 已领完")
	case p.Status == StatusExpired || !s.now().Before(p.ExpiresAt):
		b.WriteString(" · 已过期")
	default:
		fmt.Fprintf(&b, " · %s 前有效", p.ExpiresAt.Format("01-02 15:04"))
	}
	if len(claims) > 0 {
		best := -1
		if (p.Type == TypeLucky || p.Type == TypePassword) && p.ClaimedCount >= p.Count && p.Count > 1 {
			for i, c := range claims {
				if best < 0 || decAmount(c.Amount).GreaterThan(decAmount(claims[best].Amount)) {
					best = i
				}
			}
		}
		b.WriteString("\n")
		// The latest claims are listed; long packets would exceed the message size otherwise.
		start := 0
		if len(claims) > show {
			start = len(claims) - show
			fmt.Fprintf(&b, "\n…… 另有 %d 人", start)
		}
		for i := start; i < len(claims); i++ {
			c := claims[i]
			mark := ""
			if i == best {
				mark = " 👑 手气最佳"
			}
			fmt.Fprintf(&b, "\n%s　<code>%s</code> %s%s", escaper.Replace(c.Name), amount(c.Amount), escaper.Replace(unit), mark)
		}
	}
	return b.String()
}

func decAmount(v string) decimal.Decimal {
	d, _ := decimal.NewFromString(v)
	return d
}
