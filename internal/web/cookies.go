package web

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"atlassian-mcp/internal/crypto"
)

// cookieJar stores small JSON values in encrypted, authenticated cookies.
// The cookie name is bound into the ciphertext so values cannot be swapped between cookies.
type cookieJar struct {
	sealer *crypto.Sealer
	secure bool
	now    func() time.Time
}

type envelope struct {
	Exp  int64           `json:"exp"`
	Data json.RawMessage `json:"d"`
}

func (j *cookieJar) set(w http.ResponseWriter, name string, v any, ttl time.Duration) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(envelope{Exp: j.now().Add(ttl).Unix(), Data: data})
	if err != nil {
		return err
	}
	sealed, err := j.sealer.SealString(string(raw), "cookie|"+name)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: sealed, Path: "/", MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: j.secure, SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// get decodes cookie name into v. It returns false if absent, tampered or expired.
func (j *cookieJar) get(r *http.Request, name string, v any) bool {
	c, err := r.Cookie(name)
	if err != nil {
		return false
	}
	raw, err := j.sealer.OpenString(c.Value, "cookie|"+name)
	if err != nil {
		return false
	}
	var env envelope
	if json.Unmarshal([]byte(raw), &env) != nil || j.now().Unix() >= env.Exp {
		return false
	}
	return json.Unmarshal(env.Data, v) == nil
}

func (j *cookieJar) clear(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: j.secure, SameSite: http.SameSiteLaxMode,
	})
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
