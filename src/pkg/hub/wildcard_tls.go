package hub

import (
	"strings"
)

// normalizeDNSName lowercases a DNS name and strips the optional trailing root
// dot, so "Host.Example.COM." and "host.example.com" compare equal.
func normalizeDNSName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	return strings.TrimSuffix(name, ".")
}

// wildcardCoversHost reports whether a certificate issued for "*."+domain
// covers host.
//
// RFC 6125 §6.4.3: a wildcard matches exactly ONE label. Two consequences
// decide real cases here, and both are answered NO:
//
//   - The apex itself is not covered. "*.hive.hivecommons.dev" does not match
//     "hive.hivecommons.dev".
//   - A deeper name is not covered. "*.hive.hivecommons.dev" does not match
//     "a.b.hive.hivecommons.dev".
//
// The second is not hypothetical for this fleet: #5925 moves dibs to
// dibs.hivecommons.dev, one level ABOVE the wildcard's scope, which is why that
// host needs its own SAN and could never have been served by this wildcard.
// A coverage check that answered on the domain suffix alone would have said yes.
func wildcardCoversHost(host, domain string) bool {
	host = normalizeDNSName(host)
	domain = normalizeDNSName(domain)
	if host == "" || domain == "" {
		return false
	}
	suffix := "." + domain
	if !strings.HasSuffix(host, suffix) {
		return false
	}
	label := host[:len(host)-len(suffix)]
	// Empty label is the apex; a dot in the label means the host sits deeper
	// than one level under the domain. A wildcard covers neither.
	return label != "" && !strings.Contains(label, ".")
}

// servesHostFromWildcard reports whether this cluster's provisioned Ingresses
// should OMIT their per-host tls: block for host and let the ingress
// controller's default wildcard certificate serve it instead.
//
// Every condition is a veto, and the default answer is no:
//
//   - WildcardTLSSecret empty — the operator has not declared that this
//     cluster's controller serves a wildcard by default. This is the opt-in,
//     and it stays off until the cluster prerequisites in #5977 are done: the
//     wildcard secret present, and --default-ssl-certificate pointing at it.
//     Neither of those is visible from here, which is precisely why this is an
//     operator assertion rather than something inferred.
//   - OpenShift Route clusters — Routes terminate TLS with edge termination and
//     no secret, so they never minted a per-host certificate through this path
//     and --default-ssl-certificate is an ingress-nginx concept that does not
//     apply. Nothing to change, so nothing is changed.
//   - The host is not covered by *.<domain> — see wildcardCoversHost.
func (c ClusterConfig) servesHostFromWildcard(host string) bool {
	if strings.TrimSpace(c.WildcardTLSSecret) == "" {
		return false
	}
	if c.IngressType == ingressTypeOpenShiftRoute {
		return false
	}
	return wildcardCoversHost(host, c.Domain)
}
