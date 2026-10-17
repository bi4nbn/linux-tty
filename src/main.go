// linux-tty: console menu for headless Linux appliances.
// Single static binary, no external deps. Runs on RHEL/CentOS/Rocky/Alma and
// Debian/Ubuntu x86_64.
//
// Invoked by systemd on a physical tty, it shows the interactive console menu.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// ---------- globals ----------

var (
	rel         = "SSCLOUD Linux-TTY Console"
	myTTY       string // e.g. tty1 / ttyS0
	interrupt   atomic.Int32
	nmActive    atomic.Int32
	stdinIsTty  = false
	eofFlag     atomic.Bool
	origTermios *termios
	origSysrq   []byte
	netBackend  = "rhel" // "rhel" = ifcfg+nmcli, "debian" = /etc/network/interfaces, "netplan" = /etc/netplan
)

const (
	ifcfgDir    = "/etc/sysconfig/network-scripts"
	bakDir      = "/var/backups/netcfg"
	maxTries    = 3
	interfacesF = "/etc/network/interfaces"
)

// version is overridden at build time via -ldflags "-X main.version=vX.Y.Z"
var version = "v1.0.0"

// ---------- termios ----------

// Linux x86_64 struct termios: tcflag_t is uint32, not uintptr.
type termios struct {
	Iflag, Oflag, Cflag, Lflag uint32
	Line                       byte
	Cc                         [32]byte
	Ispeed, Ospeed             uint32
}

func tcGet(fd int) (*termios, error) {
	t := &termios{}
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(t)))
	if e != 0 {
		return nil, e
	}
	return t, nil
}

func tcSet(fd int, t *termios) error {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(t)))
	if e != 0 {
		return e
	}
	return nil
}

// enterCbreak puts stdin into cbreak mode: no ICANON (char-by-char), no ECHO
// (ask() does its own echoing), VMIN=0 + VTIME=2 so Read() returns after 200ms
// even with no input. Saves the original termios on first call.
func enterCbreak() {
	if !stdinIsTty {
		return
	}
	fd := int(os.Stdin.Fd())
	if origTermios == nil {
		t, err := tcGet(fd)
		if err != nil {
			return
		}
		origTermios = t
	}
	t := *origTermios
	t.Lflag &^= syscall.ICANON
	t.Lflag &^= syscall.ECHO
	t.Cc[5] = 2 // VTIME = 2 deciseconds = 200ms
	t.Cc[6] = 0 // VMIN = 0
	tcSet(fd, &t)
}

// exitCbreak restores the original termios.
func exitCbreak() {
	if stdinIsTty && origTermios != nil {
		tcSet(int(os.Stdin.Fd()), origTermios)
	}
}

// ---------- small helpers ----------

func run(name string, args ...string) (string, error) {
	c := exec.Command(name, args...)
	var b bytes.Buffer
	c.Stdout = &b
	c.Stderr = &b
	err := c.Run()
	return strings.TrimSpace(b.String()), err
}

// runBg runs a command and streams its stdout to our stdout (used by ping).
func runBg(name string, args ...string) int {
	c := exec.Command(name, args...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	err := c.Run()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return 1
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func readFileTrim(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ---------- signal handling / cleanup ----------

// disableSysrq remembers the original sysrq value (once) and turns sysrq off
// while the menu is in control. restoreSysrq writes the saved value back.
func disableSysrq() {
	if orig, err := os.ReadFile("/proc/sys/kernel/sysrq"); err == nil {
		if origSysrq == nil {
			origSysrq = orig
		}
		os.WriteFile("/proc/sys/kernel/sysrq", []byte("0"), 0644)
	}
}

func restoreSysrq() {
	if origSysrq != nil {
		os.WriteFile("/proc/sys/kernel/sysrq", origSysrq, 0644)
	}
}

// cleanup restores the terminal and sysrq. Signal handlers must use this
// instead of os.Exit after only exitCbreak: os.Exit skips deferred calls.
func cleanup() {
	exitCbreak()
	restoreSysrq()
}

func initSignals() {
	ch := make(chan os.Signal, 16)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for s := range ch {
			if s == os.Interrupt {
				interrupt.Store(1)
			} else if s == syscall.SIGTERM {
				cleanup()
				os.Exit(143)
			} else if s == syscall.SIGHUP {
				cleanup()
				os.Exit(0)
			}
		}
	}()
}

// ---------- tty / getty ----------

func detectTTY() {
	out := ""
	// Preferred: readlink our stdin. run("tty") would report "not a tty"
	// because the child's stdin defaults to /dev/null, not our console.
	if link, err := os.Readlink("/proc/self/fd/0"); err == nil {
		out = link
	}
	if !strings.HasPrefix(out, "/dev/") {
		c := exec.Command("tty")
		c.Stdin = os.Stdin
		var b bytes.Buffer
		c.Stdout = &b
		c.Stderr = &b
		c.Run()
		out = strings.TrimSpace(b.String())
	}
	myTTY = strings.TrimPrefix(out, "/dev/")
}

// ---------- migrate old ifcfg backups ----------

var bakNameRe = regexp.MustCompile(`^\.[0-9]{8}_[0-9]{6}$`)

func migrateIfcfgBackups() {
	os.MkdirAll(bakDir, 0700)
	// migrate /root/ifcfg-backup
	if entries, err := os.ReadDir("/root/ifcfg-backup"); err == nil {
		for _, e := range entries {
			os.Rename("/root/ifcfg-backup/"+e.Name(), filepath.Join(bakDir, e.Name()))
		}
		os.RemoveAll("/root/ifcfg-backup")
	}
	entries, err := os.ReadDir(ifcfgDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "ifcfg-") {
			continue
		}
		if name == "ifcfg-lo" {
			continue
		}
		// keep VLAN names like ifcfg-eth0.100, keep files ending in .bak/.old etc (those are already ignored by initscripts)
		switch {
		case strings.HasSuffix(name, "~"),
			strings.HasSuffix(name, ".bak"),
			strings.HasSuffix(name, ".old"),
			strings.HasSuffix(name, ".orig"),
			strings.HasSuffix(name, ".rpmnew"),
			strings.HasSuffix(name, ".rpmorig"),
			strings.HasSuffix(name, ".rpmsave"):
			// these are properly ignored; leave them
		default:
			// timestamped backup suffix: .YYYYMMDD_HHMMSS
			dot := strings.LastIndex(name, ".")
			if dot > 0 && bakNameRe.MatchString(name[dot:]) {
				os.Rename(filepath.Join(ifcfgDir, name), filepath.Join(bakDir, name))
			}
		}
	}
}

// ---------- ask(): char-by-char line editor ----------

// returns (value, interrupted)
func ask(prompt string) (string, bool) {
	fmt.Print(prompt)
	var buf []byte
	readBuf := make([]byte, 1)
	for {
		if eofFlag.Load() {
			return "", false
		}
		if interrupt.Load() == 1 {
			return "", true
		}
		n, err := os.Stdin.Read(readBuf)
		if err != nil {
			// On a tty, never exit on EOF/error - Ctrl-D or transient errors
			// should just return to the menu loop, not kill the process.
			// Only exit when stdin is a pipe and the writer closed it.
			if !stdinIsTty && err == io.EOF {
				eofFlag.Store(true)
				return "", false
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if n == 0 {
			// On a real tty with VMIN=0/VTIME=2 this is just a 200ms poll.
			// A non-tty stdin (e.g. /dev/null) returns (0, nil) immediately;
			// treat that as EOF instead of spinning at 100% CPU.
			if !stdinIsTty {
				eofFlag.Store(true)
				return "", false
			}
			continue
		}
		c := readBuf[0]
		switch c {
		case '\n', '\r':
			fmt.Println()
			return string(buf), false
		case 0x03: // Ctrl+C
			fmt.Println()
			interrupt.Store(1)
			return "", true
		case 0x1b: // ESC or CSI escape sequence (arrow/Home/End/Delete/PgUp/PgDn)
			peek := make([]byte, 1)
			pn, _ := os.Stdin.Read(peek)
			if pn == 1 && peek[0] == '[' {
				// Consume the rest of the CSI sequence until a final byte
				// (a letter A-Z or '~'). VMIN=0 means Read returns quickly.
				for {
					b := make([]byte, 1)
					bn, _ := os.Stdin.Read(b)
					if bn != 1 {
						break
					}
					c := b[0]
					if (c >= 'A' && c <= 'Z') || c == '~' {
						break
					}
				}
				continue // CSI sequence discarded
			}
			// Bare ESC: treat as interrupt.
			fmt.Println()
			interrupt.Store(1)
			return "", true
		case 0x7f, 0x08: // ^? and ^H
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
				fmt.Print("\b \b")
			}
		default:
			if c >= 0x20 {
				buf = append(buf, c)
				fmt.Printf("%c", c)
			}
		}
	}
}

// isVTConsole reports whether tty is a virtual console (tty1, tty2, ...),
// excluding serial (ttyS*, ttyAMA*) and pseudo-terminals.
func isVTConsole(tty string) bool {
	if !strings.HasPrefix(tty, "tty") {
		return false
	}
	rest := tty[3:]
	if rest == "" {
		return false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// validTTYName accepts names that are safe to embed in a systemd instance
// name (tty1, ttyS0, hvc0, ...). Pseudo-terminals contain '/' and are
// rejected; the update path refuses to guess which unit to restart.
func validTTYName(tty string) bool {
	if tty == "" {
		return false
	}
	for _, r := range tty {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '.' || r == '-':
		default:
			return false
		}
	}
	return true
}

func pause() {
	fmt.Println()
	ask("Press Enter to return to the menu... ")
}

func confirm(prompt string) bool {
	a, intr := ask(prompt + " [y/N]: ")
	if intr {
		return false
	}
	a = strings.TrimSpace(a)
	return a == "y" || a == "Y" || a == "yes" || a == "YES"
}

// ---------- NIC helpers ----------

func listNics() []string {
	out, _ := run("ip", "-o", "link", "show")
	var nics []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		// 1: lo: <LOOPBACK,...>
		parts := strings.SplitN(line, ": ", 3)
		if len(parts) < 3 {
			continue
		}
		name := strings.SplitN(parts[1], "@", 2)[0]
		name = strings.TrimSpace(name)
		if name == "" || name == "lo" {
			continue
		}
		if matchRe(name, `^(docker|veth|virbr|br-|tun|tap|vnet|kube|cali|flannel|cni)`) {
			continue
		}
		if !seen[name] {
			seen[name] = true
			nics = append(nics, name)
		}
	}
	return nics
}

func matchRe(s, pat string) bool {
	ok, _ := regexp.MatchString(pat, s)
	return ok
}

func nicOperstate(nic string) string {
	return readFileTrim("/sys/class/net/" + nic + "/operstate")
}

// detectDistro reads /etc/os-release and sets netBackend.
// RHEL family (centos/rhel/rocky/almalinux/fedora) -> "rhel"
// Debian family (debian/ubuntu/kali) -> "debian" or "netplan"
//
// A netplan backend is only selected when /etc/netplan contains a YAML with
// an actual interface block (ethernets/bonds/bridges/vlans). Stub files with
// only "version: 2" are ignored, avoiding a conflict with traditional
// /etc/network/interfaces on Debian/Ubuntu systems that ship both.
func detectDistro() {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return
	}
	id := ""
	idLike := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ID=") {
			id = strings.Trim(strings.TrimPrefix(line, "ID="), `"`)
		}
		if strings.HasPrefix(line, "ID_LIKE=") {
			idLike = strings.Trim(strings.TrimPrefix(line, "ID_LIKE="), `"`)
		}
	}
	combined := strings.ToLower(id + " " + idLike)
	// Debian family
	for _, d := range []string{"debian", "ubuntu", "kali"} {
		if strings.Contains(combined, d) {
			netBackend = "debian"
			if entries, err := os.ReadDir("/etc/netplan"); err == nil {
				for _, e := range entries {
					name := e.Name()
					if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
						continue
					}
					b, err := os.ReadFile(filepath.Join("/etc/netplan", name))
					if err != nil {
						continue
					}
					text := string(b)
					if strings.Contains(text, "ethernets:") ||
						strings.Contains(text, "bonds:") ||
						strings.Contains(text, "bridges:") ||
						strings.Contains(text, "vlans:") {
						netBackend = "netplan"
						return
					}
				}
			}
			return
		}
	}
	// RHEL family is default
	netBackend = "rhel"
}

// ---------- shared config helpers ----------

