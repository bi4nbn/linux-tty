package main

import (
	"net"
	"strings"
	"testing"
)

func TestSuggestGateway(t *testing.T) {
	mask24 := net.CIDRMask(24, 32)
	tests := []struct {
		ip   string
		mask net.IPMask
		want string
	}{
		{"192.168.1.1", mask24, "192.168.1.2"},
		{"192.168.1.2", mask24, "192.168.1.1"},
		{"10.0.0.5", mask24, "10.0.0.1"},
		{"192.168.1.1", net.CIDRMask(30, 32), "192.168.1.2"},
		{"192.168.1.2", net.CIDRMask(30, 32), "192.168.1.1"},
		{"192.168.1.1", net.CIDRMask(31, 32), ""},
		{"192.168.1.1", net.CIDRMask(32, 32), ""},
	}
	for _, tt := range tests {
		ip := net.ParseIP(tt.ip).To4()
		if got := suggestGateway(ip, tt.mask); got != tt.want {
			t.Errorf("suggestGateway(%s, %v) = %q, want %q", tt.ip, tt.mask, got, tt.want)
		}
	}
}

func TestMaskHelpers(t *testing.T) {
	for _, prefix := range []int{1, 8, 16, 24, 27, 30, 32} {
		mask := net.ParseIP(prefixToMask(prefix))
		if mask == nil || maskSize(mask) != prefix {
			t.Fatalf("prefixToMask/maskSize round trip failed for /%d", prefix)
		}
	}
	if netmaskIsValid("255.0.255.0") {
		t.Error("non-contiguous netmask accepted")
	}
	if netmaskIsValid("255.255.255.0") != true {
		t.Error("valid netmask rejected")
	}
	if netmaskIsValid("0.0.0.0") {
		t.Error("0.0.0.0 accepted as netmask")
	}
}

func TestParseResolvConf(t *testing.T) {
	data := "# comment\nnameserver 8.8.8.8\nnameserver 127.0.0.53\nnameserver 127.0.0.1\nnameserver 2001:db8::1\nnameserver ::1\nnameserver 0.0.0.0\n"
	got := parseResolvConf(data)
	want := []string{"8.8.8.8", "127.0.0.1", "2001:db8::1"}
	if len(got) != len(want) {
		t.Fatalf("parseResolvConf = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parseResolvConf = %v, want %v", got, want)
		}
	}
}

func TestMergeIfcfg(t *testing.T) {
	old := "UUID=abc\nHWADDR=00:11:22:33:44:55\nMTU=9000\nIPADDR=1.2.3.4\nNETMASK=255.255.255.0\nDNS1=8.8.8.8\n"
	base := "TYPE=Ethernet\nBOOTPROTO=static\nIPADDR=10.0.0.5\nNETMASK=255.255.255.0\n"
	got := mergeIfcfg(old, base, false)
	for _, keep := range []string{"UUID=abc", "HWADDR=00:11:22:33:44:55", "MTU=9000", "IPADDR=10.0.0.5"} {
		if !strings.Contains(got, keep) {
			t.Errorf("merged ifcfg lost %q:\n%s", keep, got)
		}
	}
	for _, drop := range []string{"IPADDR=1.2.3.4", "DNS1=8.8.8.8"} {
		if strings.Contains(got, drop) {
			t.Errorf("merged ifcfg kept managed key %q:\n%s", drop, got)
		}
	}
}

func TestIfcfgSpecialReason(t *testing.T) {
	for _, text := range []string{
		"TYPE=Bond\nBONDING_OPTS=mode=active-backup\n",
		"MASTER=bond0\nSLAVE=yes\n",
		"VLAN=yes\n",
		"TYPE=Bridge\n",
		"TYPE=OVSPort\n",
	} {
		if ifcfgSpecialReason(text) == "" {
			t.Errorf("ifcfgSpecialReason(%q) = empty, want special", text)
		}
	}
	if ifcfgSpecialReason("TYPE=Ethernet\nBOOTPROTO=dhcp\n") != "" {
		t.Error("normal Ethernet ifcfg flagged as special")
	}
}

func TestInterfacesSpecialReason(t *testing.T) {
	text := `auto lo
iface lo inet loopback

auto br0
iface br0 inet static
    bridge_ports eth0

auto eth0
iface eth0 inet static
    address 10.0.0.5/24
`
	if got := interfacesSpecialReason(text, "br0"); got == "" {
		t.Error("bridge master not detected")
	}
	if got := interfacesSpecialReason(text, "eth0"); got == "" {
		t.Error("bridge member not detected")
	}
	if got := interfacesSpecialReason(text, "lo"); got != "" {
		t.Error("loopback flagged as special")
	}
}

