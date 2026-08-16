package auth

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBearerTokenExtractsTheCredential(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer aiinv_ac_something")

	token, err := BearerToken(h)

	require.NoError(t, err)
	require.Equal(t, "aiinv_ac_something", token)
}

func TestBearerTokenMatchesTheSchemeCaseInsensitively(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "bearer aiinv_ac_something")

	token, err := BearerToken(h)

	require.NoError(t, err)
	require.Equal(t, "aiinv_ac_something", token)
}

func TestBearerTokenRejectsWhatIsNotAUsableCredential(t *testing.T) {
	cases := map[string]string{
		"absent":       "",
		"wrong scheme": "Basic aGk6dGhlcmU=",
		"scheme only":  "Bearer",
		"empty token":  "Bearer ",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			h := http.Header{}
			if value != "" {
				h.Set("Authorization", value)
			}
			_, err := BearerToken(h)
			require.ErrorIs(t, err, ErrNoBearerToken)
		})
	}
}
