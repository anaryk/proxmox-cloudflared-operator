package auth

import (
	"container/list"
	"strings"
	"sync"
	"time"
)

// Proxmox VE locks no user of the pam and pve realms out after wrong
// passwords, it only answers late, so the appliance's sign-in keeps a lock of
// its own, in memory: per account, the fifth failure of the password or the
// second factor locks it at pco for a minute, every further one doubles that
// up to 15 minutes, and a success clears it. Anyone who knows a user name can
// lock that user out of pco this way, not out of Proxmox VE; the API token
// goes past it.
const (
	lockAfter    = 5
	lockFirst    = time.Minute
	lockLongest  = 15 * time.Minute
	lockForget   = 15 * time.Minute // an account is forgotten this long after its last failure
	maxLockedAcc = 10000
)

// lockout counts the failures of accounts. It keeps at most max of them, in
// the order of their last failure: a new account that finds it full pushes
// out the one that failed longest ago, which only loosens that one's lock.
// An account is dropped 15 minutes after its last failure, when the next
// failure of any account looks; there is no timer.
type lockout struct {
	max int

	mu        sync.Mutex
	byAccount map[string]*list.Element // of *failures
	order     *list.List               // failed longest ago first
}

type failures struct {
	account string
	n       int
	last    time.Time
	until   time.Time

	refused  int       // sign-ins refused as locked since the last line of the log
	loggedAt time.Time // of that line
}

// lockLogEvery is how often the log names an account that is refused as
// locked.
const lockLogEvery = time.Minute

func newLockout() *lockout {
	return &lockout{max: maxLockedAcc, byAccount: map[string]*list.Element{}, order: list.New()}
}

// accountKey is how an account is counted: user@realm in lower case, so that
// another spelling of a name, which a realm such as Active Directory takes
// for the same user, counts with it.
func accountKey(user, realm string) string { return strings.ToLower(user + "@" + realm) }

// locked says whether account is locked at now, and for how much longer.
func (l *lockout) locked(account string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.byAccount[account]
	if !ok {
		return false, 0
	}
	f := e.Value.(*failures)
	if now.Sub(f.last) >= lockForget || !now.Before(f.until) {
		return false, 0
	}
	return true, f.until.Sub(now)
}

// fail counts a failure of account at now, and returns how long it is locked
// from now on.
func (l *lockout) fail(account string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	for e := l.order.Front(); e != nil && now.Sub(e.Value.(*failures).last) >= lockForget; e = l.order.Front() {
		l.drop(e)
	}
	e, ok := l.byAccount[account]
	if !ok {
		if l.order.Len() >= l.max {
			l.drop(l.order.Front())
		}
		e = l.order.PushBack(&failures{account: account})
		l.byAccount[account] = e
	}
	f := e.Value.(*failures)
	f.n++
	f.last = now
	l.order.MoveToBack(e)
	if f.n < lockAfter {
		return 0
	}
	d := lockLongest
	if shift := f.n - lockAfter; shift < 5 {
		d = min(lockFirst<<shift, lockLongest)
	}
	f.until = now.Add(d)
	return d
}

// refused counts a sign-in of account refused as locked at now, and says
// whether to log it, once a minute, with how many were refused since the line
// before.
func (l *lockout) refused(account string, now time.Time) (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.byAccount[account]
	if !ok {
		return 1, true
	}
	f := e.Value.(*failures)
	f.refused++
	if !f.loggedAt.IsZero() && now.Sub(f.loggedAt) < lockLogEvery {
		return f.refused, false
	}
	n := f.refused
	f.refused, f.loggedAt = 0, now
	return n, true
}

// clear forgets account: it signed in.
func (l *lockout) clear(account string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.byAccount[account]; ok {
		l.drop(e)
	}
}

func (l *lockout) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.order.Len()
}

func (l *lockout) drop(e *list.Element) {
	delete(l.byAccount, e.Value.(*failures).account)
	l.order.Remove(e)
}
