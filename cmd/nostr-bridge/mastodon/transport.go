package mastodon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

const defaultDialAttemptTimeout = 5 * time.Second

type ipLookup func(ctx context.Context, host string) ([]net.IPAddr, error)
type networkDial func(ctx context.Context, network, address string) (net.Conn, error)

func defaultIPLookup(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

func defaultNetworkDial(ctx context.Context, network, address string) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, network, address)
}

func dialResolvedIP(ctx context.Context, network, address string, lookup ipLookup, dial networkDial) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}

	if ip := net.ParseIP(host); ip != nil {
		netType := "tcp4"
		if ip.To4() == nil {
			netType = "tcp6"
		}
		attemptCtx, cancel := context.WithTimeout(ctx, defaultDialAttemptTimeout)
		defer cancel()
		return dial(attemptCtx, netType, net.JoinHostPort(host, port))
	}

	addrs, err := lookup(ctx, host)
	if err != nil {
		return nil, err
	}

	var v4Addrs, v6Addrs []net.IPAddr
	for _, addr := range addrs {
		if addr.IP.To4() != nil {
			v4Addrs = append(v4Addrs, addr)
		} else if addr.IP.To16() != nil {
			v6Addrs = append(v6Addrs, addr)
		}
	}

	var errs []error
	for _, addr := range append(v4Addrs, v6Addrs...) {
		netType := "tcp4"
		if addr.IP.To4() == nil {
			netType = "tcp6"
		}
		conn, err := func() (net.Conn, error) {
			attemptCtx, cancel := context.WithTimeout(ctx, defaultDialAttemptTimeout)
			defer cancel()
			return dial(attemptCtx, netType, net.JoinHostPort(addr.IP.String(), port))
		}()
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return nil, &net.OpError{Op: "dial", Net: network, Addr: nil, Err: errors.New("no addresses resolved")}
}

func newHTTPClient() *http.Client {
	var transport *http.Transport
	if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = defaultTransport.Clone()
	} else {
		transport = &http.Transport{}
	}

	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialResolvedIP(ctx, network, address, defaultIPLookup, defaultNetworkDial)
	}

	return &http.Client{
		Transport: transport,
	}
}
