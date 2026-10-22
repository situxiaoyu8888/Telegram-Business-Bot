package integration

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/redpacket"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/wallet"
)

type packetFixture struct {
	db      *gorm.DB
	w       *wallet.Service
	svc     *redpacket.Service
	sender  model.TelegramUser
	notices []string
}

func newPacketFixture(t *testing.T) *packetFixture {
	db := testDatabase(t)
	for _, c := range store.DefaultCurrencies {
		must(t, db.Create(&c).Error)
	}
	st := &store.Store{DB: db}
	f := &packetFixture{db: db, w: wallet.New(st, "USDT", "1", "0")}
	f.svc = redpacket.New(st, f.w, nil).WithNotifier(func(_ context.Context, _ uint64, tg int64, html string) error {
		f.notices = append(f.notices, fmt.Sprintf("%d:%s", tg, html))
		return nil
	})
	f.sender = f.user(t, 8000, "sender")
	must(t, f.w.CreditDeposit(model.DepositOrder{UserID: f.sender.ID, Network: "TRC20", Address: "a", Amount: "100", TxID: "tx-rp"}))
	return f
}

func (f *packetFixture) user(t *testing.T, tg int64, username string) model.TelegramUser {
	u := model.TelegramUser{TelegramUserID: tg, Username: username, FirstName: username, ReferralCode: fmt.Sprintf("rp%d", tg), Status: 1}
	must(t, f.db.Create(&u).Error)
	return u
}

func claimerOf(u model.TelegramUser) redpacket.Claimer {
	return redpacket.Claimer{UserID: u.ID, TelegramUserID: u.TelegramUserID, Username: u.Username, Name: u.FirstName}
}

func (f *packetFixture) create(t *testing.T, in redpacket.CreateInput) *model.RedPacket {
	in.SenderUserID, in.SenderTelegramID, in.SenderName = f.sender.ID, f.sender.TelegramUserID, "sender"
	if in.Currency == "" {
		in.Currency = "USDT"
	}
	p, err := f.svc.Create(in)
	must(t, err)
	return p
}

