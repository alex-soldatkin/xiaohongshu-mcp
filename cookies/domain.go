package cookies

import "strings"

// DomainMatches reports whether a cookie domain belongs to a site domain.
// Cookie domains come in three shapes — ".rednote.com", "rednote.com" and
// "www.rednote.com" — and all three belong to the site.
//
// It lives here rather than next to the deployment profiles because both the
// caller that picks a deployment and the browser manager that writes the
// session file need it, and the browser must not depend on the site package.
func DomainMatches(cookieDomain, siteDomain string) bool {
	d := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(cookieDomain), "."))
	return d == siteDomain || strings.HasSuffix(d, "."+siteDomain)
}