// writeFileAtomic writes content to path via a same-directory temp file and
// rename, so a failed/partial write never leaves a half-written ifcfg or
// netplan file behind.
func writeFileAtomic(path, content string, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".linux-tty-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// usableNameserver rejects entries that cannot be persisted as an upstream
// resolver. The systemd-resolved stub (127.0.0.53 / ::1) only works while the
// local resolver is running and must not be copied into a static config.
// Other loopback addresses (e.g. a deliberately configured local dnsmasq) are
// kept.
func usableNameserver(s string) bool {
	s = strings.Trim(strings.TrimSpace(s), `"`)
	if s == "" {
		return false
	}
	ip := net.ParseIP(s)
	if ip == nil || ip.IsUnspecified() {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil && ip4.Equal(net.IPv4(127, 0, 0, 53)) {
		return false
	}
	if ip.Equal(net.IPv6loopback) {
		return false
	}
	return true
}

func parseResolvConf(data string) []string {
	var out []string
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "nameserver" && usableNameserver(f[1]) {
			out = append(out, strings.Trim(f[1], `"`))
		}
	}
	return out
}

// readResolvConfDNS returns real upstream nameservers. When /etc/resolv.conf
// only contains the systemd-resolved stub, the upstream file written by
// resolved is used as a fallback.
func readResolvConfDNS() []string {
	var out []string
	for _, path := range []string{"/etc/resolv.conf", "/run/systemd/resolve/resolv.conf"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		out = append(out, parseResolvConf(string(data))...)
		if len(out) > 0 {
			break
		}
	}
	return dedupeStrings(out)
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

var dnsLineRe = regexp.MustCompile(`(?i)^DNS[0-9]+=(\S+)$`)

var ifcfgBadKeys = regexp.MustCompile(`(?im)^\s*(SLAVE|MASTER|BONDING_OPTS|BONDING_SLAVE|VLAN|VLAN_ID|BRIDGE|BRIDGE_MASTER|TEAM_MASTER|TEAM_PORT_CONFIG|OVS_BRIDGE|OVS_PORT|OVS_OPTIONS)\s*=`)

var ifcfgBadType = regexp.MustCompile(`(?im)^\s*TYPE\s*=\s*"?(Vlan|Bridge|Bond|Team|IPSEC|IPIP|GRE|OVSPort|OVSBridge|OVSIntPort|OVSBond)\b`)

func ifcfgSpecialReason(text string) string {
	if ifcfgBadKeys.MatchString(text) {
		return "bond/VLAN/bridge/team/OVS member"
	}
	if ifcfgBadType.MatchString(text) {
		return "bond/VLAN/bridge/team/OVS interface"
	}
	return ""
}

// ifcfgManagedKey reports whether a key is owned by this menu and must be
// dropped from the old file before merging. Everything else (UUID, HWADDR,
// MTU, ETHTOOL_OPTS, IPV6*, ...) is preserved.
func ifcfgManagedKey(key string, dhcp bool) bool {
	key = strings.ToUpper(strings.TrimSpace(key))
	switch key {
	case "TYPE", "BOOTPROTO", "NAME", "DEVICE", "ONBOOT", "NM_CONTROLLED", "PEERDNS":
		return true
	}
	for _, prefix := range []string{"IPADDR", "NETMASK", "PREFIX", "GATEWAY", "DNS"} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	if dhcp {
		switch key {
		case "DEFROUTE", "IPV4_FAILURE_FATAL":
			return true
		}
	}
	return false
}

func mergeIfcfg(old, base string, dhcp bool) string {
	if strings.TrimSpace(old) == "" {
		return base
	}
	var extras []string
	for _, line := range strings.Split(strings.ReplaceAll(old, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key := trimmed
		if i := strings.Index(key, "="); i >= 0 {
			key = key[:i]
		}
		if ifcfgManagedKey(key, dhcp) {
			continue
		}
		extras = append(extras, line)
	}
	if len(extras) == 0 {
		return base
	}
	return strings.TrimRight(base, "\n") + "\n" + strings.Join(extras, "\n") + "\n"
}

// interfacesSpecialReason detects bond/bridge/VLAN/team/OVS stanzas in
// /etc/network/interfaces, both for the master and for a member port.
func interfacesSpecialReason(text, nic string) string {
	cur := ""
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "iface":
			if len(f) >= 2 {
				cur = f[1]
			} else {
				cur = ""
			}
			continue
		case "auto", "allow-hotplug", "source", "mapping", "rename":
			cur = ""
			continue
		}
		if cur == nic {
			switch f[0] {
			case "bridge_ports", "bridge-stp", "bridge-vlan-aware", "bond-slaves",
				"bond-master", "bond-mode", "vlan-raw-device", "team-ports",
				"ovs_type", "ovs_bridge", "ovs_ports":
				return "bond/VLAN/bridge/team/OVS configuration"
			}
		}
		if len(f) >= 2 {
			switch f[0] {
			case "bridge_ports", "bond-slaves", "team-ports", "ovs_ports":
				for _, x := range f[1:] {
					if x == nic {
						return "member of another interface"
					}
				}
			case "vlan-raw-device":
				if f[1] == nic {
					return "VLAN parent"
				}
			}
		}
	}
	return ""
}

func yamlListHas(s, nic string) bool {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == '[' || r == ']' || r == ' ' || r == '\t'
	}) {
		if strings.Trim(f, `"'`) == nic {
			return true
		}
	}
	return false
}

func netplanSpecialReasonText(content, nic string) string {
	section, device := "", ""
	for _, raw := range strings.Split(content, "\n") {
		line := raw
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		trimmed := strings.TrimSpace(line)
		if indent == 2 && strings.HasSuffix(trimmed, ":") {
			section = strings.Trim(strings.TrimSuffix(trimmed, ":"), `"'`)
			device = ""
			continue
		}
		if indent == 4 && strings.HasSuffix(trimmed, ":") {
			device = strings.Trim(strings.TrimSuffix(trimmed, ":"), `"'`)
			if device == nic {
				switch section {
				case "bonds", "bridges", "vlans", "teams":
					return section + " interface"
				}
			}
			continue
		}
		if section == "openvswitch" {
			if yamlListHas(trimmed, nic) {
				return "Open vSwitch interface"
			}
			continue
		}
		if section != "bonds" && section != "bridges" && section != "teams" {
			if section == "vlans" && device != "" && strings.HasPrefix(trimmed, "link:") {
				v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "link:")), `"'`)
				if v == nic {
					return "VLAN parent"
				}
			}
			continue
		}
		key, val := trimmed, ""
		if i := strings.Index(key, ":"); i >= 0 {
			val = strings.TrimSpace(key[i+1:])
			key = strings.TrimSpace(key[:i])
		}
		switch key {
		case "interfaces", "ports":
			if yamlListHas(val, nic) {
				return "member of " + section
			}
		case "-":
			if yamlListHas(trimmed, nic) {
				return "member of " + section
			}
		default:
			if strings.HasPrefix(trimmed, "-") && yamlListHas(trimmed, nic) {
				return "member of " + section
			}
		}
	}
	return ""
}

// netplanRendererFor returns the renderer already used for nic, or the global
// renderer of the newest netplan file. This prevents switching an Ubuntu
// NetworkManager system to networkd just because we wrote a 99-*.yaml file.
func scanNetplanRenderer(content, nic string) (global, perNIC string) {
	device := ""
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimRight(raw, "\r")
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		trimmed := strings.TrimSpace(line)
		if indent == 4 && strings.HasSuffix(trimmed, ":") {
			device = strings.Trim(strings.TrimSuffix(trimmed, ":"), `"'`)
			continue
		}
		if !strings.HasPrefix(trimmed, "renderer:") {
			continue
		}
		val := strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "renderer:")), `"'`)
		if val == "" {
			continue
		}
		if indent <= 2 {
			global = val
		} else if indent >= 6 && device == nic {
			perNIC = val
		}
	}
	return
}

func normalizeNetplanRenderer(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "networkmanager":
		return "NetworkManager"
	case "networkd":
		return "networkd"
	}
	return ""
}

func netplanRendererFor(nic string) string {
	entries, err := os.ReadDir("/etc/netplan")
	if err != nil {
		return ""
	}
	global, perNIC := "", ""
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || (!strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml")) {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/etc/netplan", name))
		if err != nil {
			continue
		}
		g, p := scanNetplanRenderer(string(data), nic)
		if g != "" {
			global = g
		}
		if p != "" {
			perNIC = p
		}
	}
	if perNIC != "" {
		return normalizeNetplanRenderer(perNIC)
	}
	return normalizeNetplanRenderer(global)
}

// specialNICReason refuses to rewrite stacked interfaces. The menu only knows
// how to configure plain Ethernet NICs; overwriting a bond/bridge/VLAN/team/
// OVS member destroys the stack configuration.
func specialNICReason(nic string) string {
	if nic == "" {
		return ""
	}
	base := "/sys/class/net/" + nic
	if _, err := os.Stat(base + "/bonding"); err == nil {
		return "bond master"
	}
	if _, err := os.Stat(base + "/bridge"); err == nil {
		return "bridge"
	}
	if _, err := os.Stat(base + "/team"); err == nil {
		return "team master"
	}
	if _, err := os.Stat("/proc/net/vlan/" + nic); err == nil {
		return "VLAN"
	}
	if link, err := os.Readlink(base + "/master"); err == nil {
		return "member of " + filepath.Base(link)
	}
	switch netBackend {
	case "rhel":
		if data, err := os.ReadFile(filepath.Join(ifcfgDir, "ifcfg-"+nic)); err == nil {
			if r := ifcfgSpecialReason(string(data)); r != "" {
				return r
			}
		}
	case "debian":
		paths := []string{interfacesF}
		if entries, err := os.ReadDir("/etc/network/interfaces.d"); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					paths = append(paths, filepath.Join("/etc/network/interfaces.d", e.Name()))
				}
			}
		}
		for _, path := range paths {
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			if r := interfacesSpecialReason(string(data), nic); r != "" {
				return r
			}
		}
	case "netplan":
		entries, err := os.ReadDir("/etc/netplan")
		if err != nil {
			break
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || (!strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml")) {
				continue
			}
			data, err := os.ReadFile(filepath.Join("/etc/netplan", name))
			if err != nil {
				continue
			}
			if r := netplanSpecialReasonText(string(data), nic); r != "" {
				return r
			}
		}
	}
	return ""
}

// ---------- Debian /etc/network/interfaces backend ----------

// debianBackupIface backs up the interfaces file before rewriting.
func debianBackupIface() string {
	os.MkdirAll(bakDir, 0700)
	bak := filepath.Join(bakDir, fmt.Sprintf("interfaces.%s", time.Now().Format("20060102_150405")))
	if b, err := os.ReadFile(interfacesF); err == nil {
		os.WriteFile(bak, b, 0600)
	}
	return bak
}

// debianRewriteBlock replaces (or inserts) the auto+iface block for nic in
// /etc/network/interfaces, preserving all other stanzas (lo, source, other NICs).
func debianRewriteBlock(nic, block string) error {
	var existing []string
	if data, err := os.ReadFile(interfacesF); err == nil {
		existing = strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	} else if !os.IsNotExist(err) {
		return err
	}
	// Walk existing lines, drop old auto/allow-hotplug entries mentioning nic,
	// and drop the entire iface <nic> stanza (including indented child lines).
	var kept []string
	for i := 0; i < len(existing); i++ {
		line := existing[i]
		trimmed := strings.TrimSpace(line)
		fields := strings.Fields(trimmed)
		if len(fields) >= 2 && (fields[0] == "auto" || fields[0] == "allow-hotplug") {
			var remain []string
			for _, name := range fields[1:] {
				if name != nic {
					remain = append(remain, name)
				}
			}
			if len(remain) == 0 {
				continue
			}
			kept = append(kept, fields[0]+" "+strings.Join(remain, " "))
			continue
		}
		if len(fields) >= 2 && fields[0] == "iface" && fields[1] == nic {
			// skip this stanza and all indented continuation lines
			for i+1 < len(existing) && (strings.HasPrefix(existing[i+1], " ") || strings.HasPrefix(existing[i+1], "\t")) {
				i++
			}
			continue
		}
		kept = append(kept, line)
	}
	// Ensure loopback stanza exists
	hasLo := false
	for _, l := range kept {
		if strings.HasPrefix(strings.TrimSpace(l), "iface lo ") {
			hasLo = true
			break
		}
	}
	if !hasLo {
		kept = append([]string{"auto lo", "iface lo inet loopback", ""}, kept...)
	}
	// Append new block
	kept = append(kept, "", block)
	// Collapse 3+ consecutive blank lines into one
	var out []string
	blank := false
	for _, l := range kept {
		if strings.TrimSpace(l) == "" {
			if blank || len(out) == 0 {
				continue
			}
			blank = true
			out = append(out, "")
			continue
		}
		blank = false
		out = append(out, l)
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	final := strings.Join(out, "\n") + "\n"
	return writeFileAtomic(interfacesF, final, 0644)
}

// debianStanzaExtras returns the options of the existing iface stanza for nic
// that the menu does not manage (mtu, hwaddress, up/down hooks, ...). They are
// appended to the new stanza so switching dhcp/static does not silently drop
// unrelated settings.
func debianStanzaExtras(text, nic string) []string {
	var extras []string
	inStanza := false
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "iface ") {
			f := strings.Fields(trimmed)
			inStanza = len(f) >= 2 && f[1] == nic
			continue
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inStanza = false
			continue
		}
		if !inStanza {
			continue
		}
		f := strings.Fields(trimmed)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "address", "netmask", "gateway", "dns-nameservers", "dns-search",
			"dns-domain", "broadcast", "network", "pointopoint":
			continue
		}
		extras = append(extras, line)
	}
	return extras
}

// debianWriteStatic writes a static IP stanza into /etc/network/interfaces.
func debianWriteStatic(nic, ip, netmask, gateway, dns1, dns2 string) error {
	block := "auto " + nic + "\n" +
		"iface " + nic + " inet static\n" +
		"    address " + ip + "\n" +
		"    netmask " + netmask + "\n"
	if gateway != "" {
		block += "    gateway " + gateway + "\n"
	}
	if dns1 != "" {
		block += "    dns-nameservers " + dns1
		if dns2 != "" {
			block += " " + dns2
		}
		block += "\n"
	}
	if old, err := os.ReadFile(interfacesF); err == nil {
		for _, extra := range debianStanzaExtras(string(old), nic) {
			block += extra + "\n"
		}
	}
	return debianRewriteBlock(nic, block)
}

// debianWriteDHCP writes a DHCP stanza into /etc/network/interfaces.
func debianWriteDHCP(nic string) error {
	block := "auto " + nic + "\n" +
		"iface " + nic + " inet dhcp\n"
	if old, err := os.ReadFile(interfacesF); err == nil {
		for _, extra := range debianStanzaExtras(string(old), nic) {
			block += extra + "\n"
		}
	}
	return debianRewriteBlock(nic, block)
}

