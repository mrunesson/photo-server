package discovery

import (
	"errors"
	"net"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func probeOK(net.Interface, []net.IP) error { return nil }

func TestSelectInterfacesForWildcardListener(t *testing.T) {
	interfaces := []interfaceAddress{
		{
			iface: net.Interface{Index: 1, Name: "en0", Flags: net.FlagUp | net.FlagMulticast},
			ips:   []net.IP{net.ParseIP("192.168.1.20"), net.ParseIP("fe80::20")},
		},
		{
			iface: net.Interface{Index: 2, Name: "lo0", Flags: net.FlagUp | net.FlagLoopback | net.FlagMulticast},
			ips:   []net.IP{net.ParseIP("127.0.0.1")},
		},
		{
			iface: net.Interface{Index: 3, Name: "down0", Flags: net.FlagMulticast},
			ips:   []net.IP{net.ParseIP("192.168.1.30")},
		},
	}

	got := selectInterfaces(net.IPv6zero, interfaces)
	want := []interfaceAddress{{iface: interfaces[0].iface, ips: []net.IP{net.ParseIP("192.168.1.20"), net.ParseIP("fe80::20")}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selectInterfaces() = %#v, want %#v", got, want)
	}
}

func TestSelectInterfacesForIPv4WildcardListener(t *testing.T) {
	iface := net.Interface{Index: 1, Name: "en0", Flags: net.FlagUp | net.FlagMulticast}
	interfaces := []interfaceAddress{{
		iface: iface,
		ips:   []net.IP{net.ParseIP("192.168.1.20"), net.ParseIP("fe80::20")},
	}}

	got := selectInterfaces(net.IPv4zero, interfaces)
	want := []interfaceAddress{{iface: iface, ips: []net.IP{net.ParseIP("192.168.1.20")}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selectInterfaces() = %#v, want only IPv4 listener addresses %#v", got, want)
	}
}

func TestSelectInterfacesForSpecificListener(t *testing.T) {
	iface := net.Interface{Index: 1, Name: "en0", Flags: net.FlagUp | net.FlagMulticast}
	interfaces := []interfaceAddress{{
		iface: iface,
		ips:   []net.IP{net.ParseIP("192.168.1.20"), net.ParseIP("192.168.1.21")},
	}}

	got := selectInterfaces(net.ParseIP("192.168.1.21"), interfaces)
	want := []interfaceAddress{{iface: iface, ips: []net.IP{net.ParseIP("192.168.1.21")}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selectInterfaces() = %#v, want only the listener IP %#v", got, want)
	}
}

func TestSelectInterfacesForSpecificIPv6Listener(t *testing.T) {
	iface := net.Interface{Index: 1, Name: "en0", Flags: net.FlagUp | net.FlagMulticast}
	interfaces := []interfaceAddress{{
		iface: iface,
		ips:   []net.IP{net.ParseIP("fe80::20"), net.ParseIP("2001:db8::20")},
	}}

	listenerIP := net.ParseIP("2001:db8::20")
	got := selectInterfaces(listenerIP, interfaces)
	want := []interfaceAddress{{iface: iface, ips: []net.IP{listenerIP}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selectInterfaces() = %#v, want only the listener IP %#v", got, want)
	}
}

func TestSelectInterfacesForLoopbackListener(t *testing.T) {
	interfaces := []interfaceAddress{{
		iface: net.Interface{Index: 1, Name: "lo0", Flags: net.FlagUp | net.FlagLoopback | net.FlagMulticast},
		ips:   []net.IP{net.ParseIP("127.0.0.1")},
	}}

	if got := selectInterfaces(net.ParseIP("127.0.0.1"), interfaces); len(got) != 0 {
		t.Fatalf("selectInterfaces() = %#v, want no advertised interfaces", got)
	}
}

func TestStartUsesListenerPortAndStopsAdvertisement(t *testing.T) {
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	iface := net.Interface{Index: 1, Name: "en0", Flags: net.FlagUp | net.FlagMulticast}
	interfaces := []interfaceAddress{{iface: iface, ips: []net.IP{net.ParseIP("192.168.1.20")}}}
	var gotInstance, gotService, gotDomain, gotHost string
	var gotPort int
	var gotIPs []string
	var gotInterfaces []net.Interface
	shutdowns := 0
	stop, advertised, err := start(listener, interfaces, probeOK, func(instance, service, domain string, port int, host string, ips []string, ifaces []net.Interface) (advertisement, error) {
		gotInstance, gotService, gotDomain, gotPort, gotHost = instance, service, domain, port, host
		gotIPs, gotInterfaces = ips, ifaces
		return testAdvertisement{shutdown: func() { shutdowns++ }}, nil
	})
	if err != nil {
		t.Fatalf("start() error = %v", err)
	}
	if !advertised {
		t.Fatal("start() advertised = false, want true")
	}
	if gotInstance != "Photo Server" || gotService != "_photo-server._tcp" || gotDomain != "local." {
		t.Fatalf("registration service = (%q, %q, %q), want (Photo Server, _photo-server._tcp, local.)", gotInstance, gotService, gotDomain)
	}
	if gotPort != listener.Addr().(*net.TCPAddr).Port || gotPort == 0 {
		t.Fatalf("registration port = %d, want listener's assigned port %d", gotPort, listener.Addr().(*net.TCPAddr).Port)
	}
	if gotHost == "" {
		t.Fatal("registration host is empty")
	}
	if !reflect.DeepEqual(gotIPs, []string{"192.168.1.20"}) || !reflect.DeepEqual(gotInterfaces, []net.Interface{iface}) {
		t.Fatalf("registration network = (%#v, %#v), want selected interface and address", gotIPs, gotInterfaces)
	}
	stop()
	if shutdowns != 1 {
		t.Fatalf("shutdown calls = %d, want 1", shutdowns)
	}
}

func TestStartDoesNotAdvertiseLoopbackListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	called := false
	stop, advertised, err := start(listener, []interfaceAddress{{
		iface: net.Interface{Index: 1, Name: "en0", Flags: net.FlagUp | net.FlagMulticast},
		ips:   []net.IP{net.ParseIP("192.168.1.20")},
	}}, probeOK, func(string, string, string, int, string, []string, []net.Interface) (advertisement, error) {
		called = true
		return testAdvertisement{}, nil
	})
	if err != nil {
		t.Fatalf("start() error = %v", err)
	}
	if called || advertised {
		t.Fatalf("start() called registration = %t, advertised = %t, want both false", called, advertised)
	}
	stop()
}

func TestStartReturnsRegistrationError(t *testing.T) {
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	wantErr := errors.New("registration failed")
	stop, advertised, err := start(listener, []interfaceAddress{{
		iface: net.Interface{Index: 1, Name: "en0", Flags: net.FlagUp | net.FlagMulticast},
		ips:   []net.IP{net.ParseIP("192.168.1.20")},
	}}, probeOK, func(string, string, string, int, string, []string, []net.Interface) (advertisement, error) {
		return nil, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("start() error = %v, want %v", err, wantErr)
	}
	if advertised || stop == nil {
		t.Fatalf("start() advertised = %t, stop is nil = %t, want false and a safe no-op", advertised, stop == nil)
	}
}

func TestStartFailsWhenMulticastIsBlockedEverywhere(t *testing.T) {
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	blocked := &net.OpError{Op: "write", Net: "udp", Err: syscall.EHOSTUNREACH}
	registered := false
	stop, advertised, err := start(listener, []interfaceAddress{
		{iface: net.Interface{Index: 1, Name: "en0", Flags: net.FlagUp | net.FlagMulticast}, ips: []net.IP{net.ParseIP("192.168.1.20")}},
		{iface: net.Interface{Index: 2, Name: "en1", Flags: net.FlagUp | net.FlagMulticast}, ips: []net.IP{net.ParseIP("10.0.0.20")}},
	}, func(net.Interface, []net.IP) error {
		return blocked
	}, func(string, string, string, int, string, []string, []net.Interface) (advertisement, error) {
		registered = true
		return testAdvertisement{}, nil
	})
	if err == nil {
		t.Fatal("start() error = nil, want multicast failure")
	}
	if !errors.Is(err, syscall.EHOSTUNREACH) {
		t.Fatalf("start() error = %v, want it to wrap %v", err, syscall.EHOSTUNREACH)
	}
	if !strings.Contains(err.Error(), "en0") || !strings.Contains(err.Error(), "en1") {
		t.Fatalf("start() error = %q, want every blocked interface named", err)
	}
	if runtime.GOOS == "darwin" && !strings.Contains(err.Error(), "Local Network") {
		t.Fatalf("start() error = %q, want macOS Local Network permission hint", err)
	}
	if registered || advertised {
		t.Fatalf("start() registered = %t, advertised = %t, want both false", registered, advertised)
	}
	stop()
}

func TestStartAdvertisesOnlyInterfacesWithWorkingMulticast(t *testing.T) {
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	working := net.Interface{Index: 2, Name: "en1", Flags: net.FlagUp | net.FlagMulticast}
	var gotIPs []string
	var gotInterfaces []net.Interface
	_, advertised, err := start(listener, []interfaceAddress{
		{iface: net.Interface{Index: 1, Name: "en0", Flags: net.FlagUp | net.FlagMulticast}, ips: []net.IP{net.ParseIP("192.168.1.20")}},
		{iface: working, ips: []net.IP{net.ParseIP("10.0.0.20"), net.ParseIP("fe80::20")}},
	}, func(iface net.Interface, _ []net.IP) error {
		if iface.Name == "en0" {
			return errors.New("blocked")
		}
		return nil
	}, func(_, _, _ string, _ int, _ string, ips []string, ifaces []net.Interface) (advertisement, error) {
		gotIPs, gotInterfaces = ips, ifaces
		return testAdvertisement{}, nil
	})
	if err != nil {
		t.Fatalf("start() error = %v", err)
	}
	if !advertised {
		t.Fatal("start() advertised = false, want true")
	}
	if !reflect.DeepEqual(gotInterfaces, []net.Interface{working}) || !reflect.DeepEqual(gotIPs, []string{"10.0.0.20", "fe80::20"}) {
		t.Fatalf("registration network = (%#v, %#v), want only the interface with working multicast", gotInterfaces, gotIPs)
	}
}

type testAdvertisement struct {
	shutdown func()
}

func (a testAdvertisement) Shutdown() {
	if a.shutdown != nil {
		a.shutdown()
	}
}
