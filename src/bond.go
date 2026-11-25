package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type bondSettings struct {
	name    string
	slaves  []string
	mode    string
	mac     string
	dhcp    bool
	ip      string
	netmask string
	gateway string
	dns1    string
	dns2    string

	renameFrom string
	renameTo   string
	renameMAC  string
}

var bondMACRe = regexp.MustCompile(`^[0-9a-fA-F]{2}(:[0-9a-fA-F]{2}){5}$`)
var bondNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func bondNameOK(s string) bool {
	return s != "" && len(s) <= 15 && bondNameRe.MatchString(s)
}

func bondMACOK(s string) bool {
	return s == "" || bondMACRe.MatchString(s)
}

func bondIfaceExists(name string) bool {
	_, err := os.Stat("/sys/class/net/" + name)
	return err == nil
}

func bondIsMaster(name string) bool {
	_, err := os.Stat("/sys/class/net/" + name + "/bonding")
	return err == nil
}

func bondFreeRename(base string) string {
	base = base + "old"
	if len(base) > 15 {
		base = base[:15]
	}
	cand := base
	for i := 1; bondIfaceExists(cand); i++ {
		suffix := strconv.Itoa(i)
		if len(base)+len(suffix) > 15 {
			cand = base[:15-len(suffix)] + suffix
		} else {
			cand = base + suffix
		}
	}
	return cand
}

func bondLinkFile(to string) string {
	return "/etc/systemd/network/10-linux-tty-" + to + ".link"
}

func bondWriteRenameRule(from, to string) {
	mac := readFileTrim("/sys/class/net/" + from + "/address")
	if mac == "" {
		return
	}
	os.MkdirAll("/etc/systemd/network", 0755)
	content := "[Match]\nMACAddress=" + mac + "\n\n[Link]\nName=" + to + "\n"
	writeFileAtomic(bondLinkFile(to), content, 0644)
	// Also add a udev NAME rule for older udev implementations. Keep the
	// file line-based so existing local rules are preserved.
	rulePath := "/etc/udev/rules.d/70-linux-tty-bond-rename.rules"
	rule := "SUBSYSTEM==\"net\", ACTION==\"add\", ATTR{address}==\"" + strings.ToLower(mac) + "\", NAME=\"" + to + "\"\n"
	old, _ := os.ReadFile(rulePath)
	if !strings.Contains(string(old), "ATTR{address}==\""+strings.ToLower(mac)+"\"") {
		f, err := os.OpenFile(rulePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err == nil {
			f.WriteString(rule)
			f.Close()
		}
	}
}

func bondRemoveRenameRule(to string) {
	os.Remove(bondLinkFile(to))
	rulePath := "/etc/udev/rules.d/70-linux-tty-bond-rename.rules"
	data, err := os.ReadFile(rulePath)
	if err != nil {
		return
	}
	var kept []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, `NAME="`+to+`"`) {
			continue
		}
		if strings.TrimSpace(line) != "" {
			kept = append(kept, line)
		}
	}
	if len(kept) == 0 {
		os.Remove(rulePath)
		return
	}
	writeFileAtomic(rulePath, strings.Join(kept, "\n")+"\n", 0644)
}

func bondDetachFromMaster(name, target string) {
	link, err := os.Readlink("/sys/class/net/" + name + "/master")
	if err != nil {
		return
	}
	master := filepath.Base(link)
	run("ip", "link", "set", "dev", name, "nomaster")
	if master != "" && master != target && bondIsMaster(master) {
		run("ip", "link", "set", master, "down")
		run("ip", "link", "del", master)
		switch netBackend {
		case "rhel":
			os.Remove(filepath.Join(ifcfgDir, "ifcfg-"+master))
		case "debian":
			debianRewriteBlock(master, "# replaced by "+target)
		}
	}
}