// debianApply brings the NIC up/down after writing the config.
// Polls operstate to avoid a fixed sleep.
func debianApply(nic string) {
	exec.Command("ifdown", nic).Run()
	// Wait for link to go down (max ~3s)
	for i := 0; i < 15; i++ {
		st := readFileTrim("/sys/class/net/" + nic + "/operstate")
		if st == "down" || st == "unknown" || st == "" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := exec.Command("ifup", nic).Run(); err != nil {
		// ifup may be missing on newer Debian/Ubuntu (netplan era)
		exec.Command("systemctl", "restart", "networking").Run()
	}
}

// netplanConfigPath is where we write our own netplan YAML.
const netplanConfigPath = "/etc/netplan/99-linux-tty.yaml"

// netplanBackup copies the current netplan files into a timestamped backup
// directory. A directory (instead of one merged text file) makes an exact
// rollback possible.
func netplanBackup() string {
	os.MkdirAll(bakDir, 0700)
	bak := filepath.Join(bakDir, fmt.Sprintf("netplan.%s", time.Now().Format("20060102_150405")))
	if err := os.MkdirAll(bak, 0700); err != nil {
		return ""
	}
	entries, err := os.ReadDir("/etc/netplan")
	if err != nil {
		return bak
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/etc/netplan", e.Name()))
		if err != nil {
			continue
		}
		os.WriteFile(filepath.Join(bak, e.Name()), data, 0600)
	}
	return bak
}

func restoreNetplanBackup(bak string) bool {
	if bak == "" {
		return false
	}
	entries, err := os.ReadDir(bak)
	if err != nil {
		return false
	}
	// Remove our override first; if it existed before, restore it below.
	os.Remove(netplanConfigPath)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(bak, e.Name()))
		if err != nil {
			continue
		}
		if err := writeFileAtomic(filepath.Join("/etc/netplan", e.Name()), string(data), 0600); err != nil {
			return false
		}
	}
	netplanApply()
	return true
}

// netplanWriteStatic writes a static IP netplan YAML and applies it.
func netplanWriteStatic(nic, ip, netmask, gateway, dns1, dns2 string) error {
	maskIP := net.ParseIP(netmask).To4()
	if maskIP == nil {
		return fmt.Errorf("invalid netmask: %s", netmask)
	}
	ones := maskSize(maskIP)
	if ones == 0 || ones > 32 {
		return fmt.Errorf("invalid netmask prefix: %s", netmask)
	}
	renderer := netplanRendererFor(nic)
	if renderer == "" {
		renderer = "networkd"
	}
	prefix := strconv.Itoa(ones)
	yaml := "network:\n  version: 2\n  renderer: " + renderer + "\n  ethernets:\n    " + nic + ":\n" +
		"      addresses: [" + ip + "/" + prefix + "]\n"
	if gateway != "" {
		yaml += "      routes:\n        - to: 0.0.0.0/0\n          via: " + gateway + "\n          on-link: true\n"
	}
	if dns1 != "" {
		dnsList := dns1
		if dns2 != "" {
			dnsList += ", " + dns2
		}
		yaml += "      nameservers:\n        addresses: [" + dnsList + "]\n"
	}
	return writeFileAtomic(netplanConfigPath, yaml, 0600)
}

// netplanWriteDHCP writes a DHCP netplan YAML and applies it.
func netplanWriteDHCP(nic string) error {
	renderer := netplanRendererFor(nic)
	if renderer == "" {
		renderer = "networkd"
	}
	yaml := "network:\n  version: 2\n  renderer: " + renderer + "\n  ethernets:\n    " + nic + ":\n" +
		"      dhcp4: true\n"
	return writeFileAtomic(netplanConfigPath, yaml, 0600)
}

// netplanApply runs `netplan apply` and reports failures, so the operator
// sees syntax / conflict errors instead of a silent "no IPv4".
func netplanApply() {
	out, err := run("netplan", "apply")
	if err != nil {
		if out != "" {
			fmt.Printf("netplan apply: %s\n", out)
		} else {
			fmt.Printf("netplan apply failed: %v\n", err)
		}
	}
}

// ---------- network apply ----------

func isNMActive() bool {
	c := exec.Command("systemctl", "is-active", "--quiet", "NetworkManager")
	if err := c.Start(); err != nil {
		return false
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case <-done:
		return c.ProcessState.Success()
	case <-time.After(3 * time.Second):
		c.Process.Kill()
		return false
	}
}

// nicApply applies the current config for dev. For the RHEL backend it relies
// on nmActive having been pre-set by applyNic() so we don't fork systemctl
// on every retry.
func nicApply(dev string) {
	switch netBackend {
	case "debian":
		debianApply(dev)
		return
	case "netplan":
		netplanApply()
		return
	}
	if nmActive.Load() == 1 {
		exec.Command("nmcli", "con", "reload").Run()
		con, _ := run("nmcli", "-t", "-f", "NAME,DEVICE", "con", "show")
		conName := dev
		for _, line := range strings.Split(con, "\n") {
			ff := strings.SplitN(line, ":", 2)
			if len(ff) == 2 && ff[1] == dev {
				conName = ff[0]
				break
			}
		}
		exec.Command("nmcli", "con", "up", conName).Run()
	} else {
		exec.Command("ifdown", dev).Run()
		exec.Command("ifup", dev).Run()
	}
}

func nicReady(dev string) bool {
	out, _ := run("ip", "-4", "-o", "addr", "show", dev)
	ip4 := ""
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 4 {
			ip4 = f[3]
			break
		}
	}
	if ip4 == "" {
		return false
	}
	if nmActive.Load() == 1 {
		st, _ := run("nmcli", "-t", "-f", "DEVICE,STATE", "dev", "status")
		good := false
		for _, line := range strings.Split(st, "\n") {
			ff := strings.SplitN(line, ":", 2)
			// Newer NetworkManager reports "connected (externally)".
			if len(ff) == 2 && ff[0] == dev && strings.HasPrefix(ff[1], "connected") {
				good = true
				break
			}
		}
		if !good {
			return false
		}
	}
	return true
}

