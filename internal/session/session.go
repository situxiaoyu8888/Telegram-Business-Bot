package session

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type Session struct {
	Scene string         `json:"scene"`
	Step  string         `json:"step"`
	Data  map[string]any `json:"data,omitempty"`
}

type Manager struct {
	rdb       *redis.Client
	ttl       time.Duration
	namespace string
}

func New(rdb *redis.Client, ttl time.Duration) *Manager {
	return &Manager{rdb: rdb, ttl: ttl, namespace: "user"}
}

func (m *Manager) key(userID int64) string {
	return fmt.Sprintf("tg:session:%s:%d", m.namespace, userID)
}

func (m *Manager) Get(ctx context.Context, userID int64) (*Session, error) {
	raw, err := m.rdb.Get(ctx, m.key(userID)).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (m *Manager) Set(ctx context.Context, userID int64, s Session) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return m.rdb.Set(ctx, m.key(userID), raw, m.ttl).Err()
}

func (m *Manager) Clear(ctx context.Context, userID int64) error {
	return m.rdb.Del(ctx, m.key(userID)).Err()
}

func (m *Manager) WithNamespace(namespace string) *Manager {
	return &Manager{rdb: m.rdb, ttl: m.ttl, namespace: namespace}
}