func bondRenameExisting(c bondSettings) bool {
	if c.renameFrom == "" || c.renameTo == "" {
		return true
	}
	bondDetachFromMaster(c.renameFrom, c.name)
	run("ip", "link", "set", "dev", c.renameFrom, "down")
	if out, err := run("ip", "link", "set", "dev", c.renameFrom, "name", c.renameTo); err != nil {
		fmt.Printf("ERROR: rename %s -> %s failed: %s\n", c.renameFrom, c.renameTo, out)
		return false
	}
	if !bondIfaceExists(c.renameTo) {
		fmt.Printf("ERROR: renamed interface %s not found.\n", c.renameTo)
		return false
	}
	bondWriteRenameRule(c.renameTo, c.renameTo)
	// Persist the rename in the native config before the bond writer replaces
	// the slave stanza.
	switch netBackend {
	case "rhel":
		oldPath := filepath.Join(ifcfgDir, "ifcfg-"+c.renameFrom)
		if b, err := os.ReadFile(oldPath); err == nil {
			text := string(b)
			reDev := regexp.MustCompile(`(?m)^DEVICE=.*$`)
			reName := regexp.MustCompile(`(?m)^NAME=.*$`)
			text = reDev.ReplaceAllString(text, "DEVICE="+c.renameTo)
			text = reName.ReplaceAllString(text, "NAME="+c.renameTo)
			writeFileAtomic(filepath.Join(ifcfgDir, "ifcfg-"+c.renameTo), text, 0644)
			os.Remove(oldPath)
		}
	case "debian":
		// Remove the old iface/auto stanza; bondWriteDebian will create the
		// renamed slave stanza explicitly.
		debianRewriteBlock(c.renameFrom, "# renamed to "+c.renameTo)
	}
	return true
}

