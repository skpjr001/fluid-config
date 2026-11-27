package config

import "errors"

// TLSConfig enables HTTPS on a listener, from certificate files or ACME.
type TLSConfig struct {
	CertFile   string      `json:"cert_file,omitempty"`   // PEM certificate chain; re-read when it changes
	KeyFile    string      `json:"key_file,omitempty"`    // PEM private key
	ACME       *ACMEConfig `json:"acme,omitempty"`        // automatic certificates (Let's Encrypt)
	MinVersion string      `json:"min_version,omitempty"` // "1.2" (default) or "1.3"
}

// ACMEConfig obtains certificates automatically. The TLS-ALPN-01 challenge
// works when the listener is reachable on :443; otherwise set
// http_challenge_addr (":80") for HTTP-01.
type ACMEConfig struct {
	Domains           []string `json:"domains"`
	Email             string   `json:"email,omitempty"`
	CacheDir          string   `json:"cache_dir,omitempty"`           // default <database dir>/acme
	DirectoryURL      string   `json:"directory_url,omitempty"`       // default Let's Encrypt production
	CAFile            string   `json:"ca_file,omitempty"`             // PEM roots to trust for a private ACME server (step-ca, Pebble)
	HTTPChallengeAddr string   `json:"http_challenge_addr,omitempty"` // e.g. ":80"; also redirects other HTTP to HTTPS
	AcceptTOS         bool     `json:"accept_tos"`                    // must be true: you accept the CA's terms
}

// Validate checks the TLS block; a nil block is valid (plain HTTP).
func (t *TLSConfig) Validate() error {
	if t == nil {
		return nil
	}
	files := t.CertFile != "" || t.KeyFile != ""
	switch {
	case files && t.ACME != nil:
		return errors.New("set cert_file/key_file or acme, not both")
	case files && (t.CertFile == "" || t.KeyFile == ""):
		return errors.New("cert_file and key_file must both be set")
	case !files && t.ACME == nil:
		return errors.New("set cert_file + key_file, or acme")
	}
	if a := t.ACME; a != nil {
		if len(a.Domains) == 0 {
			return errors.New("acme.domains is required")
		}
		if !a.AcceptTOS {
			return errors.New("acme.accept_tos must be true (you accept the certificate authority's terms of service)")
		}
	}
	if t.MinVersion != "" && t.MinVersion != "1.2" && t.MinVersion != "1.3" {
		return errors.New(`min_version must be "1.2" or "1.3"`)
	}
	return nil
}
