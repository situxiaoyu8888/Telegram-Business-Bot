package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	mysqldriver "github.com/go-sql-driver/mysql"
	"time"

	"telegram-business-bot/internal/model"

	"github.com/redis/go-redis/v9"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Store struct {
	DB    *gorm.DB
	Redis *redis.Client
}

func New(mysqlDSN, redisAddr, redisPassword string, redisDB int) (*Store, error) {
	db, err := gorm.Open(mysql.Open(mysqlDSN), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("connect mysql: %w", err)
	}
	if err := db.AutoMigrate(
		&model.TelegramUser{}, &model.WalletAccount{}, &model.WalletTransaction{}, &model.DepositAddress{}, &model.DepositOrder{},
		&model.WithdrawalOrder{}, &model.ReferralCommission{}, &model.SupportTicket{}, &model.MenuItem{}, &model.SystemConfig{}, &model.ActionLog{},
		&model.Merchant{}, &model.BusinessBot{}, &model.BotSubscription{}, &model.PushRoute{}, &model.PushCampaign{}, &model.PushRecipient{}, &model.ReferralGrant{}, &model.SeedMigration{}, &model.AdminUser{}, &model.AdminRole{},
		&model.ShopCategory{}, &model.ShopProduct{}, &model.ShopSKU{}, &model.ShopVirtualCode{}, &model.ShopCartItem{}, &model.ShopAddress{},
		&model.ShopOrder{}, &model.ShopOrderItem{}, &model.ShopCoupon{}, &model.ShopUserCoupon{}, &model.ShopAfterSale{},
		&model.ShopBanner{}, &model.ShopReview{}, &model.ShopMerchantEntry{}, &model.ShopSettlement{}, &model.ShopFavorite{}, &model.ShopView{}, &model.ShopMember{}, &model.ShopPointsLog{}, &model.ShopGroup{},
		&model.ShopMerchantApplication{}, &model.ShopConversation{}, &model.ShopMessage{}, &model.AdminAuditLog{},
		&model.WalletCurrency{}, &model.WalletExchange{}, &model.WalletTransfer{}, &model.RedPacket{}, &model.RedPacketClaim{}, &model.RedPacketMessage{}, &model.RedPacketCover{}, &model.DepositPoolAddress{}, &model.PaymentPassword{},
		&model.NewsCategory{}, &model.NewsArticle{}, &model.NewsFeed{}, &model.NewsChat{}, &model.NewsSubscription{}, &model.NewsDelivery{},
	); err != nil {
		return nil, fmt.Errorf("auto migrate: %w", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr, Password: redisPassword, DB: redisDB})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("connect redis: %w", err)
	}
	s := &Store{DB: db, Redis: rdb}
	if err := s.seedDefaults(); err != nil {
		return nil, err
	}
	if err := s.seedSubscriptions(); err != nil {
		return nil, err
	}
	if err := s.seedShopCommissions(); err != nil {
		return nil, err
	}
	// Added after defaults_v1; the unique key makes this a no-op once present.
	for _, c := range []model.SystemConfig{
		{Key: "welcome_text", Value: model.DefaultWelcomeText, Remark: "首次激活欢迎语（留空则不发送）"},
		{Key: "shop_auto_complete_days", Value: "7", Remark: "商城快递订单发货后自动确认收货的天数（0 为不自动确认）"},
		{Key: "shop_aftersale_days", Value: "7", Remark: "商城订单完成后可申请售后的天数（0 为不允许完成后申请）"},
		{Key: "shop_return_address", Value: "", Remark: "商城退货寄回地址（留空则使用商家地址）"},
		{Key: "shop_low_stock", Value: "5", Remark: "商城销售报表和每日待办中的低库存阈值"},
		{Key: "shop_digest_hour", Value: "9", Remark: "每天几点（0–23）给商家和管理员发送待办提醒；-1 为关闭"},
		{Key: "shop_ship_overdue_hours", Value: "24", Remark: "付款后超过多少小时未发货算作超时，计入每日待办"},
		{Key: "shop_points_per_usdt", Value: "10", Remark: "商城每实付 1 USDT 获得的积分（订单完成且售后期结束后发放，0 为不发放）"},
		{Key: "shop_points_value", Value: "0.01", Remark: "1 积分可抵扣的 USDT（0 为不能抵扣）"},
		{Key: "shop_points_max_ratio", Value: "0.5", Remark: "积分最多抵扣订单金额的比例（0.5 = 50%）"},
		{Key: "shop_member_levels", Value: "白银:100:0.98,黄金:500:0.95,钻石:2000:0.9", Remark: "会员等级，格式：名称:累计消费 USDT:折扣,…（0.95 = 95 折）；留空则不分等级"},
		{Key: "redpacket_expire_hours", Value: "24", Remark: "红包有效时长（小时，1–168）；到期未领完的金额自动退回发红包的人"},
		{Key: "redpacket_max_count", Value: "100", Remark: "单个红包最多可拆分的份数（1–1000）"},
		{Key: "exchange_fee_rate", Value: "0", Remark: "换汇手续费比例（0.005 = 0.5%，从兑换所得中扣除）"},
		{Key: "shop_referral_rate", Value: "0", Remark: "商城推广返佣比例（0.05 = 5%，0 为关闭）；订单完成且售后期结束后发给直属上级"},
		{Key: "shop_categories_from_menu", Value: "1", Remark: "商城分类跟随机器人菜单：开启时 H5 商城和后台「商城分类」显示机器人菜单的分类；关闭后在「商城分类」中手动配置"},
	} {
		if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&c).Error; err != nil {
			return nil, err
		}
	}
	// Currencies are seeded once each; rates and switches are then managed in the console.
	for _, c := range DefaultCurrencies {
		if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&c).Error; err != nil {
			return nil, err
		}
	}
	if err := s.seedCurrencyFlags(); err != nil {
		return nil, err
	}
	if err := s.seedRedPacketMenu(); err != nil {
		return nil, err
	}
	return s, nil
}

