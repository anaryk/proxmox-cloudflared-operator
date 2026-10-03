package cfapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// VerifyToken asks Cloudflare whether the token is usable. A token that is
// rejected comes back as an error for which IsAuth is true.
//
// A token owned by a user is verified at /user/tokens/verify, one owned by an
// account at /accounts/{account_id}/tokens/verify. The user form is asked
// first. What it answers a token of an account is not documented, so any
// refusal sends VerifyToken on to the account form: of the first five
// accounts the token sees, in turn, until one verifies it. A rate limit, a
// server error or a request that got no answer says nothing about the token
// and ends the check. The error of a token neither form verifies names both
// attempts.
func (c *Client) VerifyToken(ctx context.Context) (TokenStatus, error) {
	st, userErr := c.verifyAt(ctx, "/user/tokens/verify")
	switch {
	case userErr == nil:
		return st, nil
	case IsUnanswered(userErr):
		return TokenStatus{}, fmt.Errorf("verifying token: %w", userErr)
	}
	st, accountErr := c.verifyAsAccountToken(ctx)
	switch {
	case accountErr == nil:
		return st, nil
	case IsUnanswered(accountErr):
		// The account form may still verify it: the error is one to try again.
		return TokenStatus{}, fmt.Errorf("verifying token: as a user token: %s; as an account token: %w", userErr.Error(), accountErr)
	}
	// Both forms answered, and neither verified it: the user form says what
	// kind of refusal it is.
	return TokenStatus{}, fmt.Errorf("verifying token: as a user token: %w; as an account token: %s", userErr, accountErr.Error())
}

// maxAccountTries bounds the accounts the account form of the token check
// asks: a token that sees many accounts is usually a user's.
const maxAccountTries = 5

// verifyAsAccountToken verifies the token at the account form of the first
// accounts it sees, and stops at the first that answers yes, or that cannot
// answer.
func (c *Client) verifyAsAccountToken(ctx context.Context) (TokenStatus, error) {
	accounts, err := c.Accounts(ctx)
	if err != nil {
		return TokenStatus{}, err
	}
	if len(accounts) == 0 {
		return TokenStatus{}, errors.New("it sees no account")
	}
	none := "no account it sees verifies it"
	if len(accounts) > maxAccountTries {
		none = fmt.Sprintf("none of the first %d of its %d accounts verifies it", maxAccountTries, len(accounts))
		accounts = accounts[:maxAccountTries]
	}
	var refused errorList
	for _, a := range accounts {
		st, err := c.verifyAtAccount(ctx, a.ID)
		switch {
		case err == nil:
			return st, nil
		case IsUnanswered(err):
			return TokenStatus{}, fmt.Errorf("account %s: %w", a.ID, err)
		}
		refused = append(refused, fmt.Errorf("account %s: %w", a.ID, err))
	}
	return TokenStatus{}, fmt.Errorf("%s: %w", none, refused)
}

func (c *Client) verifyAtAccount(ctx context.Context, accountID string) (TokenStatus, error) {
	path, err := accountTokenPath(accountID)
	if err != nil {
		return TokenStatus{}, err
	}
	return c.verifyAt(ctx, path)
}

func accountTokenPath(accountID string) (string, error) {
	if err := CheckID("account id", accountID); err != nil {
		return "", err
	}
	return joinPath("accounts", accountID, "tokens", "verify")
}

func (c *Client) verifyAt(ctx context.Context, path string) (TokenStatus, error) {
	var got struct {
		ID        string     `json:"id"`
		Status    string     `json:"status"`
		ExpiresOn *time.Time `json:"expires_on"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &got); err != nil {
		return TokenStatus{}, err
	}
	if got.Status == "" {
		return TokenStatus{}, fmt.Errorf("%w: no status", errUnexpected)
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
	err := listEach(ctx, c, "/accounts", nil, strict, func(w wire) error {
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
	err := listEach(ctx, c, "/zones", nil, strict, func(w wire) error {
		if w.ID == "" || w.Name == "" || w.Account.ID == "" {
			return fmt.Errorf("%w: zone without an id, a name or an account", errUnexpected)
		}
		out = append(out, Zone{ID: w.ID, Name: w.Name, Status: w.Status, AccountID: w.Account.ID})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing zones: %w", err)
	}
	return out, nil
}
