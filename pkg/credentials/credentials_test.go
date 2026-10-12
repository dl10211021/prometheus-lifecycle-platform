package credentials

import (
	"errors"
	"strings"
	"testing"
)

type failingRandomReader struct{}

func (failingRandomReader) Read(_ []byte) (int, error) {
	return 0, errors.New("random unavailable")
}

func TestResolveCipherSecretUsesEphemeralEntropyWithoutPersistentStorage(t *testing.T) {
	provided := strings.Repeat("a", 64)
	resolved, err := resolveCipherSecret(provided, false, failingRandomReader{})
	if err != nil {
		t.Fatal(err)
	}
	if resolved != provided {
		t.Fatal("non-persisted random secret was replaced instead of retained for this process")
	}

	generated, err := resolveCipherSecret("", false, strings.NewReader(strings.Repeat("b", 32)))
	if err != nil {
		t.Fatal(err)
	}
	if generated != strings.Repeat("62", 32) {
		t.Fatalf("generated secret = %q", generated)
	}
}

func TestResolveCipherSecretFailsClosedWithoutEntropy(t *testing.T) {
	if _, err := resolveCipherSecret("", false, failingRandomReader{}); err == nil {
		t.Fatal("missing secret and failed entropy source were accepted")
	}
}
