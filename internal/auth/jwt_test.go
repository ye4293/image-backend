package auth

import "testing"

func TestGenerateAndParseToken(t *testing.T) {
	token, err := GenerateToken(42, "test-secret")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	id, err := ParseToken(token, "test-secret")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if id != 42 {
		t.Fatalf("expected 42, got %d", id)
	}
	if _, err := ParseToken(token, "wrong-secret"); err == nil {
		t.Fatal("expected error with wrong secret")
	}
	if _, err := ParseToken("garbage", "test-secret"); err == nil {
		t.Fatal("expected error with garbage token")
	}
}
