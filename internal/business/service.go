package business

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"telegram-business-bot/internal/lease"
	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/telegram"
)

type Service struct {
	st                 *store.Store
	primary            telegram.Sender
	primaryID, adminID int64
	primaryToken       string
	aead               cipher.AEAD
	verify             func(context.Context, string) (*telegram.User, error)
	Router             func(model.BusinessBot, *telegram.Client) *telegram.Router
}

func New(st *store.Store, primary telegram.Sender, key, primaryToken, adminToken string) *Service {
	// Existing installations can use their high-entropy primary token as the key source.
	// An explicit BUSINESS_BOT_KEY keeps encryption independent of primary token rotation.
	if key == "" {
		key = primaryToken
	}
	hash := sha256.Sum256([]byte("business-bot-token:v1:" + key))
	block, _ := aes.NewCipher(hash[:])
	aead, _ := cipher.NewGCM(block)
	parse := func(token string) int64 { id, _ := strconv.ParseInt(strings.Split(token, ":")[0], 10, 64); return id }
	return &Service{st: st, primary: primary, primaryToken: primaryToken, primaryID: parse(primaryToken), adminID: parse(adminToken), aead: aead, verify: func(ctx context.Context, token string) (*telegram.User, error) {
		return telegram.NewClient(token).GetMe(ctx)
	}}
}
func (s *Service) encrypt(token string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(s.aead.Seal(nonce, nonce, []byte(token), nil)), nil
}
func (s *Service) decrypt(value string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(raw) < s.aead.NonceSize() {
		return "", errors.New("机器人密钥无法读取")
	}
	plain, err := s.aead.Open(nil, raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():], nil)
	if err != nil {
		return "", errors.New("机器人密钥无法解密，请检查 BUSINESS_BOT_KEY")
	}
	return string(plain), nil
}
func (s *Service) List() ([]model.BusinessBot, error) {
	rows := []model.BusinessBot{}
	err := s.st.DB.Order("id asc").Find(&rows).Error
	return rows, err
}

// Bot purposes: every feature, only red packets and the wallet, or only news.
const (
	PurposeFull      = "full"
	PurposeRedPacket = "redpacket"
	PurposeNews      = "news"
)

func (s *Service) Save(ctx context.Context, id uint64, name, token string, enabled bool, purpose string) (*model.BusinessBot, error) {
	if purpose == "" {
		purpose = PurposeFull
	}
	if purpose != PurposeFull && purpose != PurposeRedPacket && purpose != PurposeNews {
		return nil, errors.New("未知的机器人用途")
	}
	name = strings.TrimSpace(name)
	token = strings.TrimSpace(token)
	if name == "" || len([]rune(name)) > 128 {
		return nil, errors.New("请输入 1–128 字的机器人名称")
	}
	var row model.BusinessBot
	if id > 0 {
		if err := s.st.DB.First(&row, id).Error; err != nil {
			return nil, err
		}
	}
	if token != "" {
		checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		me, err := s.verify(checkCtx, token)
		if err != nil {
			return nil, errors.New("Token 验证失败，请检查 Token 或网络连接")
		}
		if me.ID == s.primaryID || me.ID == s.adminID {
			return nil, errors.New("默认或管理机器人已由环境配置管理，请使用另一个业务机器人")
		}
		if id > 0 && me.ID != row.TelegramBotID {
			return nil, errors.New("不能将已有渠道替换为另一个机器人，请新增渠道")
		}
		encrypted, err := s.encrypt(token)
		if err != nil {
			return nil, err
		}
		row.TelegramBotID = me.ID
		row.Username = me.Username
		row.TokenEncrypted = encrypted
	}
	if row.TokenEncrypted == "" {
		return nil, errors.New("新增机器人需要 Token")
	}
	row.Name = name
	row.Enabled = enabled
	row.Purpose = purpose
	var err error
	if id == 0 {
		err = s.st.DB.Create(&row).Error
	} else {
		// Never overwrite the poller's offset or health state with an old UI snapshot.
		err = s.st.DB.Model(&model.BusinessBot{}).Where("id=?", id).Updates(map[string]any{"name": row.Name, "enabled": enabled, "purpose": purpose, "token_encrypted": row.TokenEncrypted, "username": row.Username}).Error
	}
	if err != nil {
		return nil, errors.New("保存失败：机器人可能已存在，请检查数据库连接")
	}
	return &row, nil
}
func (s *Service) Sender(ctx context.Context, id uint64) (telegram.Sender, error) {
	if id == 0 {
		return s.primary, nil
	}
	var row model.BusinessBot
	if err := s.st.DB.WithContext(ctx).First(&row, id).Error; err != nil {
		return nil, err
	}
	if !row.Enabled {
		return nil, errors.New("发送机器人已停用，请启用后恢复任务")
	}
	token, err := s.decrypt(row.TokenEncrypted)
	if err != nil {
		return nil, err
	}
	return telegram.NewClient(token), nil
}

// NotifyUser sends an HTML message to a user through the first bot that reaches them: prefer
// (the bot they used for the operation), then the primary bot, then the other enabled business
// bots except news bots. A bot can only message users who have started it.
func (s *Service) NotifyUser(ctx context.Context, prefer uint64, telegramUserID int64, html string) error {
	ids := []uint64{prefer}
	if prefer != 0 {
		ids = append(ids, 0)
	}
	var rows []model.BusinessBot
	if err := s.st.DB.WithContext(ctx).Where("enabled = ? AND purpose <> ?", true, "news").Order("id asc").Find(&rows).Error; err == nil {
		for _, b := range rows {
			if b.ID != prefer {
				ids = append(ids, b.ID)
			}
		}
	}
	var last error
	for _, id := range ids {
		sender, err := s.Sender(ctx, id)
		if err == nil {
			if err = sender.SendMessageWithParseMode(ctx, telegramUserID, html, "HTML", nil); err == nil {
				return nil
			}
		}
		last = err
	}
	return last
}