// applyNic applies the config and returns whether the interface came up with
// an IPv4 address. A false return lets the caller restore the backup.
func applyNic(dev, bak string) bool {
	if dev == "" {
		fmt.Println("FAILED: no interface given.")
		return false
	}
	if netBackend == "rhel" {
		if !fileExists(filepath.Join(ifcfgDir, "ifcfg-"+dev)) {
			fmt.Printf("FAILED: %s/ifcfg-%s is missing.\n", ifcfgDir, dev)
			return false
		}
	}
	// Cache NM state once (avoids repeated systemctl forks per retry).
	nmUp := netBackend == "rhel" && isNMActive()
	if nmUp {
		nmActive.Store(1)
	} else {
		nmActive.Store(0)
	}
	if netBackend == "debian" {
		fmt.Printf("Applying config on %s (ifdown/ifup) ...\n", dev)
	} else if netBackend == "netplan" {
		fmt.Printf("Applying config on %s (netplan apply) ...\n", dev)
	} else if nmUp {
		fmt.Printf("Applying config on %s (nmcli) ...\n", dev)
	} else {
		fmt.Printf("Applying config on %s (ifdown/ifup) ...\n", dev)
	}
	for i := 0; i < maxTries; i++ {
		nicApply(dev)
		if nicReady(dev) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if nicReady(dev) {
		ip4, _ := run("ip", "-4", "-o", "addr", "show", dev)
		addr := ""
		for _, f := range strings.Fields(ip4) {
			if strings.Contains(f, "/") {
				addr = f
				break
			}
		}
		fmt.Printf("OK: %s is up (%s)\n", dev, addr)
		return true
	}
	ip4, _ := run("ip", "-4", "-o", "addr", "show", dev)
	addr := ""
	for _, f := range strings.Fields(ip4) {
		if strings.Contains(f, "/") {
			addr = f
			break
		}
	}
	if addr == "" {
		fmt.Printf("FAILED: %s has no IPv4 address after %d attempt(s)", dev, maxTries)
		if bak != "" {
			fmt.Printf(" (backup: %s)", bak)
		}
		fmt.Println()
		return false
	}
	// Has an IPv4 but is not "fully ready". Explain per backend.
	if nmUp {
		st, _ := run("nmcli", "-t", "-f", "DEVICE,STATE", "dev", "status")
		stt := "unknown"
		for _, line := range strings.Split(st, "\n") {
			ff := strings.SplitN(line, ":", 2)
			if len(ff) == 2 && ff[0] == dev {
				stt = ff[1]
				break
			}
		}
		fmt.Printf("FAILED: %s has %s but NetworkManager state is '%s' after %d attempt(s)", dev, addr, stt, maxTries)
	} else {
		fmt.Printf("FAILED: %s has %s but the interface is not fully ready after %d attempt(s)", dev, addr, maxTries)
	}
	if bak != "" {
		fmt.Printf(" (backup: %s)", bak)
	}
	fmt.Println()
	return false
}

// rollbackConfig restores the pre-change config and re-applies it. It is only
// called when applyNic failed, so a bad static IP does not leave the machine
// unreachable until the next console visit.
func rollbackConfig(bak, dev string) bool {
	if bak == "" || dev == "" {
		return false
	}
	switch netBackend {
	case "rhel":
		data, err := os.ReadFile(bak)
		if err != nil {
			return false
		}
		if err := writeFileAtomic(filepath.Join(ifcfgDir, "ifcfg-"+dev), string(data), 0644); err != nil {
			return false
		}
		if isNMActive() {
			nmActive.Store(1)
		} else {
			nmActive.Store(0)
		}
		nicApply(dev)
		return true
	case "debian":
		data, err := os.ReadFile(bak)
		if err != nil {
			return false
		}
		if err := writeFileAtomic(interfacesF, string(data), 0644); err != nil {
			return false
		}
		debianApply(dev)
		return true
	case "netplan":
		return restoreNetplanBackup(bak)
	}
	return false
}

// ---------- static IP (formerly ip.sh) ----------

func isValidIPv4(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() != nil
}

func staticConfig() {
	fmt.Println("===================== NETWORK CONFIG (static) =====================")
	nics := listNics()
	if len(nics) == 0 {
		fmt.Println("No network interface found.")
		return
	}
	fmt.Println("Interfaces:")
	for i, n := range nics {
		fmt.Printf("  %d. %-16s link=%s\n", i+1, n, nicOperstate(n))
	}
	// choose NIC
	sel, intr := ask("Select interface number: ")
	if intr {
		return
	}
	sel = strings.TrimSpace(sel)
	n, err := strconv.Atoi(sel)
	if err != nil || n < 1 || n > len(nics) {
		fmt.Println("ERROR: invalid interface number.")
		return
	}
	nic := nics[n-1]
	// RHEL backend requires an existing ifcfg file; Debian backend writes
	// /etc/network/interfaces regardless of pre-existing config.
	cfg := filepath.Join(ifcfgDir, "ifcfg-"+nic)
	if netBackend == "rhel" && !fileExists(cfg) {
		fmt.Printf("ERROR: %s does not exist - this tool only edits an existing ifcfg.\n", cfg)
		return
	}
	if reason := specialNICReason(nic); reason != "" {
		fmt.Printf("ERROR: %s is a %s - refusing to touch it.\n", nic, reason)
		fmt.Println("       Nothing was changed.")
		return
	}
	// IP/CIDR input
	var ipAddr, netmask string
	ipOK := false
	for t := 0; t < maxTries && !ipOK; t++ {
		v, i2 := ask("IP address (CIDR or IP; prefix optional, e.g. 10.0.0.5/24): ")
		if i2 {
			return
		}
		v = strings.TrimSpace(v)
		if v == "" {
			fmt.Printf("  Empty input - try again (%d/%d).\n", t+1, maxTries)
			continue
		}
		if strings.Contains(v, "/") {
			parts := strings.SplitN(v, "/", 2)
			addr, pfx := parts[0], parts[1]
			if !isValidIPv4(addr) {
				fmt.Printf("  Invalid address: %s - try again (%d/%d).\n", addr, t+1, maxTries)
				continue
			}
			p, err := strconv.Atoi(pfx)
			if err != nil || p < 1 || p > 32 {
				fmt.Printf("  Invalid prefix: /%s - try again (%d/%d).\n", pfx, t+1, maxTries)
				continue
			}
			ipAddr = addr
			netmask = prefixToMask(p)
			ipOK = true
		} else {
			if !isValidIPv4(v) {
				fmt.Printf("  Invalid address: %s - try again (%d/%d).\n", v, t+1, maxTries)
				continue
			}
			ipAddr = v
			ipOK = true
		}
	}
	if !ipOK {
		fmt.Println("ERROR: too many invalid inputs - nothing has been changed.")
		return
	}
	// ask mask if not given
	if netmask == "" {
		got := false
		for t := 0; t < maxTries && !got; t++ {
			v, i2 := ask("Subnet mask (e.g. 24 or 255.255.255.0): ")
			if i2 {
				return
			}
			v = strings.TrimSpace(v)
			if v == "" {
				fmt.Printf("  Empty input - try again (%d/%d).\n", t+1, maxTries)
				continue
			}
			if p, err := strconv.Atoi(v); err == nil && p >= 1 && p <= 32 {
				netmask = prefixToMask(p)
				got = true
			} else if isValidIPv4(v) && netmaskIsValid(v) {
				netmask = v
				got = true
			} else {
				fmt.Printf("  Invalid value: %s - try again (%d/%d).\n", v, t+1, maxTries)
			}
		}
		if !got {
			fmt.Println("ERROR: too many invalid inputs - nothing has been changed.")
			return
		}
	}
	// gateway suggestion = first usable host that is not this machine
	ip := net.ParseIP(ipAddr).To4()
	mask := net.ParseIP(netmask).To4()
	ipnet := net.IPNet{IP: ip, Mask: net.IPMask(mask)}
	prefix := maskSize(mask)
	gwSuggest := suggestGateway(ip, net.IPMask(mask))
	if prefix >= 31 {
		fmt.Printf("NOTE: /%d has no usable host address - no gateway is suggested.\n", prefix)
	}
	gateway := ""
	gwOK := false
	for t := 0; t < maxTries && !gwOK; t++ {
		var v string
		var i2 bool
		if gwSuggest != "" {
			v, i2 = ask(fmt.Sprintf("Gateway [%s] (Enter = use, 'none' = no gateway, 'auto' = same): ", gwSuggest))
		} else {
			v, i2 = ask("Gateway (blank/'none' = no default route): ")
		}
		if i2 {
			return
		}
		v = strings.TrimSpace(v)
		switch v {
		case "":
			if gwSuggest != "" {
				gateway = gwSuggest
			} else {
				fmt.Println("WARNING: no gateway - the default route will not be configured.")
			}
			gwOK = true
		case "none", "no", "-":
			fmt.Println("WARNING: no gateway - the default route will not be configured.")
			gwOK = true
		case "auto":
			if gwSuggest == "" {
				fmt.Printf("  Cannot derive a gateway for %s/%s - type one.\n", ipAddr, netmask)
				continue
			}
			gateway = gwSuggest
			gwOK = true
		default:
			if isValidIPv4(v) {
				if v == ipAddr {
					fmt.Printf("  Gateway cannot be the interface IP itself - try again (%d/%d).\n", t+1, maxTries)
					continue
				}
				gateway = v
				gwOK = true
			} else {
				fmt.Printf("  Invalid gateway: %s - try again (%d/%d).\n", v, t+1, maxTries)
			}
		}
	}
	if !gwOK {
		fmt.Println("ERROR: too many invalid inputs - nothing has been changed.")
		return
	}
	if gateway == ipAddr {
		fmt.Println("WARNING: gateway equals the IP address - double-check it.")
	}
	if gateway != "" {
		gwIP := net.ParseIP(gateway).To4()
		if !ipnet.Contains(gwIP) {
			fmt.Printf("WARNING: gateway %s is not in %s/%s - the default route may not work.\n", gateway, ipAddr, netmask)
		}
	}
	// DNS: preserve the real upstream resolvers.
	existingDNS := readResolvConfDNS()
	// RHEL backend: also read DNS entries from the existing ifcfg file.
	if netBackend == "rhel" && fileExists(cfg) {
		if cfgBytes2, err := os.ReadFile(cfg); err == nil {
			for _, line := range strings.Split(string(cfgBytes2), "\n") {
				line = strings.TrimSpace(line)
				if m := dnsLineRe.FindStringSubmatch(line); m != nil && usableNameserver(m[1]) {
					existingDNS = append(existingDNS, strings.Trim(m[1], `"`))
				}
			}
		}
	}
	existingDNS = dedupeStrings(existingDNS)
	var dnsBlock string
	if len(existingDNS) > 0 {
		dnsBlock = "PEERDNS=no"
		for i, d := range existingDNS {
			if i >= 2 {
				break
			}
			dnsBlock += "\nDNS" + strconv.Itoa(i+1) + "=" + d
		}
	} else {
		dnsBlock = "PEERDNS=yes\nDNS1=223.6.6.6\nDNS2=223.5.5.5"
	}
	// backup
	os.MkdirAll(bakDir, 0700)
	bak := ""
	switch netBackend {
	case "rhel":
		bak = filepath.Join(bakDir, fmt.Sprintf("ifcfg-%s.%s", nic, time.Now().Format("20060102_150405")))
		if b, err := os.ReadFile(cfg); err == nil {
			os.WriteFile(bak, b, 0600)
		}
	case "debian":
		bak = debianBackupIface()
	case "netplan":
		bak = netplanBackup()
	}
	// write config per backend
	switch netBackend {
	case "rhel":
		gwBlock := ""
		if gateway != "" {
			gwBlock = "GATEWAY=" + gateway + "\n"
		}
		base := fmt.Sprintf("TYPE=Ethernet\nBOOTPROTO=static\nNAME=%s\nDEVICE=%s\nONBOOT=yes\nNM_CONTROLLED=yes\nIPADDR=%s\nNETMASK=%s\n%s%s\n",
			nic, nic, ipAddr, netmask, gwBlock, dnsBlock)
		old := ""
		if b, err := os.ReadFile(cfg); err == nil {
			old = string(b)
		}
		if err := writeFileAtomic(cfg, mergeIfcfg(old, base, false), 0644); err != nil {
			fmt.Printf("ERROR: cannot write %s: %v - nothing was changed.\n", cfg, err)
			return
		}
	case "debian":
		d1, d2 := "", ""
		if len(existingDNS) > 0 {
			d1 = existingDNS[0]
			if len(existingDNS) > 1 {
				d2 = existingDNS[1]
			}
		}
		if err := debianWriteStatic(nic, ipAddr, netmask, gateway, d1, d2); err != nil {
			fmt.Printf("ERROR: cannot write %s: %v\n", interfacesF, err)
			return
		}
		fmt.Printf("Wrote static config to %s\n", interfacesF)
	case "netplan":
		d1, d2 := "", ""
		if len(existingDNS) > 0 {
			d1 = existingDNS[0]
			if len(existingDNS) > 1 {
				d2 = existingDNS[1]
			}
		}
		if err := netplanWriteStatic(nic, ipAddr, netmask, gateway, d1, d2); err != nil {
			fmt.Printf("ERROR: cannot write %s: %v\n", netplanConfigPath, err)
			return
		}
		fmt.Printf("Wrote static config to %s\n", netplanConfigPath)
	}
	if !applyNic(nic, bak) {
		if rollbackConfig(bak, nic) {
			fmt.Printf("Restored the previous configuration from %s.\n", bak)
		}
		return
	}
	// quick ping
	pingTarget := gateway
	if pingTarget == "" {
		pingTarget = "223.6.6.6"
	}
	c := exec.Command("ping", "-c", "2", "-W", "2", pingTarget)
	if c.Run() == nil {
		fmt.Println("PING: OK")
	} else {
		fmt.Println("PING: FAILED")
	}
}

func prefixToMask(p int) string {
	m := net.CIDRMask(p, 32)
	return net.IP(m).To4().String()
}

// suggestGateway returns the first usable host address in the subnet that is
// not the machine's own address. For common 192.168.1.1/24 setups this picks
// .2 instead of suggesting the host's own address as its gateway.
func suggestGateway(ip net.IP, mask net.IPMask) string {
	ip4 := ip.To4()
	mask4 := []byte(mask)
	if ip4 == nil || len(mask4) != 4 {
		return ""
	}
	ones := maskSize(net.IP(mask4))
	if ones < 1 || ones > 30 {
		return ""
	}
	ipn := binary.BigEndian.Uint32(ip4)
	m := binary.BigEndian.Uint32(mask4)
	network := ipn & m
	broadcast := network | ^m
	for cand := network + 1; cand < broadcast; cand++ {
		if cand != ipn {
			return net.IPv4(byte(cand>>24), byte(cand>>16), byte(cand>>8), byte(cand)).String()
		}
	}
	return ""
}

func netmaskIsValid(m string) bool {
	ip := net.ParseIP(m)
	if ip == nil {
		return false
	}
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	// reject 0.0.0.0 and 255.255.255.255
	if v4[0] == 0 && v4[1] == 0 && v4[2] == 0 && v4[3] == 0 {
		return false
	}
	if v4[0] == 255 && v4[1] == 255 && v4[2] == 255 && v4[3] == 255 {
		return false
	}
	// check contiguous
	b := []byte(v4)
	seen0 := false
	for _, by := range b {
		for bit := 7; bit >= 0; bit-- {
			if by&(1<<bit) == 0 {
				seen0 = true
			} else if seen0 {
				return false
			}
		}
	}
	return true
}

func maskSize(m net.IP) int {
	b := []byte(m.To4())
	ones := 0
	for _, by := range b {
		for bit := 7; bit >= 0; bit-- {
			if by&(1<<bit) != 0 {
				ones++
			} else {
				return ones
			}
		}
	}
	return ones
}

// ---------- DHCP ----------

func dhcpConfig() {
	nics := listNics()
	if len(nics) == 0 {
		fmt.Println("No network interface found.")
		pause()
		return
	}
	fmt.Println("===================== NETWORK CONFIG (dhcp) =====================")
	fmt.Println("Interfaces:")
	for i, n := range nics {
		fmt.Printf("  %d. %-16s link=%s\n", i+1, n, nicOperstate(n))
	}
	sel, intr := ask("Select interface number: ")
	if intr {
		return
	}
	sel = strings.TrimSpace(sel)
	n, err := strconv.Atoi(sel)
	if err != nil || n < 1 || n > len(nics) {
		fmt.Println("ERROR: invalid interface number.")
		pause()
		return
	}
	nic := nics[n-1]
	if reason := specialNICReason(nic); reason != "" {
		fmt.Printf("ERROR: %s is a %s - refusing to touch it.\n", nic, reason)
		fmt.Println("       Nothing was changed.")
		pause()
		return
	}
	cfg := filepath.Join(ifcfgDir, "ifcfg-"+nic)
	bak := ""
	switch netBackend {
	case "rhel":
		old := ""
		if b, err := os.ReadFile(cfg); err == nil {
			old = string(b)
			os.MkdirAll(bakDir, 0700)
			bak = filepath.Join(bakDir, fmt.Sprintf("ifcfg-%s.%s", nic, time.Now().Format("20060102_150405")))
			os.WriteFile(bak, b, 0600)
		}
		base := fmt.Sprintf("TYPE=Ethernet\nBOOTPROTO=dhcp\nDEFROUTE=yes\nIPV4_FAILURE_FATAL=no\nNM_CONTROLLED=yes\nNAME=%s\nDEVICE=%s\nONBOOT=yes\n", nic, nic)
		if err := writeFileAtomic(cfg, mergeIfcfg(old, base, true), 0644); err != nil {
			fmt.Printf("ERROR: cannot write %s: %v - nothing was changed.\n", cfg, err)
			pause()
			return
		}
	case "debian":
		bak = debianBackupIface()
		if err := debianWriteDHCP(nic); err != nil {
			fmt.Printf("ERROR: cannot write %s: %v\n", interfacesF, err)
			pause()
			return
		}
		fmt.Printf("Wrote DHCP config to %s\n", interfacesF)
	case "netplan":
		bak = netplanBackup()
		if err := netplanWriteDHCP(nic); err != nil {
			fmt.Printf("ERROR: cannot write %s: %v\n", netplanConfigPath, err)
			pause()
			return
		}
		fmt.Printf("Wrote DHCP config to %s\n", netplanConfigPath)
	}
	if !applyNic(nic, bak) {
		if rollbackConfig(bak, nic) {
			fmt.Printf("Restored the previous configuration from %s.\n", bak)
		}
	}
	pause()
}

// ---------- ping ----------

func pingTest() {
	fmt.Println("======================== PING TEST ========================")
	runBg("ping", "-c", "4", "-W", "2", "223.6.6.6")
	fmt.Println("==========================================================================")
	if interrupt.Load() == 1 {
		interrupt.Store(0)
		return
	}
	pause()
}

// ---------- disks ----------

// resolveToPhysicalDisks walks UP /sys/class/block/<dev>/slaves/ recursively
// (BFS), following LVM, dm-crypt, md, and multipath stacks, and returns all
// leaf whole-disk names (sda, sdb, nvme0n1, ...).
//
// multipath / RAID1 have multiple slaves — all of them are returned, so the
// caller can mark every physical member of the system volume as "system".
func resolveToPhysicalDisks(dev string) []string {
	seen := map[string]bool{}
	var out []string
	queue := []string{dev}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == "" || seen[cur] {
			continue
		}
		seen[cur] = true
		entries, err := os.ReadDir("/sys/class/block/" + cur + "/slaves")
		if err == nil && len(entries) > 0 {
			for _, e := range entries {
				queue = append(queue, e.Name())
			}
			continue
		}
		// Leaf: if it's a partition, peel off the parent whole disk.
		if _, err := os.Stat("/sys/class/block/" + cur + "/partition"); err == nil {
			if link, err := os.Readlink("/sys/class/block/" + cur); err == nil {
				parent := filepath.Base(filepath.Dir(link))
				if parent != "" && parent != "." && parent != "/" {
					cur = parent
				}
			}
		}
		out = append(out, cur)
	}
	return out
}

// systemDisks finds which whole disks host the root filesystem, /boot, and
// /boot/efi. Uses /proc/mounts plus /sys/class/block to resolve the underlying
// physical disks without depending on lsblk. Falls back to /proc/cmdline root=
// when /proc/mounts shows /dev/root (initramfs common).
func systemDisks() []string {
	disks := map[string]bool{}
	addDev := func(device string) {
		if !strings.HasPrefix(device, "/dev/") {
			return
		}
		name := strings.TrimPrefix(device, "/dev/")
		// /dev/mapper/vg-root -> dm-0
		if strings.HasPrefix(name, "mapper/") {
			if link, err := os.Readlink("/dev/" + name); err == nil {
				name = filepath.Base(link)
			}
		}
		// Must be a real block device; /dev/root is a fake path from initramfs.
		if _, err := os.Stat("/sys/class/block/" + name); err != nil {
			return
		}
		for _, phys := range resolveToPhysicalDisks(name) {
			if phys != "" {
				disks[phys] = true
			}
		}
	}
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		device, mountPoint := fields[0], fields[1]
		if mountPoint != "/" && mountPoint != "/boot" && mountPoint != "/boot/efi" {
			continue
		}
		addDev(device)
	}
	// Fallback: /proc/cmdline root= (handles /dev/root, UUID=, LABEL=)
	if len(disks) == 0 {
		if cmd, err := os.ReadFile("/proc/cmdline"); err == nil {
			for _, f := range strings.Fields(string(cmd)) {
				if strings.HasPrefix(f, "root=") {
					r := strings.TrimPrefix(f, "root=")
					if strings.HasPrefix(r, "UUID=") || strings.HasPrefix(r, "LABEL=") {
						if out, err := run("blkid", "-t", r, "-o", "device"); err == nil {
							addDev(strings.TrimSpace(out))
						}
					} else {
						addDev(r)
					}
					break
				}
			}
		}
	}
	var res []string
	for d := range disks {
		res = append(res, d)
	}
	sort.Strings(res)
	return res
}

func diskType(dev string) string {
	if strings.HasPrefix(dev, "nvme") {
		return "NVMe-SSD"
	}
	bus, _, _ := diskInfo(dev)
	rot := readFileTrim("/sys/block/" + dev + "/queue/rotational")
	media := "SSD"
	if rot == "1" {
		media = "HDD"
	}
	switch bus {
	case "ata", "sata":
		return "SATA-" + media
	case "scsi", "sas":
		return "SAS-" + media
	}
	return media
}

