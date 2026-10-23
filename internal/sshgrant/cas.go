package sshgrant

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nolith/geas/internal/config"
)

// SystemCAs is the CA list the system provides.
const SystemCAs = "/etc/agent-grant/cas"

// CA is an agent CA: a key on a YubiKey, known by the YubiKey's serial
// number, with its key handle, a file useless without that YubiKey.
type CA struct {
	Serial, Name, Handle string
}

// userCAs is the user's own CA list, $XDG_CONFIG_HOME/geas/cas, next to the
// configuration.
func userCAs() (string, error) {
	path, err := config.DefaultPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), "cas"), nil
}

// knownCAs are the CAs this machine can sign with, from the system's list,
// then the user's: one "<serial> <yk-name> <handle path>" per line, other
// lines and comments skipped. The first CA of a name wins.
func (t *Tool) knownCAs() ([]CA, error) {
	user, err := userCAs()
	if err != nil {
		return nil, err
	}
	var cas []CA
	seen := map[string]bool{}
	for _, path := range []string{t.SystemCAs, user} {
		file, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, ca := range parseCAs(file) {
			if !seen[ca.Name] {
				seen[ca.Name] = true
				cas = append(cas, ca)
			}
		}
		file.Close()
	}
	return cas, nil
}

func parseCAs(r io.Reader) []CA {
	var cas []CA
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 3 && !strings.HasPrefix(fields[0], "#") {
			cas = append(cas, CA{Serial: fields[0], Name: fields[1], Handle: fields[2]})
		}
	}
	return cas
}

// pluggedSerials are the serial numbers of the YubiKeys plugged in, as
// ykman lists them; none when ykman fails.
func pluggedSerials() []string {
	raw, err := exec.Command("ykman", "list", "--serials").Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(raw))
}

func names(cas []CA) string {
	var all []string
	for _, ca := range cas {
		all = append(all, ca.Name)
	}
	return strings.Join(all, " ")
}

// pickCA is the CA to sign with: the one named, else the only known one
// whose YubiKey is plugged in.
func (t *Tool) pickCA(name string) (CA, error) {
	cas, err := t.knownCAs()
	if err != nil {
		return CA{}, err
	}
	if len(cas) == 0 {
		return CA{}, errors.New("no agent CA on this machine: enrol a YubiKey with geas ssh enroll <yk-name>")
	}
	if name != "" {
		want := Slug(name)
		for _, ca := range cas {
			if ca.Name == want {
				return ca, nil
			}
		}
		return CA{}, fmt.Errorf("no agent CA named %s (known: %s)", want, names(cas))
	}
	plugged := map[string]bool{}
	for _, serial := range pluggedSerials() {
		plugged[serial] = true
	}
	var matches []CA
	for _, ca := range cas {
		if plugged[ca.Serial] {
			matches = append(matches, ca)
		}
	}
	switch len(matches) {
	case 0:
		return CA{}, errors.New("plug in one of the agent CA YubiKeys: " + names(cas))
	case 1:
		return matches[0], nil
	}
	return CA{}, errors.New("several agent CA YubiKeys are plugged in, pick one with --ca: " + names(matches))
}

// listCAs prints the known CAs, and which are plugged in.
func (t *Tool) listCAs(output io.Writer) error {
	cas, err := t.knownCAs()
	if err != nil {
		return err
	}
	if len(cas) == 0 {
		return errors.New("no agent CA on this machine")
	}
	plugged := map[string]bool{}
	for _, serial := range pluggedSerials() {
		plugged[serial] = true
	}
	for _, ca := range cas {
		state := ""
		if plugged[ca.Serial] {
			state = ", plugged in"
		}
		if _, err = fmt.Fprintf(output, "%s (serial %s%s): %s\n", ca.Name, ca.Serial, state, ca.Handle); err != nil {
			return err
		}
	}
	return nil
}