func bondCandidates() []string {
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if n == "lo" || strings.HasPrefix(n, "veth") || strings.HasPrefix(n, "docker") ||
			strings.HasPrefix(n, "virbr") || strings.HasPrefix(n, "tun") || strings.HasPrefix(n, "tap") ||
			strings.HasPrefix(n, "br-") || strings.HasPrefix(n, "vnet") {
			continue
		}
		if _, err := os.Stat("/sys/class/net/" + n + "/device"); err != nil {
			continue
		}
		if bondIsMaster(n) {
			continue
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func bondPrompt(prompt, def string) (string, bool) {
	if def != "" {
		prompt = fmt.Sprintf("%s [%s]: ", prompt, def)
	} else if !strings.HasSuffix(prompt, ": ") {
		prompt += ": "
	}
	v, intr := ask(prompt)
	if intr {
		return "", true
	}
	v = strings.TrimSpace(v)
	if v == "" {
		v = def
	}
	return v, false
}

func bondConfirm(prompt string) bool {
	v, intr := ask(prompt + " [y/N]: ")
	if intr {
		return false
	}
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "y" || v == "yes"
}

func bondReadIPv4(def string) (string, string, bool) {
	for i := 0; i < maxTries; i++ {
		v, intr := bondPrompt("Bond IPv4 address (CIDR or IP; prefix optional)", def)
		if intr {
			return "", "", false
		}
		if v == "" {
			continue
		}
		if !strings.Contains(v, "/") {
			v += "/24"
		}
		ip, ipnet, err := net.ParseCIDR(v)
		if err != nil || ip.To4() == nil {
			fmt.Println("ERROR: invalid IPv4 address/prefix.")
			continue
		}
		ones, _ := ipnet.Mask.Size()
		return ip.String(), prefixToMask(ones), true
	}
	return "", "", false
}

func bondReadGateway(ip, mask, def string) string {
	suggest := def
	if suggest == "" {
		suggest = suggestGateway(net.ParseIP(ip).To4(), net.IPMask(net.ParseIP(mask).To4()))
	}
	for i := 0; i < maxTries; i++ {
		v, intr := bondPrompt("Gateway (blank/'none' = no default route)", suggest)
		if intr {
			return ""
		}
		v = strings.TrimSpace(v)
		if v == "" || v == "none" || v == "-" {
			return ""
		}
		if !isValidIPv4(v) {
			fmt.Println("ERROR: invalid IPv4 gateway.")
			continue
		}
		if v == ip {
			fmt.Println("ERROR: gateway cannot equal the bond IP.")
			continue
		}
		return v
	}
	return ""
}

func bondReadDNS() (string, string) {
	d := readResolvConfDNS()
	d1, d2 := "223.6.6.6", "223.5.5.5"
	if len(d) > 0 && usableNameserver(d[0]) {
		d1 = d[0]
	}
	if len(d) > 1 && usableNameserver(d[1]) {
		d2 = d[1]
	}
	v, intr := bondPrompt("DNS1", d1)
	if intr {
		return "", ""
	}
	w, _ := bondPrompt("DNS2", d2)
	if !usableNameserver(v) {
		v = d1
	}
	if !usableNameserver(w) {
		w = d2
	}
	return v, w
}

func ensureIfenslave() bool {
	if _, err := exec.LookPath("ifenslave"); err == nil {
		return true
	}
	// Debian 12 ships ifenslave as ifupdown hooks only; there is no
	// /usr/sbin/ifenslave binary. The hook is what actually enslaves ports.
	if fileExists("/etc/network/if-pre-up.d/ifenslave") &&
		fileExists("/etc/network/if-up.d/ifenslave") {
		return true
	}
	fmt.Println("ifenslave is required for Debian bond configuration.")
	if !bondConfirm("Install ifenslave with apt-get now?") {
		fmt.Println("Cancelled: ifenslave is required.")
		return false
	}
	c := exec.Command("apt-get", "update")
	c.Stdout, c.Stderr, c.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := c.Run(); err != nil {
		fmt.Printf("ERROR: apt-get update failed: %v\n", err)
		return false
	}
	c = exec.Command("apt-get", "install", "-y", "ifenslave")
	c.Stdout, c.Stderr, c.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := c.Run(); err != nil {
		fmt.Printf("ERROR: apt-get install ifenslave failed: %v\n", err)
		return false
	}
	_, err := exec.LookPath("ifenslave")
	return err == nil
}

func bondModeOptions(mode string) string {
	opts := "mode=" + mode + " miimon=100"
	if mode == "802.3ad" {
		opts += " lacp_rate=fast xmit_hash_policy=layer3+4"
	}
	return opts
}

func bondDebianModeOpts(mode string) []string {
	out := []string{"bond-mode " + mode, "bond-miimon 100"}
	if mode == "802.3ad" {
		out = append(out, "bond-lacp-rate fast", "bond-xmit-hash-policy layer3+4")
	}
	return out
}

func bondWriteRHEL(c bondSettings, nmControlled bool) error {
	os.MkdirAll(ifcfgDir, 0755)
	nm := "no"
	if nmControlled {
		nm = "yes"
	}
	master := "TYPE=Bond\nBONDING_MASTER=yes\n"
	master += "DEVICE=" + c.name + "\nNAME=" + c.name + "\n"
	master += "ONBOOT=yes\nNM_CONTROLLED=" + nm + "\n"
	master += "IPV4_FAILURE_FATAL=no\n"
	if c.dhcp {
		master += "BOOTPROTO=dhcp\n"
	} else {
		master += "BOOTPROTO=none\n"
		master += "IPADDR=" + c.ip + "\nNETMASK=" + c.netmask + "\n"
		if c.gateway != "" {
			master += "GATEWAY=" + c.gateway + "\n"
		}
		if c.dns1 != "" {
			master += "DNS1=" + c.dns1 + "\n"
		}
		if c.dns2 != "" {
			master += "DNS2=" + c.dns2 + "\n"
		}
		master += "PEERDNS=no\n"
	}
	if c.mac != "" {
		master += "MACADDR=" + c.mac + "\n"
	}
	master += "BONDING_OPTS=\"" + bondModeOptions(c.mode) + "\"\n"
	if err := writeFileAtomic(filepath.Join(ifcfgDir, "ifcfg-"+c.name), master, 0644); err != nil {
		return err
	}
	for _, s := range c.slaves {
		slave := "TYPE=Ethernet\nDEVICE=" + s + "\nNAME=" + s + "\n"
		slave += "BOOTPROTO=none\nONBOOT=yes\nNM_CONTROLLED=" + nm + "\n"
		slave += "MASTER=" + c.name + "\nSLAVE=yes\n"
		if err := writeFileAtomic(filepath.Join(ifcfgDir, "ifcfg-"+s), slave, 0644); err != nil {
			return err
		}
	}
	return nil
}

func bondWriteDebian(c bondSettings) error {
	for _, s := range c.slaves {
		block := "auto " + s + "\n" +
			"iface " + s + " inet manual\n" +
			"    bond-master " + c.name + "\n"
		if err := debianRewriteBlock(s, block); err != nil {
			return err
		}
	}
	var b strings.Builder
	b.WriteString("auto " + c.name + "\n")
	if c.dhcp {
		b.WriteString("iface " + c.name + " inet dhcp\n")
	} else {
		b.WriteString("iface " + c.name + " inet static\n")
		b.WriteString("    address " + c.ip + "\n")
		b.WriteString("    netmask " + c.netmask + "\n")
		if c.gateway != "" {
			b.WriteString("    gateway " + c.gateway + "\n")
		}
		if c.dns1 != "" {
			b.WriteString("    dns-nameservers " + c.dns1)
			if c.dns2 != "" {
				b.WriteString(" " + c.dns2)
			}
			b.WriteString("\n")
		}
	}
	for _, line := range bondDebianModeOpts(c.mode) {
		b.WriteString("    " + line + "\n")
	}
	b.WriteString("    bond-slaves " + strings.Join(c.slaves, " ") + "\n")
	if c.mac != "" {
		b.WriteString("    hwaddress ether " + c.mac + "\n")
	}
	return debianRewriteBlock(c.name, b.String())
}

func bondWriteNetplan(c bondSettings) error {
	renderer := netplanRendererFor(c.name)
	if renderer == "" {
		renderer = "networkd"
	}
	var b strings.Builder
	b.WriteString("network:\n  version: 2\n  renderer: " + renderer + "\n  ethernets:\n")
	for _, s := range c.slaves {
		b.WriteString("    " + s + ":\n      dhcp4: false\n      optional: true\n")
	}
	b.WriteString("  bonds:\n    " + c.name + ":\n      interfaces: [" + strings.Join(c.slaves, ", ") + "]\n")
	if c.dhcp {
		b.WriteString("      dhcp4: true\n")
	} else {
		prefix := "24"
		if m := net.ParseIP(c.netmask).To4(); m != nil {
			prefix = strconv.Itoa(maskSize(m))
		}
		b.WriteString("      dhcp4: false\n      addresses: [" + c.ip + "/" + prefix + "]\n")
		if c.gateway != "" {
			b.WriteString("      routes:\n        - to: default\n          via: " + c.gateway + "\n")
		}
	}
	if c.dns1 != "" {
		b.WriteString("      nameservers:\n        addresses: [" + c.dns1)
		if c.dns2 != "" {
			b.WriteString(", " + c.dns2)
		}
		b.WriteString("]\n")
	}
	if c.mac != "" {
		b.WriteString("      macaddress: " + c.mac + "\n")
	}
	b.WriteString("      parameters:\n        mode: " + c.mode + "\n        mii-monitor-interval: 100\n")
	if c.mode == "802.3ad" {
		b.WriteString("        lacp-rate: fast\n        transmit-hash-policy: layer3+4\n")
	}
	return writeFileAtomic(netplanConfigPath, b.String(), 0600)
}

func bondLoadModule() {
	run("modprobe", "bonding")
	os.MkdirAll("/etc/modules-load.d", 0755)
	writeFileAtomic("/etc/modules-load.d/bonding.conf", "bonding\n", 0644)
	exec.Command("systemctl", "enable", "--now", "systemd-modules-load.service").Run()
}

func bondSetMAC(name, mac string) {
	if mac == "" {
		return
	}
	if out, err := run("ip", "link", "set", "dev", name, "address", mac); err != nil {
		run("ip", "link", "set", "dev", name, "down")
		run("ip", "link", "set", "dev", name, "address", mac)
		run("ip", "link", "set", "dev", name, "up")
		fmt.Printf("MAC set warning: %s\n", strings.TrimSpace(out))
	}
}

func bondPrepareSlaves(c bondSettings) {
	for _, s := range c.slaves {
		run("ip", "link", "set", "dev", s, "nomaster")
		run("ip", "link", "set", "dev", s, "down")
		run("ip", "-4", "addr", "flush", "dev", s)
		run("ip", "-6", "addr", "flush", "dev", s, "scope", "global")
	}
}

func bondApply(c bondSettings) bool {
	bondLoadModule()
	bondPrepareSlaves(c)
	switch netBackend {
	case "rhel":
		if isNMActive() {
			run("nmcli", "con", "reload")
			run("nmcli", "con", "up", c.name)
			for _, s := range c.slaves {
				run("nmcli", "con", "up", s)
			}
			// Some CentOS 7 NetworkManager versions import ifcfg-bond0
			// without wiring the slaves. Fall back to ifup if NM did not
			// bring the bond all the way up.
			if !bondReady(c) {
				run("ifdown", c.name)
				for _, s := range c.slaves {
					run("ifdown", s)
				}
				run("ifup", c.name)
				for _, s := range c.slaves {
					run("ifup", s)
				}
			}
		} else {
			run("ifdown", c.name)
			for _, s := range c.slaves {
				run("ifdown", s)
			}
			run("ifup", c.name)
			for _, s := range c.slaves {
				run("ifup", s)
			}
		}
	case "debian":
		run("ifdown", c.name)
		for _, s := range c.slaves {
			run("ifdown", s)
		}
		run("ifup", c.name)
		for _, s := range c.slaves {
			run("ifup", s)
		}
		// ifup handles bond-slaves, but restart networking is the fallback
		// for ifupdown versions that do not auto-enslave.
		if !bondReady(c) {
			run("systemctl", "restart", "networking")
		}
	case "netplan":
		netplanApply()
	}
	bondSetMAC(c.name, c.mac)
	for i := 0; i < 20; i++ {
		if bondReady(c) {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

func bondReady(c bondSettings) bool {
	if !bondIfaceExists(c.name) {
		return false
	}
	if _, err := os.Stat("/proc/net/bonding/" + c.name); err != nil {
		return false
	}
	out, _ := run("ip", "-4", "-o", "addr", "show", c.name)
	return strings.Contains(out, "inet ")
}

func bondBackup(c bondSettings) string {
	os.MkdirAll(bakDir, 0700)
	dir := filepath.Join(bakDir, "bond."+time.Now().Format("20060102_150405"))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return ""
	}
	switch netBackend {
	case "rhel":
		names := append([]string{c.name}, c.slaves...)
		if c.renameFrom != "" {
			names = append(names, c.renameFrom)
		}
		for _, n := range names {
			src := filepath.Join(ifcfgDir, "ifcfg-"+n)
			if b, err := os.ReadFile(src); err == nil {
				os.WriteFile(filepath.Join(dir, "ifcfg-"+n), b, 0600)
			}
		}
	case "debian":
		if b, err := os.ReadFile(interfacesF); err == nil {
			os.WriteFile(filepath.Join(dir, "interfaces"), b, 0600)
		}
	case "netplan":
		entries, _ := os.ReadDir("/etc/netplan")
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if b, err := os.ReadFile(filepath.Join("/etc/netplan", e.Name())); err == nil {
				os.WriteFile(filepath.Join(dir, e.Name()), b, 0600)
			}
		}
	}
	if b, err := os.ReadFile("/etc/modules-load.d/bonding.conf"); err == nil {
		os.WriteFile(filepath.Join(dir, "bonding.conf"), b, 0600)
	}
	return dir
}

func bondRestore(dir string, c bondSettings) {
	if dir == "" {
		return
	}
	run("ip", "link", "set", c.name, "down")
	if c.renameFrom != "" && c.renameTo != "" {
		if bondIsMaster(c.name) {
			run("ip", "link", "del", c.name)
		}
		if bondIfaceExists(c.renameTo) && !bondIfaceExists(c.renameFrom) {
			run("ip", "link", "set", c.renameTo, "down")
			run("ip", "link", "set", "dev", c.renameTo, "name", c.renameFrom)
		}
		bondRemoveRenameRule(c.renameTo)
	}
	switch netBackend {
	case "rhel":
		entries, _ := os.ReadDir(dir)
		have := map[string]bool{}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "ifcfg-") {
				have[e.Name()] = true
			}
		}
		for _, n := range append([]string{c.name}, c.slaves...) {
			target := filepath.Join(ifcfgDir, "ifcfg-"+n)
			if !have["ifcfg-"+n] {
				os.Remove(target)
			}
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "ifcfg-") {
				b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
				os.WriteFile(filepath.Join(ifcfgDir, e.Name()), b, 0644)
			}
		}
		if isNMActive() {
			run("nmcli", "con", "reload")
		}
		run("systemctl", "restart", "network")
	case "debian":
		if b, err := os.ReadFile(filepath.Join(dir, "interfaces")); err == nil {
			os.WriteFile(interfacesF, b, 0644)
		}
		run("systemctl", "restart", "networking")
	case "netplan":
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.Name() == "bonding.conf" {
				continue
			}
			b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			os.WriteFile(filepath.Join("/etc/netplan", e.Name()), b, 0600)
		}
		netplanApply()
	}
}

