/**
 * Tencent is pleased to support the open source community by making CL5 available.
 *
 * Copyright (C) 2019 THL A29 Limited, a Tencent company. All rights reserved.
 *
 * Licensed under the BSD 3-Clause License (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * https://opensource.org/licenses/BSD-3-Clause
 *
 * Unless required by applicable law or agreed to in writing, software distributed
 * under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR
 * CONDITIONS OF ANY KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations under the License.
 */

package meshproxy

import (
	"context"
	"net"
	"strings"
	"sync/atomic"

	"github.com/miekg/dns"

	"github.com/polarismesh/polaris-sidecar/log"
)

type LocalDNSServer struct {
	// dns look up table
	lookupTable atomic.Value
	dnsTtl      uint32
}

func (h *LocalDNSServer) UpdateLookupTable(polarisServices []string, dnsResponseIp string) {
	lookupTable := &LookupTable{
		allHosts: map[string]struct{}{},
		name4:    map[string][]dns.RR{},
		name6:    map[string][]dns.RR{},
		cname:    map[string][]dns.RR{},
		dnsTtl:   h.dnsTtl,
	}

	var altHosts map[string]struct{}
	for _, service := range polarisServices {
		altHosts = map[string]struct{}{service + ".": {}}
		lookupTable.buildDNSAnswers(altHosts, []net.IP{net.ParseIP(dnsResponseIp)}, nil)
	}
	h.lookupTable.Store(lookupTable)
	log.Infof("[mesh] updated lookup table with %d hosts", len(lookupTable.allHosts))
}

type LookupTable struct {
	// This table will be first looked up to see if the host is something that we got a Nametable entry for
	// (i.e. came from istiod's service registry). If it is, then we will be able to confidently return
	// NXDOMAIN errors for AAAA records for such hosts when only A records exist (or vice versa). If the
	// host does not exist in this map, then we will return nil, causing the caller to query the upstream
	// DNS server to resolve the host. Without this map, we would end up making unnecessary upstream DNS queries
	// for hosts that will never resolve (e.g., AAAA for svc1.ns1.svc.cluster.local.svc.cluster.local.)
	allHosts map[string]struct{}

	// The key is a FQDN matching a DNS query (like example.com.), the value is pre-created DNS RR records
	// of A or AAAA type as appropriate.
	name4 map[string][]dns.RR
	name6 map[string][]dns.RR
	// The cname records here (comprised of different variants of the hosts above,
	// expanded by the search namespaces) pointing to the actual host.
	cname map[string][]dns.RR

	dnsTtl uint32
}

func (table *LookupTable) buildDNSAnswers(altHosts map[string]struct{}, ipv4 []net.IP, ipv6 []net.IP) {
	for h := range altHosts {
		h = strings.ToLower(h)
		table.allHosts[h] = struct{}{}
		if len(ipv4) > 0 {
			table.name4[h] = a(h, ipv4, table.dnsTtl)
		}
		if len(ipv6) > 0 {
			table.name6[h] = aaaa(h, ipv6, table.dnsTtl)
		}
	}
}

// Given a host, this function first decides if the host is part of our service registry.
// If it is not part of the registry, return nil so that caller queries upstream. If it is part
// of registry, we will look it up in one of our tables, failing which we will return NXDOMAIN.
func (table *LookupTable) lookupHost(qtype uint16, hostname string) ([]dns.RR, bool) {
	var hostFound bool
	if _, hostFound = table.allHosts[hostname]; !hostFound {
		// this is not from our registry
		return nil, false
	}

	var out []dns.RR
	// Odds are, the first query will always be an expanded hostname
	// (productpage.ns1.svc.cluster.local.ns1.svc.cluster.local)
	// So lookup the cname table first
	cn := table.cname[hostname]
	if len(cn) > 0 {
		// this was a cname match
		hostname = cn[0].(*dns.CNAME).Target
	}
	var ipAnswers []dns.RR
	switch qtype {
	case dns.TypeA:
		ipAnswers = table.name4[hostname]
	case dns.TypeAAAA:
		ipAnswers = table.name6[hostname]
	}

	if len(ipAnswers) > 0 {
		// We will return a chained response. In a chained response, the first entry is the cname record,
		// and the second one is the A/AAAA record itself. Some clients do not follow cname redirects
		// with additional DNS queries. Instead, they expect all the resolved records to be in the same
		// big DNS response (presumably assuming that a recursive DNS query should do the deed, resolve
		// cname et al and return the composite response).
		out = append(out, cn...)
		out = append(out, ipAnswers...)
	}
	return out, hostFound
}

func newLocalDNSServer(dnsTtl uint32) (*LocalDNSServer, error) {
	h := &LocalDNSServer{
		dnsTtl: dnsTtl,
	}
	return h, nil
}

func (h *LocalDNSServer) ServeDNS(ctx context.Context, question *dns.Question) *dns.Msg {
	var response *dns.Msg
	lp := h.lookupTable.Load()
	if lp == nil {
		return nil
	}

	lookupTable := lp.(*LookupTable)
	var answers []dns.RR

	hostname := strings.ToLower(question.Name)
	answers, hostFound := lookupTable.lookupHost(question.Qtype, hostname)

	if hostFound {
		response = new(dns.Msg)
		response.Authoritative = true
		response.Answer = answers
		response.Rcode = dns.RcodeSuccess
		return response
	}
	return nil
}

// Borrowed from https://github.com/coredns/coredns/blob/master/plugin/hosts/hosts.go
// a takes a slice of net.IPs and returns a slice of A RRs.
func a(host string, ips []net.IP, ttl uint32) []dns.RR {
	answers := make([]dns.RR, len(ips))
	for i, ip := range ips {
		r := new(dns.A)
		r.Hdr = dns.RR_Header{Name: host, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl}
		r.A = ip
		answers[i] = r
	}
	return answers
}

// aaaa takes a slice of net.IPs and returns a slice of AAAA RRs.
func aaaa(host string, ips []net.IP, ttl uint32) []dns.RR {
	answers := make([]dns.RR, len(ips))
	for i, ip := range ips {
		r := new(dns.AAAA)
		r.Hdr = dns.RR_Header{Name: host, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: ttl}
		r.AAAA = ip
		answers[i] = r
	}
	return answers
}

func cname(host string, targetHost string, ttl uint32) []dns.RR {
	answer := new(dns.CNAME)
	answer.Hdr = dns.RR_Header{
		Name:   host,
		Rrtype: dns.TypeCNAME,
		Class:  dns.ClassINET,
		Ttl:    ttl,
	}
	answer.Target = targetHost
	return []dns.RR{answer}
}
