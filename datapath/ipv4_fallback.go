package datapath

import "net"

// Only used for an IPv6 TLS destination. DNS is resolved through the selected
// physical network, while the app retains its original TLS session and SNI.
func (p *lightPolicy) withIPv4Fallback(host string, alternatives []net.IP) []net.IP {
	if host == "" {
		return alternatives
	}
	for _, ip := range alternatives {
		if ip.To4() != nil {
			return alternatives
		}
	}
	query, err := makeDNSQuery(host, 1)
	if err != nil || p.doh == nil {
		return alternatives
	}
	response, err := p.doh.resolve(query)
	if err != nil {
		return alternatives
	}
	name, addresses, err := parseDNSAnswer(response)
	if err != nil || name != host {
		return alternatives
	}
	for _, ip := range addresses {
		if ip.To4() != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() {
			alternatives = append(alternatives, ip)
			if len(alternatives) >= 4 {
				break
			}
		}
	}
	return alternatives
}
