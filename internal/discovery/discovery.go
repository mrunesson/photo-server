package discovery

import (
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"syscall"

	"github.com/grandcat/zeroconf"
	"github.com/miekg/dns"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const (
	InstanceName = "Photo Server"
	ServiceType  = "_photo-server._tcp"
	Domain       = "local."
)

var (
	mdnsGroupIPv4 = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
	mdnsGroupIPv6 = &net.UDPAddr{IP: net.ParseIP("ff02::fb"), Port: 5353}
)

type advertisement interface {
	Shutdown()
}

type registrationFunc func(instance, service, domain string, port int, host string, ips []string, ifaces []net.Interface) (advertisement, error)

// multicastProbeFunc reports whether mDNS multicast can be sent on the interface.
type multicastProbeFunc func(iface net.Interface, ips []net.IP) error

type interfaceAddress struct {
	iface net.Interface
	ips   []net.IP
}

// Start advertises the HTTP listener when it has a reachable multicast interface.
// The returned function withdraws the service and is always safe to call.
func Start(listener net.Listener) (func(), bool, error) {
	interfaces, err := localInterfaces()
	if err != nil {
		return func() {}, false, fmt.Errorf("list network interfaces: %w", err)
	}
	return start(listener, interfaces, probeMulticast, registerService)
}

func start(listener net.Listener, interfaces []interfaceAddress, probe multicastProbeFunc, register registrationFunc) (func(), bool, error) {
	noOp := func() {}
	tcpAddress, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return noOp, false, fmt.Errorf("HTTP listener address %T is not TCP", listener.Addr())
	}
	if tcpAddress.Port < 1 || tcpAddress.Port > 65535 {
		return noOp, false, fmt.Errorf("HTTP listener has invalid port %d", tcpAddress.Port)
	}

	selected := selectInterfaces(tcpAddress.IP, interfaces)
	if len(selected) == 0 {
		return noOp, false, nil
	}
	reachable, err := reachableInterfaces(selected, probe)
	if err != nil {
		return noOp, false, err
	}
	ifaces := make([]net.Interface, 0, len(reachable))
	var ips []net.IP
	for _, candidate := range reachable {
		ifaces = append(ifaces, candidate.iface)
		ips = append(ips, candidate.ips...)
	}

	host, err := os.Hostname()
	if err != nil {
		return noOp, false, fmt.Errorf("determine zeroconf host name: %w", err)
	}
	ipStrings := make([]string, len(ips))
	for i, ip := range ips {
		ipStrings[i] = ip.String()
	}
	server, err := register(InstanceName, ServiceType, Domain, tcpAddress.Port, host, ipStrings, ifaces)
	if err != nil {
		return noOp, false, fmt.Errorf("register zeroconf service: %w", err)
	}
	return server.Shutdown, true, nil
}

func registerService(instance, service, domain string, port int, host string, ips []string, ifaces []net.Interface) (advertisement, error) {
	return zeroconf.RegisterProxy(instance, service, domain, port, host, ips, nil, ifaces)
}

// reachableInterfaces keeps the interfaces where an mDNS multicast send succeeds.
// The zeroconf responder discards send errors, so without this check a blocked
// multicast path (for example macOS Local Network privacy) looks like success.
func reachableInterfaces(candidates []interfaceAddress, probe multicastProbeFunc) ([]interfaceAddress, error) {
	reachable := make([]interfaceAddress, 0, len(candidates))
	var failures []error
	for _, candidate := range candidates {
		if err := probe(candidate.iface, candidate.ips); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", candidate.iface.Name, err))
			continue
		}
		reachable = append(reachable, candidate)
	}
	if len(reachable) == 0 {
		return nil, multicastError(errors.Join(failures...))
	}
	return reachable, nil
}

func multicastError(err error) error {
	if runtime.GOOS == "darwin" && errors.Is(err, syscall.EHOSTUNREACH) {
		return fmt.Errorf("mDNS multicast is blocked for this process; allow the launching application under System Settings > Privacy & Security > Local Network: %w", err)
	}
	return fmt.Errorf("mDNS multicast unavailable: %w", err)
}

