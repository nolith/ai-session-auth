package sshgrant

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nolith/geas/internal/secretfile"
)

// caDir is where enroll keeps the key handles it creates,
// $XDG_STATE_HOME/geas/agent-ca.
func caDir() (string, error) {
	state, err := secretfile.StateDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "agent-ca"), nil
}

// word reports whether text has name as a whole word, as grep -w finds it:
// fido2-token lists "noclientPin" for a YubiKey without a PIN.
func word(text, name string) bool {
	return regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(name) + `([^A-Za-z0-9_]|$)`).MatchString(text)
}

// fidoSuffix is what follows a device's path in fido2-token -L; macOS paths
// contain a colon themselves (ioreg://4295076776), so the path is what is
// left, not what comes before the first colon.
var fidoSuffix = regexp.MustCompile(`: vendor=.*$`)

// enroll creates an agent CA key on the one YubiKey plugged in, and prints
// its serial, public key and handle path, for the CA lists and the hosts.
func (t *Tool) enroll(ykName string) error {
	yk := Slug(ykName)
	if yk == "" {
		return errors.New("bad YubiKey name: " + ykName)
	}
	dir, err := caDir()
	if err != nil {
		return err
	}
	key := filepath.Join(dir, yk)
	if _, err = os.Lstat(key); err == nil {
		return errors.New(key + " already exists")
	}

	// The serial is how grants find this CA later, so exactly one YubiKey.
	serials := pluggedSerials()
	if len(serials) != 1 {
		return errors.New("plug in exactly one YubiKey to enrol (found: " + strings.Join(serials, " ") + ")")
	}
	serial := serials[0]
	cas, err := t.knownCAs()
	if err != nil {
		return err
	}
	for _, ca := range cas {
		if ca.Serial == serial {
			return errors.New("the YubiKey with serial " + serial + " is already an agent CA")
		}
	}

	// verify-required needs a FIDO2 PIN, and the YubiKey must already have
	// one: ssh-keygen would not offer to set it.
	devices, _ := exec.Command("fido2-token", "-L").Output()
	device, _, _ := strings.Cut(string(devices), "\n")
	if device = fidoSuffix.ReplaceAllString(device, ""); device == "" {
		return errors.New("no FIDO2 device found")
	}
	raw, err := exec.Command("fido2-token", "-I", device).Output()
	if err != nil {
		return errors.New("fido2-token cannot read the YubiKey's information")
	}
	info := string(raw)
	if !word(info, "clientPin") {
		return errors.New("the YubiKey has no FIDO2 PIN: set one with ykman --device " + serial + " fido access change-pin")
	}
	// OpenSSH creates a verify-required key only on authenticators that can
	// enforce it themselves (credProtect, YubiKey firmware 5.2 and later),
	// and fails with "requested feature not supported" otherwise.
	if !word(info, "credProtect") {
		return errors.New("this YubiKey cannot require its PIN for every signature (no FIDO2 credProtect, firmware before 5.2), so it cannot be an agent CA")
	}
	// Ed25519 needs firmware 5.2 or later; older YubiKeys only do ECDSA P-256.
	kind := "ecdsa-sk"
	if strings.Contains(info, "eddsa") {
		kind = "ed25519-sk"
	}

	if err = t.requireTerminal(); err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return err
	}
	fmt.Fprintf(t.Stderr, "Creating the agent CA (%s) on %s (serial %s): enter its FIDO2 PIN, then touch it.\n", kind, yk, serial)
	// Non-resident: the file is a handle, useless without the YubiKey, so it
	// needs no passphrase. verify-required asks for the PIN at every grant.
	if err = t.sshKeygen("create the agent CA", "-q", "-t", kind, "-O", "verify-required", "-O", "application=ssh:agent-ca", "-N", "", "-C", "Agent CA on "+yk, "-f", key); err != nil {
		return err
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		return err
	}
	fields := strings.Fields(string(pub))
	if len(fields) < 2 {
		return errors.New("ssh-keygen wrote no public key")
	}
	fmt.Fprintln(t.Stdout, "serial: "+serial)
	fmt.Fprintln(t.Stdout, "public key: "+fields[0]+" "+fields[1])
	fmt.Fprintln(t.Stdout, "handle: "+key)
	return nil
}
