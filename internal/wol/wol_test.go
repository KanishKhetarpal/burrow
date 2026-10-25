package wol

import (
	"bytes"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestBuildPacket(t *testing.T) {
	mac, err := ParseMAC("AA:BB:CC:DD:EE:FF")
	if err != nil {
		t.Fatalf("ParseMAC: %v", err)
	}
	pkt, err := BuildPacket(mac)
	if err != nil {
		t.Fatalf("BuildPacket: %v", err)
	}
	// 102 is spelled out so the constant itself is pinned by the test.
	if len(pkt) != 102 || PacketLen != 102 {
		t.Fatalf("packet length = %d (PacketLen = %d), want 102", len(pkt), PacketLen)
	}
	if !bytes.Equal(pkt[:6], []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}) {
		t.Errorf("sync header = %x, want six 0xFF bytes", pkt[:6])
	}
	for i := 0; i < 16; i++ {
		off := 6 + i*6
		if !bytes.Equal(pkt[off:off+6], mac) {
			t.Fatalf("repetition %d = %x, want %x", i, pkt[off:off+6], mac)
		}
	}
}

func TestBuildPacketRejectsBadMAC(t *testing.T) {
	if _, err := BuildPacket(net.HardwareAddr{1, 2, 3}); err == nil {
		t.Error("BuildPacket accepted a 3-byte MAC")
	}
}

func TestParseMAC(t *testing.T) {
	want := net.HardwareAddr{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}
	for _, in := range []string{"AA:BB:CC:DD:EE:FF", "aa-bb-cc-dd-ee-ff", "AABBCCDDEEFF"} {
		got, err := ParseMAC(in)
		if err != nil {
			t.Fatalf("ParseMAC(%q): %v", in, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("ParseMAC(%q) = %x, want %x", in, got, want)
		}
	}
	for _, bad := range []string{"", "AA:BB:CC", "not-a-mac", "AA:BB:CC:DD:EE:FF:00:11"} {
		if _, err := ParseMAC(bad); err == nil {
			t.Errorf("ParseMAC(%q) = nil error, want a failure", bad)
		}
	}
}

func TestBroadcastAddr(t *testing.T) {
	cases := []struct{ in, want string }{
		{"192.168.1.0/24", "192.168.1.255"},
		{"192.168.1.37/24", "192.168.1.255"},
		{"10.0.0.0/8", "10.255.255.255"},
		{"172.16.4.0/22", "172.16.7.255"},
		{"192.168.1.0/30", "192.168.1.3"},
		{"192.168.1.5/32", "192.168.1.5"},
	}
	for _, tc := range cases {
		p, err := netip.ParsePrefix(tc.in)
		if err != nil {
			t.Fatalf("ParsePrefix(%q): %v", tc.in, err)
		}
		got, err := BroadcastAddr(p)
		if err != nil {
			t.Fatalf("BroadcastAddr(%q): %v", tc.in, err)
		}
		if got.String() != tc.want {
			t.Errorf("BroadcastAddr(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestBroadcastAddrRejectsIPv6(t *testing.T) {
	p := netip.MustParsePrefix("fd00::/64")
	if _, err := BroadcastAddr(p); err == nil {
		t.Error("BroadcastAddr accepted an IPv6 prefix; WoL is IPv4 broadcast only")
	}
}

func TestTargets(t *testing.T) {
	got, err := Targets("192.168.1.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].String() != "192.168.1.255" || got[1].String() != "255.255.255.255" {
		t.Errorf("Targets = %v, want the directed broadcast then the limited broadcast", got)
	}

	// With no prefix configured, only the limited broadcast is used.
	got, err = Targets("")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].String() != "255.255.255.255" {
		t.Errorf("Targets(\"\") = %v, want only 255.255.255.255", got)
	}

	if _, err := Targets("not-a-prefix"); err == nil {
		t.Error("Targets accepted a malformed prefix")
	}
}

// recorder captures sends instead of touching the network.
type recorder struct {
	sent []netip.AddrPort
	pkts [][]byte
}

func (r *recorder) Send(pkt []byte, to netip.AddrPort) error {
	r.sent = append(r.sent, to)
	r.pkts = append(r.pkts, append([]byte(nil), pkt...))
	return nil
}

func TestWakeSendsEveryPortAndTarget(t *testing.T) {
	mac, _ := ParseMAC("AA:BB:CC:DD:EE:FF")
	rec := &recorder{}
	var slept time.Duration
	if err := Wake(rec, mac, "192.168.1.0/24", func(d time.Duration) { slept += d }); err != nil {
		t.Fatalf("Wake: %v", err)
	}

	// 3 repeats x 2 targets x 2 ports.
	want := Repeats * 2 * len(Ports)
	if len(rec.sent) != want {
		t.Fatalf("sent %d packets, want %d", len(rec.sent), want)
	}
	if slept != time.Duration(Repeats-1)*RepeatDelay {
		t.Errorf("slept %v between repeats, want %v", slept, time.Duration(Repeats-1)*RepeatDelay)
	}
	for i, p := range rec.pkts {
		if len(p) != PacketLen {
			t.Fatalf("packet %d has length %d, want %d", i, len(p), PacketLen)
		}
	}

	seen := map[string]bool{}
	for _, a := range rec.sent {
		seen[a.String()] = true
	}
	for _, want := range []string{"192.168.1.255:9", "192.168.1.255:7", "255.255.255.255:9", "255.255.255.255:7"} {
		if !seen[want] {
			t.Errorf("never sent to %s", want)
		}
	}
}

// failing fails on one specific address to check Wake keeps going.
type failing struct {
	recorder
	failOn string
}

func (f *failing) Send(pkt []byte, to netip.AddrPort) error {
	if to.String() == f.failOn {
		return errSend
	}
	return f.recorder.Send(pkt, to)
}

var errSend = &sendErr{}

type sendErr struct{}

func (*sendErr) Error() string { return "send failed" }

func TestWakeKeepsGoingAfterOneFailure(t *testing.T) {
	mac, _ := ParseMAC("AA:BB:CC:DD:EE:FF")
	f := &failing{failOn: "192.168.1.255:9"}
	err := Wake(f, mac, "192.168.1.0/24", func(time.Duration) {})
	if err == nil {
		t.Fatal("Wake reported success although one target failed")
	}
	// Everything except the failing address:port must still have been tried.
	want := Repeats * (2*len(Ports) - 1)
	if len(f.sent) != want {
		t.Errorf("sent %d packets, want %d (all but the failing target)", len(f.sent), want)
	}
}