// probeMulticast sends one DNS-SD browse query for the service type through the
// interface, using the address families the interface will be advertised with.
func probeMulticast(iface net.Interface, ips []net.IP) error {
	query := new(dns.Msg)
	query.SetQuestion(ServiceType+"."+Domain, dns.TypePTR)
	query.RecursionDesired = false
	payload, err := query.Pack()
	if err != nil {
		return fmt.Errorf("build mDNS probe: %w", err)
	}

	var hasIPv4, hasIPv6 bool
	for _, ip := range ips {
		if ip.To4() != nil {
			hasIPv4 = true
		} else {
			hasIPv6 = true
		}
	}

	var failures []error
	if hasIPv4 {
		if err := sendMulticastIPv4(iface, payload); err == nil {
			return nil
		} else {
			failures = append(failures, err)
		}
	}
	if hasIPv6 {
		if err := sendMulticastIPv6(iface, payload); err == nil {
			return nil
		} else {
			failures = append(failures, err)
		}
	}
	if len(failures) == 0 {
		return errors.New("no IPv4 or IPv6 address to advertise")
	}
	return errors.Join(failures...)
}

func sendMulticastIPv4(iface net.Interface, payload []byte) error {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return err
	}
	defer conn.Close()
	packetConn := ipv4.NewPacketConn(conn)
	if err := packetConn.SetMulticastInterface(&iface); err != nil {
		return err
	}
	_, err = packetConn.WriteTo(payload, nil, mdnsGroupIPv4)
	return err
}

func sendMulticastIPv6(iface net.Interface, payload []byte) error {
	conn, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6unspecified})
	if err != nil {
		return err
	}
	defer conn.Close()
	packetConn := ipv6.NewPacketConn(conn)
	if err := packetConn.SetMulticastInterface(&iface); err != nil {
		return err
	}
	_, err = packetConn.WriteTo(payload, nil, mdnsGroupIPv6)
	return err
}

func localInterfaces() ([]interfaceAddress, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	result := make([]interfaceAddress, 0, len(interfaces))
	for _, iface := range interfaces {
		if !eligibleInterface(iface) {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}

		ips := make([]net.IP, 0, len(addresses))
		for _, address := range addresses {
			var ip net.IP
			switch address := address.(type) {
			case *net.IPNet:
				ip = address.IP
			case *net.IPAddr:
				ip = address.IP
			}
			if usableIP(ip) {
				ips = append(ips, ip)
			}
		}
		if len(ips) > 0 {
			result = append(result, interfaceAddress{iface: iface, ips: ips})
		}
	}
	return result, nil
}

func selectInterfaces(listenerIP net.IP, candidates []interfaceAddress) []interfaceAddress {
	if listenerIP != nil && !listenerIP.IsUnspecified() {
		if listenerIP.IsLoopback() || !usableIP(listenerIP) {
			return nil
		}
		for _, candidate := range candidates {
			if !eligibleInterface(candidate.iface) {
				continue
			}
			for _, ip := range candidate.ips {
				if ip.Equal(listenerIP) && usableIP(ip) {
					return []interfaceAddress{{iface: candidate.iface, ips: []net.IP{ip}}}
				}
			}
		}
		return nil
	}

	ipv4OnlyWildcard := listenerIP != nil && listenerIP.To4() != nil
	var selected []interfaceAddress
	seenInterfaces := make(map[int]struct{})
	seenIPs := make(map[string]struct{})
	for _, candidate := range candidates {
		if !eligibleInterface(candidate.iface) {
			continue
		}
		if _, ok := seenInterfaces[candidate.iface.Index]; ok {
			continue
		}
		var ips []net.IP
		for _, ip := range candidate.ips {
			if !usableIP(ip) || (ipv4OnlyWildcard && ip.To4() == nil) {
				continue
			}
			key := ip.String()
			if _, ok := seenIPs[key]; ok {
				continue
			}
			seenIPs[key] = struct{}{}
			ips = append(ips, ip)
		}
		if len(ips) > 0 {
			seenInterfaces[candidate.iface.Index] = struct{}{}
			selected = append(selected, interfaceAddress{iface: candidate.iface, ips: ips})
		}
	}
	return selected
}

func eligibleInterface(iface net.Interface) bool {
	return iface.Flags&net.FlagUp != 0 && iface.Flags&net.FlagLoopback == 0 && iface.Flags&net.FlagMulticast != 0
}

func usableIP(ip net.IP) bool {
	return ip != nil && !ip.IsUnspecified() && !ip.IsLoopback() && !ip.IsMulticast()
}
