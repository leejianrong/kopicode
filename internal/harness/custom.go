package harness

import (
	"net"
	"net/url"
	"strings"
)

// A custom provider URL (ADR-0027) points a session at a local or proxy
// endpoint instead of OpenRouter. This file holds what is specific to that:
// validating the URL, and the configuration name that keeps such a run from
// ever pooling with a registered arm.

// FlagProviderURL is the flag that names a custom provider URL.
const FlagProviderURL = "provider-url"

// CustomConfigNamePrefix marks a configuration resolved for a custom endpoint.
// It is in the hash preimage through [Config.Name], the same device
// [DeclaredConfigNamePrefix] uses, so a run against an endpoint nobody pinned
// can never be compared with a pinned one.
const CustomConfigNamePrefix = "custom:"

// ValidateProviderURL checks a custom provider URL and returns it with any
// trailing slash removed.
//
// It refuses what would make the endpoint a place the credential or the code
// could leak: credentials embedded in the URL, a scheme other than https, and
// http for any host that is not loopback. A query or fragment is refused too, as
// the client appends a path and would silently mangle one.
func ValidateProviderURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", usagef("provider url %q is not a URL (want https://host/v1, or http://localhost:11434/v1)", raw)
	}
	if u.User != nil {
		return "", usagef("provider url must not carry credentials; put the key in %s", "KOPICODE_PROVIDER_API_KEY")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", usagef("provider url must not have a query or a fragment")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !IsLoopbackHost(u.Hostname()) {
			return "", usagef("provider url %q is http to a host that is not loopback; use https, "+
				"or a loopback address for a local server", raw)
		}
	default:
		return "", usagef("provider url %q has scheme %q; want https, or http for loopback", raw, u.Scheme)
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

// IsLoopbackHost reports whether host is localhost or a loopback address.
func IsLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ProviderHost is the host (and port) of a validated custom provider URL, which
// is all the journal records of it: never a path, query or credential.
func ProviderHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Host
}

// ProviderURLIsLoopback reports whether a validated URL names a loopback host,
// the one case where a custom endpoint needs no credential.
func ProviderURLIsLoopback(rawURL string) bool {
	u, err := url.Parse(rawURL)
	return err == nil && IsLoopbackHost(u.Hostname())
}
