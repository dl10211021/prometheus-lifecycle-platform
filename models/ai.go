package models

import (
	"time"

	"github.com/promlifecycle/platform/pkg/credentials"
	"gorm.io/gorm"
)

const AIConfigSingletonID uint = 1

// AIConfig stores the single OpenAI-compatible provider configuration.
// APIKey is decrypted after reads for use by the backend but is never exposed
// through JSON.
type AIConfig struct {
	ID           uint      `gorm:"primaryKey" json:"-"`
	Provider     string    `gorm:"size:64;not null" json:"provider"`
	BaseURL      string    `gorm:"size:512;not null" json:"base_url"`
	Model        string    `gorm:"size:128;not null" json:"model"`
	APIKey       string    `gorm:"type:text" json:"-"`
	SystemPrompt string    `gorm:"type:text" json:"system_prompt"`
	Timeout      int       `gorm:"not null;default:60" json:"timeout"`
	Enabled      bool      `gorm:"not null;default:false" json:"enabled"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// BeforeCreate pins every configuration to the singleton primary key.
func (c *AIConfig) BeforeCreate(tx *gorm.DB) error {
	c.ID = AIConfigSingletonID
	return c.encryptAPIKey()
}

// BeforeSave encrypts the API key at rest. credentials.Encrypt is idempotent,
// so repeated saves cannot double-encrypt it.
func (c *AIConfig) BeforeSave(tx *gorm.DB) error {
	return c.encryptAPIKey()
}

// AfterFind decrypts the key for backend-only use. Legacy plaintext values are
// accepted and encrypted on the next save by the credentials package.
func (c *AIConfig) AfterFind(tx *gorm.DB) error {
	key, err := credentials.Decrypt(c.APIKey)
	if err != nil {
		return err
	}
	c.APIKey = key
	return nil
}

func (c *AIConfig) encryptAPIKey() error {
	key, err := credentials.Encrypt(c.APIKey)
	if err != nil {
		return err
	}
	c.APIKey = key
	return nil
}
