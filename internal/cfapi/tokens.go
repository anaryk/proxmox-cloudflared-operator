package cfapi

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// VerifyToken asks Cloudflare whether the token is usable. A token that is
// rejected comes back as an error for which IsAuth is true.
//
// This is the endpoint of user-owned tokens. Account-owned tokens are verified
// at another path.
func (c *Client) VerifyToken(ctx context.Context) (TokenStatus, error) {
	var got struct {
		ID        string     `json:"id"`
		Status    string     `json:"status"`
		ExpiresOn *time.Time `json:"expires_on"`
	}
	if err := c.do(ctx, http.MethodGet, "/user/tokens/verify", nil, nil, &got); err != nil {
		return TokenStatus{}, fmt.Errorf("verifying token: %w", err)
	}
	if got.Status == "" {
		return TokenStatus{}, fmt.Errorf("verifying token: %w: no status", errUnexpected)
	}
	return TokenStatus{ID: got.ID, Status: got.Status, ExpiresOn: got.ExpiresOn}, nil
}

// Accounts lists the accounts the token can see.
func (c *Client) Accounts(ctx context.Context) ([]Account, error) {
	type wire struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	var out []Account
	err := listEach(ctx, c, "/accounts", nil, func(w wire) error {
		if w.ID == "" {
			return fmt.Errorf("%w: account without an id", errUnexpected)
		}
		out = append(out, Account(w))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing accounts: %w", err)
	}
	return out, nil
}

// Zones lists the zones the token can see.
func (c *Client) Zones(ctx context.Context) ([]Zone, error) {
	type wire struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Status  string `json:"status"`
		Account struct {
			ID string `json:"id"`
		} `json:"account"`
	}
	var out []Zone
	err := listEach(ctx, c, "/zones", nil, func(w wire) error {
		if w.ID == "" || w.Name == "" {
			return fmt.Errorf("%w: zone without an id or a name", errUnexpected)
		}
		out = append(out, Zone{ID: w.ID, Name: w.Name, Status: w.Status, AccountID: w.Account.ID})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing zones: %w", err)
	}
	return out, nil
}