// seedCurrencyFlags runs once for currencies seeded by the first multi-currency release (no
// icons, fiat exchange off): it adds the default flags and opens exchange for every currency.
func (s *Store) seedCurrencyFlags() error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		claim := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.SeedMigration{Name: "wallet_currencies_v2"})
		if claim.Error != nil || claim.RowsAffected == 0 {
			return claim.Error
		}
		for _, c := range DefaultCurrencies {
			if c.Icon == "" {
				continue
			}
			if err := tx.Model(&model.WalletCurrency{}).Where("code = ? AND icon = ''", c.Code).Update("icon", c.Icon).Error; err != nil {
				return err
			}
		}
		return tx.Model(&model.WalletCurrency{}).Where("exchangeable = ?", false).Update("exchangeable", true).Error
	})
}

// seedRedPacketMenu adds the "发红包" entry once; administrators may move or disable it later.
func (s *Store) seedRedPacketMenu() error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		claim := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.SeedMigration{Name: "menu_redpacket_v1"})
		if claim.Error != nil || claim.RowsAffected == 0 {
			return claim.Error
		}
		var n int64
		if err := tx.Model(&model.MenuItem{}).Where("code = ?", "redpacket").Count(&n).Error; err != nil || n > 0 {
			return err
		}
		return tx.Create(&model.MenuItem{Code: "redpacket", Title: "🧧 发红包", ActionType: "builtin", ActionValue: "redpacket", Role: "user", Sort: 75, RowNo: 4, Enabled: true}).Error
	})
}