func diskMedia(dev string) string {
	if readFileTrim("/sys/block/"+dev+"/queue/rotational") == "1" {
		return "HDD"
	}
	return "SSD"
}

func parseSmartBus(out string) string {
	low := strings.ToLower(out)
	if strings.Contains(low, "sata version is:") || strings.Contains(low, "transport protocol: sata") {
		return "SATA"
	}
	if strings.Contains(low, "transport protocol: sas") {
		return "SAS"
	}
	return ""
}

// smartBus resolves the ambiguity of ID_BUS=scsi: a SATA disk attached to a
// SAS HBA is also SCSI. smartctl -i reports the real transport protocol.
func smartBus(dev string) string {
	vendor := strings.TrimSpace(readFileTrim("/sys/block/" + dev + "/device/vendor"))
	if strings.EqualFold(vendor, "ATA") {
		return "SATA"
	}
	if smartctlPath() == "" {
		return ""
	}
	return parseSmartBus(runSmartctl("-i", "/dev/"+dev))
}

func refineDiskType(dev, current string) string {
	if strings.HasPrefix(dev, "nvme") || strings.HasPrefix(current, "SATA-") {
		return current
	}
	if bus := smartBus(dev); bus != "" {
		return bus + "-" + diskMedia(dev)
	}
	return current
}

func diskInfo(dev string) (bus, model, serial string) {
	props, _ := run("udevadm", "info", "--query=property", "--name=/dev/"+dev)
	for _, line := range strings.Split(props, "\n") {
		switch {
		case strings.HasPrefix(line, "ID_BUS="):
			bus = strings.TrimPrefix(line, "ID_BUS=")
		case strings.HasPrefix(line, "ID_MODEL="):
			model = strings.TrimPrefix(line, "ID_MODEL=")
		case strings.HasPrefix(line, "ID_SERIAL_SHORT="):
			serial = strings.TrimPrefix(line, "ID_SERIAL_SHORT=")
		}
	}
	if serial == "" {
		for _, line := range strings.Split(props, "\n") {
			if strings.HasPrefix(line, "ID_SERIAL=") {
				serial = strings.TrimPrefix(line, "ID_SERIAL=")
			}
		}
	}
	// RAID virtual disks (PERC/H700 etc) expose ID_SERIAL as
	// "controllerSerial_virtual/path" — e.g. "6509a4c0afca2_data_hdd/storage1_hdd".
	// Only strip the trailing virtual-disk path when '/' is present (a virtual
	// separator); normal SATA/NVMe serials (Samsung_SSD_870_...) never contain '/'.
	if strings.Contains(serial, "/") {
		if idx := strings.Index(serial, "_"); idx > 0 {
			serial = serial[:idx]
		}
	}
	if serial == "" {
		serial = readFileTrim("/sys/block/" + dev + "/device/serial")
	}
	if model == "" {
		model = strings.TrimSpace(readFileTrim("/sys/block/" + dev + "/device/model"))
	}
	if bus == "" {
		bus = "-"
	}
	return
}

// allDisks enumerates real whole disks from /sys/block.
// Only devices with a /device symlink (physical backing) are included,
// and partitions are skipped: a whole disk has no /sys/block/<dev>/partition
// attribute file, while partitions (sda1, nvme0n1p1, ...) do.
func allDisks() []string {
	entries, err := os.ReadDir("/sys/block/")
	if err != nil {
		return nil
	}
	var res []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "sr") ||
			strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "zram") ||
			strings.HasPrefix(name, "dm-") || strings.HasPrefix(name, "md") ||
			strings.HasPrefix(name, "nbd") {
			continue
		}
		// skip partitions (e.g. sda1, nvme0n1p1): they carry a "partition"
		// attribute file; whole disks do not.
		if _, err := os.Stat("/sys/block/" + name + "/partition"); err == nil {
			continue
		}
		if _, err := os.Stat("/sys/block/" + name + "/device"); err != nil {
			continue
		}
		res = append(res, name)
	}
	sort.Strings(res)
	return res
}

// diskSizeHuman reads /sys/block/<dev>/size (512-byte sectors) and formats.
func diskSizeHuman(dev string) string {
	sectorsStr := readFileTrim("/sys/block/" + dev + "/size")
	sectors, err := strconv.ParseInt(sectorsStr, 10, 64)
	if err != nil || sectors == 0 {
		return "-"
	}
	bytes := sectors * 512
	gb := float64(bytes) / (1 << 30)
	if gb >= 100 {
		return fmt.Sprintf("%.0fG", gb)
	}
	return fmt.Sprintf("%.1fG", gb)
}

// percDiskInfo holds physical disk info from perccli64.
type percDiskInfo struct {
	EID      int
	Slot     int
	DID      int
	DG       int
	Raid     string // RAID0 / RAID5 / ...
	Size     string
	State    string // Onln / UGood / Rbld / Offln / UBad ...
	Serial   string
	Model    string
	Intf     string // SATA / SAS
	Med      string // SSD / HDD
	MediaErr int
	OtherErr int
	PredFail int
	Smart    bool
}

var percCache map[int][]percDiskInfo
var percOnce sync.Once

func diskPercMapCache() (map[int][]percDiskInfo, bool) {
	percOnce.Do(func() {
		m, err := diskPercMap()
		if err == nil && len(m) > 0 {
			percCache = m
		}
	})
	return percCache, percCache != nil
}

func percToolPaths() []string {
	var res []string
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		res = append(res, p)
	}
	// storcli first: on Huawei/LSI cards perccli may be a vendor wrapper.
	for _, name := range []string{"storcli64", "perccli64", "storcli", "MegaCli64", "MegaCli"} {
		if p, err := exec.LookPath(name); err == nil {
			add(p)
		}
	}
	for _, p := range []string{
		"/opt/MegaRAID/storcli/storcli64",
		"/opt/MegaRAID/perccli/perccli64",
		"/usr/local/bin/storcli64",
		"/usr/local/bin/perccli64",
		"/usr/sbin/storcli64",
		"/usr/sbin/perccli64",
	} {
		if _, err := os.Stat(p); err == nil {
			add(p)
		}
	}
	return res
}

func diskPercMap() (map[int][]percDiskInfo, error) {
	tools := percToolPaths()
	if len(tools) == 0 {
		return nil, fmt.Errorf("no perccli/storcli found")
	}
	var lastErr error
	for _, tool := range tools {
		out, err := run(tool, "/c0/eall/sall", "show", "all")
		if err != nil {
			lastErr = err
			continue
		}
		res := parsePercDiskOutput(out)
		if len(res) == 0 {
			lastErr = fmt.Errorf("%s returned no physical drives", tool)
			continue
		}
		// VD list carries the RAID level (Dell H330: 0/0 RAID0 ...).
		if vdOut, verr := run(tool, "/c0/vall", "show"); verr == nil {
			raid := parsePercRaidLevels(vdOut)
			for _, disks := range res {
				for i := range disks {
					disks[i].Raid = raid[disks[i].DG]
				}
			}
		}
		return res, nil
	}
	return nil, lastErr
}

func percBySerial(serial string) ([]percDiskInfo, bool) {
	serial = strings.TrimSpace(serial)
	if serial == "" {
		return nil, false
	}
	pm, ok := diskPercMapCache()
	if !ok {
		return nil, false
	}
	for _, disks := range pm {
		for _, d := range disks {
			if strings.TrimSpace(d.Serial) == serial {
				return []percDiskInfo{d}, true
			}
		}
	}
	return nil, false
}

func parsePercRaidLevels(out string) map[int]string {
	m := map[int]string{}
	re := regexp.MustCompile(`^(\d+)/\d+\s+(\w+)`)
	for _, line := range strings.Split(out, "\n") {
		mm := re.FindStringSubmatch(strings.TrimSpace(line))
		if mm == nil {
			continue
		}
		dg, err := strconv.Atoi(mm[1])
		if err != nil {
			continue
		}
		raid := strings.ToUpper(mm[2])
		if strings.HasPrefix(raid, "RAID") {
			m[dg] = raid
		}
	}
	return m
}

// parsePercDiskOutput parses "perccli64 /c0/eall/sall show all" (Dell PERC,
// LSI MegaRAID). Dell H330 RAID0 output has one "Drive /c0/e32/sN" section per
// physical drive; the table row carries EID:Slot, DID and DG.
func afterEquals(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "=")
	return strings.TrimSpace(s)
}

func parsePercDiskOutput(out string) map[int][]percDiskInfo {
	res := map[int][]percDiskInfo{}
	var curEID, curDG, curSlot, curDID int = -1, -1, -1, -1
	var curSerial, curModel, curIntf, curMed, curSize, curState string
	var curMediaErr, curOtherErr, curPredFail int
	var curSmart bool
	flush := func() {
		if curSlot >= 0 && curDID >= 0 {
			info := percDiskInfo{EID: curEID, Slot: curSlot, DID: curDID, DG: curDG, Size: curSize, State: curState, Serial: curSerial, Model: curModel, Intf: curIntf, Med: curMed, MediaErr: curMediaErr, OtherErr: curOtherErr, PredFail: curPredFail, Smart: curSmart}
			if curDG >= 0 {
				res[curDG] = append(res[curDG], info)
			}
			res[-curDID-1] = append(res[-curDID-1], info)
		}
	}
	// Table row: "32:0      0 Onln   1 5.457 TB SATA HDD N   N  512B ST6000NM0115-1YZ110"
	tblRe := regexp.MustCompile(`^(\d+):(\d+)\s+(\d+)\s+(\w+)\s+(-?\d+|-)\s+([\d.]+)\s+(\w+)\s+(\w+)\s+(\w+)`)
	snRe := regexp.MustCompile(`^SN\s*=\s*(\S+)`)
	modelRe := regexp.MustCompile(`^Model Number\s*=\s*(.+)`)
	driveRe := regexp.MustCompile(`^Drive /c\d+/e\d+/s\d+\s*:$`)
	for _, line := range strings.Split(out, "\n") {
		trim := strings.TrimSpace(line)
		if driveRe.MatchString(trim) {
			flush()
			curEID, curDG, curSlot, curDID = -1, -1, -1, -1
			curSerial, curModel, curIntf, curMed, curSize, curState = "", "", "", "", "", ""
			curMediaErr, curOtherErr, curPredFail = 0, 0, 0
			curSmart = false
			continue
		}
		if m := tblRe.FindStringSubmatch(trim); m != nil {
			curEID, _ = strconv.Atoi(m[1])
			curSlot, _ = strconv.Atoi(m[2])
			curDID, _ = strconv.Atoi(m[3])
			curState = strings.TrimSpace(m[4])
			if strings.TrimSpace(m[5]) == "-" {
				curDG = -1
			} else {
				curDG, _ = strconv.Atoi(m[5])
			}
			curSize = compactSize(m[6], m[7])
			curIntf = strings.ToUpper(m[8])
			curMed = strings.ToUpper(m[9])
			continue
		}
		if m := snRe.FindStringSubmatch(trim); m != nil {
			curSerial = strings.TrimSpace(m[1])
			continue
		}
		if m := modelRe.FindStringSubmatch(trim); m != nil {
			curModel = strings.TrimSpace(m[1])
			continue
		}
		switch {
		case strings.HasPrefix(trim, "Media Error Count"):
			curMediaErr, _ = strconv.Atoi(afterEquals(strings.TrimPrefix(trim, "Media Error Count")))
		case strings.HasPrefix(trim, "Other Error Count"):
			curOtherErr, _ = strconv.Atoi(afterEquals(strings.TrimPrefix(trim, "Other Error Count")))
		case strings.HasPrefix(trim, "Predictive Failure Count"):
			curPredFail, _ = strconv.Atoi(afterEquals(strings.TrimPrefix(trim, "Predictive Failure Count")))
		case strings.HasPrefix(trim, "S.M.A.R.T alert flagged by drive"):
			v := strings.ToLower(afterEquals(strings.TrimPrefix(trim, "S.M.A.R.T alert flagged by drive")))
			curSmart = v == "yes"
		}
	}
	flush()
	return res
}

func percSlotLabel(d percDiskInfo) string {
	return fmt.Sprintf("%02d", d.Slot)
}

func healthFromStatus(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || s == "-" {
		return "-"
	}
	for _, bad := range []string{"fail", "bad", "offln", "offline", "ubad", "missing", "msng", "error"} {
		if strings.Contains(s, bad) {
			return "BAD"
		}
	}
	for _, warn := range []string{"predict", "degrad", "rebuild", "rbld", "copyback", "cpybck", "foreign", "transition"} {
		if strings.Contains(s, warn) {
			return "WARN"
		}
	}
	for _, ok := range []string{"ok", "onln", "online", "ugood", "good", "optl", "optimal", "ghs", "dhs"} {
		if strings.Contains(s, ok) {
			return "OK"
		}
	}
	return "-"
}

func percHealth(d percDiskInfo) string {
	h := healthFromStatus(d.State)
	if h == "BAD" {
		return h
	}
	if d.MediaErr > 0 || d.OtherErr > 0 || d.PredFail > 0 || d.Smart {
		return "WARN"
	}
	return h
}

func hpHealth(d hpDiskInfo) string {
	h := healthFromStatus(d.Status)
	lh := healthFromStatus(d.LDStatus)
	if lh == "BAD" {
		return "BAD"
	}
	if lh == "WARN" && h != "BAD" {
		return "WARN"
	}
	if h == "-" {
		return lh
	}
	return h
}

var smartctlOnce sync.Once
var smartctlBin string

func smartctlPath() string {
	smartctlOnce.Do(func() {
		if p, err := exec.LookPath("smartctl"); err == nil {
			smartctlBin = p
			return
		}
		for _, p := range []string{"/usr/sbin/smartctl", "/usr/local/sbin/smartctl"} {
			if _, err := os.Stat(p); err == nil {
				smartctlBin = p
				return
			}
		}
	})
	return smartctlBin
}

func runSmartctl(args ...string) string {
	bin := smartctlPath()
	if bin == "" {
		return ""
	}
	out, _ := exec.Command(bin, args...).CombinedOutput()
	return string(out)
}

