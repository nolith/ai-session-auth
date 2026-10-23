package sshgrant

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// certType is the only certificate geas ssh signs and reads: the agent's
// key is always ed25519.
const certType = "ssh-ed25519-cert-v01@openssh.com"

// Certificate is what geas ssh reads from an agent's certificate, in the
// format of OpenSSH's PROTOCOL.certkeys. The signature is the host's
// business: sshd checks it against the CAs it trusts.
type Certificate struct {
	// PublicKey is the ed25519 key the certificate is for.
	PublicKey  []byte
	Serial     uint64
	KeyID      string
	Principals []string
	// ValidAfter and ValidBefore are seconds since the epoch, as written:
	// 0 and math.MaxUint64 mean no bound.
	ValidAfter, ValidBefore uint64
	// The names of the critical options and of the extensions.
	CriticalOptions, Extensions []string
}

// ParseCertificate reads a user certificate as ssh-keygen -s writes it, one
// line: type, base64 and comment.
func ParseCertificate(raw []byte) (Certificate, error) {
	invalid := errors.New("not an ed25519 user certificate")
	fields := strings.Fields(string(raw))
	if len(fields) < 2 || fields[0] != certType {
		return Certificate{}, invalid
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return Certificate{}, invalid
	}
	w := &wire{b: blob}
	var c Certificate
	kind := w.string()
	w.string() // nonce
	c.PublicKey = w.string()
	c.Serial = w.uint64()
	userCert := w.uint32()
	c.KeyID = string(w.string())
	principals := w.string()
	c.ValidAfter, c.ValidBefore = w.uint64(), w.uint64()
	critical, extensions := w.string(), w.string()
	w.string() // reserved
	signer, signature := w.string(), w.string()
	if w.err || len(w.b) != 0 || len(signer) == 0 || len(signature) == 0 || string(kind) != certType || userCert != 1 || len(c.PublicKey) != 32 {
		return Certificate{}, invalid
	}
	var ok bool
	if c.Principals, ok = packed(principals, false); !ok {
		return Certificate{}, invalid
	}
	if c.CriticalOptions, ok = packed(critical, true); !ok {
		return Certificate{}, invalid
	}
	if c.Extensions, ok = packed(extensions, true); !ok {
		return Certificate{}, invalid
	}
	return c, nil
}

// ValidAt reports whether the certificate is valid at t.
func (c Certificate) ValidAt(t time.Time) bool {
	now := t.Unix()
	return now >= 0 && uint64(now) >= c.ValidAfter && uint64(now) < c.ValidBefore
}

// Expires is when the certificate stops being valid, or the zero time when
// it never does.
func (c Certificate) Expires() time.Time {
	if c.ValidBefore == math.MaxUint64 || c.ValidBefore > math.MaxInt64 {
		return time.Time{}
	}
	return time.Unix(int64(c.ValidBefore), 0)
}

// Validity is the certificate's validity as ssh-keygen -L writes it, in
// local time.
func (c Certificate) Validity() string {
	at := func(seconds uint64) string {
		if seconds > math.MaxInt64 {
			seconds = math.MaxInt64
		}
		return time.Unix(int64(seconds), 0).Format("2006-01-02T15:04:05")
	}
	switch {
	case c.ValidAfter == 0 && c.ValidBefore == math.MaxUint64:
		return "forever"
	case c.ValidAfter == 0:
		return "before " + at(c.ValidBefore)
	case c.ValidBefore == math.MaxUint64:
		return "after " + at(c.ValidAfter)
	}
	return "from " + at(c.ValidAfter) + " to " + at(c.ValidBefore)
}

// Describe is the part of ssh-keygen -L's listing that geas ssh show prints:
// key ID, serial, validity and principals.
func (c Certificate) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "        Key ID: %q\n", c.KeyID)
	fmt.Fprintf(&b, "        Serial: %d\n", c.Serial)
	fmt.Fprintf(&b, "        Valid: %s\n", c.Validity())
	b.WriteString("        Principals: ")
	if len(c.Principals) == 0 {
		b.WriteString("(none)")
	}
	for _, p := range c.Principals {
		b.WriteString("\n                " + p)
	}
	b.WriteString("\n")
	return b.String()
}

// ForKey reports whether the certificate is for the public key in pub, a
// line as ssh-keygen writes id_ed25519.pub.
func (c Certificate) ForKey(pub []byte) bool {
	fields := strings.Fields(string(pub))
	if len(fields) < 2 || fields[0] != "ssh-ed25519" {
		return false
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return false
	}
	w := &wire{b: blob}
	kind, key := w.string(), w.string()
	return !w.err && string(kind) == "ssh-ed25519" && bytes.Equal(key, c.PublicKey)
}

// packed reads a packed list of strings, or of name and data pairs when pairs
// is set, returning the names.
func packed(raw []byte, pairs bool) ([]string, bool) {
	w := &wire{b: raw}
	var names []string
	for len(w.b) > 0 {
		name := w.string()
		if pairs {
			w.string()
		}
		if w.err {
			return nil, false
		}
		names = append(names, string(name))
	}
	return names, true
}

// wire reads the SSH wire format. A short read sets err, and every read
// after it returns zero values.
type wire struct {
	b   []byte
	err bool
}

func (w *wire) take(n uint64) []byte {
	if w.err || uint64(len(w.b)) < n {
		w.err, w.b = true, nil
		return nil
	}
	out := w.b[:n]
	w.b = w.b[n:]
	return out
}

func (w *wire) uint32() uint32 {
	if b := w.take(4); b != nil {
		return binary.BigEndian.Uint32(b)
	}
	return 0
}

func (w *wire) uint64() uint64 {
	if b := w.take(8); b != nil {
		return binary.BigEndian.Uint64(b)
	}
	return 0
}

func (w *wire) string() []byte { return w.take(uint64(w.uint32())) }
