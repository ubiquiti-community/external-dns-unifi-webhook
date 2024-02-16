package stackitprovider

import "sigs.k8s.io/external-dns/endpoint"

func (d *StackitDNSProvider) GetDomainFilter() endpoint.DomainFilter {
	return d.domainFilter
}
