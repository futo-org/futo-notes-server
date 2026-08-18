package auth

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"testing"

	"golang.org/x/crypto/scrypt"
)

func TestVerifyScrypt(t *testing.T) {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		t.Fatal(err)
	}
	key, err := scrypt.Key([]byte("hunter2"), salt, scryptN, scryptR, scryptP, scryptKeyLen)
	if err != nil {
		t.Fatal(err)
	}
	stored := fmt.Sprintf("scrypt:N=16384,r=8,p=1:%s:%s", hex.EncodeToString(salt), hex.EncodeToString(key))

	ok, err := VerifyScrypt("hunter2", stored)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("correct password did not verify")
	}

	ok, err = VerifyScrypt("wrong", stored)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("wrong password verified")
	}

	if _, err := VerifyScrypt("hunter2", "bcrypt:whatever"); err == nil {
		t.Error("expected an error for an unrecognized format")
	}
}
