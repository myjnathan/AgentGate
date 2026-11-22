// Package orgsync syncs the employee directory from a third-party
// collaboration platform (DingTalk / Feishu / WeCom) into AgentGate
// accounts (backlog F21), and binds the third-party identity to the
// platform account at login/sync time, in parallel with F9 OIDC.
//
// Design borrowed from dsh-ee (hikariming/dsh-ee, Apache-2.0,
// compatible with AGPL-3.0): server/internal/dingtalk/client.go and
// directory.go (app-token cache, {errcode,errmsg,result} envelope,
// BFS department walk with cursor pagination, fail the whole sync on
// any page error, per-user state confirmation before disabling) and
// server/internal/feishu/client.go (tenant token, contact v3
// parentchildren / find_by_department, error codes 41012/41050).
// The code here is adapted to AgentGate's store/secretbox types and
// unified across the three providers behind one DirectoryClient
// interface; WeCom follows the official qyapi.weixin.qq.com shapes
// (gettoken / department/list / user/list / user/get), which dsh-ee
// does not implement. See docs/org-sync.md and the DEVLOG entry.
package orgsync

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Provider is a third-party directory platform.
type Provider string

const (
	ProviderDingTalk Provider = "dingtalk"
	ProviderFeishu   Provider = "feishu"
	ProviderWeCom    Provider = "wecom"
)

// ParseProvider validates a provider name from API input.
func ParseProvider(s string) (Provider, bool) {
	switch Provider(strings.ToLower(strings.TrimSpace(s))) {
	case ProviderDingTalk:
		return ProviderDingTalk, true
	case ProviderFeishu:
		return ProviderFeishu, true
	case ProviderWeCom:
		return ProviderWeCom, true
	}
	return "", false
}

// DefaultBaseURL is the provider's public API host. A provider
// config may override it (private deployment / test server).
func (p Provider) DefaultBaseURL() string {
	switch p {
	case ProviderDingTalk:
		return "https://oapi.dingtalk.com"
	case ProviderFeishu:
		return "https://open.feishu.cn"
	case ProviderWeCom:
		return "https://qyapi.weixin.qq.com"
	}
	return ""
}

// Subject is the provider-scoped stable identity key, e.g.
// "dingtalk:<corpID>:<userID>". The same person on two providers
// (or two corps) never collides; dsh-ee uses the same shape
// (dingtalk:<CorpId>:<userId>) for its external subject.
func Subject(provider Provider, corpID, externalID string) string {
	return string(provider) + ":" + corpID + ":" + externalID
}

// Config is the decrypted provider configuration a client needs.
// Secrets arrive decrypted from the store (they are encrypted at
// rest with the master key); they are never logged or returned.
type Config struct {
	Provider  Provider
	CorpID    string // DingTalk corpId / WeCom corpid; Feishu uses AppKey
	AppKey    string // DingTalk appKey / Feishu app_id; WeCom unused
	AppSecret string // DingTalk appSecret / Feishu app_secret / WeCom corpsecret
	BaseURL   string // optional override; DefaultBaseURL when empty
	HTTP      *http.Client
	// AutoCreate provisions a platform account for a directory
	// member with no account yet; off means "sync only maps
	// pre-provisioned accounts" (dsh-ee AutoCreate).
	AutoCreate bool
	// DisableMissing disables provider-created accounts the
	// provider explicitly says are gone (dsh-ee DingTalkSyncDisable).
	DisableMissing bool
}

func (c Config) baseURL() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return c.Provider.DefaultBaseURL()
}

func (c Config) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Validate checks the config before any network call.
func (c Config) Validate() error {
	switch c.Provider {
	case ProviderDingTalk:
		if c.CorpID == "" {
			return fmt.Errorf("dingtalk requires corp_id")
		}
		if c.AppKey == "" {
			return fmt.Errorf("dingtalk requires app_key")
		}
	case ProviderFeishu:
		if c.AppKey == "" {
			return fmt.Errorf("feishu requires app_key (app_id)")
		}
	case ProviderWeCom:
		if c.CorpID == "" {
			return fmt.Errorf("wecom requires corp_id")
		}
	default:
		return fmt.Errorf("unknown provider %q", c.Provider)
	}
	if c.AppSecret == "" {
		return fmt.Errorf("%s requires app_secret", c.Provider)
	}
	if c.BaseURL != "" && !strings.HasPrefix(c.BaseURL, "http://") && !strings.HasPrefix(c.BaseURL, "https://") {
		return fmt.Errorf("base_url must be http(s)")
	}
	return nil
}

// DirectoryUser is one member of a remote directory.
type DirectoryUser struct {
	ExternalID string   // dingtalk userid / feishu open_id / wecom userid
	Name       string   // display name
	Email      string   // corporate mailbox when the provider gives one
	DeptIDs    []string // external department ids
	Title      string
	JobNumber  string
}

// Directory is one fully fetched organisation: department id ->
// name, plus every member. A sync only applies a complete
// directory; a partial fetch is discarded (dsh-ee rule).
type Directory struct {
	Departments map[string]string
	Users       []DirectoryUser
}

// UserState is what a provider says about one member.
type UserState int

const (
	// UserActive: still employed (or the lookup succeeded).
	UserActive UserState = iota
	// UserGone: the provider explicitly says this person is gone
	// (resigned / removed). Only this state disables an account.
	UserGone
	// UserOutOfScope: outside the app's directory permission
	// scope; absence from a fetched directory says nothing here.
	UserOutOfScope
)

// DirectoryClient fetches a remote directory and confirms one
// member's state. Each provider implements it against its own
// address-book API.
type DirectoryClient interface {
	Directory(ctx context.Context) (*Directory, error)
	UserState(ctx context.Context, externalID string) (UserState, error)
}

// NewClient builds the directory client for a provider config.
func NewClient(cfg Config) (DirectoryClient, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	switch cfg.Provider {
	case ProviderDingTalk:
		return &dingTalkClient{cfg: cfg}, nil
	case ProviderFeishu:
		return &feishuClient{cfg: cfg}, nil
	case ProviderWeCom:
		return &wecomClient{cfg: cfg}, nil
	}
	return nil, fmt.Errorf("unknown provider %q", cfg.Provider)
}
