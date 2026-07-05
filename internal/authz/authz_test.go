package authz

import (
	"math/rand"
	"testing"
	"time"

	"github.com/lazorfuzz/memba/pkg/memory"
)

var secret = []byte("test-secret")

func TestSignVerifyRoundTrip(t *testing.T) {
	p := memory.Principal{TenantID: "acme", ID: "agent:coder-1", ACLSubjects: []string{"team:payments"}}
	tok, err := Sign(secret, p, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Verify(secret, tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.TenantID != "acme" || got.ID != "agent:coder-1" {
		t.Fatalf("principal mismatch: %+v", got)
	}
	// Implicit subjects: self + tenant.
	if !Allowed(got, memory.ACL{Read: []string{"agent:coder-1"}}) {
		t.Fatal("principal must hold its own subject")
	}
	if !Allowed(got, memory.ACL{Read: []string{"tenant:acme"}}) {
		t.Fatal("principal must hold its tenant subject")
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	p := memory.Principal{TenantID: "acme", ID: "user:x"}
	tok, _ := Sign(secret, p, time.Hour)
	if _, err := Verify([]byte("other-secret"), tok); err == nil {
		t.Fatal("wrong secret accepted")
	}
	if _, err := Verify(secret, tok+"x"); err == nil {
		t.Fatal("tampered signature accepted")
	}
	if _, err := Verify(secret, "memba1.garbage.sig"); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	p := memory.Principal{TenantID: "acme", ID: "user:x"}
	tok, _ := Sign(secret, p, -time.Minute)
	if _, err := Verify(secret, tok); err != ErrExpired {
		t.Fatalf("expected ErrExpired, got %v", err)
	}
}

func TestAllowedEmptyACLDeniesEveryone(t *testing.T) {
	p := memory.Principal{TenantID: "acme", ID: "user:x", ACLSubjects: []string{"user:x", "tenant:acme"}}
	if Allowed(p, memory.ACL{}) {
		t.Fatal("empty read set must deny (unservable until re-scoped, §15.2)")
	}
}

func TestDeriveACLIntersection(t *testing.T) {
	acl := DeriveACL([]memory.ACL{
		{Read: []string{"team:payments", "team:platform", "user:a"}},
		{Read: []string{"team:payments", "user:b"}},
	})
	if len(acl.Read) != 1 || acl.Read[0] != "team:payments" {
		t.Fatalf("expected intersection {team:payments}, got %v", acl.Read)
	}
	// Disjoint sources → empty (valid but unservable).
	acl = DeriveACL([]memory.ACL{{Read: []string{"team:a"}}, {Read: []string{"team:b"}}})
	if len(acl.Read) != 0 {
		t.Fatalf("expected empty intersection, got %v", acl.Read)
	}
}

// Property test (spec §14.6): for random principals and ACLs, Allowed is
// true iff the subject sets intersect, and derived ACLs never widen access.
func TestACLProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	subjects := []string{"user:a", "user:b", "team:x", "team:y", "agent:z", "svc:i", "tenant:t1", "tenant:t2"}
	pick := func(n int) []string {
		var out []string
		for _, s := range subjects {
			if rng.Intn(len(subjects)) < n {
				out = append(out, s)
			}
		}
		return out
	}
	for i := 0; i < 10000; i++ {
		held := pick(3)
		read := pick(3)
		p := memory.Principal{TenantID: "t", ID: "user:p", ACLSubjects: held}
		want := false
		for _, h := range held {
			for _, r := range read {
				if h == r {
					want = true
				}
			}
		}
		if got := Allowed(p, memory.ACL{Read: read}); got != want {
			t.Fatalf("iter %d: Allowed=%v want %v (held=%v read=%v)", i, got, want, held, read)
		}
		// Derived ACL from N sources must be a subset of each source's read set.
		srcs := []memory.ACL{{Read: pick(4)}, {Read: pick(4)}, {Read: pick(4)}}
		derived := DeriveACL(srcs)
		for _, d := range derived.Read {
			for _, src := range srcs {
				found := false
				for _, r := range src.Read {
					if r == d {
						found = true
					}
				}
				if !found {
					t.Fatalf("iter %d: derived subject %q not in every source (privilege escalation)", i, d)
				}
			}
		}
	}
}