func TestNetplanSpecialReasonText(t *testing.T) {
	text := `network:
  version: 2
  ethernets:
    eth0:
      dhcp4: true
    eth1:
      dhcp4: true
  bonds:
    bond0:
      interfaces: [eth0, eth1]
      parameters:
        mode: active-backup
`
	if got := netplanSpecialReasonText(text, "bond0"); got == "" {
		t.Error("bond master not detected")
	}
	if got := netplanSpecialReasonText(text, "eth0"); got == "" {
		t.Error("bond member not detected")
	}
	if got := netplanSpecialReasonText(text, "eth9"); got != "" {
		t.Error("unrelated NIC flagged as special")
	}
}

func TestScanNetplanRenderer(t *testing.T) {
	global := `network:
  version: 2
  renderer: NetworkManager
  ethernets:
    eth0:
      dhcp4: true
`
	g, p := scanNetplanRenderer(global, "eth0")
	if g != "NetworkManager" || p != "" {
		t.Errorf("global renderer = %q/%q, want NetworkManager/empty", g, p)
	}

	perNIC := `network:
  version: 2
  ethernets:
    eth0:
      renderer: networkd
      dhcp4: true
`
	g, p = scanNetplanRenderer(perNIC, "eth0")
	if g != "" || p != "networkd" {
		t.Errorf("per-NIC renderer = %q/%q, want empty/networkd", g, p)
	}
}

func TestNormalizeNetplanRenderer(t *testing.T) {
	if normalizeNetplanRenderer("NetworkManager") != "NetworkManager" {
		t.Error("NetworkManager not normalized")
	}
	if normalizeNetplanRenderer("networkd") != "networkd" {
		t.Error("networkd not normalized")
	}
	if normalizeNetplanRenderer("bogus") != "" {
		t.Error("unknown renderer accepted")
	}
}

func TestDebianStanzaExtras(t *testing.T) {
	text := `iface eth0 inet static
    address 10.0.0.5
    netmask 255.255.255.0
    mtu 9000
    dns-nameservers 8.8.8.8
    up ip link set $IFACE up
`
	extras := debianStanzaExtras(text, "eth0")
	joined := strings.Join(extras, "\n")
	if !strings.Contains(joined, "mtu 9000") || !strings.Contains(joined, "up ip link") {
		t.Errorf("extras = %q, want mtu and up hook", joined)
	}
	if strings.Contains(joined, "address") || strings.Contains(joined, "dns-nameservers") {
		t.Errorf("extras kept managed directives: %q", joined)
	}
}

func TestIsVTConsole(t *testing.T) {
	for _, tty := range []string{"tty1", "tty12"} {
		if !isVTConsole(tty) {
			t.Errorf("isVTConsole(%q) = false", tty)
		}
	}
	for _, tty := range []string{"", "ttyS0", "ttyAMA0", "pts/0"} {
		if isVTConsole(tty) {
			t.Errorf("isVTConsole(%q) = true", tty)
		}
	}
}

func TestValidTTYName(t *testing.T) {
	for _, tty := range []string{"tty1", "ttyS0", "hvc0", "ttyUSB0"} {
		if !validTTYName(tty) {
			t.Errorf("validTTYName(%q) = false", tty)
		}
	}
	for _, tty := range []string{"", "pts/0", "tty1;rm", "tty 1"} {
		if validTTYName(tty) {
			t.Errorf("validTTYName(%q) = true", tty)
		}
	}
}

func TestBuiltinSlotOverrideFor(t *testing.T) {
	cases := []struct {
		product, bdf, want string
	}{
		{"PowerEdge R730xd", "0000:05:00.0", "Slot 6-1"},
		{"PowerEdge R730xd", "06:00.0", "Slot 6-2"},
		{"PowerEdge R730xd", "0000:07:00.0", ""},
		{"ProLiant DL360 Gen9", "0000:05:00.0", ""},
	}
	for _, c := range cases {
		if got := builtinSlotOverrideFor(c.product, c.bdf); got != c.want {
			t.Errorf("builtinSlotOverrideFor(%q, %q) = %q, want %q", c.product, c.bdf, got, c.want)
		}
	}
}

func TestParseSlotOverrides(t *testing.T) {
	m := parseSlotOverrides(`# PCIe bifurcation mapping
05:00.0        Slot 6-1
0000:06:00.0   Slot 6-2
nvme0n1        Slot 6-1
S438NA0N910155 Slot 6-1
`)
	for key, want := range map[string]string{
		"05:00.0":        "Slot 6-1",
		"0000:06:00.0":   "Slot 6-2",
		"06:00.0":        "Slot 6-2",
		"nvme0n1":        "Slot 6-1",
		"s438na0n910155": "Slot 6-1",
	} {
		if got := m[key]; got != want {
			t.Errorf("slot override %q = %q, want %q", key, got, want)
		}
	}
}

