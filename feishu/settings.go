package feishu

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	livechat "github.com/KevinGong2013/feishu-livechat"
)

// Settings is a connected app and its 话题群 — what the operator page's scan
// flow produces. It holds the app secret, so store it sealed.
type Settings struct {
	AppID       string    `json:"app_id"`
	AppSecret   string    `json:"app_secret"`
	ChatID      string    `json:"chat_id"`
	Lark        bool      `json:"lark,omitempty"`
	BotName     string    `json:"bot_name,omitempty"`
	GroupName   string    `json:"group_name,omitempty"`
	ConnectedAt time.Time `json:"connected_at"`
}

// SettingsStore persists Settings. Load returns nil, nil when nothing has
// been stored yet.
type SettingsStore interface {
	Load(ctx context.Context) (*Settings, error)
	Save(ctx context.Context, s *Settings) error
}

// KV is a small key/value store; sqlstore.Store implements it.
type KV interface {
	GetSetting(ctx context.Context, key string) (string, error) // livechat.ErrNotFound when absent
	PutSetting(ctx context.Context, key, value string) error
}

// Sealer encrypts settings at rest. Hosts pass their own key management
// (envelope encryption, a KMS); nil stores them as plain JSON.
type Sealer interface {
	Seal(plaintext []byte) ([]byte, error)
	Open(sealed []byte) ([]byte, error)
}

const settingsKey = "feishu"

// KVSettings stores Settings in kv under one key, sealed when sealer is not
// nil.
func KVSettings(kv KV, sealer Sealer) SettingsStore {
	return &kvSettings{kv: kv, sealer: sealer}
}

type kvSettings struct {
	kv     KV
	sealer Sealer
}

func (k *kvSettings) Load(ctx context.Context) (*Settings, error) {
	v, err := k.kv.GetSetting(ctx, settingsKey)
	if errors.Is(err, livechat.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	raw := []byte(v)
	if k.sealer != nil {
		sealed, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, err
		}
		if raw, err = k.sealer.Open(sealed); err != nil {
			return nil, err
		}
	}
	var s Settings
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (k *kvSettings) Save(ctx context.Context, s *Settings) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	v := string(raw)
	if k.sealer != nil {
		sealed, err := k.sealer.Seal(raw)
		if err != nil {
			return err
		}
		v = base64.StdEncoding.EncodeToString(sealed)
	}
	return k.kv.PutSetting(ctx, settingsKey, v)
}