func TestRedPacketConcurrentClaimsSplitExactly(t *testing.T) {
	f := newPacketFixture(t)
	p := f.create(t, redpacket.CreateInput{Type: redpacket.TypeLucky, Amount: "10", Count: 5, RequestID: "rp-1"})
	again := f.create(t, redpacket.CreateInput{Type: redpacket.TypeLucky, Amount: "10", Count: 5, RequestID: "rp-1"})
	if again.ID != p.ID || balanceOf(t, f.w, f.sender.ID, "USDT") != "90" {
		t.Fatal("repeated create charged twice")
	}
	var users []model.TelegramUser
	for i := 0; i < 12; i++ {
		users = append(users, f.user(t, int64(8100+i), fmt.Sprintf("grab%d", i)))
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := 0
	for _, u := range users {
		for k := 0; k < 2; k++ { // each user taps twice
			wg.Add(1)
			go func(u model.TelegramUser) {
				defer wg.Done()
				if _, _, err := f.svc.Claim(p.Code, claimerOf(u)); err == nil {
					mu.Lock()
					got++
					mu.Unlock()
				}
			}(u)
		}
	}
	wg.Wait()
	if got != 5 {
		t.Fatalf("%d successful claims, want 5", got)
	}
	sum := decimal.Zero
	for _, u := range users {
		b, _ := decimal.NewFromString(balanceOf(t, f.w, u.ID, "USDT"))
		sum = sum.Add(b)
	}
	if !sum.Equal(decimal.NewFromInt(10)) {
		t.Fatalf("claimed %s, want 10", sum)
	}
	q, claims, err := f.svc.ByCode(p.Code)
	must(t, err)
	if q.Status != redpacket.StatusFinished || len(claims) != 5 || !strings.Contains(f.svc.Card(q, claims), "手气最佳") {
		t.Fatalf("status %s, %d claims", q.Status, len(claims))
	}
}

func TestRedPacketExclusivePasswordAndCurrencies(t *testing.T) {
	f := newPacketFixture(t)
	alice, bob := f.user(t, 8201, "alice"), f.user(t, 8202, "bob")
	must(t, f.w.AdminAdjust(f.sender.ID, "THB", "100", "admin", "test"))
	ex := f.create(t, redpacket.CreateInput{Type: redpacket.TypeExclusive, Currency: "THB", Amount: "20.5", Target: "@Alice", RequestID: "rp-ex"})
	if _, _, err := f.svc.Claim(ex.Code, claimerOf(bob)); err == nil {
		t.Fatal("exclusive packet claimed by someone else")
	}
	if _, _, err := f.svc.Claim(ex.Code, claimerOf(alice)); err != nil {
		t.Fatal(err)
	}
	if balanceOf(t, f.w, alice.ID, "THB") != "20.5" || balanceOf(t, f.w, f.sender.ID, "THB") != "79.5" {
		t.Fatal("exclusive THB packet balances wrong")
	}
	pw := f.create(t, redpacket.CreateInput{Type: redpacket.TypePassword, Amount: "3", Count: 2, Password: "发财", ChatID: -100777, RequestID: "rp-pw"})
	if _, _, err := f.svc.Claim(pw.Code, claimerOf(bob)); err == nil {
		t.Fatal("password packet claimed with the button")
	}
	if _, _, matched, _ := f.svc.ClaimByPassword(-100777, "不对", claimerOf(bob)); matched {
		t.Fatal("wrong password matched")
	}
	if _, _, matched, _ := f.svc.ClaimByPassword(-100888, "发财", claimerOf(bob)); matched {
		t.Fatal("password matched in another group")
	}
	if c, _, matched, err := f.svc.ClaimByPassword(-100777, "发财", claimerOf(bob)); !matched || err != nil || c == nil {
		t.Fatalf("password claim failed: %v", err)
	}
	if _, _, matched, err := f.svc.ClaimByPassword(0, "发财", claimerOf(bob)); !matched || err == nil {
		t.Fatal("second password claim by the same user accepted")
	}
	// Equal packets need enough money for every share; not enough balance is refused.
	if _, err := f.svc.Create(redpacket.CreateInput{SenderUserID: f.sender.ID, Type: redpacket.TypeEqual, Currency: "USDT", Amount: "50", Count: 3, RequestID: "rp-big"}); err == nil {
		t.Fatal("packet larger than the balance accepted")
	}
	if _, err := f.svc.Create(redpacket.CreateInput{SenderUserID: f.sender.ID, Type: redpacket.TypeLucky, Currency: "MMK", Amount: "2", Count: 3, RequestID: "rp-small"}); err == nil {
		t.Fatal("packet smaller than one unit per share accepted")
	}
}

func TestRedPacketExpiryRefundsTheRest(t *testing.T) {
	f := newPacketFixture(t)
	grabber := f.user(t, 8301, "grabber")
	p := f.create(t, redpacket.CreateInput{Type: redpacket.TypeEqual, Amount: "2", Count: 4, RequestID: "rp-exp"})
	_, _, err := f.svc.Claim(p.Code, claimerOf(grabber))
	must(t, err)
	must(t, f.db.Model(&model.RedPacket{}).Where("id = ?", p.ID).Update("expires_at", time.Now().Add(-time.Minute)).Error)
	if _, _, err := f.svc.Claim(p.Code, claimerOf(f.user(t, 8302, "late"))); err == nil {
		t.Fatal("expired packet claimed")
	}
	must(t, f.svc.ExpireDue(context.Background()))
	must(t, f.svc.ExpireDue(context.Background())) // a second run must not refund again
	if balanceOf(t, f.w, f.sender.ID, "USDT") != "98" || len(f.notices) != 1 {
		t.Fatalf("sender balance %s, notices %v", balanceOf(t, f.w, f.sender.ID, "USDT"), f.notices)
	}
	q, _, _ := f.svc.ByCode(p.Code)
	if q.Status != redpacket.StatusExpired || q.RefundAmount != "6.000000" {
		t.Fatalf("status %s refund %s", q.Status, q.RefundAmount)
	}
}
