package cffake

import (
	"net/http"
	"time"
)

// The page sizes of the listings of accounts and zones: what the API gives
// when none is asked for, and from least to most what it accepts.
const (
	defaultAccountPage, minAccountPage, maxAccountPage = 20, 5, 50
	defaultZonePage, minZonePage, maxZonePage          = 20, 5, 50
)

type wireToken struct {
	ID        string     `json:"id"`
	Status    string     `json:"status"`
	ExpiresOn *time.Time `json:"expires_on,omitempty"`
}

func (h *handler) verifyUser(r *http.Request, _ params) (reply, error) {
	return h.verify(r, "")
}

func (h *handler) verifyAccount(r *http.Request, p params) (reply, error) {
	return h.verify(r, p["account"])
}

func (h *handler) verify(r *http.Request, accountID string) (reply, error) {
	if err := checkQuery(r.URL.Query()); err != nil {
		return reply{}, err
	}
	st, err := h.f.verifyForm(r.Context(), accountID)
	if err != nil {
		return reply{}, err
	}
	w := wireToken{ID: st.ID, Status: st.Status}
	if st.ExpiresOn != nil {
		w.ExpiresOn = utc(*st.ExpiresOn)
	}
	return reply{result: w}, nil
}

// utc returns a copy of t in UTC, the zone Cloudflare writes its times in.
func utc(t time.Time) *time.Time {
	t = t.UTC()
	return &t
}

type wireAccount struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

func (h *handler) listAccounts(r *http.Request, _ params) (reply, error) {
	q := r.URL.Query()
	if err := checkQuery(q, "page", "per_page"); err != nil {
		return reply{}, err
	}
	pg, err := readPaging(q, defaultAccountPage, minAccountPage, maxAccountPage)
	if err != nil {
		return reply{}, err
	}
	all, err := h.f.Accounts(r.Context())
	if err != nil {
		return reply{}, err
	}
	page := pageOf(all, pg)
	items := make([]wireAccount, len(page))
	for i, a := range page {
		items[i] = wireAccount{ID: a.ID, Name: a.Name, Type: "standard"}
	}
	return reply{items, pg.info(len(items), len(all), true)}, nil
}

type wireZone struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Account struct {
		ID string `json:"id"`
	} `json:"account"`
}

func (h *handler) listZones(r *http.Request, _ params) (reply, error) {
	q := r.URL.Query()
	if err := checkQuery(q, "page", "per_page"); err != nil {
		return reply{}, err
	}
	pg, err := readPaging(q, defaultZonePage, minZonePage, maxZonePage)
	if err != nil {
		return reply{}, err
	}
	all, err := h.f.Zones(r.Context())
	if err != nil {
		return reply{}, err
	}
	page := pageOf(all, pg)
	items := make([]wireZone, len(page))
	for i, z := range page {
		items[i] = wireZone{ID: z.ID, Name: z.Name, Status: z.Status}
		items[i].Account.ID = z.AccountID
	}
	return reply{items, pg.info(len(items), len(all), true)}, nil
}
