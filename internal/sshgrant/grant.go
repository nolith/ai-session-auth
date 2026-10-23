package sshgrant

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// defaultFor is a grant's duration without --for; maxMinutes is the
// longest, 12 h.
const (
	defaultFor = "4h"
	maxMinutes = 12 * 60
)

// hostName is what a host given to grant may look like: a short name, as
// the host's principal host:<name> has it.
var hostName = regexp.MustCompile(`^[a-z0-9-]+$`)

// minutes is how many minutes a duration such as 30m or 4h is.
func minutes(duration string) (int, error) {
	bad := errors.New("bad duration: " + duration + " (use e.g. 30m or 4h)")
	if len(duration) < 2 {
		return 0, bad
	}
	digits, unit := duration[:len(duration)-1], duration[len(duration)-1]
	if strings.Trim(digits, "0123456789") != "" || (unit != 'm' && unit != 'h') {
		return 0, bad
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n > maxMinutes {
		// Too long for any unit.
		return maxMinutes + 1, nil
	}
	if unit == 'h' {
		n *= 60
	}
	return n, nil
}

// requireTerminal refuses to sign unless the human's terminal is there to
// type the PIN and touch the YubiKey: never from a dialog an agent could
// have triggered.
func (t *Tool) requireTerminal() error {
	if !t.Terminal() {
		return errors.New("signing needs your terminal: run geas ssh yourself")
	}
	return nil
}

// sshKeygen runs ssh-keygen to do what, at the human's terminal, with no
// askpass program to ask for the PIN instead.
func (t *Tool) sshKeygen(what string, args ...string) error {
	command := exec.Command("ssh-keygen", args...)
	command.Stdin, command.Stdout, command.Stderr = t.Stdin, t.Stdout, t.Stderr
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "SSH_ASKPASS=") && !strings.HasPrefix(variable, "SSH_ASKPASS_REQUIRE=") {
			command.Env = append(command.Env, variable)
		}
	}
	if err := command.Run(); err != nil {
		return fmt.Errorf("ssh-keygen could not %s: %w", what, err)
	}
	return nil
}

// grant signs the agent's key for hosts, for duration, with the CA named
// caName or the only one plugged in, generating the key first if the agent
// has none.
func (t *Tool) grant(agent string, hosts []string, duration, caName string) error {
	name := Slug(agent)
	if name == "" {
		return errors.New("empty agent name")
	}
	if len(hosts) == 0 {
		return errors.New("no hosts given")
	}
	var principals []string
	for _, host := range hosts {
		if !hostName.MatchString(host) {
			return errors.New("bad host name: " + host)
		}
		principals = append(principals, "host:"+host)
	}
	mins, err := minutes(duration)
	if err != nil {
		return err
	}
	if mins <= 0 || mins > maxMinutes {
		return errors.New("duration must be between 1m and 12h")
	}
	ca, err := t.pickCA(caName)
	if err != nil {
		return err
	}
	if err = t.requireTerminal(); err != nil {
		return err
	}

	dir, err := AgentDir(name)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	for _, d := range []string{filepath.Dir(dir), dir} {
		if err = os.Chmod(d, 0700); err != nil {
			return err
		}
	}
	key := filepath.Join(dir, KeyFile)
	if _, err = os.Lstat(key); errors.Is(err, os.ErrNotExist) {
		if err = t.sshKeygen("generate the agent's key", "-q", "-t", "ed25519", "-N", "", "-C", "agent:"+name, "-f", key); err != nil {
			return err
		}
	}
	// ssh-keygen writes public keys and certificates readable by others.
	if err = os.Chmod(filepath.Join(dir, PublicKeyFile), 0600); err != nil {
		return err
	}

	now := t.Now()
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(host, ".")
	fmt.Fprintln(t.Stdout, "Granting "+name+" SSH access as agent to: "+strings.Join(hosts, " "))
	fmt.Fprintf(t.Stdout, "Until %s (%s)\n", now.Add(time.Duration(mins)*time.Minute).Format("2006-01-02 15:04 MST"), duration)
	fmt.Fprintln(t.Stdout, "Signing with "+ca.Name+": enter its FIDO2 PIN, then touch it.")

	// ssh-keygen writes the certificate next to the public key: sign a copy,
	// then rename the certificate into place, so that a session watching
	// the agent's directory never reads half of one.
	signing, err := os.MkdirTemp(dir, ".sign-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(signing)
	pub, err := os.ReadFile(filepath.Join(dir, PublicKeyFile))
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(signing, PublicKeyFile), pub, 0600); err != nil {
		return err
	}
	// Valid from 5 minutes ago, for hosts whose clock runs slightly behind.
	err = t.sshKeygen("sign the certificate", "-q", "-s", ca.Handle,
		"-I", name+"@"+host+" "+now.UTC().Format("2006-01-02T15:04Z"),
		"-z", strconv.FormatInt(now.Unix(), 10),
		"-n", strings.Join(principals, ","),
		"-V", "-5m:+"+strconv.Itoa(mins)+"m",
		"-O", "clear", "-O", "permit-pty",
		filepath.Join(signing, PublicKeyFile))
	if err != nil {
		return err
	}
	signed := filepath.Join(signing, CertificateFile)
	if err = os.Chmod(signed, 0600); err != nil {
		return err
	}
	if err = os.Rename(signed, filepath.Join(dir, CertificateFile)); err != nil {
		return err
	}
	fmt.Fprintln(t.Stdout, "Done. The certificate is in "+dir+".")
	return nil
}
