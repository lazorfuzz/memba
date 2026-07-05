// Package authz implements bearer-token authentication and ACL evaluation
// (spec §13, §15.2). Tokens are HMAC-SHA256-signed JSON payloads carrying
// {tenant_id, principal, acl_subjects[]} — issued by the host platform
// (memctl token in dev); memd validates signature + expiry.
package authz

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lazorfuzz/memba/pkg/memory"
)

const tokenPrefix = "memba1"

var (
	ErrBadToken = errors.New("invalid token")
	ErrExpired  = errors.New("token expired")
)

type claims struct {
	TenantID    string   `json:"tenant_id"`
	Principal   string   `json:"principal"`
	ACLSubjects []string `json:"acl_subjects"`
	Exp         int64    `json:"exp,omitempty"` // unix seconds; 0 = no expiry
}

// Sign mints a token for a principal.
func Sign(secret []byte, p memory.Principal, ttl time.Duration) (string, error) {
	if p.TenantID == "" || p.ID == "" {
		return "", fmt.Errorf("tenant_id and principal required")
	}
	c := claims{TenantID: p.TenantID, Principal: p.ID, ACLSubjects: p.ACLSubjects}
	if ttl != 0 {
		c.Exp = time.Now().Add(ttl).Unix()
	}
	body, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(tokenPrefix + "." + enc))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return tokenPrefix + "." + enc + "." + sig, nil
}

// Verify validates a token and returns the principal.
func Verify(secret []byte, token string) (memory.Principal, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != tokenPrefix {
		return memory.Principal{}, ErrBadToken
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	want := mac.Sum(nil)
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(want, got) {
		return memory.Principal{}, ErrBadToken
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return memory.Principal{}, ErrBadToken
	}
	var c claims
	if err := json.Unmarshal(body, &c); err != nil {
		return memory.Principal{}, ErrBadToken
	}
	if c.Exp != 0 && time.Now().Unix() > c.Exp {
		return memory.Principal{}, ErrExpired
	}
	subjects := c.ACLSubjects
	// Every principal implicitly holds itself and its tenant subject.
	subjects = appendUnique(subjects, c.Principal)
	subjects = appendUnique(subjects, "tenant:"+c.TenantID)
	return memory.Principal{TenantID: c.TenantID, ID: c.Principal, ACLSubjects: subjects}, nil
}

// Allowed reports whether a principal may read an object with the given ACL:
// principal.acl_subjects ∩ acl.read ≠ ∅ (spec §15.2). An empty read set
// denies everyone (unservable until re-scoped).
func Allowed(p memory.Principal, acl memory.ACL) bool {
	if len(acl.Read) == 0 {
		return false
	}
	held := make(map[string]struct{}, len(p.ACLSubjects))
	for _, s := range p.ACLSubjects {
		held[s] = struct{}{}
	}
	for _, r := range acl.Read {
		if _, ok := held[r]; ok {
			return true
		}
	}
	return false
}

// DeriveACL computes a derived object's ACL as the intersection of all its
// sources' read sets (spec §15.2). An empty result is legal: the object is
// valid but unservable until a human re-scopes it.
func DeriveACL(sources []memory.ACL) memory.ACL {
	if len(sources) == 0 {
		return memory.ACL{Read: nil}
	}
	counts := map[string]int{}
	for _, a := range sources {
		seen := map[string]struct{}{}
		for _, r := range a.Read {
			if _, dup := seen[r]; dup {
				continue
			}
			seen[r] = struct{}{}
			counts[r]++
		}
	}
	var out []string
	for s, n := range counts {
		if n == len(sources) {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return memory.ACL{Read: out}
}

func appendUnique(list []string, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
}