// DefaultCurrencies are the wallet currencies created on first start.
var DefaultCurrencies = []model.WalletCurrency{
	{Code: "USDT", Name: "USDT-TRC20", Kind: "trc20", Rate: "1", Decimals: 6, MinWithdraw: "0", WithdrawFee: "0", Exchangeable: true, Withdrawable: true, Enabled: true, Sort: 10},
	{Code: "USDT_ERC20", Name: "USDT-ERC20", Kind: "erc20", Rate: "1", Decimals: 6, MinWithdraw: "10", WithdrawFee: "0", Exchangeable: true, Withdrawable: true, Enabled: true, Sort: 20},
	{Code: "CNY", Name: "人民币", Icon: "🇨🇳", Kind: "fiat", Rate: "7.2", Decimals: 2, MinWithdraw: "100", WithdrawFee: "0", Exchangeable: true, Enabled: true, Sort: 30},
	{Code: "THB", Name: "泰铢", Icon: "🇹🇭", Kind: "fiat", Rate: "33", Decimals: 2, MinWithdraw: "500", WithdrawFee: "0", Exchangeable: true, Enabled: true, Sort: 40},
	{Code: "MMK", Name: "缅币", Icon: "🇲🇲", Kind: "fiat", Rate: "4200", Decimals: 0, MinWithdraw: "50000", WithdrawFee: "0", Exchangeable: true, Enabled: true, Sort: 50},
	{Code: "USD", Name: "美元", Icon: "🇺🇸", Kind: "fiat", Rate: "1", Decimals: 2, MinWithdraw: "10", WithdrawFee: "0", Exchangeable: true, Enabled: true, Sort: 60},
}

func (s *Store) Close() error {
	if sqlDB, e := s.DB.DB(); e == nil {
		_ = sqlDB.Close()
	}
	return s.Redis.Close()
}

func (s *Store) UpsertTelegramUser(userID int64, username, firstName, lastName, language string) (*model.TelegramUser, error) {
	u := model.TelegramUser{TelegramUserID: userID, Username: username, FirstName: firstName, LastName: lastName, LanguageCode: language, ReferralCode: fmt.Sprintf("TG%X", userID), Status: 1}
	err := s.DB.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "telegram_user_id"}}, DoUpdates: clause.AssignmentColumns([]string{"username", "first_name", "last_name", "language_code", "updated_at"})}).Create(&u).Error
	if err != nil {
		return nil, err
	}
	err = s.DB.Where("telegram_user_id=?", userID).First(&u).Error
	return &u, err
}

func (s *Store) UserByTelegramID(tgID int64) (model.TelegramUser, error) {
	var u model.TelegramUser
	err := s.DB.Where("telegram_user_id=?", tgID).First(&u).Error
	return u, err
}
func (s *Store) LogAction(userID int64, action string, payload any) {
	b, _ := json.Marshal(payload)
	_ = s.DB.Create(&model.ActionLog{TelegramUserID: userID, Action: action, Payload: string(b), CreatedAt: time.Now()}).Error
}

