package index

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/store"
)

type linkKind string

const (
	// linkUnrecorded is a link a revoke or join named before its link record
	// arrived; records live in each writer's own files.
	linkUnrecorded linkKind = ""
	linkRead       linkKind = "read"
	linkInvite     linkKind = "invite"
)

const (
	linkIDLen  = 12
	tokenBytes = 16
)

type TokenHash [sha256.Size]byte

func ParseToken(s string) (TokenHash, bool) {
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || len(b) != tokenBytes || base64.RawURLEncoding.EncodeToString(b) != s {
		return TokenHash{}, false
	}
	return sha256.Sum256([]byte(s)), true
}

func newToken() (string, TokenHash) {
	b := make([]byte, tokenBytes)
	rand.Read(b)
	s := base64.RawURLEncoding.EncodeToString(b)
	return s, sha256.Sum256([]byte(s))
}

var (
	ErrLinkUnknown = errors.New("no such link")
	ErrLinkGone    = errors.New("this link was revoked or has expired")
	ErrKeyMember   = errors.New("this key has already joined through another invite")
)

type link struct {
	id               string
	kind             linkKind
	token            TokenHash
	sessions         []string
	sessionAuthors   []string
	title, note      string
	cutoff, expires  time.Time
	created          time.Time
	author, tenant   string
	invite           string
	revoked          bool
	joinKey, machine string
}

func (l *link) live(now time.Time) bool {
	return l.kind != linkUnrecorded && !l.revoked && (now.Before(l.expires) || l.joinKey != "")
}

func (c *Controls) applyLink(r controlRecord) {
	l := c.links[r.LinkID]
	if l == nil {
		l = &link{id: r.LinkID}
		c.links[r.LinkID] = l
	}
	switch r.Action {
	case actLink:
		if l.kind != linkUnrecorded {
			return
		}
		l.kind, l.sessions, l.sessionAuthors, l.title, l.note = r.Kind, r.SessionIDs, r.SessionAuthors, r.Title, r.Note
		l.cutoff, l.expires, l.created = r.Cutoff, r.Expires, r.TS
		l.author, l.tenant, l.invite = r.Author, r.Tenant, r.InviteAuthor
		hex.Decode(l.token[:], []byte(r.TokenSHA256))
		c.byToken[l.token] = l
	case actJoin:
		if l.kind != linkInvite || l.revoked || l.joinKey != "" || c.byKey[r.Key] != nil {
			return
		}
		l.joinKey, l.machine = r.Key, r.Machine
		c.byKey[r.Key] = l
	case actRevoke:
		l.revoked = true
		if l.joinKey != "" && c.byKey[l.joinKey] == l {
			delete(c.byKey, l.joinKey)
		}
	}
}

func (c *Controls) Member(key string) (auth.Member, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	l := c.byKey[key]
	if l == nil {
		return auth.Member{}, false
	}
	return auth.Member{Key: key, Tenant: l.tenant, Author: l.invite}, true
}

func (c *Controls) Members() []auth.Member {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]auth.Member, 0, len(c.byKey))
	for key, l := range c.byKey {
		out = append(out, auth.Member{Key: key, Tenant: l.tenant, Author: l.invite})
	}
	return out
}

type ReadLink struct {
	ID, Title, Note string
	SessionIDs      []string
	Cutoff          time.Time
}

// OpenRead is the read link h names. A session shows while it is not deleted
// and its sole author is still the one the link recorded, so a foreign line
// pushed under a linked id hides the session instead of joining the link.
func (c *Controls) OpenRead(db *DB, h TokenHash, now time.Time) (ReadLink, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	l := c.byToken[h]
	switch {
	case l == nil || l.kind != linkRead:
		return ReadLink{}, ErrLinkUnknown
	case !l.live(now):
		return ReadLink{}, ErrLinkGone
	}
	rl := ReadLink{ID: l.id, Title: l.title, Note: l.note, Cutoff: l.cutoff}
	for i, s := range l.sessions {
		if !c.sessions[s].deleted && slices.Equal(db.sessionAuthors(s), []string{l.sessionAuthors[i]}) {
			rl.SessionIDs = append(rl.SessionIDs, s)
		}
	}
	return rl, nil
}

type Invite struct {
	ID, Author string
	Expires    time.Time
}

func (c *Controls) OpenInvite(h TokenHash, now time.Time) (Invite, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	l := c.byToken[h]
	switch {
	case l == nil || l.kind != linkInvite:
		return Invite{}, ErrLinkUnknown
	case !l.live(now) || l.joinKey != "":
		return Invite{}, ErrLinkGone
	}
	return Invite{ID: l.id, Author: l.invite, Expires: l.expires}, nil
}

func (c *Controls) Join(st *store.Committer, h TokenHash, key, machine string, now time.Time) (string, error) {
	c.write.Lock()
	defer c.write.Unlock()
	c.mu.RLock()
	l, known := c.byToken[h], c.byKey[key]
	c.mu.RUnlock()
	switch {
	case l == nil || l.kind != linkInvite:
		return "", ErrLinkUnknown
	case l.revoked:
		return "", ErrLinkGone
	case l.joinKey == key:
		return l.invite, nil
	case l.joinKey != "" || !now.Before(l.expires):
		return "", ErrLinkGone
	case known != nil:
		return "", ErrKeyMember
	}
	_, err := c.recordLocked(st, l.author, controlRecord{Action: actJoin, LinkID: l.id, Key: key, Machine: machine,
		RequestKey: recordKey(actJoin, l.id, key, "")})
	if err != nil {
		return "", err
	}
	return l.invite, nil
}

func (c *Controls) newLink(st *store.Committer, r controlRecord) (controlRecord, string, error) {
	id := make([]byte, linkIDLen/2)
	rand.Read(id)
	token, h := newToken()
	r.Action, r.LinkID, r.TokenSHA256, r.RequestKey = actLink, hex.EncodeToString(id), hex.EncodeToString(h[:]), randomKey()
	if _, err := c.recordLocked(st, r.Author, r); err != nil {
		return r, "", err
	}
	return r, token, nil
}

func owns(id auth.Identity, l *link) bool {
	return l.kind != linkUnrecorded && l.tenant == id.Tenant() && (l.author == id.Author() || id.Can(auth.Admin))
}

func (c *Controls) revoke(st *store.Committer, id auth.Identity, linkID string) error {
	c.write.Lock()
	defer c.write.Unlock()
	c.mu.RLock()
	l := c.links[linkID]
	c.mu.RUnlock()
	if l == nil || !owns(id, l) {
		return argErrorf("link %s not found", linkID)
	}
	if l.revoked {
		return nil
	}
	_, err := c.recordLocked(st, id.Author(), controlRecord{Action: actRevoke, LinkID: linkID, RequestKey: recordKey(actRevoke, linkID, "", "")})
	return err
}

func (c *Controls) authorInvitedElsewhere(author, tenant string, now time.Time) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, l := range c.links {
		if l.kind == linkInvite && l.invite == author && l.tenant != tenant && l.live(now) {
			return true
		}
	}
	return false
}

func (c *Controls) owned(id auth.Identity, now time.Time) []link {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []link
	for _, l := range c.links {
		if owns(id, l) && l.live(now) {
			out = append(out, *l)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].created.Equal(out[j].created) {
			return out[i].created.After(out[j].created)
		}
		return out[i].id < out[j].id
	})
	return out
}
