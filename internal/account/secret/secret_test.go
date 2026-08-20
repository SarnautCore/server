package secret_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/SarnautCore/server/internal/account/secret"
)

const sentinel = "SENTINEL-PW-2f9c41"

// Every rendering path a value can take by accident must already be redacted.
// This is the mechanism ADR 0030 §5 asks for: reaching the plaintext requires
// Reveal, which is greppable, and nothing else.
func TestEveryRenderingRedacts(t *testing.T) {
	t.Parallel()

	value := secret.New(sentinel)
	renderings := map[string]string{
		"%v":      fmt.Sprintf("%v", value),
		"%s":      fmt.Sprintf("%s", value),
		"%q":      fmt.Sprintf("%q", value),
		"%#v":     fmt.Sprintf("%#v", value),
		"%x":      fmt.Sprintf("%x", value),
		"%d":      fmt.Sprintf("%d", value),
		"String":  value.String(),
		"pointer": fmt.Sprintf("%v", &value),
	}
	for verb, rendered := range renderings {
		if strings.Contains(rendered, sentinel) {
			t.Errorf("%s rendered the secret: %q", verb, rendered)
		}
		if !strings.Contains(rendered, secret.Redaction) {
			t.Errorf("%s = %q, want the redaction", verb, rendered)
		}
	}
}

func TestJSONAndTextEncodingRedact(t *testing.T) {
	t.Parallel()

	document := struct {
		Password secret.Value `json:"password"`
	}{Password: secret.New(sentinel)}

	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if bytes.Contains(encoded, []byte(sentinel)) {
		t.Errorf("json.Marshal wrote the secret: %s", encoded)
	}

	text, err := secret.New(sentinel).MarshalText()
	if err != nil {
		t.Fatalf("MarshalText() error = %v", err)
	}
	if string(text) != secret.Redaction {
		t.Errorf("MarshalText() = %q, want the redaction", text)
	}
}

func TestSlogAttributesRedact(t *testing.T) {
	t.Parallel()

	var buffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buffer, nil))
	logger.Info("a log line that names a secret", "password", secret.New(sentinel))

	if strings.Contains(buffer.String(), sentinel) {
		t.Errorf("slog wrote the secret: %s", buffer.String())
	}
}

func TestUnmarshalKeepsTheValueSecretFromTheMomentItIsParsed(t *testing.T) {
	t.Parallel()

	var document struct {
		Password secret.Value `json:"password"`
	}
	if err := json.Unmarshal([]byte(`{"password":"`+sentinel+`"}`), &document); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if document.Password.Reveal() != sentinel {
		t.Errorf("Reveal() = %q, want the parsed value", document.Password.Reveal())
	}
	if fmt.Sprintf("%v", document.Password) != secret.Redaction {
		t.Error("a parsed secret does not redact")
	}
}

func TestDerivedFormsAreTheOnlyOnesPermittedInTelemetry(t *testing.T) {
	t.Parallel()

	token := secret.New("sarnaut_as_abcdefghijklmnopqrstuvwxyz0123456789ABC")
	if len(token.Digest()) != 64 {
		t.Errorf("Digest() has %d characters, want 64", len(token.Digest()))
	}
	if len(token.ID()) != 16 || !strings.HasPrefix(token.Digest(), token.ID()) {
		t.Errorf("ID() = %q, want the first 16 characters of the digest", token.ID())
	}
	if strings.Contains(token.Digest(), token.Reveal()) {
		t.Error("the digest contains the token")
	}

	email := secret.New("Sentinel-2f9c41@Example.Invalid")
	if got := email.EmailDomain(); got != "example.invalid" {
		t.Errorf("EmailDomain() = %q, want %q", got, "example.invalid")
	}
	// A value that is not an address must yield nothing rather than leaking
	// what it actually was.
	if got := secret.New(sentinel).EmailDomain(); got != "" {
		t.Errorf("EmailDomain() = %q for a non-address, want empty", got)
	}
	if got := secret.New("trailing@").EmailDomain(); got != "" {
		t.Errorf("EmailDomain() = %q for a trailing @, want empty", got)
	}
}

func TestZeroValueIsUsable(t *testing.T) {
	t.Parallel()

	var value secret.Value
	if !value.Empty() || value.Len() != 0 {
		t.Error("the zero Value is not empty")
	}
	if value.String() != secret.Redaction {
		t.Error("the zero Value does not redact")
	}
}