func parseSmartHealth(out string) string {
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		low := strings.ToLower(t)
		if !strings.Contains(low, "overall-health self-assessment test result:") &&
			!strings.HasPrefix(low, "smart health status:") {
			continue
		}
		i := strings.Index(t, ":")
		if i < 0 {
			continue
		}
		v := strings.ToLower(afterEquals(t[i:]))
		if strings.Contains(v, "passed") || strings.Contains(v, "ok") {
			return "OK"
		}
		if strings.Contains(v, "failed") {
			return "BAD"
		}
	}
	return "-"
}

func smartPercent(s string) int {
	f := strings.Fields(s)
	if len(f) == 0 {
		return -1
	}
	v := strings.TrimSuffix(f[0], "%")
	n, err := strconv.Atoi(v)
	if err != nil {
		return -1
	}
	return n
}

func smartInt(s string) int {
	f := strings.Fields(s)
	if len(f) == 0 {
		return -1
	}
	v := strings.ReplaceAll(f[0], ",", "")
	n, err := strconv.Atoi(v)
	if err != nil {
		return -1
	}
	return n
}

func smartPercentageUsed(out string) int {
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "Percentage Used:") {
			return smartPercent(strings.TrimPrefix(t, "Percentage Used:"))
		}
	}
	return -1
}

// smartCritical flags NVMe conditions that are not reflected in the overall
// PASSED/FAILED line: critical warning, spare below threshold, or media
// integrity errors. High wear is handled separately as a lifetime percentage.
func smartCritical(out string) bool {
	spare, threshold := -1, -1
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "Critical Warning:"):
			v := strings.ToLower(afterEquals(strings.TrimPrefix(t, "Critical Warning:")))
			if v != "0x00" && v != "0" && v != "" {
				return true
			}
		case strings.HasPrefix(t, "Available Spare Threshold:"):
			threshold = smartPercent(strings.TrimPrefix(t, "Available Spare Threshold:"))
		case strings.HasPrefix(t, "Available Spare:"):
			spare = smartPercent(strings.TrimPrefix(t, "Available Spare:"))
		case strings.HasPrefix(t, "Media and Data Integrity Errors:"):
			if n := smartInt(strings.TrimPrefix(t, "Media and Data Integrity Errors:")); n > 0 {
				return true
			}
		}
	}
	return spare >= 0 && threshold >= 0 && spare < threshold
}

func smartAttrWarn(out string) bool {
	return smartCritical(out) || smartPercentageUsed(out) >= 90
}

// nvmeLifeFromAttrs returns remaining endurance as e.g. "98%" (100 - wear).
func nvmeLifeFromAttrs(out string) string {
	if used := smartPercentageUsed(out); used >= 0 {
		life := 100 - used
		if life < 0 {
			life = 0
		}
		if life > 100 {
			life = 100
		}
		return fmt.Sprintf("%d%%", life)
	}
	if smartCritical(out) {
		return "WARN"
	}
	return "-"
}

func nvmeLife(dev string) string {
	if smartctlPath() == "" {
		return "-"
	}
	m := nvmeCtrlRe.FindStringSubmatch(dev)
	if m == nil {
		return "-"
	}
	target := "/dev/" + m[1]
	if parseSmartHealth(runSmartctl("-H", target)) == "BAD" {
		return "BAD"
	}
	return nvmeLifeFromAttrs(runSmartctl("-A", target))
}

func smartHealth(dev string) string {
	if smartctlPath() == "" {
		return "-"
	}
	target := dev
	if !strings.HasPrefix(dev, "/dev/") {
		target = "/dev/" + dev
	}
	out := runSmartctl("-H", target)
	return parseSmartHealth(out)
}

func worstHealth(hs ...string) string {
	rank := map[string]int{"-": 0, "OK": 1, "WARN": 2, "BAD": 3}
	worst, wr := "-", 0
	for _, h := range hs {
		r := rank[h]
		if r > wr {
			worst, wr = h, r
		}
	}
	return worst
}

// hpDiskInfo holds physical drive info from ssacli/hpssacli.
type hpDiskInfo struct {
	Slot     string // port:box:bay, e.g. 1I:0:2
	Port     string
	Box      int
	Bay      int
	Raid     string // RAID0 / RAID5 / ...
	Size     string
	Status   string // physical drive status
	LDStatus string // logical drive status
	Serial   string
	Model    string
	Intf     string // SATA / SAS / NVMe
	Med      string // SSD / HDD
}

type hpArrayInfo struct {
	lds []string // /dev/sdX names of logical drives in this array
	pds []hpDiskInfo
}

var hpCache map[string][]hpDiskInfo
var hpOnce sync.Once

func diskHPMapCache() (map[string][]hpDiskInfo, bool) {
	hpOnce.Do(func() {
		m, err := diskHPMap()
		if err == nil && len(m) > 0 {
			hpCache = m
		}
	})
	return hpCache, hpCache != nil
}

func hpToolPath() string {
	for _, name := range []string{"ssacli", "hpssacli", "hpacucli"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	for _, p := range []string{
		"/usr/sbin/ssacli",
		"/usr/sbin/hpssacli",
		"/usr/sbin/hpacucli",
		"/opt/hp/hpssacli/bld/hpssacli",
		"/opt/hp/hpacucli/bld/hpacucli",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func diskHPMap() (map[string][]hpDiskInfo, error) {
	tool := hpToolPath()
	if tool == "" {
		return nil, fmt.Errorf("no ssacli/hpssacli/hpacucli found")
	}
	out, err := run(tool, "ctrl", "all", "show", "config", "detail")
	if err != nil {
		return nil, err
	}
	return parseHPDiskConfig(out), nil
}

var (
	hpArrayRe = regexp.MustCompile(`^(?:Array:\s*|Array\s+)([A-Za-z0-9]+)\b`)
	hpPDRe    = regexp.MustCompile(`(?i)^physicaldrive\s+(\S+)`)
)

// parseHPDiskConfig maps each logical-drive block device to the physical
// drives of its array. HP Smart Array drives are usually hidden from the OS
// ("Drive exposed to OS: False"), so this is the only way to show bay numbers.
func parseHPDiskConfig(out string) map[string][]hpDiskInfo {
	var arrays []*hpArrayInfo
	var cur *hpArrayInfo
	var pd *hpDiskInfo
	inLD := false
	curRaid := ""
	curLDStatus := ""
	flushPD := func() {
		if cur != nil && pd != nil {
			cur.pds = append(cur.pds, *pd)
			pd = nil
		}
	}
	for _, line := range strings.Split(out, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" {
			continue
		}
		if strings.HasPrefix(trim, "Smart Array ") || strings.HasPrefix(trim, "HP Smart Array ") {
			flushPD()
			cur = nil
			inLD = false
			curRaid = ""
			curLDStatus = ""
			continue
		}
		if strings.HasPrefix(strings.ToLower(trim), "unassigned") {
			flushPD()
			cur = nil
			inLD = false
			continue
		}
		// "Array Type: Data" is a controller property, not an array header.
		if strings.HasPrefix(trim, "Array Type:") {
			continue
		}
		if m := hpArrayRe.FindStringSubmatch(trim); m != nil {
			flushPD()
			cur = &hpArrayInfo{}
			arrays = append(arrays, cur)
			inLD = false
			curRaid = ""
			curLDStatus = ""
			continue
		}
		if cur == nil {
			continue
		}
		if strings.HasPrefix(trim, "Logical Drive:") {
			flushPD()
			inLD = true
			curLDStatus = ""
			continue
		}
		if strings.HasPrefix(trim, "Status:") && inLD && pd == nil {
			curLDStatus = strings.TrimSpace(strings.TrimPrefix(trim, "Status:"))
			continue
		}
		if strings.HasPrefix(trim, "Fault Tolerance:") {
			curRaid = hpRaidLevel(strings.TrimSpace(strings.TrimPrefix(trim, "Fault Tolerance:")))
			continue
		}
		if strings.HasPrefix(trim, "Disk Name:") {
			if inLD {
				f := strings.Fields(trim)
				if len(f) >= 3 {
					cur.lds = append(cur.lds, strings.TrimPrefix(f[2], "/dev/"))
				}
			}
			continue
		}
		if m := hpPDRe.FindStringSubmatch(trim); m != nil {
			flushPD()
			inLD = false
			pd = &hpDiskInfo{Slot: strings.Trim(m[1], `"`), Raid: curRaid, LDStatus: curLDStatus}
			continue
		}
		if pd == nil {
			continue
		}
		switch {
		case strings.HasPrefix(trim, "Status:"):
			pd.Status = strings.TrimSpace(strings.TrimPrefix(trim, "Status:"))
		case strings.HasPrefix(trim, "Port:"):
			pd.Port = strings.TrimSpace(strings.TrimPrefix(trim, "Port:"))
		case strings.HasPrefix(trim, "Box:"):
			pd.Box, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(trim, "Box:")))
		case strings.HasPrefix(trim, "Bay:"):
			pd.Bay, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(trim, "Bay:")))
		case strings.HasPrefix(trim, "Interface Type:"):
			pd.Intf, pd.Med = hpBusMedia(strings.TrimSpace(strings.TrimPrefix(trim, "Interface Type:")))
		case strings.HasPrefix(trim, "Rotational Speed:"):
			v := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(trim, "Rotational Speed:")))
			if strings.Contains(v, "solid state") {
				pd.Med = "SSD"
			} else if strings.Contains(v, "rpm") && pd.Med == "" {
				pd.Med = "HDD"
			}
		case strings.HasPrefix(trim, "Size:"):
			f := strings.Fields(strings.TrimSpace(strings.TrimPrefix(trim, "Size:")))
			if len(f) >= 2 {
				pd.Size = compactSize(f[0], f[1])
			}
		case strings.HasPrefix(trim, "Serial Number:"):
			pd.Serial = strings.TrimSpace(strings.TrimPrefix(trim, "Serial Number:"))
		case strings.HasPrefix(trim, "Model:"):
			pd.Model = strings.Join(strings.Fields(strings.TrimSpace(strings.TrimPrefix(trim, "Model:"))), " ")
		}
	}
	flushPD()

	res := map[string][]hpDiskInfo{}
	for _, a := range arrays {
		if len(a.lds) == 0 || len(a.pds) == 0 {
			continue
		}
		for _, dev := range a.lds {
			res[dev] = append([]hpDiskInfo(nil), a.pds...)
		}
	}
	return res
}

func hpRaidLevel(ft string) string {
	switch strings.TrimSpace(ft) {
	case "0":
		return "RAID0"
	case "1":
		return "RAID1"
	case "5":
		return "RAID5"
	case "6":
		return "RAID6"
	case "10", "1+0":
		return "RAID10"
	case "50":
		return "RAID50"
	case "60":
		return "RAID60"
	}
	return ""
}

func hpBusMedia(s string) (bus, media string) {
	up := strings.ToUpper(s)
	switch {
	case strings.Contains(up, "SATA"):
		bus = "SATA"
	case strings.Contains(up, "SAS"):
		bus = "SAS"
	case strings.Contains(up, "NVME"):
		bus = "NVMe"
	}
	switch {
	case strings.Contains(up, "SSD"), strings.Contains(up, "SOLID STATE"):
		media = "SSD"
	case strings.Contains(up, "HDD"):
		media = "HDD"
	}
	return
}

// hpSlotLabels prefers compact bay numbers (02,04,06). If bays repeat across
// ports/boxes it falls back to full port:box:bay identifiers.
func hpSlotLabels(disks []hpDiskInfo) []string {
	seen := map[int]bool{}
	allBays := true
	for _, d := range disks {
		if d.Bay <= 0 || seen[d.Bay] {
			allBays = false
			break
		}
		seen[d.Bay] = true
	}
	var parts []string
	for _, d := range disks {
		switch {
		case allBays:
			parts = append(parts, fmt.Sprintf("%02d", d.Bay))
		case d.Slot != "":
			parts = append(parts, d.Slot)
		case d.Port != "" && d.Bay > 0:
			parts = append(parts, fmt.Sprintf("%s:%d:%d", d.Port, d.Box, d.Bay))
		}
	}
	return parts
}

func hpSlotList(disks []hpDiskInfo) string {
	return strings.Join(hpSlotLabels(disks), ",")
}

var pciSlots map[string]string
var pciSlotsOnce sync.Once

// pciSlotName maps a PCI BDF to the physical slot name from SMBIOS, e.g.
// "0000:05:00.0" -> "Slot 2". dmidecode is optional; if unavailable we fall
// back to the BDF itself in nvmeSlot.
func pciSlotName(bdf string) string {
	pciSlotsOnce.Do(func() {
		pciSlots = map[string]string{}
		out, err := run("dmidecode", "-t", "slot")
		if err != nil {
			return
		}
		designation := ""
		for _, line := range strings.Split(out, "\n") {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "Designation:") {
				designation = shortSlotName(strings.TrimSpace(strings.TrimPrefix(t, "Designation:")))
				continue
			}
			if strings.HasPrefix(t, "Bus Address:") && designation != "" {
				addr := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(t, "Bus Address:")))
				pciSlots[addr] = designation
				designation = ""
			}
		}
	})
	return pciSlots[strings.ToLower(bdf)]
}

func shortSlotName(s string) string {
	low := strings.ToLower(s)
	for _, prefix := range []string{"pci-e ", "pcie ", "pci "} {
		if strings.HasPrefix(low, prefix) {
			return strings.TrimSpace(s[len(prefix):])
		}
	}
	return s
}

var nvmeCtrlRe = regexp.MustCompile(`^(nvme\d+)`)

// nvmeSlot returns the physical PCIe slot for an NVMe namespace, its PCI
// BDF when SMBIOS has no slot mapping, and the BDF for override matching.
func nvmeSlot(dev string) (slot, bdf string) {
	m := nvmeCtrlRe.FindStringSubmatch(dev)
	if m == nil {
		return "", ""
	}
	ctrl := m[1]
	bdf = readFileTrim("/sys/class/nvme/" + ctrl + "/address")
	if bdf == "" {
		return ctrl, ""
	}
	if name := pciSlotName(bdf); name != "" {
		return name, bdf
	}
	if parts := strings.SplitN(bdf, ":", 2); len(parts) == 2 && len(parts[0]) == 4 {
		return parts[1], bdf
	}
	return bdf, bdf
}

