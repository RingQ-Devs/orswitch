package sip

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"
)

// Resolve turns a host (and optional port) into a transport address. Literal
// IPs are used directly; names without a port are looked up via DNS SRV
// (_sip._udp / _sip._tcp) and fall back to A/AAAA records on port 5060.
func Resolve(host string, port int, transport string) (Addr, error) {
	if transport == "" {
		transport = "udp"
	}
	if port < 0 || port > 65535 {
		return Addr{}, fmt.Errorf("sip: invalid port %d for %q", port, host)
	}
	host = strings.Trim(host, "[]")
	defPort, srvService, srvProto := 5060, "sip", transport
	if transport == "tls" { // SIP over TLS: port 5061, SRV _sips._tcp
		defPort, srvService, srvProto = 5061, "sips", "tcp"
	}
	if a, err := netip.ParseAddr(host); err == nil {
		if port == 0 {
			port = defPort
		}
		return Addr{Transport: transport, AP: netip.AddrPortFrom(a.Unmap(), uint16(port))}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := net.DefaultResolver
	if port == 0 {
		if _, srvs, err := r.LookupSRV(ctx, srvService, srvProto, host); err == nil && len(srvs) > 0 {
			for _, srv := range srvs {
				target := strings.TrimSuffix(srv.Target, ".")
				if ips, err := r.LookupNetIP(ctx, "ip4", target); err == nil && len(ips) > 0 {
					return Addr{Transport: transport, AP: netip.AddrPortFrom(ips[0].Unmap(), srv.Port), Host: host}, nil
				}
			}
		}
		port = defPort
	}
	ips, err := r.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return Addr{}, fmt.Errorf("sip: cannot resolve %q: %v", host, err)
	}
	best := ips[0]
	for _, ip := range ips {
		if ip.Unmap().Is4() {
			best = ip
			break
		}
	}
	return Addr{Transport: transport, AP: netip.AddrPortFrom(best.Unmap(), uint16(port)), Host: host}, nil
}
