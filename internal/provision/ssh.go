package provision

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/vefgh/botchecker/internal/settings"
)

// A floating IP is routed to the server but not answered by it. Hetzner leaves
// the address off the interface, so until something inside the machine adds it,
// every probe reads as a dead host — which looks exactly like a blocked address
// and would get a perfectly good IP thrown in the ledger.
//
// This runs the two lines that fix it, when a key is configured. When one is
// not, the same script is sent to Telegram for a person to run, and nothing is
// concluded about the address until they say they have.

// floatingIPScript is what has to run on the machine. The first line makes the
// address answer now; the file makes it survive a reboot.
//
// Written as a netplan drop-in rather than edited into the existing file: the
// Hetzner images generate 50-cloud-init.yaml and will overwrite it, while a
// higher-numbered file beside it is merged and left alone.
func floatingIPScript(address string) string {
	return fmt.Sprintf(`set -e
ip addr add %[1]s/32 dev eth0 2>/dev/null || true
cat >/etc/netplan/60-botchecker-floating-ip.yaml <<'YAML'
network:
  version: 2
  ethernets:
    eth0:
      addresses:
        - %[1]s/32
YAML
chmod 600 /etc/netplan/60-botchecker-floating-ip.yaml
netplan apply 2>/dev/null || true
ip -4 addr show dev eth0 | grep -q %[1]s && echo BOTCHECKER_OK`, address)
}

// sshConfigured reports whether the service can do this itself.
func (m *Manager) sshConfigured() bool {
	return strings.TrimSpace(m.set.Get(settings.HetznerSSHPrivateKey)) != ""
}

// configureFloatingIP adds the address to the machine's interface over SSH.
//
// The host key is not checked. That is a deliberate and narrow choice: these
// are machines this service created minutes ago from its own snapshot, there is
// no stored key to compare against, and the only thing sent over the connection
// is a public IP address that is already visible in the Hetzner console. It is
// noted here rather than hidden because the same shortcut would not be
// acceptable for anything carrying a secret.
func (m *Manager) configureFloatingIP(ctx context.Context, host, address string) error {
	key := strings.TrimSpace(m.set.Get(settings.HetznerSSHPrivateKey))
	if key == "" {
		return fmt.Errorf("no SSH private key is configured")
	}
	signer, err := ssh.ParsePrivateKey([]byte(key))
	if err != nil {
		return fmt.Errorf("the configured SSH private key could not be read: %w", err)
	}

	user := strings.TrimSpace(m.set.Get(settings.HetznerSSHUser))
	if user == "" {
		user = "root"
	}

	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         20 * time.Second,
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, "22"))
	if err != nil {
		return fmt.Errorf("could not reach %s on port 22: %w", host, err)
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, net.JoinHostPort(host, "22"), cfg)
	if err != nil {
		conn.Close()
		return fmt.Errorf("ssh handshake with %s failed: %w", host, err)
	}
	client := ssh.NewClient(c, chans, reqs)
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()

	out, err := sess.CombinedOutput(floatingIPScript(address))
	if err != nil {
		return fmt.Errorf("configuring %s on %s failed: %w (%s)",
			address, host, err, strings.TrimSpace(string(out)))
	}
	// The script prints this only after confirming the address is actually on
	// the interface. Without the check a silent failure would be read as
	// success, and the address would then be blamed for it.
	if !strings.Contains(string(out), "BOTCHECKER_OK") {
		return fmt.Errorf("%s did not appear on eth0 of %s (%s)",
			address, host, strings.TrimSpace(string(out)))
	}
	m.log.Info("floating address configured on the machine", "server", host, "address", address)
	return nil
}