var slotOverrides map[string]string
var slotOverridesOnce sync.Once

func parseSlotOverrides(data string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := strings.ToLower(fields[0])
		slot := strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
		if slot == "" {
			continue
		}
		m[key] = slot
		if strings.HasPrefix(key, "0000:") {
			m[key[5:]] = slot
		}
	}
	return m
}

// loadSlotOverrides reads an optional admin mapping. Keys may be a PCI BDF,
// kernel device name or disk serial; the value is the slot text to display.
func loadSlotOverrides() {
	slotOverrides = map[string]string{}
	path := os.Getenv("LINUX_TTY_SLOTS")
	if path == "" {
		path = "/etc/linux-tty.slots"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	slotOverrides = parseSlotOverrides(string(data))
}

func slotOverride(dev, serial, bdf string) string {
	slotOverridesOnce.Do(loadSlotOverrides)
	for _, k := range []string{dev, serial, bdf} {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" {
			continue
		}
		if v, ok := slotOverrides[k]; ok {
			return v
		}
		if strings.HasPrefix(k, "0000:") {
			if v, ok := slotOverrides[k[5:]]; ok {
				return v
			}
		}
	}
	if v := builtinSlotOverride(bdf); v != "" {
		return v
	}
	return ""
}

var productNameOnce sync.Once
var productName string

func systemProductName() string {
	productNameOnce.Do(func() {
		out, err := run("dmidecode", "-s", "system-product-name")
		if err == nil {
			productName = strings.TrimSpace(out)
		}
	})
	return productName
}

// builtinSlotOverride encodes known bifurcated-slot layouts that SMBIOS does
// not describe (Slot 6 bus address is 0000:ff:1f.7 on these Dell models).
func builtinSlotOverrideFor(product, bdf string) string {
	if !strings.EqualFold(strings.TrimSpace(product), "PowerEdge R730xd") {
		return ""
	}
	bdf = strings.ToLower(strings.TrimSpace(bdf))
	if strings.HasPrefix(bdf, "0000:") {
		bdf = bdf[5:]
	}
	switch bdf {
	case "05:00.0":
		return "Slot 6-1"
	case "06:00.0":
		return "Slot 6-2"
	}
	return ""
}

func builtinSlotOverride(bdf string) string {
	return builtinSlotOverrideFor(systemProductName(), bdf)
}

// sysfsTarget returns the SCSI target id for a block device (== VD/DG number).
func sysfsTarget(dev string) int {
	link, err := os.Readlink("/sys/class/block/" + dev)
	if err != nil {
		return -1
	}
	for _, p := range strings.Split(link, "/") {
		f := strings.Split(p, ":")
		if len(f) == 4 {
			if t, err := strconv.Atoi(f[2]); err == nil {
				return t
			}
		}
	}
	return -1
}

func compactSize(num, unit string) string {
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return strings.TrimSpace(num + unit)
	}
	switch strings.ToUpper(unit) {
	case "TB":
		if f >= 100 {
			return fmt.Sprintf("%.0fT", f)
		}
		return fmt.Sprintf("%.1fT", f)
	case "GB":
		if f >= 100 {
			return fmt.Sprintf("%.0fG", f)
		}
		return fmt.Sprintf("%.1fG", f)
	case "MB":
		return fmt.Sprintf("%.0fM", f)
	}
	return fmt.Sprintf("%.1f%s", f, strings.ToLower(unit))
}

func centerStr(s string, n int) string {
	if len(s) >= n {
		return s[:n]
	}
	left := (n - len(s)) / 2
	right := n - len(s) - left
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", right)
}

func padCell(s string, w int, center bool) string {
	s = truncateStr(s, w)
	if center {
		return centerStr(s, w)
	}
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

func truncateStr(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

// getTTYSize returns the terminal size. LINES/COLUMNS are used as a fallback
// when stdout is not a tty (pipes/tests).
func getTTYSize() (rows, cols int) {
	var ws struct {
		Row, Col, Xpixel, Ypixel uint16
	}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, os.Stdout.Fd(),
		uintptr(0x5413), uintptr(unsafe.Pointer(&ws)))
	if errno == 0 {
		rows, cols = int(ws.Row), int(ws.Col)
	}
	if rows <= 0 {
		if v, err := strconv.Atoi(os.Getenv("LINES")); err == nil {
			rows = v
		}
	}
	if cols <= 0 {
		if v, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil {
			cols = v
		}
	}
	if rows <= 0 {
		rows = 24
	}
	if cols <= 0 {
		cols = 80
	}
	return
}

// diskWidths distributes the available terminal width across the six
// columns. At 80 columns it keeps the original minimum widths; wider
// consoles get the extra space proportionally so the table fills the screen.
func diskWidths(cols int) (deviceW, slotW, healthW, serialW, lineWidth int) {
	lineWidth = cols - 2
	if lineWidth < 78 {
		lineWidth = 78
	}
	widths := []int{10, 6, 9, 10, 6, 30} // device,size,type,slot,health,serial
	const baseSum = 71
	extra := lineWidth - 78
	if extra > 0 {
		for i := range widths {
			widths[i] += extra * widths[i] / baseSum
		}
		used := 0
		for _, w := range widths {
			used += w
		}
		widths[5] += lineWidth - (used + 7)
	}
	return widths[0], widths[3], widths[4], widths[5], lineWidth
}

func showDisks() {
	fmt.Print("\033[2J\033[H")
	syslist := systemDisks()
	sysSet := map[string]bool{}
	for _, d := range syslist {
		sysSet[d] = true
	}
	if len(syslist) == 0 {
		fmt.Println("WARNING: unable to identify system disk (/) - list below may")
		fmt.Println("         include the disk hosting the OS.")
	}
	// Dedup by serial: the same physical SSD may appear as multiple block
	// devices (NVMe multi-path / multiple namespaces, e.g. nvme0n1 +
	// nvme0c0n1). Only apply dedup to NVMe devices; RAID virtual disks
	// (sdb, sdc, ...) share the controller serial but are distinct volumes.
	all := allDisks()
	deduped := []string{}
	chosen := map[string]string{} // serial -> chosen device name
	for _, dev := range all {
		if !strings.HasPrefix(dev, "nvme") {
			deduped = append(deduped, dev)
			continue
		}
		_, _, serial := diskInfo(dev)
		key := strings.TrimSpace(serial)
		if key == "" {
			deduped = append(deduped, dev)
			continue
		}
		if prev, ok := chosen[key]; ok {
			if len(dev) < len(prev) {
				for i, d := range deduped {
					if d == prev {
						deduped[i] = dev
						break
					}
				}
				chosen[key] = dev
			}
			continue
		}
		chosen[key] = dev
		deduped = append(deduped, dev)
	}

	type diskChild struct {
		slot   string
		size   string
		dt     string
		health string
		serial string
	}
	type diskRow struct {
		display  string
		size     string
		dt       string
		slot     string
		health   string
		serial   string
		children []diskChild
	}
	var rows []diskRow
	for _, dev := range deduped {
		size := diskSizeHuman(dev)
		if size == "" || size == "-" {
			continue
		}
		display := dev
		if sysSet[dev] {
			display = dev + " [SYS]"
		}
		dt := diskType(dev)
		_, _, serial := diskInfo(dev)
		slot := "-"
		health := "-"
		controllerType := false
		var children []diskChild
		// For sdX behind perccli, override udevadm controller info with
		// physical disk info. RAID mode: sysfs target == DG. Passthrough:
		// sysfs target == DID (stored as negative key -DID-1).
		if t := sysfsTarget(dev); t >= 0 && strings.HasPrefix(dev, "sd") {
			if pm, ok := diskPercMapCache(); ok {
				disks, found := pm[t]
				if !found {
					disks, found = pm[-t-1]
				}
				if !found {
					disks, found = percBySerial(serial)
				}
				if found && len(disks) > 0 {
					first := disks[0]
					controllerType = true
					if first.Intf != "" && first.Med != "" {
						dt = first.Intf + "-" + first.Med
					}
					children = nil
					if len(disks) == 1 {
						slot = percSlotLabel(first)
						health = percHealth(first)
						if first.Raid != "" {
							slot = first.Raid + ":" + slot
						}
						if first.Serial != "" {
							serial = first.Serial
						}
					} else {
						slot = first.Raid
						if slot == "" {
							slot = fmt.Sprintf("RAID(%d)", len(disks))
						}
						serial = "-"
						var childHealth []string
						for _, d := range disks {
							cdt := ""
							if d.Intf != "" && d.Med != "" {
								cdt = d.Intf + "-" + d.Med
							}
							ch := percHealth(d)
							childHealth = append(childHealth, ch)
							children = append(children, diskChild{
								slot:   percSlotLabel(d),
								size:   d.Size,
								dt:     cdt,
								health: ch,
								serial: d.Serial,
							})
						}
						health = worstHealth(childHealth...)
					}
				}
			}
		}
		// HP Smart Array: physical drives are normally hidden from the OS, so
		// use ssacli to map the logical volume (/dev/sda) back to its bays.
		if hm, ok := diskHPMapCache(); ok {
			if disks, found := hm[dev]; found && len(disks) > 0 {
				first := disks[0]
				controllerType = true
				if first.Intf != "" && first.Med != "" {
					dt = first.Intf + "-" + first.Med
				}
				labels := hpSlotLabels(disks)
				children = nil
				if len(disks) == 1 {
					if len(labels) > 0 {
						slot = labels[0]
					}
					health = hpHealth(first)
					if first.Raid != "" {
						slot = first.Raid + ":" + slot
					}
					if first.Serial != "" {
						serial = first.Serial
					}
				} else {
					slot = first.Raid
					if slot == "" {
						slot = fmt.Sprintf("RAID(%d)", len(disks))
					}
					serial = "-"
					var childHealth []string
					for i, d := range disks {
						label := ""
						if i < len(labels) {
							label = labels[i]
						}
						cdt := ""
						if d.Intf != "" && d.Med != "" {
							cdt = d.Intf + "-" + d.Med
						}
						ch := hpHealth(d)
						childHealth = append(childHealth, ch)
						children = append(children, diskChild{
							slot:   label,
							size:   d.Size,
							dt:     cdt,
							health: ch,
							serial: d.Serial,
						})
					}
					health = worstHealth(childHealth...)
				}
			}
		}
		bdf := ""
		if strings.HasPrefix(dev, "nvme") {
			auto, addr := nvmeSlot(dev)
			if slot == "-" && auto != "" {
				slot = auto
			}
			bdf = addr
		}
		if !controllerType {
			dt = refineDiskType(dev, dt)
		}
		if o := slotOverride(dev, serial, bdf); o != "" {
			slot = o
		}
		if slot == "-" {
			if t := sysfsTarget(dev); t >= 0 {
				slot = fmt.Sprintf("%02d", t)
			}
		}
		if health == "-" {
			if strings.HasPrefix(dev, "nvme") {
				health = nvmeLife(dev)
			} else {
				health = smartHealth(dev)
			}
		}
		rows = append(rows, diskRow{
			display:  display,
			size:     size,
			dt:       dt,
			slot:     slot,
			health:   health,
			serial:   serial,
			children: children,
		})
	}

	// Fill the terminal vertically. Header takes 4 lines, footer/pause takes
	// 3 lines; the remaining rows are available for disk data.
	termRows, termCols := getTTYSize()
	deviceW, slotW, healthW, serialW, lineWidth := diskWidths(termCols)
	pageLines := termRows - 7
	if pageLines < 5 {
		pageLines = 5
	}
	// Paginate by rendered lines so the page fills the terminal height.
	// A RAID tree contributes one line per physical drive.
	var pages [][]diskRow
	var pageRows []diskRow
	pageLineCount := 0
	for _, r := range rows {
		lines := 1 + len(r.children)
		if len(pageRows) > 0 && pageLineCount+lines > pageLines {
			pages = append(pages, pageRows)
			pageRows = nil
			pageLineCount = 0
		}
		pageRows = append(pageRows, r)
		pageLineCount += lines
	}
	if len(pageRows) > 0 {
		pages = append(pages, pageRows)
	}
	totalPages := len(pages)
	if totalPages == 0 {
		totalPages = 1
	}

	printHeader := func(page, total int) {
		title := fmt.Sprintf("DISKS (%d/%d)", page, total)
		pad := lineWidth - len(title) - 2
		if pad < 0 {
			pad = 0
		}
		left := pad / 2
		right := pad - left
		fmt.Printf("%s %s %s\n", strings.Repeat("=", left), title, strings.Repeat("=", right))
		fmt.Println()
		fmt.Printf("  %s %s %s %s %s %s\n",
			centerStr("DEVICE", deviceW), centerStr("SIZE", 6),
			centerStr("TYPE", 9), centerStr("SLOT", slotW),
			centerStr("HEALTH", healthW), centerStr("SERIAL", serialW))
		fmt.Println("  " + strings.Repeat("-", lineWidth-2))
	}

	if len(rows) == 0 {
		printHeader(1, 1)
		fmt.Println("  (no disk found)")
	} else {
		for page := 0; page < totalPages; page++ {
			if page > 0 {
				fmt.Print("\033[2J\033[H")
			}
			printHeader(page+1, totalPages)
			for _, r := range pages[page] {
				serial := r.serial
				if serial == "" {
					serial = "-"
				}
				health := r.health
				if health == "" {
					health = "-"
				}
				wide := lineWidth > 78
				fmt.Printf("  %s %s %s %s %s %s\n",
					padCell(r.display, deviceW, wide),
					padCell(r.size, 6, wide),
					padCell(r.dt, 9, wide),
					padCell(r.slot, slotW, wide),
					padCell(health, healthW, wide),
					padCell(serial, serialW, wide))
				// Standard tree for wide consoles; fall back to ASCII on a
				// plain vt100/serial terminal where box drawing may not exist.
				branch, lastBranch := "├─", "└─"
				term := os.Getenv("TERM")
				if term == "vt100" || term == "dumb" || term == "" {
					branch, lastBranch = "|-", "`-"
				}
				for ci, ch := range r.children {
					prefix := "  " + branch
					if ci == len(r.children)-1 {
						prefix = "  " + lastBranch
					}
					childSlot := ch.slot
					if childSlot == "" {
						childSlot = "-"
					}
					childSize := ch.size
					if childSize == "" {
						childSize = "-"
					}
					childType := ch.dt
					if childType == "" {
						childType = r.dt
					}
					childHealth := ch.health
					if childHealth == "" {
						childHealth = "-"
					}
					childSerial := ch.serial
					if childSerial == "" {
						childSerial = "-"
					}
					// Keep the tree marker in DEVICE and put the slot back
					// in SLOT so every column, especially SERIAL, aligns
					// with the table header.
					label := truncateStr(prefix, deviceW)
					wide := lineWidth > 78
					fmt.Printf("  %s %s %s %s %s %s\n",
						padCell(label, deviceW, false),
						padCell(childSize, 6, wide),
						padCell(childType, 9, wide),
						padCell(childSlot, slotW, wide),
						padCell(childHealth, healthW, wide),
						padCell(childSerial, serialW, wide))
				}
			}
			if page+1 < totalPages {
				a, intr := ask(fmt.Sprintf("\n  -- Page %d/%d -- Enter=next, q=quit: ", page+1, totalPages))
				if intr {
					return
				}
				if strings.TrimSpace(a) == "q" {
					break
				}
			}
		}
	}
	fmt.Println(strings.Repeat("=", lineWidth))
	pause()
}

// ---------- menu header ----------

func cpuInfo() (sockets, model string) {
	out, _ := run("lscpu")
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Socket(s):") {
			sockets = strings.TrimSpace(strings.TrimPrefix(line, "Socket(s):"))
		}
		if strings.HasPrefix(line, "Model name:") {
			model = strings.TrimSpace(strings.TrimPrefix(line, "Model name:"))
		}
	}
	if model == "" {
		b, _ := os.ReadFile("/proc/cpuinfo")
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "model name") {
				f := strings.SplitN(line, ":", 2)
				if len(f) == 2 {
					model = strings.TrimSpace(f[1])
				}
				break
			}
		}
	}
	if sockets == "" {
		// count unique physical ids
		seen := map[string]bool{}
		b, _ := os.ReadFile("/proc/cpuinfo")
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "physical id") {
				f := strings.SplitN(line, ":", 2)
				if len(f) == 2 {
					seen[strings.TrimSpace(f[1])] = true
				}
			}
		}
		sockets = strconv.Itoa(len(seen))
		if sockets == "0" {
			sockets = "1"
		}
	}
	if model == "" {
		model = "unknown"
	}
	return
}