// BotToken pairs a bot ID (0 = primary bot) with its token.
type BotToken struct {
	ID    uint64
	Token string
}

// Tokens lists the primary bot and every enabled business bot, e.g. to verify Mini App sign-ins.
func (s *Service) Tokens(ctx context.Context) ([]BotToken, error) {
	out := []BotToken{{ID: 0, Token: s.primaryToken}}
	var rows []model.BusinessBot
	if err := s.st.DB.WithContext(ctx).Where("enabled=?", true).Order("id asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, b := range rows {
		if token, err := s.decrypt(b.TokenEncrypted); err == nil {
			out = append(out, BotToken{ID: b.ID, Token: token})
		}
	}
	return out, nil
}
func (s *Service) Resolve(ctx context.Context, category string, override *uint64) (uint64, error) {
	id := uint64(0)
	if override != nil {
		id = *override
	} else {
		var route model.PushRoute
		err := s.st.DB.WithContext(ctx).Where("category=?", category).First(&route).Error
		if err == nil {
			id = route.BotID
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, err
		}
	}
	_, err := s.Sender(ctx, id)
	return id, err
}
func (s *Service) Routes() ([]model.PushRoute, error) {
	rows := []model.PushRoute{}
	err := s.st.DB.Order("category asc").Find(&rows).Error
	return rows, err
}
func (s *Service) SaveRoute(ctx context.Context, category string, botID uint64) error {
	category = strings.TrimSpace(category)
	if category == "" || len([]rune(category)) > 64 {
		return errors.New("业务类型需为 1–64 字")
	}
	if _, err := s.Sender(ctx, botID); err != nil {
		return err
	}
	return s.st.DB.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "category"}}, DoUpdates: clause.AssignmentColumns([]string{"bot_id", "updated_at"})}).Create(&model.PushRoute{Category: category, BotID: botID}).Error
}
func (s *Service) DeleteRoute(id uint64) error {
	res := s.st.DB.Delete(&model.PushRoute{}, id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// Run reconciles enabled channels without requiring an application restart.
func (s *Service) Run(ctx context.Context) {
	type running struct {
		cancel  context.CancelFunc
		done    chan struct{}
		token   string
		purpose string
	}
	active := map[uint64]running{}
	var wg sync.WaitGroup
	defer func() {
		for _, r := range active {
			r.cancel()
		}
		wg.Wait()
	}()
	reconcile := func() {
		rows, err := s.List()
		if err != nil {
			return
		}
		enabled := map[uint64]model.BusinessBot{}
		for _, b := range rows {
			if b.Enabled {
				enabled[b.ID] = b
			}
		}
		for id, r := range active {
			b, ok := enabled[id]
			finished := false
			select {
			case <-r.done:
				finished = true
			default:
			}
			// A new token or purpose restarts the bot with a new router.
			if !ok || b.TokenEncrypted != r.token || b.Purpose != r.purpose || finished {
				r.cancel()
				<-r.done
				delete(active, id)
			}
		}
		for id, b := range enabled {
			if _, ok := active[id]; ok {
				continue
			}
			token, err := s.decrypt(b.TokenEncrypted)
			if err != nil {
				s.st.DB.Model(&b).Update("last_error", err.Error())
				continue
			}
			child, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			active[id] = running{cancel, done, b.TokenEncrypted, b.Purpose}
			wg.Add(1)
			go func(id uint64, token string) {
				defer wg.Done()
				defer close(done)
				_ = lease.Run(child, s.st.Redis, fmt.Sprintf("business:poll:%d", id), func(ctx context.Context) error { return s.poll(ctx, id, token) })
			}(id, token)
		}
	}
	reconcile()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcile()
		}
	}
}
func (s *Service) poll(ctx context.Context, id uint64, token string) error {
	client := telegram.NewClient(token)
	if err := client.DeleteWebhook(ctx, false); err != nil {
		s.st.DB.Model(&model.BusinessBot{}).Where("id=?", id).Update("last_error", "启动失败，请检查 Token 和网络")
		return errors.New("initialize business bot failed")
	}
	var row model.BusinessBot
	if err := s.st.DB.First(&row, id).Error; err != nil {
		return err
	}
	router := s.Router(row, client)
	commandCtx, cancelCommands := context.WithCancel(ctx)
	defer cancelCommands()
	go router.SyncCommandMenu(commandCtx)
	offset := row.Offset
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		updates, err := client.GetUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.st.DB.Model(&row).Update("last_error", "获取更新失败，请检查网络或是否有其他服务使用此机器人")
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
			continue
		}
		for _, u := range updates {
			if err := router.Handle(ctx, u); err != nil {
				return err
			}
			offset = u.UpdateID + 1
			if err := s.st.DB.Model(&row).Update("offset", offset).Error; err != nil {
				return err
			}
		}
		now := time.Now()
		if err := s.st.DB.Model(&row).Updates(map[string]any{"last_seen_at": &now, "last_error": ""}).Error; err != nil {
			return err
		}
	}
}
