// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package dns

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"inet.af/netaddr"
	"tailscale.com/types/logger"
	"tailscale.com/util/dnsname"
)

const (
	backupConf = "/etc/resolv.pre-tailscale-backup.conf"
	resolvConf = "/etc/resolv.conf"
)

// writeResolvConf writes DNS configuration in resolv.conf format to the given writer.
func writeResolvConf(w io.Writer, servers []netaddr.IP, domains []dnsname.FQDN) {
	io.WriteString(w, "# resolv.conf(5) file generated by tailscale\n")
	io.WriteString(w, "# DO NOT EDIT THIS FILE BY HAND -- CHANGES WILL BE OVERWRITTEN\n\n")
	for _, ns := range servers {
		io.WriteString(w, "nameserver ")
		io.WriteString(w, ns.String())
		io.WriteString(w, "\n")
	}
	if len(domains) > 0 {
		io.WriteString(w, "search")
		for _, domain := range domains {
			io.WriteString(w, " ")
			io.WriteString(w, domain.WithoutTrailingDot())
		}
		io.WriteString(w, "\n")
	}
}

func readResolv(r io.Reader) (config OSConfig, err error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		i := strings.IndexByte(line, '#')
		if i >= 0 {
			line = line[:i]
		}

		if strings.HasPrefix(line, "nameserver") {
			s := strings.TrimPrefix(line, "nameserver")
			nameserver := strings.TrimSpace(s)
			if len(nameserver) == len(s) {
				return OSConfig{}, fmt.Errorf("missing space after \"nameserver\" in %q", line)
			}
			ip, err := netaddr.ParseIP(nameserver)
			if err != nil {
				return OSConfig{}, err
			}
			config.Nameservers = append(config.Nameservers, ip)
			continue
		}

		if strings.HasPrefix(line, "search") {
			s := strings.TrimPrefix(line, "search")
			domain := strings.TrimSpace(s)
			if len(domain) == len(s) {
				// No leading space?!
				return OSConfig{}, fmt.Errorf("missing space after \"domain\" in %q", line)
			}
			fqdn, err := dnsname.ToFQDN(domain)
			if err != nil {
				return OSConfig{}, fmt.Errorf("parsing search domains %q: %w", line, err)
			}
			config.SearchDomains = append(config.SearchDomains, fqdn)
			continue
		}
	}

	return config, nil
}

