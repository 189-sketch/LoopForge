package domain

import "time"

type BotChat struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Type            string `json:"type"`
	LastMessageTime string `json:"lastMessageTime,omitempty"`
}

// Bot platforms. An empty Provider on a stored bot means WeCom, which predates the field.
// Feishu and Lark share larksuite/cli and differ only in the tenant brand.
const (
	BotProviderWeCom  = "wecom"
	BotProviderFeishu = "feishu"
	BotProviderLark   = "lark"
)

// NormalizeBotProvider maps a requested platform to a stored value; ok is false for unknown platforms.
func NormalizeBotProvider(provider string) (string, bool) {
	switch provider {
	case "", BotProviderWeCom:
		return BotProviderWeCom, true
	case BotProviderFeishu, BotProviderLark:
		return provider, true
	}
	return "", false
}

type NotificationBot struct {
	ID                 string     `json:"id"`
	ProjectID          string     `json:"projectId"`
	OwnerID            int64      `json:"ownerId"`
	Provider           string     `json:"provider,omitempty"`
	Model              string     `json:"model,omitempty"`
	Name               string     `json:"name"`
	BotID              string     `json:"botId"`
	InteractionEnabled bool       `json:"interactionEnabled"`
	Enabled            bool       `json:"enabled"`
	SecretConfigured   bool       `json:"secretConfigured"`
	Target             *BotChat   `json:"target,omitempty"`
	Health             string     `json:"health"`
	LastCheckedAt      *time.Time `json:"lastCheckedAt,omitempty"`
	CreatedAt          time.Time  `json:"createdAt"`
	UpdatedAt          time.Time  `json:"updatedAt"`
}

// Platform returns the bot's platform, treating bots stored before the field existed as WeCom.
func (b NotificationBot) Platform() string {
	if b.Provider == "" {
		return BotProviderWeCom
	}
	return b.Provider
}

// BotPlatformLabel is the product name shown to users for a platform.
func BotPlatformLabel(provider string) string {
	switch provider {
	case BotProviderFeishu:
		return "飞书"
	case BotProviderLark:
		return "Lark"
	}
	return "企业微信"
}

type NotificationBinding struct {
	BotID         string   `json:"botId"`
	Events        []string `json:"events"` // succeeded, failed, waiting
	Template      string   `json:"template,omitempty"`
	Scope         string   `json:"scope,omitempty"`       // node (default), run
	ContentMode   string   `json:"contentMode,omitempty"` // template (default), ai
	AIInstruction string   `json:"aiInstruction,omitempty"`
	WaitingPolicy string   `json:"waitingPolicy,omitempty"`
}
type NodeNotification = NotificationBinding

const DefaultNotificationReportModel = "default"

type NotificationReportRequest struct {
	Binding NotificationBinding
	Context NotificationContext
}

type NotificationDelivery struct {
	ReportRequest string `json:"-" gorm:"type:text"`
	ReportState   string `json:"reportState,omitempty" gorm:"size:32"`
	ReportModel   string `json:"reportModel,omitempty" gorm:"size:191"`
	ReportError   string `json:"reportError,omitempty" gorm:"size:255"`

	WaitingPolicy string     `json:"waitingPolicy,omitempty" gorm:"size:32;default:actionable"`
	SourceType    string     `json:"sourceType" gorm:"size:32;default:run;index:idx_notification_source,priority:1"`
	SourceID      string     `json:"sourceId,omitempty" gorm:"size:191;index:idx_notification_source,priority:2"`
	SourceName    string     `json:"sourceName,omitempty" gorm:"size:255"`
	ContentSource string     `json:"contentSource,omitempty" gorm:"size:32"`
	ID            string     `json:"id" gorm:"primaryKey;size:64"`
	ProjectID     string     `json:"projectId" gorm:"size:191;index"`
	BotID         string     `json:"botId" gorm:"size:191;index"`
	RunID         string     `json:"runId,omitempty" gorm:"size:191"`
	NodeID        string     `json:"nodeId,omitempty" gorm:"size:191"`
	EventType     string     `json:"eventType" gorm:"size:64"`
	TargetID      string     `json:"-" gorm:"type:text"`
	TargetType    string     `json:"targetType,omitempty" gorm:"size:16"`
	TargetName    string     `json:"targetName" gorm:"size:255"`
	Content       string     `json:"-" gorm:"type:text"`
	Status        string     `json:"status" gorm:"size:40;index:idx_notification_due,priority:1"`
	Attempts      int        `json:"attempts"`
	Attempt       int        `json:"-"`
	ReworkCount   int        `json:"-"`
	NextAttemptAt time.Time  `json:"nextAttemptAt" gorm:"index:idx_notification_due,priority:2"`
	LeaseUntil    *time.Time `json:"-"`
	LeaseToken    string     `json:"-" gorm:"size:64"`
	LastError     string     `json:"lastError,omitempty" gorm:"size:255"`
	CreatedAt     time.Time  `json:"createdAt"`
	SentAt        *time.Time `json:"sentAt,omitempty"`
}

func (NotificationDelivery) TableName() string { return "notification_deliveries" }