func (s *Store) seedDefaults() error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		var marker model.SeedMigration
		err := tx.Where("name=?", "defaults_v1").First(&marker).Error
		if err == nil {
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		claim := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.SeedMigration{Name: "defaults_v1"})
		if claim.Error != nil {
			return claim.Error
		}
		if claim.RowsAffected == 0 {
			return nil
		}
		var existing int64
		if err := tx.Model(&model.MenuItem{}).Count(&existing).Error; err != nil {
			return err
		}

		// A pre-upgrade installation may have intentionally deleted every menu.
		var configs int64
		if err := tx.Model(&model.SystemConfig{}).Count(&configs).Error; err != nil {
			return err
		}
		defaults := []model.MenuItem{
			{Code: "profile", Title: "👤 个人中心", ActionType: "builtin", ActionValue: "profile", Role: "user", Sort: 10, RowNo: 1, Enabled: true},
			{Code: "wallet", Title: "💰 我的钱包", ActionType: "builtin", ActionValue: "wallet", Role: "user", Sort: 20, RowNo: 1, Enabled: true},
			{Code: "deposit", Title: "💵 USDT充值", ActionType: "builtin", ActionValue: "deposit", Role: "user", Sort: 30, RowNo: 2, Enabled: true},
			{Code: "withdraw", Title: "💸 USDT提现", ActionType: "builtin", ActionValue: "withdraw", Role: "user", Sort: 40, RowNo: 2, Enabled: true},
			{Code: "records", Title: "📜 资金记录", ActionType: "builtin", ActionValue: "records", Role: "user", Sort: 50, RowNo: 3, Enabled: true},
			{Code: "referral", Title: "🎁 推广代理", ActionType: "builtin", ActionValue: "referral", Role: "user", Sort: 60, RowNo: 3, Enabled: true},
			{Code: "support", Title: "🎧 在线客服", ActionType: "builtin", ActionValue: "support", Role: "user", Sort: 70, RowNo: 4, Enabled: true},
			{Code: "admin_dashboard", Title: "📊 管理概览", ActionType: "builtin", ActionValue: "admin_dashboard", Role: "admin", Sort: 80, RowNo: 5, Enabled: true},
			{Code: "admin_withdrawals", Title: "💸 待审提现", ActionType: "builtin", ActionValue: "admin_withdrawals", Role: "admin", Sort: 85, RowNo: 5, Enabled: true},
			{Code: "admin_tickets", Title: "🎧 待处理工单", ActionType: "builtin", ActionValue: "admin_tickets", Role: "admin", Sort: 90, RowNo: 6, Enabled: true},
			{Code: "admin", Title: "🛡 Web后台", ActionType: "builtin", ActionValue: "admin", Role: "admin", Sort: 100, RowNo: 6, Enabled: true},
		}
		if existing == 0 && configs == 0 {
			for _, item := range defaults {
				var c int64
				if err := tx.Model(&model.MenuItem{}).Where("code=?", item.Code).Count(&c).Error; err != nil {
					return err
				}
				if c == 0 {
					if err := tx.Create(&item).Error; err != nil {
						return err
					}
				}
			}
		}
		cfgs := []model.SystemConfig{{Key: "site_name", Value: "Telegram Business Bot", Remark: "机器人显示名称"}, {Key: "support_text", Value: "请描述你的问题，客服会在后台处理。", Remark: "客服入口文案"}, {Key: "deposit_notice", Value: "仅支持 USDT TRC20，请勿充值其他资产。", Remark: "充值提醒"}}
		for _, c := range cfgs {
			var n int64
			if err := tx.Model(&model.SystemConfig{}).Where("`key`=?", c.Key).Count(&n).Error; err != nil {
				return err
			}
			if n == 0 {
				if err := tx.Create(&c).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *Store) seedSubscriptions() error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		var marker model.SeedMigration
		err := tx.Where("name=?", "default_bot_subscriptions_v1").First(&marker).Error
		if err == nil {
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		claim := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.SeedMigration{Name: "default_bot_subscriptions_v1"})
		if claim.Error != nil {
			return claim.Error
		}
		if claim.RowsAffected == 0 {
			return nil
		}
		return tx.Exec("INSERT IGNORE INTO bot_subscriptions (bot_id, telegram_user_id, created_at) SELECT 0, telegram_user_id, CURRENT_TIMESTAMP FROM telegram_users").Error
	})
}

// seedShopCommissions marks orders that already ended before shop referral commissions existed,
// so enabling shop_referral_rate pays only for orders completed from now on.
func (s *Store) seedShopCommissions() error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		claim := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.SeedMigration{Name: "shop_commission_v1"})
		if claim.Error != nil || claim.RowsAffected == 0 {
			return claim.Error
		}
		return tx.Model(&model.ShopOrder{}).Where("status IN ?", []string{"completed", "canceled"}).Update("commission_done", true).Error
	})
}

// IsDuplicate reports whether err is a MySQL/MariaDB unique-key violation (error 1062).
// Use it instead of OnConflict{DoNothing} plus RowsAffected for tables with an auto-increment ID:
// on MariaDB GORM adds RETURNING to those inserts, so a skipped duplicate still reports one row.
func IsDuplicate(err error) bool {
	var me *mysqldriver.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}