// resolvOwner returns the apparent owner of the resolv.conf
// configuration in bs - one of "resolvconf", "systemd-resolved" or
// "NetworkManager", or "" if no known owner was found.
func resolvOwner(bs []byte) string {
	likely := ""
	b := bytes.NewBuffer(bs)
	for {
		line, err := b.ReadString('\n')
		if err != nil {
			return likely
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line[0] != '#' {
			// First non-empty, non-comment line. Assume the owner
			// isn't hiding further down.
			return likely
		}

		if strings.Contains(line, "systemd-resolved") {
			likely = "systemd-resolved"
		} else if strings.Contains(line, "NetworkManager") {
			likely = "NetworkManager"
		} else if strings.Contains(line, "resolvconf") {
			likely = "resolvconf"
		}
	}
}

// isResolvedRunning reports whether systemd-resolved is running on the system,
// even if it is not managing the system DNS settings.
func isResolvedRunning() bool {
	if runtime.GOOS != "linux" {
		return false
	}

	// systemd-resolved is never installed without systemd.
	_, err := exec.LookPath("systemctl")
	if err != nil {
		return false
	}

	// is-active exits with code 3 if the service is not active.
	err = exec.Command("systemctl", "is-active", "systemd-resolved.service").Run()

	return err == nil
}

// directManager is an OSConfigurator which replaces /etc/resolv.conf with a file
// generated from the given configuration, creating a backup of its old state.
//
// This way of configuring DNS is precarious, since it does not react
// to the disappearance of the Tailscale interface.
// The caller must call Down before program shutdown
// or as cleanup if the program terminates unexpectedly.
type directManager struct {
	logf logger.Logf
	fs   wholeFileFS
	// renameBroken is set if fs.Rename to or from /etc/resolv.conf
	// fails. This can happen in some container runtimes, where
	// /etc/resolv.conf is bind-mounted from outside the container,
	// and therefore /etc and /etc/resolv.conf are different
	// filesystems as far as rename(2) is concerned.
	//
	// In those situations, we fall back to emulating rename with file
	// copies and truncations, which is not as good (opens up a race
	// where a reader can see an empty or partial /etc/resolv.conf),
	// but is better than having non-functioning DNS.
	renameBroken bool
}

func newDirectManager(logf logger.Logf) *directManager {
	return &directManager{
		logf: logf,
		fs:   directFS{},
	}
}

func newDirectManagerOnFS(logf logger.Logf, fs wholeFileFS) *directManager {
	return &directManager{
		logf: logf,
		fs:   fs,
	}
}

func (m *directManager) readResolvFile(path string) (OSConfig, error) {
	b, err := m.fs.ReadFile(path)
	if err != nil {
		return OSConfig{}, err
	}
	return readResolv(bytes.NewReader(b))
}

// ownedByTailscale reports whether /etc/resolv.conf seems to be a
// tailscale-managed file.
func (m *directManager) ownedByTailscale() (bool, error) {
	isRegular, err := m.fs.Stat(resolvConf)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !isRegular {
		return false, nil
	}
	bs, err := m.fs.ReadFile(resolvConf)
	if err != nil {
		return false, err
	}
	if bytes.Contains(bs, []byte("generated by tailscale")) {
		return true, nil
	}
	return false, nil
}

// backupConfig creates or updates a backup of /etc/resolv.conf, if
// resolv.conf does not currently contain a Tailscale-managed config.
func (m *directManager) backupConfig() error {
	if _, err := m.fs.Stat(resolvConf); err != nil {
		if os.IsNotExist(err) {
			// No resolv.conf, nothing to back up. Also get rid of any
			// existing backup file, to avoid restoring something old.
			m.fs.Remove(backupConf)
			return nil
		}
		return err
	}

	owned, err := m.ownedByTailscale()
	if err != nil {
		return err
	}
	if owned {
		return nil
	}

	return m.rename(resolvConf, backupConf)
}

func (m *directManager) restoreBackup() (restored bool, err error) {
	if _, err := m.fs.Stat(backupConf); err != nil {
		if os.IsNotExist(err) {
			// No backup, nothing we can do.
			return false, nil
		}
		return false, err
	}
	owned, err := m.ownedByTailscale()
	if err != nil {
		return false, err
	}
	_, err = m.fs.Stat(resolvConf)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	resolvConfExists := !os.IsNotExist(err)

	if resolvConfExists && !owned {
		// There's already a non-tailscale config in place, get rid of
		// our backup.
		m.fs.Remove(backupConf)
		return false, nil
	}

	// We own resolv.conf, and a backup exists.
	if err := m.rename(backupConf, resolvConf); err != nil {
		return false, err
	}

	return true, nil
}

// rename tries to rename old to new using m.fs.Rename, and falls back
// to hand-copying bytes and truncating old if that fails.
//
// This is a workaround to /etc/resolv.conf being a bind-mounted file
// some container environments, which cannot be moved elsewhere in
// /etc (because that would be a cross-filesystem move) or deleted
// (because that would break the bind in surprising ways).
func (m *directManager) rename(old, new string) error {
	if !m.renameBroken {
		err := m.fs.Rename(old, new)
		if err == nil {
			return nil
		}
		m.logf("rename of %q to %q failed (%v), falling back to copy+delete", old, new, err)
		m.renameBroken = true
	}

	bs, err := m.fs.ReadFile(old)
	if err != nil {
		return fmt.Errorf("reading %q to rename: %v", old, err)
	}
	if err := m.fs.WriteFile(new, bs, 0644); err != nil {
		return fmt.Errorf("writing to %q in rename of %q: %v", new, old, err)
	}

	if err := m.fs.Remove(old); err != nil {
		err2 := m.fs.Truncate(old)
		if err2 != nil {
			return fmt.Errorf("remove of %q failed (%v) and so did truncate: %v", old, err, err2)
		}
	}
	return nil
}

func (m *directManager) SetDNS(config OSConfig) (err error) {
	var changed bool
	if config.IsZero() {
		changed, err = m.restoreBackup()
		if err != nil {
			return err
		}
	} else {
		changed = true
		if err := m.backupConfig(); err != nil {
			return err
		}

		buf := new(bytes.Buffer)
		writeResolvConf(buf, config.Nameservers, config.SearchDomains)
		if err := m.atomicWriteFile(m.fs, resolvConf, buf.Bytes(), 0644); err != nil {
			return err
		}
	}

	// We might have taken over a configuration managed by resolved,
	// in which case it will notice this on restart and gracefully
	// start using our configuration. This shouldn't happen because we
	// try to manage DNS through resolved when it's around, but as a
	// best-effort fallback if we messed up the detection, try to
	// restart resolved to make the system configuration consistent.
	//
	// We take care to only kick systemd-resolved if we've made some
	// change to the system's DNS configuration, because this codepath
	// can end up running in cases where the user has manually
	// configured /etc/resolv.conf to point to systemd-resolved (but
	// it's not managed explicitly by systemd-resolved), *and* has
	// --accept-dns=false, meaning we pass an empty configuration to
	// the running DNS manager. In that very edge-case scenario, we
	// cause a disruptive DNS outage each time we reset an empty
	// OS configuration.
	if changed && isResolvedRunning() && !runningAsGUIDesktopUser() {
		exec.Command("systemctl", "restart", "systemd-resolved.service").Run()
	}

	return nil
}

func (m *directManager) SupportsSplitDNS() bool {
	return false
}

func (m *directManager) GetBaseConfig() (OSConfig, error) {
	owned, err := m.ownedByTailscale()
	if err != nil {
		return OSConfig{}, err
	}
	fileToRead := resolvConf
	if owned {
		fileToRead = backupConf
	}

	return m.readResolvFile(fileToRead)
}

func (m *directManager) Close() error {
	// We used to keep a file for the tailscale config and symlinked
	// to it, but then we stopped because /etc/resolv.conf being a
	// symlink to surprising places breaks snaps and other sandboxing
	// things. Clean it up if it's still there.
	m.fs.Remove("/etc/resolv.tailscale.conf")

	if _, err := m.fs.Stat(backupConf); err != nil {
		if os.IsNotExist(err) {
			// No backup, nothing we can do.
			return nil
		}
		return err
	}
	owned, err := m.ownedByTailscale()
	if err != nil {
		return err
	}
	_, err = m.fs.Stat(resolvConf)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	resolvConfExists := !os.IsNotExist(err)

	if resolvConfExists && !owned {
		// There's already a non-tailscale config in place, get rid of
		// our backup.
		m.fs.Remove(backupConf)
		return nil
	}

	// We own resolv.conf, and a backup exists.
	if err := m.rename(backupConf, resolvConf); err != nil {
		return err
	}

	if isResolvedRunning() && !runningAsGUIDesktopUser() {
		exec.Command("systemctl", "restart", "systemd-resolved.service").Run() // Best-effort.
	}

	return nil
}

func (m *directManager) atomicWriteFile(fs wholeFileFS, filename string, data []byte, perm os.FileMode) error {
	var randBytes [12]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		return fmt.Errorf("atomicWriteFile: %w", err)
	}

	tmpName := fmt.Sprintf("%s.%x.tmp", filename, randBytes[:])
	defer fs.Remove(tmpName)

	if err := fs.WriteFile(tmpName, data, perm); err != nil {
		return fmt.Errorf("atomicWriteFile: %w", err)
	}
	return m.rename(tmpName, filename)
}

