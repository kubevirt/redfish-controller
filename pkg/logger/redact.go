package logger

import "net/url"

// RedactURL returns a form of a URL that is safe to log: scheme, host and
// path only. Image URLs handed to the bridge carry credentials in the query
// string (an image service token) or in the userinfo part, and the log files
// are read by people who must not get them. An input that does not parse is
// replaced entirely, since the reason it does not parse may be the secret.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" && u.Host == "" {
		return "<unparsable url>"
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
}