func TestParsePercRaidLevels(t *testing.T) {
	out := `
---------------------------------------------------------------
DG/VD TYPE  State Access Consist Cache Cac sCC       Size Name 
---------------------------------------------------------------
0/0   RAID0 Optl  RW     Yes     NRWTD -   OFF 744.625 GB      
1/1   RAID5 Optl  RW     Yes     NRWTD -   OFF   5.457 TB      
---------------------------------------------------------------
`
	m := parsePercRaidLevels(out)
	if m[0] != "RAID0" || m[1] != "RAID5" {
		t.Fatalf("parsePercRaidLevels = %#v", m)
	}
}

func TestParseHPDiskConfig(t *testing.T) {
	out := `
Smart Array P440ar in Slot 0 (Embedded)   (sn: TEST)

   Port Name: 1I

   Array: A
      Interface Type: Solid State SATA
      Unused Space: 1 MB
      Logical Drive: 1
         Size: 1.46 TB
         Fault Tolerance: 5
         Status: OK
         Disk Name: /dev/sda
         Logical Drive Label: TEST
      physicaldrive 1I:0:2
         Port: 1I
         Box: 0
         Bay: 2
         Status: OK
         Size: 800 GB
         Interface Type: Solid State SATA
         Serial Number: SN2
         Model: ATA     INTEL SSDSC2BB80
      physicaldrive 1I:0:4
         Port: 1I
         Box: 0
         Bay: 4
         Interface Type: Solid State SATA
         Serial Number: SN4
         Model: ATA     INTEL SSDSC2BB80
      physicaldrive 2I:0:6
         Port: 2I
         Box: 0
         Bay: 6
         Interface Type: Solid State SATA
         Serial Number: SN6
         Model: ATA     INTEL SSDSC2BB80
`
	m := parseHPDiskConfig(out)
	pds, ok := m["sda"]
	if !ok || len(pds) != 3 {
		t.Fatalf("parseHPDiskConfig sda = %+v, want 3 drives", pds)
	}
	if got := hpSlotList(pds); got != "02,04,06" {
		t.Errorf("hpSlotList = %q, want 02,04,06", got)
	}
	if pds[0].Model != "ATA INTEL SSDSC2BB80" {
		t.Errorf("model = %q", pds[0].Model)
	}
	if pds[0].Raid != "RAID5" {
		t.Errorf("raid = %q, want RAID5", pds[0].Raid)
	}
	if pds[0].Size != "800G" {
		t.Errorf("size = %q, want 800G", pds[0].Size)
	}
	if pds[0].Status != "OK" || pds[0].LDStatus != "OK" {
		t.Errorf("status = %q ld = %q, want OK/OK", pds[0].Status, pds[0].LDStatus)
	}
	if got := hpHealth(pds[0]); got != "OK" {
		t.Errorf("hpHealth = %q, want OK", got)
	}
	if pds[0].Intf != "SATA" || pds[0].Med != "SSD" {
		t.Errorf("type = %s/%s, want SATA/SSD", pds[0].Intf, pds[0].Med)
	}
	if pds[2].Serial != "SN6" || pds[2].Bay != 6 {
		t.Errorf("third drive = %+v", pds[2])
	}

	// Same bay number on two ports must fall back to full identifiers.
	dup := []hpDiskInfo{
		{Slot: "1I:0:1", Port: "1I", Bay: 1},
		{Slot: "2I:0:1", Port: "2I", Bay: 1},
	}
	if got := hpSlotList(dup); got != "1I:0:1,2I:0:1" {
		t.Errorf("duplicate-bay slots = %q", got)
	}
}

func TestHPBusMedia(t *testing.T) {
	tests := []struct {
		in        string
		bus, medi string
	}{
		{"Solid State SATA", "SATA", "SSD"},
		{"SAS", "SAS", ""},
		{"NVMe", "NVMe", ""},
	}
	for _, tt := range tests {
		bus, med := hpBusMedia(tt.in)
		if bus != tt.bus || med != tt.medi {
			t.Errorf("hpBusMedia(%q) = %s/%s, want %s/%s", tt.in, bus, med, tt.bus, tt.medi)
		}
	}
}