func bondPrintStatus(c bondSettings) {
	fmt.Println()
	fmt.Println("Bond status:")
	if out, err := run("ip", "-br", "addr", "show", c.name); err == nil && out != "" {
		fmt.Println("  " + out)
	}
	if out, err := run("cat", "/proc/net/bonding/"+c.name); err == nil {
		for _, line := range strings.Split(out, "\n") {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "Bonding Mode:") || strings.HasPrefix(t, "MII Status:") {
				fmt.Println("  " + t)
			}
		}
	}
	if c.mac != "" {
		if out, err := run("ip", "link", "show", c.name); err == nil {
			for _, line := range strings.Split(out, "\n") {
				if strings.Contains(line, "link/ether") {
					fmt.Println("  " + strings.TrimSpace(line))
					break
				}
			}
		}
	}
}

func bondConfig() {
	fmt.Println("===================== NETWORK CONFIG (bond0) =====================")
	nics := bondCandidates()
	if len(nics) < 2 {
		fmt.Println("ERROR: at least two physical NICs are required for a bond.")
		fmt.Println("       Found:", strings.Join(nics, " "))
		pause()
		return
	}
	fmt.Println("Physical interfaces:")
	for i, n := range nics {
		fmt.Printf("  %d. %-16s %s\n", i+1, n, nicLinkLabel(n))
	}
	var slaves []string
	for {
		sel, intr := ask("Select 2 or more slave interfaces (space separated, e.g. 1 2): ")
		if intr {
			return
		}
		slaves = nil
		seen := map[int]bool{}
		ok := true
		for _, p := range strings.Fields(sel) {
			n, err := strconv.Atoi(p)
			if err != nil || n < 1 || n > len(nics) {
				ok = false
				break
			}
			if seen[n] {
				continue
			}
			seen[n] = true
			slaves = append(slaves, nics[n-1])
		}
		if ok && len(slaves) >= 2 {
			break
		}
		fmt.Println("ERROR: choose at least two different interfaces by number.")
	}
	name, intr := bondPrompt("Bond interface name", "bond0")
	if intr {
		return
	}
	if !bondNameOK(name) {
		fmt.Println("ERROR: invalid interface name (max 15 chars: letters, digits, . _ -).")
		pause()
		return
	}
	renameFrom, renameTo := "", ""
	renameMAC := ""
	if bondIfaceExists(name) && !bondIsMaster(name) {
		found := -1
		for i, s := range slaves {
			if s == name {
				found = i
				break
			}
		}
		if found < 0 {
			fmt.Printf("ERROR: %s already exists and is not a bond master.\n", name)
			fmt.Println("       Select it as a slave if you want it renamed and reused, or choose another bond name.")
			pause()
			return
		}
		renameFrom = name
		renameTo = bondFreeRename(name)
		renameMAC = readFileTrim("/sys/class/net/" + name + "/address")
		fmt.Printf("NOTE: %s already exists. It will be renamed to %s and used as a slave.\n", name, renameTo)
		if !bondConfirm("Rename " + name + " -> " + renameTo + " and create bond " + name + "?") {
			fmt.Println("Cancelled.")
			pause()
			return
		}
		slaves[found] = renameTo
	}
	mac, intr := bondPrompt("Bond MAC address (blank = keep hardware MAC)", "")
	if intr {
		return
	}
	if !bondMACOK(mac) {
		fmt.Println("ERROR: invalid MAC address, expected aa:bb:cc:dd:ee:ff.")
		pause()
		return
	}
	fmt.Println("Bond mode:")
	fmt.Println("  1. 802.3ad LACP (default)")
	fmt.Println("  2. active-backup")
	fmt.Println("  3. balance-rr")
	modeSel, intr := bondPrompt("Select mode", "1")
	if intr {
		return
	}
	mode := "802.3ad"
	switch modeSel {
	case "2":
		mode = "active-backup"
	case "3":
		mode = "balance-rr"
	}
	fmt.Println("IPv4 mode:")
	fmt.Println("  1. static (default)")
	fmt.Println("  2. DHCP")
	ipSel, intr := bondPrompt("Select IPv4 mode", "1")
	if intr {
		return
	}
	c := bondSettings{name: name, slaves: slaves, mode: mode, mac: mac, dhcp: ipSel == "2",
		renameFrom: renameFrom, renameTo: renameTo, renameMAC: renameMAC}
	if !c.dhcp {
		ip, mask, ok := bondReadIPv4("")
		if !ok {
			return
		}
		c.ip, c.netmask = ip, mask
		c.gateway = bondReadGateway(ip, mask, "")
		c.dns1, c.dns2 = bondReadDNS()
	}
	fmt.Println()
	fmt.Printf("Bond %s: %s\n", c.name, strings.Join(c.slaves, " + "))
	fmt.Printf("Mode: %s  MAC: %s  IPv4: %s\n", c.mode, func() string {
		if c.mac == "" {
			return "auto"
		}
		return c.mac
	}(), func() string {
		if c.dhcp {
			return "DHCP"
		}
		return c.ip + "/" + c.netmask
	}())
	if !bondConfirm("Apply bond now? SSH may drop if it uses a selected NIC") {
		fmt.Println("Cancelled.")
		pause()
		return
	}
	if netBackend == "debian" && !ensureIfenslave() {
		pause()
		return
	}
	bak := bondBackup(c)
	if c.renameFrom != "" && !bondRenameExisting(c) {
		bondRestore(bak, c)
		pause()
		return
	}
	var err error
	switch netBackend {
	case "rhel":
		err = bondWriteRHEL(c, isNMActive())
	case "debian":
		err = bondWriteDebian(c)
	case "netplan":
		err = bondWriteNetplan(c)
	default:
		err = fmt.Errorf("unsupported network backend: %s", netBackend)
	}
	if err != nil {
		fmt.Printf("ERROR: writing bond config failed: %v\n", err)
		bondRestore(bak, c)
		pause()
		return
	}
	fmt.Println("Applying bond configuration ...")
	if !bondApply(c) {
		fmt.Println("FAILED: bond did not come up - restoring previous configuration.")
		bondRestore(bak, c)
		pause()
		return
	}
	fmt.Println("OK: bond is active.")
	bondPrintStatus(c)
	fmt.Printf("Backup: %s\n", bak)
	pause()
}
