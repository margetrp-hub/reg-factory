package claudehttp

import (
	"context"
	"time"
)

// Mailbox supplies the login link. Live sources belong to the shared mail pool;
// this protocol package does not keep or refresh a second copy of mail secrets.
type Mailbox interface {
	Address() string
	WaitForMagicLink(ctx context.Context, timeout time.Duration) (string, error)
}

// StaticMailbox 供测试或人工粘贴链接使用。
type StaticMailbox struct {
	Addr string
	Link string
}

func (m *StaticMailbox) Address() string { return m.Addr }

func (m *StaticMailbox) WaitForMagicLink(ctx context.Context, timeout time.Duration) (string, error) {
	if m.Link == "" {
		return "", &Error{Kind: ErrMailbox, Detail: "未提供 magic link"}
	}
	return m.Link, nil
}
