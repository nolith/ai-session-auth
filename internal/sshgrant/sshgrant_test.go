package sshgrant

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nolith/geas/internal/testutil"
)

// tool is geas ssh with a terminal, its output kept, its state and
// configuration in new directories, and no system CA list.
func tool(t *testing.T) (*Tool, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GEAS_SESSION_DIR", "")
	t.Setenv("AGENT_SSH_NAME", "")
	t.Setenv("HERDR_WORKSPACE_ID", "")
	var out, errs bytes.Buffer
	return &Tool{
		Stdout:    &out,
		Stderr:    &errs,
		Terminal:  func() bool { return true },
		Now:       time.Now,
		SystemCAs: filepath.Join(t.TempDir(), "absent"),
	}, &out, &errs
}

// ykman fakes ykman with the serials plugged in.
func ykman(t *testing.T, serials ...string) {
	t.Helper()
	testutil.Executable(t, "ykman", `[ "$*" = "list --serials" ] || exit 2
printf '%s\n' `+strings.Join(serials, " ")+"\n")
}

// userCAs writes the user's CA list.
func writeUserCAs(t *testing.T, lines string) {
	t.Helper()
	path, err := userCAs()
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(path), 0700)
	if err = os.WriteFile(path, []byte(lines), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"claude-phase7":     "claude-phase7",
		"My Work_Space":     "my-work-space",
		"--a--b--":          "a-b",
		"../../etc":         "etc",
		"caffè latte":       "caff-latte",
		"":                  "",
		"!!!":               "",
		"YK-5C-Nano-FIPS-2": "yk-5c-nano-fips-2",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestName(t *testing.T) {
	ctx := context.Background()
	herdr := `[ "$1 $2 $3" = "workspace get w7" ] || exit 2
echo '{"result":{"workspace":{"id":"w7","label":"Phase 7!"}}}'
`
	cases := []struct {
		name, tool, known, workspace, herdr, want, err string
	}{
		{name: "from herdr", tool: "claude", workspace: "w7", herdr: herdr, want: "claude-phase-7"},
		{name: "a session's name needs no herdr", tool: "claude", known: "claude-phase7", workspace: "w7", want: "claude-phase7"},
		{name: "the session's name without a tool", known: "Claude-Phase7", want: "claude-phase7"},
		{name: "another tool's name asks herdr", tool: "opencode", known: "claude-phase7", workspace: "w7", herdr: herdr, want: "opencode-phase-7"},
		{name: "herdr unreachable", tool: "claude", workspace: "w7", herdr: "echo 'ping refused' >&2; exit 1\n", err: "cannot ask herdr for the workspace's label"},
		{name: "no herdr", tool: "claude", workspace: "w7", err: "herdr not found, and AGENT_SSH_NAME names no agent of this tool"},
		{name: "no workspace", tool: "claude", herdr: herdr, err: "not in a herdr workspace, and AGENT_SSH_NAME names no agent of this tool"},
		{name: "no label", tool: "claude", workspace: "w7", herdr: `echo '{"result":{"workspace":{"label":"!!"}}}'` + "\n", err: "the herdr workspace has no usable label"},
		{name: "bad tool", tool: "Claude", workspace: "w7", herdr: herdr, err: "bad tool name: Claude"},
		{name: "nothing", err: "no agent: give a <tool>, or run inside a geas session"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A PATH without any herdr, unless the case fakes one.
			t.Setenv("PATH", t.TempDir())
			if c.herdr != "" {
				testutil.Executable(t, "herdr", c.herdr)
			}
			t.Setenv("AGENT_SSH_NAME", c.known)
			t.Setenv("HERDR_WORKSPACE_ID", c.workspace)
			got, err := Name(ctx, c.tool)
			if c.err != "" {
				if err == nil || err.Error() != c.err {
					t.Fatalf("got %q, %v; want error %q", got, err, c.err)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

func TestNameCommand(t *testing.T) {
	ssh, out, _ := tool(t)
	t.Setenv("AGENT_SSH_NAME", "claude-phase7")
	// Inside a session too.
	t.Setenv("GEAS_SESSION_DIR", "/somewhere")
	if code, err := ssh.Run(context.Background(), []string{"name"}); code != 0 || err != nil || out.String() != "claude-phase7\n" {
		t.Fatalf("%d %v %q", code, err, out.String())
	}
	if code, err := ssh.Run(context.Background(), []string{"name", "a", "b"}); code != 2 || err == nil {
		t.Fatalf("%d %v", code, err)
	}
}

func TestMinutes(t *testing.T) {
	for in, want := range map[string]int{"30m": 30, "4h": 240, "12h": 720, "0m": 0, "721m": 721, "13h": 13 * 60, "99999999999999999999h": maxMinutes + 1} {
		if got, err := minutes(in); err != nil || got != want {
			t.Errorf("minutes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "m", "h", "4", "4d", "1.5h", "-1h", "h4", "4hm", " 4h", "1x2m"} {
		if _, err := minutes(in); err == nil || err.Error() != "bad duration: "+in+" (use e.g. 30m or 4h)" {
			t.Errorf("minutes(%q): %v", in, err)
		}
	}
}

func TestGrantValidation(t *testing.T) {
	ssh, _, _ := tool(t)
	ykman(t)
	for _, c := range []struct {
		args []string
		err  string
	}{
		{[]string{"!!", "transcoder"}, "empty agent name"},
		{[]string{"claude-x"}, "no hosts given"},
		{[]string{"claude-x", "Transcoder"}, "bad host name: Transcoder"},
		{[]string{"claude-x", "transcoder.tail.net"}, "bad host name: transcoder.tail.net"},
		{[]string{"claude-x", "a", "b_c"}, "bad host name: b_c"},
		{[]string{"claude-x", "transcoder", "--for", "2d"}, "bad duration: 2d (use e.g. 30m or 4h)"},
		{[]string{"claude-x", "transcoder", "--for", "0m"}, "duration must be between 1m and 12h"},
		{[]string{"claude-x", "transcoder", "--for", "13h"}, "duration must be between 1m and 12h"},
		{[]string{"claude-x", "transcoder", "--for", "721m"}, "duration must be between 1m and 12h"},
		{[]string{"claude-x", "transcoder", "--for", "12h"}, "no agent CA on this machine: enrol a YubiKey with geas ssh enroll <yk-name>"},
	} {
		code, err := ssh.Run(context.Background(), append([]string{"grant"}, c.args...))
		if code != 1 || err == nil || err.Error() != c.err {
			t.Errorf("%v: %d %v, want %q", c.args, code, err, c.err)
		}
	}
	for _, args := range [][]string{{"grant"}, {"grant", "x", "h", "--for"}, {"grant", "x", "h", "--as", "y"}} {
		if code, err := ssh.Run(context.Background(), args); code != 2 || err == nil {
			t.Errorf("%v: %d %v", args, code, err)
		}
	}
	if agents, _ := AgentsDir(); exists(agents) {
		t.Fatal("a refused grant created the agents' directory")
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestCAs(t *testing.T) {
	ssh, out, _ := tool(t)
	system := filepath.Join(t.TempDir(), "cas")
	os.WriteFile(system, []byte("19339231 yk-one /run/agenix/agent-ca-yk-one\n# 1 commented /x\n\n34325893 yk-two /run/agenix/agent-ca-yk-two\nbroken line\n"), 0644)
	ssh.SystemCAs = system
	// The user's list adds a CA and cannot replace the system's yk-one.
	writeUserCAs(t, "555 yk-one /elsewhere\n777 yk-local /home/me/handle extra\n777 yk-local /home/me/handle\n")
	cas, err := ssh.knownCAs()
	want := []CA{{"19339231", "yk-one", "/run/agenix/agent-ca-yk-one"}, {"34325893", "yk-two", "/run/agenix/agent-ca-yk-two"}, {"777", "yk-local", "/home/me/handle"}}
	if err != nil || !slices.Equal(cas, want) {
		t.Fatalf("%v %v", cas, err)
	}

	ykman(t, "34325893", "1")
	if code, err := ssh.Run(context.Background(), []string{"list-cas"}); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	wantList := "yk-one (serial 19339231): /run/agenix/agent-ca-yk-one\nyk-two (serial 34325893, plugged in): /run/agenix/agent-ca-yk-two\nyk-local (serial 777): /home/me/handle\n"
	if out.String() != wantList {
		t.Fatalf("got:\n%s", out.String())
	}

	pick := func(name string) (string, error) {
		ca, err := ssh.pickCA(name)
		return ca.Name, err
	}
	if got, err := pick(""); got != "yk-two" || err != nil {
		t.Fatal(got, err)
	}
	// --ca needs no YubiKey plugged in: ssh-keygen asks for it.
	if got, err := pick("YK-Local"); got != "yk-local" || err != nil {
		t.Fatal(got, err)
	}
	if _, err := pick("yk-three"); err == nil || err.Error() != "no agent CA named yk-three (known: yk-one yk-two yk-local)" {
		t.Fatal(err)
	}
	ykman(t, "777", "19339231")
	if _, err := pick(""); err == nil || err.Error() != "several agent CA YubiKeys are plugged in, pick one with --ca: yk-one yk-local" {
		t.Fatal(err)
	}
	ykman(t, "5", "555")
	if _, err := pick(""); err == nil || err.Error() != "plug in one of the agent CA YubiKeys: yk-one yk-two yk-local" {
		t.Fatal(err)
	}
	// ykman failing, or missing, is no YubiKey plugged in.
	testutil.Executable(t, "ykman", "exit 1\n")
	if _, err := pick(""); err == nil || !strings.HasPrefix(err.Error(), "plug in one of") {
		t.Fatal(err)
	}
}

// grantFixture is geas ssh with a software CA, test-ca, whose stand-in
// YubiKey is plugged in.
func grantFixture(t *testing.T) (*Tool, *bytes.Buffer, string) {
	t.Helper()
	ssh, out, _ := tool(t)
	ca := testutil.SSHCA(t)
	writeUserCAs(t, "4242 test-ca "+ca+"\n")
	ykman(t, "4242")
	return ssh, out, ca
}

func certificate(t *testing.T, name string) (Certificate, []byte) {
	t.Helper()
	dir, err := AgentDir(name)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, CertificateFile))
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := os.ReadFile(filepath.Join(dir, PublicKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	return c, pub
}

func TestGrant(t *testing.T) {
	ssh, out, ca := grantFixture(t)
	t.Setenv("SSH_ASKPASS", "/bin/false")
	start := time.Now()
	if code, err := ssh.Run(context.Background(), []string{"grant", "Claude-Phase7", "transcoder", "--for", "2h", "mac-lab"}); code != 0 || err != nil {
		t.Fatalf("%d %v\n%s", code, err, out)
	}
	dir, _ := AgentDir("claude-phase7")
	lines := strings.Split(out.String(), "\n")
	if len(lines) != 5 || lines[0] != "Granting claude-phase7 SSH access as agent to: transcoder mac-lab" ||
		!regexp.MustCompile(`^Until \d{4}-\d\d-\d\d \d\d:\d\d \S+ \(2h\)$`).MatchString(lines[1]) ||
		lines[2] != "Signing with test-ca: enter its FIDO2 PIN, then touch it." ||
		lines[3] != "Done. The certificate is in "+dir+"." {
		t.Fatalf("output:\n%s", out)
	}
	c, pub := certificate(t, "claude-phase7")
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(host, ".")
	if !c.ForKey(pub) || !slices.Equal(c.Principals, []string{"host:transcoder", "host:mac-lab"}) ||
		len(c.CriticalOptions) != 0 || !slices.Equal(c.Extensions, []string{"permit-pty"}) ||
		!strings.HasPrefix(c.KeyID, "claude-phase7@"+host+" ") || c.Serial < uint64(start.Unix()) {
		t.Fatalf("%+v", c)
	}
	// Valid from 5 minutes ago, for 2 hours.
	after, before := time.Unix(int64(c.ValidAfter), 0), c.Expires()
	if d := start.Add(-5 * time.Minute).Sub(after); d < -time.Minute || d > time.Minute {
		t.Errorf("valid after %v, started %v", after, start)
	}
	if d := before.Sub(start.Add(2 * time.Hour)); d < -time.Minute || d > time.Minute {
		t.Errorf("valid before %v, started %v", before, start)
	}
	// Signed by the CA.
	fingerprint := strings.Fields(string(testutil.SSHKeygen(t, "-l", "-E", "sha256", "-f", ca+".pub")))[1]
	if listing := string(testutil.SSHKeygen(t, "-L", "-f", filepath.Join(dir, CertificateFile))); !strings.Contains(listing, "Signing CA: ED25519 "+fingerprint+" ") {
		t.Errorf("not signed by %s:\n%s", fingerprint, listing)
	}
	modes := map[string]os.FileMode{filepath.Dir(dir): 0700, dir: 0700, filepath.Join(dir, KeyFile): 0600, filepath.Join(dir, PublicKeyFile): 0600, filepath.Join(dir, CertificateFile): 0600}
	for path, mode := range modes {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != mode {
			t.Errorf("%s: %v %v, want %v", path, info.Mode(), err, mode)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 3 {
		t.Errorf("leftovers in %s: %v", dir, entries)
	}

	// Another grant signs the same key again, for other hosts.
	key, _ := os.ReadFile(filepath.Join(dir, KeyFile))
	out.Reset()
	if code, err := ssh.Run(context.Background(), []string{"grant", "claude-phase7", "transcoder", "--ca", "test-ca"}); code != 0 || err != nil {
		t.Fatalf("%d %v\n%s", code, err, out)
	}
	again, pubAgain := certificate(t, "claude-phase7")
	keyAgain, _ := os.ReadFile(filepath.Join(dir, KeyFile))
	if !bytes.Equal(key, keyAgain) || !bytes.Equal(pub, pubAgain) || !again.ForKey(pub) || !slices.Equal(again.Principals, []string{"host:transcoder"}) {
		t.Fatalf("%+v", again)
	}
	// The default duration is 4 hours.
	if d := again.Expires().Sub(time.Now().Add(4 * time.Hour)); d < -time.Minute || d > time.Minute {
		t.Errorf("valid before %v", again.Expires())
	}
}

func TestGrantAndEnrollRefuse(t *testing.T) {
	ssh, _, _ := grantFixture(t)
	ssh.Terminal = func() bool { return false }
	code, err := ssh.Run(context.Background(), []string{"grant", "claude-x", "transcoder"})
	if code != 1 || err == nil || err.Error() != "signing needs your terminal: run geas ssh yourself" {
		t.Fatal(code, err)
	}
	if agents, _ := AgentsDir(); exists(agents) {
		t.Fatal("a refused grant created the agents' directory")
	}
	ssh.Terminal = func() bool { return true }
	t.Setenv("GEAS_SESSION_DIR", "/somewhere")
	for _, command := range []string{"grant", "enroll"} {
		code, err = ssh.Run(context.Background(), []string{command, "claude-x", "transcoder"})
		if code != 1 || err == nil || err.Error() != "geas ssh "+command+" refuses to run inside a geas session: run it yourself, at your own terminal" {
			t.Fatal(code, err)
		}
	}
}

// keygenListing is what agent-grant --show printed: ssh-keygen -L from Key
// ID, Valid or Principals up to Critical Options, without that line.
func keygenListing(t *testing.T, path string) string {
	t.Helper()
	var b strings.Builder
	printing := false
	for _, line := range strings.SplitAfter(string(testutil.SSHKeygen(t, "-L", "-f", path)), "\n") {
		if regexp.MustCompile(`Key ID|Valid:|Principals:`).MatchString(line) {
			printing = true
		}
		if strings.Contains(line, "Critical") {
			break
		}
		if printing {
			b.WriteString(line)
		}
	}
	return b.String()
}

func TestShowAndDrop(t *testing.T) {
	ssh, out, _ := tool(t)
	ctx := context.Background()
	if code, err := ssh.Run(ctx, []string{"show"}); code != 0 || err != nil || out.String() != "no agent has a key\n" {
		t.Fatal(code, err, out)
	}
	valid, _ := AgentDir("claude-valid")
	expired, _ := AgentDir("opencode-old")
	keyOnly, _ := AgentDir("pi-new")
	testutil.SSHCertificate(t, valid, "-5m:+2h", "host:transcoder", "host:mac-lab")
	testutil.SSHCertificate(t, expired, "20260101000000:20260101020000", "host:transcoder")
	os.MkdirAll(keyOnly, 0700)
	testutil.SSHKeygen(t, "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(keyOnly, KeyFile))
	os.Mkdir(filepath.Join(filepath.Dir(valid), "no-key"), 0700)

	out.Reset()
	if code, err := ssh.Run(ctx, []string{"show", "claude-valid"}); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	if want := keygenListing(t, filepath.Join(valid, CertificateFile)); out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
	out.Reset()
	ssh.Run(ctx, []string{"show", "opencode-old"})
	if want := keygenListing(t, filepath.Join(expired, CertificateFile)); out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
	if code, err := ssh.Run(ctx, []string{"show", "pi-new"}); code != 1 || err == nil || err.Error() != "pi-new has no certificate" {
		t.Fatal(code, err)
	}

	out.Reset()
	if code, err := ssh.Run(ctx, []string{"show"}); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	c, _ := certificate(t, "claude-valid")
	old, _ := certificate(t, "opencode-old")
	want := "claude-valid  until " + c.Expires().Format("2006-01-02 15:04 MST") + ": host:transcoder host:mac-lab\n" +
		"opencode-old  expired " + old.Expires().Format("2006-01-02 15:04 MST") + ": host:transcoder\n" +
		"pi-new        no certificate\n"
	if out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}

	out.Reset()
	if code, err := ssh.Run(ctx, []string{"drop", "opencode-old"}); code != 0 || err != nil || out.String() != "Dropped opencode-old.\n" || exists(expired) {
		t.Fatal(code, err, out)
	}
	if code, err := ssh.Run(ctx, []string{"drop", "opencode-old"}); code != 1 || err == nil || err.Error() != "opencode-old has nothing to drop" {
		t.Fatal(code, err)
	}
	// A file drop does not know stays, and so does the directory.
	os.WriteFile(filepath.Join(valid, "notes"), nil, 0600)
	if code, err := ssh.Run(ctx, []string{"drop", "claude-valid"}); code != 1 || err == nil || !strings.HasPrefix(err.Error(), "remove "+valid+": ") || exists(filepath.Join(valid, KeyFile)) {
		t.Fatal(code, err)
	}
}

func TestParseCertificate(t *testing.T) {
	dir := t.TempDir()
	testutil.SSHCertificate(t, dir, "always:forever")
	raw, _ := os.ReadFile(filepath.Join(dir, CertificateFile))
	c, err := ParseCertificate(raw)
	if err != nil || c.Validity() != "forever" || !c.Expires().IsZero() || !c.ValidAt(time.Now()) || len(c.Principals) != 0 {
		t.Fatalf("%+v %v", c, err)
	}
	if !strings.Contains(c.Describe(), "        Principals: (none)\n") {
		t.Fatal(c.Describe())
	}
	pub, _ := os.ReadFile(filepath.Join(dir, PublicKeyFile))
	if !c.ForKey(pub) {
		t.Fatal("certificate is not for its key")
	}
	other := filepath.Join(t.TempDir(), "other")
	testutil.SSHKeygen(t, "-q", "-t", "ed25519", "-N", "", "-f", other)
	otherPub, _ := os.ReadFile(other + ".pub")
	if c.ForKey(otherPub) || c.ForKey(nil) {
		t.Fatal("certificate is for another key")
	}
	fields := strings.Fields(string(raw))
	for _, bad := range []string{"", fields[0], "ssh-ed25519 " + fields[1], fields[0] + " " + fields[1][:len(fields[1])-12], fields[0] + " !!"} {
		if _, err := ParseCertificate([]byte(bad)); err == nil {
			t.Errorf("parsed %q", bad)
		}
	}
	// A public key is not a certificate.
	if _, err := ParseCertificate(pub); err == nil {
		t.Error("parsed a public key")
	}
}

func TestEnroll(t *testing.T) {
	ssh, out, errs := tool(t)
	ykman(t, "4242")
	info := "options: rk, up, noplat, clientPin, credentialMgmtPreview\nextension strings: credProtect, hmac-secret\nalgorithms: es256 (public-key), eddsa (public-key)\n"
	fido := func(info string) {
		testutil.Executable(t, "fido2-token", `case "$1" in
-L) echo 'ioreg://4295076776: vendor=0x1050, product=0x0407 (Yubico YubiKey OTP+FIDO+CCID)' ;;
-I) [ "$2" = ioreg://4295076776 ] || exit 2; printf '%s' '`+info+`' ;;
esac
`)
	}
	// ssh-keygen cannot create a key on a YubiKey that is not there: this
	// one records its arguments and writes a handle.
	log := filepath.Join(t.TempDir(), "args")
	testutil.Executable(t, "ssh-keygen", `printf '%s\n' "$@" >`+log+`
for last; do :; done
echo handle >"$last"
echo 'sk-ssh-ed25519@openssh.com AAAAGnNrLXNzaC1lZDI1NTE5QG9wZW5zc2guY29t Agent CA' >"$last.pub"
`)
	ctx := context.Background()
	fido(strings.Replace(info, "clientPin", "noclientPin", 1))
	if code, err := ssh.Run(ctx, []string{"enroll", "yk-new"}); code != 1 || err == nil || err.Error() != "the YubiKey has no FIDO2 PIN: set one with ykman --device 4242 fido access change-pin" {
		t.Fatal(code, err)
	}
	fido(strings.Replace(info, "credProtect, ", "", 1))
	if code, err := ssh.Run(ctx, []string{"enroll", "yk-new"}); code != 1 || err == nil || !strings.HasPrefix(err.Error(), "this YubiKey cannot require its PIN") {
		t.Fatal(code, err)
	}
	fido(info)
	ssh.Terminal = func() bool { return false }
	if code, err := ssh.Run(ctx, []string{"enroll", "yk-new"}); code != 1 || err == nil || err.Error() != "signing needs your terminal: run geas ssh yourself" {
		t.Fatal(code, err)
	}
	ssh.Terminal = func() bool { return true }
	if code, err := ssh.Run(ctx, []string{"enroll", "YK New"}); code != 0 || err != nil {
		t.Fatal(code, err, errs)
	}
	dir, _ := caDir()
	handle := filepath.Join(dir, "yk-new")
	if want := "serial: 4242\npublic key: sk-ssh-ed25519@openssh.com AAAAGnNrLXNzaC1lZDI1NTE5QG9wZW5zc2guY29t\nhandle: " + handle + "\n"; out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
	if errs.String() != "Creating the agent CA (ed25519-sk) on yk-new (serial 4242): enter its FIDO2 PIN, then touch it.\n" {
		t.Fatal(errs.String())
	}
	args, _ := os.ReadFile(log)
	if want := strings.Join([]string{"-q", "-t", "ed25519-sk", "-O", "verify-required", "-O", "application=ssh:agent-ca", "-N", "", "-C", "Agent CA on yk-new", "-f", handle}, "\n") + "\n"; string(args) != want {
		t.Fatalf("ssh-keygen %q", args)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0700 {
		t.Fatal(info, err)
	}
	if code, err := ssh.Run(ctx, []string{"enroll", "yk-new"}); code != 1 || err == nil || err.Error() != handle+" already exists" {
		t.Fatal(code, err)
	}
	writeUserCAs(t, "4242 yk-new "+handle+"\n")
	if code, err := ssh.Run(ctx, []string{"enroll", "yk-other"}); code != 1 || err == nil || err.Error() != "the YubiKey with serial 4242 is already an agent CA" {
		t.Fatal(code, err)
	}
	ykman(t, "1", "2")
	if code, err := ssh.Run(ctx, []string{"enroll", "yk-other"}); code != 1 || err == nil || err.Error() != "plug in exactly one YubiKey to enrol (found: 1 2)" {
		t.Fatal(code, err)
	}
}

// The real ssh-keygen is what the other tests sign with.
func TestMain(m *testing.M) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		panic("the tests need ssh-keygen on PATH")
	}
	os.Exit(m.Run())
}
