package codexauth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Obviously fake values; the tests never touch a real login.
const (
	fakeAccount = "acct-FAKE-0000"
	fakeRefresh = "refresh-FAKE-1111"
	fakeID      = "id-FAKE-2222"
)

var now = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

// jwtWith builds an unsigned three-part token whose payload carries exp.
func jwtWith(exp time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	payload := fmt.Sprintf(`{"exp":%d}`, exp.Unix())
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(payload)) + "." + enc([]byte("sig"))
}

func writeAuth(t *testing.T, mode os.FileMode, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func goodBody(access string) string {
	return fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"id_token":%q,"access_token":%q,"refresh_token":%q,"account_id":%q},"last_refresh":"2026-10-12T10:00:00.123456789Z"}`,
		fakeID, access, fakeRefresh, fakeAccount)
}

func TestLoadReadsAGoodFile(t *testing.T) {
	access := jwtWith(now.Add(time.Hour))
	c, err := Load(writeAuth(t, 0o600, goodBody(access)), now)
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessToken.Reveal() != access || c.AccountID.Reveal() != fakeAccount {
		t.Error("the credentials were not read back")
	}
	if !c.Expires.Equal(now.Add(time.Hour)) {
		t.Errorf("Expires = %v, want %v", c.Expires, now.Add(time.Hour))
	}
	if c.LastRefresh.IsZero() {
		t.Error("last_refresh was not read")
	}
}

func TestLoadAcceptsATokenThatIsNotAJWT(t *testing.T) {
	c, err := Load(writeAuth(t, 0o600, goodBody("opaque-FAKE-token")), now)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Expires.IsZero() {
		t.Errorf("an opaque token got an expiry %v; it states none", c.Expires)
	}
}

func TestLoadRefusesAnExpiredToken(t *testing.T) {
	_, err := Load(writeAuth(t, 0o600, goodBody(jwtWith(now.Add(-time.Minute)))), now)
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("err = %v, want ErrExpired", err)
	}
	if !strings.Contains(err.Error(), "run Codex CLI") {
		t.Errorf("the error does not say what to do: %v", err)
	}
}

func TestLoadAbsentFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), FileName), now)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestLoadRefusesAFileOthersCanRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o660} {
		_, err := Load(writeAuth(t, mode, goodBody(jwtWith(now.Add(time.Hour)))), now)
		if !errors.Is(err, ErrInsecure) {
			t.Errorf("mode %04o: err = %v, want ErrInsecure", mode, err)
		}
	}
}

func TestLoadRefusesAnAPIKeyLogin(t *testing.T) {
	_, err := Load(writeAuth(t, 0o600, `{"auth_mode":"apikey","OPENAI_API_KEY":"sk-FAKE-9999"}`), now)
	if !errors.Is(err, ErrNotChatGPT) {
		t.Fatalf("err = %v, want ErrNotChatGPT", err)
	}
	if strings.Contains(err.Error(), "sk-FAKE-9999") {
		t.Error("the error quoted the key")
	}
}

func TestLoadMalformedFilesNeverQuoteTheirContent(t *testing.T) {
	for name, body := range map[string]string{
		"not json":        `access_token=secret-FAKE-7777`,
		"truncated":       `{"auth_mode":"chatgpt","tokens":{"access_token":"secret-FAKE-7777`,
		"no tokens":       `{"auth_mode":"chatgpt"}`,
		"empty token":     `{"auth_mode":"chatgpt","tokens":{"access_token":""}}`,
		"wrong type":      `{"auth_mode":"chatgpt","tokens":"secret-FAKE-7777"}`,
		"empty file":      ``,
		"array at top":    `["secret-FAKE-7777"]`,
		"number as token": `{"auth_mode":"chatgpt","tokens":{"access_token":7777}}`,
	} {
		_, err := Load(writeAuth(t, 0o600, body), now)
		if !errors.Is(err, ErrMalformed) && !errors.Is(err, ErrNotChatGPT) {
			t.Errorf("%s: err = %v, want a malformed-file refusal", name, err)
			continue
		}
		if strings.Contains(err.Error(), "secret-FAKE-7777") {
			t.Errorf("%s: the error quoted the file: %v", name, err)
		}
	}
}

// The credential must not reach a log, an error, a journal or a fixture by being
// handed to something that formats it. Every path the standard library has.
func TestACredentialCannotBePrintedByAccident(t *testing.T) {
	access := jwtWith(now.Add(time.Hour))
	c, err := Load(writeAuth(t, 0o600, goodBody(access)), now)
	if err != nil {
		t.Fatal(err)
	}

	var outputs []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		outputs = append(outputs, fmt.Sprintf(verb, c), fmt.Sprintf(verb, c.AccessToken), fmt.Sprintf(verb, c.AccountID))
	}
	j, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	outputs = append(outputs, string(j))
	jm, _ := json.Marshal(map[string]Secret{"t": c.AccessToken})
	outputs = append(outputs, string(jm))

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("loaded", "credentials", c, "token", c.AccessToken)
	slog.New(slog.NewTextHandler(&buf, nil)).Info("loaded", "credentials", c, "token", c.AccessToken)
	outputs = append(outputs, buf.String())

	for _, out := range outputs {
		for _, secret := range []string{access, fakeAccount, fakeRefresh, fakeID} {
			if strings.Contains(out, secret) {
				t.Errorf("a secret appeared in %q", out)
			}
		}
	}
	if !strings.Contains(fmt.Sprint(c.AccessToken), Redacted) {
		t.Error("a Secret does not render as the redaction marker")
	}
}

func TestRefreshAndIDTokensAreNotKept(t *testing.T) {
	c, err := Load(writeAuth(t, 0o600, goodBody(jwtWith(now.Add(time.Hour)))), now)
	if err != nil {
		t.Fatal(err)
	}
	// Reflect on every field's rendered value: nothing in the struct holds them.
	if got := fmt.Sprintf("%#v", c); strings.Contains(got, fakeRefresh) || strings.Contains(got, fakeID) {
		t.Error("a token the first slice does not need is held in Credentials")
	}
}

func TestDirPrefersCodexHome(t *testing.T) {
	got, err := Dir(func(k string) string {
		if k == HomeEnv {
			return "/somewhere/codex"
		}
		return ""
	})
	if err != nil || got != "/somewhere/codex" {
		t.Errorf("Dir = %q, %v", got, err)
	}
	t.Setenv("HOME", "/home/test")
	t.Setenv("USERPROFILE", `C:\Users\test`)
	got, err = Dir(func(string) string { return "" })
	if err != nil || filepath.Base(got) != ".codex" {
		t.Errorf("default Dir = %q, %v; want a .codex directory", got, err)
	}
}
