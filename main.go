/*
 * Copyright (C) 2019 The "MysteriumNetwork/openvpn-forwarder" Authors.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package main

import (
	"flag"
	"log"
	"net"
	"net/url"
	"strings"
	"sync"

	"github.com/mysteriumnetwork/openvpn-forwarder/api"
	"github.com/mysteriumnetwork/openvpn-forwarder/proxy"
	netproxy "golang.org/x/net/proxy"
)

var proxyAddr = flag.String("proxy.bind", ":8443", "Proxy address for incoming connections")
var proxyAPIAddr = flag.String("proxy.api-bind", ":8000", "HTTP proxy API address")
var proxyUpstreamURL = flag.String(
	"proxy.upstream-url",
	"",
	`Upstream HTTPS proxy where to forward traffic (e.g. "http://superproxy.com:8080")`,
)
var proxyMapPort = FlagArray(
	"proxy.map.port",
	`Explicitly map source port to destination port (separated by comma - "8443:443,18443:8443")`,
)

var stickyStoragePath = flag.String("stickiness-db-path", proxy.MemoryStorage, "Path to the database for stickiness mapping")

var filterHostnames = FlagArray(
	"filter.hostnames",
	`Explicitly forward just several hostnames (separated by comma - "ipinfo.io,ipify.org")`,
)
var filterZones = FlagArray(
	"filter.zones",
	`Explicitly forward just several DNS zones. A zone of "example.com" matches "example.com" and all of its subdomains. (separated by comma - "ipinfo.io,ipify.org")`,
)

var enableDomainTracer = flag.Bool("enable-domain-tracer", false, "Enable tracing domain names from requests")

type domainTracker interface {
	Inc(domain string)
	Dump() map[string]uint64
}

func main() {
	flag.Parse()

	dialerUpstreamURL, err := url.Parse(*proxyUpstreamURL)
	if err != nil || dialerUpstreamURL.Scheme != "http" {
		log.Fatalf("Invalid upstream URL: %s", *proxyUpstreamURL)
	}

	sm, err := proxy.NewStickyMapper(*stickyStoragePath)
	if err != nil {
		log.Fatalf("Failed to create sticky mapper, %v", err)
	}

	var domainTracer domainTracker = proxy.NewNoopTracer()
	if *enableDomainTracer {
		domainTracer = proxy.NewDomainTracer()
	}

	apiServer := api.NewServer(*proxyAPIAddr, sm, domainTracer)
	go apiServer.ListenAndServe()

	dialerUpstream := proxy.NewDialerHTTPConnect(proxy.DialerDirect, dialerUpstreamURL.Host)

	var dialer netproxy.Dialer
	if len(*filterHostnames) > 0 || len(*filterZones) > 0 {
		dialerPerHost := netproxy.NewPerHost(proxy.DialerDirect, dialerUpstream)
		for _, host := range *filterHostnames {
			log.Printf("Redirecting: %s -> %s", host, dialerUpstreamURL)
			dialerPerHost.AddHost(host)
		}
		for _, zone := range *filterZones {
			log.Printf("Redirecting: *.%s -> %s", zone, dialerUpstreamURL)
			dialerPerHost.AddZone(zone)
		}
		dialer = dialerPerHost
	} else {
		dialer = dialerUpstream
		log.Printf("Redirecting: * -> %s", dialerUpstreamURL)
	}

	portMap := parsePortMap(*proxyMapPort, *proxyAddr)
	proxyServer := proxy.NewServer(dialer, sm, domainTracer, portMap)

	var wg sync.WaitGroup
	for p := range portMap {
		wg.Add(1)
		go func(p string) {
			log.Print("Serving HTTPS proxy on :", p)
			proxyServer.ListenAndServe(":" + p)
			wg.Done()
		}(p)
	}

	wg.Wait()
}

func parsePortMap(ports flagArray, proxyAddr string) map[string]string {
	_, port, err := net.SplitHostPort(proxyAddr)
	if err != nil {
		log.Fatalf("Failed to parse port: %s", proxyAddr)
	}

	portsMap := map[string]string{port: "443"}
	for _, p := range ports {
		portMap := strings.Split(p, ":")
		if len(portMap) != 2 {
			log.Fatalf("Failed to parse port mapping: %s", p)
		}
		portsMap[portMap[0]] = portMap[1]
	}
	return portsMap
}

// FlagArray defines a string array flag
func FlagArray(name string, usage string) *flagArray {
	p := &flagArray{}
	flag.Var(p, name, usage)
	return p
}

type flagArray []string

func (flag *flagArray) String() string {
	return strings.Join(*flag, ",")
}

func (flag *flagArray) Set(s string) error {
	*flag = strings.FieldsFunc(s, func(c rune) bool {
		return c == ','
	})
	return nil
}