// ipEntry is one line of "ip -o addr show" we want to display.
type ipEntry struct {
	nic string
	ip  string
	v6  bool
}

func drawMenu() {
	fmt.Print("\033[2J\033[H")
	ipOut, _ := run("ip", "-o", "addr", "show", "scope", "global")
	var entries []ipEntry
	seen := map[string]bool{}
	for _, line := range strings.Split(ipOut, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		nic := f[1]
		// keep real NICs (have /device) and bond masters (have /bonding); skip bridge/veth/dummy/tun
		if _, err := os.Stat("/sys/class/net/" + nic + "/device"); err != nil {
			if _, err := os.Stat("/sys/class/net/" + nic + "/bonding"); err != nil {
				continue
			}
		}
		ip := f[3]
		key := nic + "/" + ip
		if seen[key] {
			continue
		}
		seen[key] = true
		entries = append(entries, ipEntry{nic: nic, ip: ip, v6: f[2] == "inet6"})
	}
	// Stable order: by NIC, then IPv4 before IPv6, then by address string.
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].nic != entries[j].nic {
			return entries[i].nic < entries[j].nic
		}
		if entries[i].v6 != entries[j].v6 {
			return !entries[i].v6
		}
		return entries[i].ip < entries[j].ip
	})
	var ips []string
	for _, e := range entries {
		if e.v6 {
			ips = append(ips, e.nic+"["+e.ip+"]")
		} else {
			ips = append(ips, e.nic+"("+e.ip+")")
		}
	}
	ipStr := strings.Join(ips, " ")
	if ipStr == "" {
		ipStr = "(no IP address yet)"
	}
	// Clip extremely long multi-IP lines so the menu never wraps.
	if len(ipStr) > 60 {
		ipStr = ipStr[:57] + "..."
	}
	mem := "unknown"
	if memData, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(memData), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					if kb, err := strconv.Atoi(fields[1]); err == nil {
						if kb >= 1024*1024 {
							mem = fmt.Sprintf("%.1fG", float64(kb)/(1024*1024))
						} else if kb >= 1024 {
							mem = fmt.Sprintf("%.0fM", float64(kb)/1024)
						} else {
							mem = fmt.Sprintf("%dK", kb)
						}
					}
				}
				break
			}
		}
	}
	boot := "Legacy-BIOS"
	if st, err := os.Stat("/sys/firmware/efi"); err == nil && st.IsDir() {
		boot = "UEFI"
	}
	host, _ := os.Hostname()
	sockets, model := cpuInfo()
	threads := 0
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "processor") {
				threads++
			}
		}
	}
	kernel := readFileTrim("/proc/sys/kernel/osrelease")

	w := 72
	// Truncate into a LOCAL title so we never mutate the global `rel` across
	// redraws.
	title := rel
	if len(title) > w-4 {
		title = title[:w-4]
	}
	tp := (w - len(title) - 2) / 2
	if tp < 0 {
		tp = 0
	}
	right := w - tp - len(title) - 2
	if right < 0 {
		right = 0
	}
	fmt.Printf("%s %s %s\n", strings.Repeat("=", tp), title, strings.Repeat("=", right))
	fmt.Printf(" Hostname : %s\n", host)
	if len(model) > 40 {
		model = model[:40]
	}
	fmt.Printf(" CPU      : %s x %s (%d threads)\n", sockets, model, threads)
	fmt.Printf(" Memory   : %s\n", mem)
	fmt.Printf(" Kernel   : %-26s Boot : %s\n", kernel, boot)
	fmt.Printf(" Version  : %s\n", version)
	fmt.Printf(" IP       : %s\n", ipStr)
	fmt.Println(strings.Repeat("-", w))
	fmt.Println("  1. Configure network - static IP")
	fmt.Println("  2. Configure network - DHCP")
	fmt.Println("  3. Show disks")
	fmt.Println("  4. Ping test")
	fmt.Println(strings.Repeat("-", w))
	fmt.Println("  5. Reboot")
	fmt.Println("  6. Power off")
	fmt.Println("  7. Exit and enter the system")
	fmt.Println("  8. Update tty")
	fmt.Println(strings.Repeat("=", w))
}

// ---------- main ----------

func main() {
	// clear screen on startup
	fmt.Print("\033[2J\033[H")
	// root check
	if os.Geteuid() != 0 {
		fmt.Println("ERROR: this menu must run as root.")
		time.Sleep(3 * time.Second)
		os.Exit(1)
	}
	detectDistro()
	rel = readFileTrim("/etc/linux-tty-release")
	if rel == "" {
		rel = "SSCLOUD Linux-TTY Console"
	}
	// TERM fallback
	term := os.Getenv("TERM")
	switch term {
	case "", "dumb", "unknown":
		t, _ := run("tty")
		if strings.HasPrefix(t, "/dev/ttyS") || strings.HasPrefix(t, "/dev/ttyAMA") {
			os.Setenv("TERM", "vt100")
		} else {
			os.Setenv("TERM", "linux")
		}
	}
	// verify terminfo
	if _, err := exec.LookPath("infocmp"); err == nil {
		if c := exec.Command("infocmp", os.Getenv("TERM")); c.Run() != nil {
			os.Setenv("TERM", "vt100")
		}
	} else {
		os.Setenv("TERM", "vt100")
	}
	// Detect whether stdin is a real terminal. A character device such as
	// /dev/null also has ModeCharDevice set, so verify with TCGETS.
	if fi, err := os.Stdin.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) != 0 {
		if _, terr := tcGet(int(os.Stdin.Fd())); terr == nil {
			stdinIsTty = true
		}
	}
	detectTTY()
	// cbreak mode (char-by-char, no echo, VMIN=0/VTIME=2)
	enterCbreak()
	// disable sysrq while the menu owns the console; cleanup restores it on
	// every normal exit path, including signals and agetty re-exec failures.
	disableSysrq()
	initSignals()
	migrateIfcfgBackups()
	defer cleanup()

	for {
		drawMenu()
		choice, intr := ask("Select [1-8]: ")
		if eofFlag.Load() {
			return
		}
		if intr {
			interrupt.Store(0)
			continue
		}
		choice = strings.TrimSpace(choice)
		switch choice {
		case "1":
			staticConfig()
			if interrupt.Load() == 1 {
				interrupt.Store(0)
				continue
			}
			pause()
		case "2":
			dhcpConfig()
		case "3":
			showDisks()
		case "4":
			pingTest()
		case "5":
			if confirm("Reboot now?") {
				fmt.Println("Rebooting ...")
				syscall.Sync()
				if err := exec.Command("systemctl", "reboot").Run(); err != nil {
					fmt.Printf("ERROR: reboot failed: %v\n", err)
					pause()
					continue
				}
				time.Sleep(10 * time.Second)
			}
		case "6":
			if confirm("Power off now?") {
				fmt.Println("Powering off ...")
				syscall.Sync()
				if err := exec.Command("systemctl", "poweroff").Run(); err != nil {
					fmt.Printf("ERROR: poweroff failed: %v\n", err)
					pause()
					continue
				}
				time.Sleep(10 * time.Second)
			}
		case "7":
			// Resolve agetty FIRST, so a missing binary does not leave us in
			// canonical mode with no way to get back to cbreak.
			exe, err := exec.LookPath("agetty")
			if err != nil {
				fmt.Println("ERROR: agetty not found - cannot drop to login prompt.")
				pause()
				continue
			}
			if myTTY == "" {
				fmt.Println("ERROR: cannot determine the current tty.")
				pause()
				continue
			}
			cleanup()
			argv := []string{"agetty", "--noclear", myTTY, "linux"}
			if err := syscall.Exec(exe, argv, os.Environ()); err != nil {
				// Exec only returns on failure.
				fmt.Printf("ERROR: exec agetty: %v\n", err)
				enterCbreak()
				disableSysrq()
				pause()
				continue
			}
			// Unreachable on success.
		case "8":
			// Pull and run the official tty.sh installer. The installer is
			// asked to skip its own restart (LINUX_TTY_NO_RESTART) so this
			// menu can restart exactly the tty it is running on.
			if myTTY == "" || !validTTYName(myTTY) {
				fmt.Println("ERROR: cannot determine a safe tty name for update.")
				pause()
				continue
			}
			var updateCmd string
			if _, err := exec.LookPath("curl"); err == nil {
				updateCmd = "curl -fsSL https://bash.niteng.net/iso/tty.sh | bash"
			} else if _, err := exec.LookPath("wget"); err == nil {
				updateCmd = "wget -qO- https://bash.niteng.net/iso/tty.sh | bash"
			} else {
				fmt.Println("ERROR: online update needs curl or wget.")
				pause()
				continue
			}
			fmt.Println("Updating linux-tty ...")
			cleanup()
			c := exec.Command("bash", "-o", "pipefail", "-c", updateCmd)
			env := make([]string, 0, len(os.Environ())+2)
			for _, e := range os.Environ() {
				if strings.HasPrefix(e, "LINUX_TTY_TARGET=") || strings.HasPrefix(e, "LINUX_TTY_NO_RESTART=") {
					continue
				}
				env = append(env, e)
			}
			env = append(env, "LINUX_TTY_TARGET="+myTTY, "LINUX_TTY_NO_RESTART=1")
			c.Env = env
			c.Stdin = os.Stdin
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			err := c.Run()
			fmt.Println()
			enterCbreak()
			disableSysrq()
			if err != nil {
				fmt.Printf("ERROR: update failed: %v\n", err)
				pause()
				continue
			}
			unit := "linux-tty@" + myTTY + ".service"
			fmt.Printf("Update installed; restarting %s ...\n", unit)
			if err := exec.Command("systemctl", "restart", unit).Run(); err != nil {
				fmt.Printf("ERROR: restart %s failed: %v\n", unit, err)
				pause()
				continue
			}
			// systemd normally replaces this process; wait as a fallback.
			time.Sleep(5 * time.Second)
		case "666":
			// hidden entry: passwordless root shell (VT consoles only)
			if !isVTConsole(myTTY) {
				fmt.Println("Not available on this console.")
				time.Sleep(1 * time.Second)
				break
			}
			if _, err := os.Stat("/usr/local/sbin/linux-root-shell"); err != nil {
				fmt.Println("ERROR: linux-root-shell not installed.")
				pause()
				continue
			}
			exe, err := exec.LookPath("agetty")
			if err != nil {
				fmt.Println("ERROR: agetty not found - cannot drop to root shell.")
				pause()
				continue
			}
			if myTTY == "" {
				fmt.Println("ERROR: cannot determine the current tty.")
				pause()
				continue
			}
			cleanup()
			// Directly exec agetty — replaces this process image,
			// no systemd Conflicts race. agetty runs root shell;
			// on exit, linux-root-shell exec's back to linux-tty.
			argv := []string{
				"agetty", "--autologin", "root",
				"--login-program", "/usr/local/sbin/linux-root-shell",
				"--noclear", myTTY, "linux",
			}
			if err := syscall.Exec(exe, argv, os.Environ()); err != nil {
				fmt.Printf("ERROR: exec agetty: %v\n", err)
				enterCbreak()
				disableSysrq()
				pause()
				continue
			}
			// Unreachable on success.
		default:
			// empty / invalid: loop redraws
		}
		// clear any leftover interrupt flag from sub-functions
		if interrupt.Load() == 1 {
			interrupt.Store(0)
			continue
		}
	}
}