func TestSmartHealthParsing(t *testing.T) {
	if got := parseSmartHealth("SMART overall-health self-assessment test result: PASSED"); got != "OK" {
		t.Errorf("PASSED = %q", got)
	}
	if got := parseSmartHealth("SMART overall-health self-assessment test result: FAILED!"); got != "BAD" {
		t.Errorf("FAILED = %q", got)
	}
	if got := parseSmartHealth("SMART Health Status: OK"); got != "OK" {
		t.Errorf("SMART Health Status OK = %q", got)
	}
	if got := parseSmartHealth("A mandatory SMART command failed: exiting."); got != "-" {
		t.Errorf("unsupported SMART command = %q, want -", got)
	}
	okAttr := `Critical Warning:                   0x00
Available Spare:                    100%
Available Spare Threshold:          10%
Percentage Used:                    0%
Media and Data Integrity Errors:    0`
	if smartAttrWarn(okAttr) {
		t.Error("healthy NVMe attrs flagged")
	}
	for name, out := range map[string]string{
		"critical": "Critical Warning: 0x01",
		"spare":    "Available Spare: 5%\nAvailable Spare Threshold: 10%",
		"wear":     "Percentage Used: 95%",
		"media":    "Media and Data Integrity Errors: 1",
	} {
		if !smartAttrWarn(out) {
			t.Errorf("%s not flagged", name)
		}
	}
}

func TestNVMeLife(t *testing.T) {
	cases := []struct {
		attrs string
		want  string
	}{
		{"Percentage Used: 2%", "98%"},
		{"Percentage Used: 100%", "0%"},
		{"Percentage Used: 105%", "0%"},
		{"Critical Warning: 0x01", "WARN"},
		{"", "-"},
	}
	for _, c := range cases {
		if got := nvmeLifeFromAttrs(c.attrs); got != c.want {
			t.Errorf("nvmeLifeFromAttrs(%q) = %q, want %q", c.attrs, got, c.want)
		}
	}
}

func TestHealthHelpers(t *testing.T) {
	cases := []struct {
		pd   percDiskInfo
		want string
	}{
		{percDiskInfo{State: "Onln"}, "OK"},
		{percDiskInfo{State: "Onln", MediaErr: 1}, "WARN"},
		{percDiskInfo{State: "Rbld"}, "WARN"},
		{percDiskInfo{State: "Offln"}, "BAD"},
		{percDiskInfo{State: "UBad"}, "BAD"},
	}
	for _, c := range cases {
		if got := percHealth(c.pd); got != c.want {
			t.Errorf("percHealth(%+v) = %q, want %q", c.pd, got, c.want)
		}
	}
	if got := hpHealth(hpDiskInfo{Status: "OK", LDStatus: "Degraded"}); got != "WARN" {
		t.Errorf("hpHealth degraded = %q, want WARN", got)
	}
	if got := hpHealth(hpDiskInfo{Status: "Failed"}); got != "BAD" {
		t.Errorf("hpHealth failed = %q, want BAD", got)
	}
	if got := worstHealth("OK", "WARN", "OK"); got != "WARN" {
		t.Errorf("worstHealth = %q, want WARN", got)
	}
}

func TestParsePercDiskOutputDell(t *testing.T) {
	out := `Drive /c0/e32/s0 :
================
32:0      0 Onln   1 5.457 TB SATA HDD N   N  512B ST6000NM0115-1YZ110 U  -    
SN =             ZAD25CH0
Model Number = ST6000NM0115-1YZ110
Media Error Count = 2
Other Error Count = 0
Predictive Failure Count = 1
S.M.A.R.T alert flagged by drive = Yes

Drive /c0/e32/s12 :
================
32:12    12 Onln   0 744.625 GB SATA SSD N   N  512B INTEL SSDSC2BB800G6R U  -    
SN =             BTWA54910752800HGN
Model Number = INTEL SSDSC2BB800G6R
`
	m := parsePercDiskOutput(out)
	if got := m[1]; len(got) != 1 {
		t.Fatalf("DG1 = %+v, want 1 drive", got)
	} else if got[0].EID != 32 || got[0].Slot != 0 || got[0].Serial != "ZAD25CH0" || got[0].DG != 1 {
		t.Errorf("DG1 drive = %+v", got[0])
	}
	if got := m[0]; len(got) != 1 {
		t.Fatalf("DG0 = %+v, want 1 drive", got)
	} else if got[0].Slot != 12 || got[0].Med != "SSD" || got[0].Serial != "BTWA54910752800HGN" {
		t.Errorf("DG0 drive = %+v", got[0])
	}
	if got := percSlotLabel(m[1][0]); got != "00" {
		t.Errorf("percSlotLabel = %q, want 00", got)
	}
	if got := percSlotLabel(m[0][0]); got != "12" {
		t.Errorf("percSlotLabel = %q, want 12", got)
	}
}
