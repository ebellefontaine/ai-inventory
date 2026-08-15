package auth

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGenerateProducesVerifiableToken(t *testing.T) {
	plain, hash, err := GenerateToken("aiinv_et")
	require.NoError(t, err)
	require.True(t, len(plain) > 32)
	require.Contains(t, plain, "aiinv_et_")
	require.NotContains(t, hash, plain, "hash must not embed the plaintext")
	require.True(t, VerifyToken(plain, hash))
}

func TestVerifyRejectsWrongToken(t *testing.T) {
	_, hash, err := GenerateToken("aiinv_et")
	require.NoError(t, err)
	require.False(t, VerifyToken("aiinv_et_wrong", hash))
}

func TestGenerateIsUnique(t *testing.T) {
	a, _, _ := GenerateToken("aiinv_et")
	b, _, _ := GenerateToken("aiinv_et")
	require.NotEqual(t, a, b)
}
