package tls

import "golang.org/x/crypto/acme"

// stagingClient returns an autocert.Client wired to LE's staging
// directory. Used by Options.Staging so CI / smoke setups don't
// burn the production rate limit (and don't hand out
// browser-untrusted certs into prod).
func stagingClient() *acme.Client {
	return &acme.Client{
		DirectoryURL: "https://acme-staging-v02.api.letsencrypt.org/directory",
	}
}
