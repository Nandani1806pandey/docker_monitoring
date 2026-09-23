package auth

import "testing"

func TestHashAndVerifyPassword_Correct(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	ok, err := VerifyPassword("correct horse battery staple", hash)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Fatalf("expected correct password to verify")
	}
}

func TestVerifyPassword_Wrong(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	ok, err := VerifyPassword("wrong password entirely", hash)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if ok {
		t.Fatalf("expected wrong password to fail verification")
	}
}

func TestHashPassword_UniqueSaltPerCall(t *testing.T) {
	h1, err := HashPassword("same password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	h2, err := HashPassword("same password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if h1 == h2 {
		t.Fatalf("expected two hashes of the same password to differ (random salt), got identical: %s", h1)
	}
	// Both must still verify correctly despite differing.
	for _, h := range []string{h1, h2} {
		ok, err := VerifyPassword("same password", h)
		if err != nil || !ok {
			t.Fatalf("hash %s failed to verify: ok=%v err=%v", h, ok, err)
		}
	}
}

func TestVerifyPassword_MalformedHash(t *testing.T) {
	cases := []string{
		"",
		"not-a-hash-at-all",
		"$argon2id$v=19$m=65536,t=1,p=4$onlyfourparts",
		"$bcrypt$v=19$m=65536,t=1,p=4$c2FsdA$aGFzaA",
	}
	for _, c := range cases {
		if _, err := VerifyPassword("anything", c); err == nil {
			t.Errorf("expected error for malformed hash %q, got nil", c)
		}
	}
}

func TestVerifyPassword_EmptyPasswordDoesNotVerify(t *testing.T) {
	hash, err := HashPassword("a real password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	ok, err := VerifyPassword("", hash)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if ok {
		t.Fatalf("expected empty password not to verify against a real hash")
	}
}