// wholeFileFS is a high-level file system abstraction designed just for use
// by directManager, with the goal that it is easy to implement over wsl.exe.
//
// All name parameters are absolute paths.
type wholeFileFS interface {
	Stat(name string) (isRegular bool, err error)
	Rename(oldName, newName string) error
	Remove(name string) error
	ReadFile(name string) ([]byte, error)
	Truncate(name string) error
	WriteFile(name string, contents []byte, perm os.FileMode) error
}

// directFS is a wholeFileFS implemented directly on the OS.
type directFS struct {
	// prefix is file path prefix.
	//
	// All name parameters are absolute paths so this is typically a
	// testing temporary directory like "/tmp".
	prefix string
}

func (fs directFS) path(name string) string { return filepath.Join(fs.prefix, name) }

func (fs directFS) Stat(name string) (isRegular bool, err error) {
	fi, err := os.Stat(fs.path(name))
	if err != nil {
		return false, err
	}
	return fi.Mode().IsRegular(), nil
}

func (fs directFS) Rename(oldName, newName string) error {
	return os.Rename(fs.path(oldName), fs.path(newName))
}

func (fs directFS) Remove(name string) error { return os.Remove(fs.path(name)) }

func (fs directFS) ReadFile(name string) ([]byte, error) {
	return ioutil.ReadFile(fs.path(name))
}

func (fs directFS) Truncate(name string) error {
	return os.Truncate(fs.path(name), 0)
}

func (fs directFS) WriteFile(name string, contents []byte, perm os.FileMode) error {
	return ioutil.WriteFile(fs.path(name), contents, perm)
}

// runningAsGUIDesktopUser reports whether it seems that this code is
// being run as a regular user on a Linux desktop. This is a quick
// hack to fix Issue 2672 where PolicyKit pops up a GUI dialog asking
// to proceed we do a best effort attempt to restart
// systemd-resolved.service. There's surely a better way.
func runningAsGUIDesktopUser() bool {
	return os.Getuid() != 0 && os.Getenv("DISPLAY") != ""
}
