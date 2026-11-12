package logger

import (
	"net/url"
	"strings"
)

const redactedMark = "<redacted>"

// RedactURL returns a form of a URL that is safe to log: scheme, host and
// path, with a marker in place of each removed piece (userinfo, query string,
// fragment) so the reader sees the URL is incomplete and does not try it as
// is. Image URLs handed to the bridge carry credentials in the query string
// (an image service token) or in the userinfo part, and the log files are
// read by people who must not get them. An input that does not parse is
// replaced entirely, since the reason it does not parse may be the secret.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" && u.Host == "" {
		return "<unparsable url>"
	}

	// The markers are added as text: url.URL would percent-encode them.
	redacted := (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
	if u.User != nil {
		redacted = strings.Replace(redacted, "//", "//"+redactedMark+"@", 1)
	}
	if u.RawQuery != "" {
		redacted += "?" + redactedMark
	}
	if u.Fragment != "" {
		redacted += "#" + redactedMark
	}
	return redacted
}
