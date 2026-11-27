package server

import (
	"errors"
	"fmt"
	"os"
	"regexp"
)

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ---------- admin.json ----------

type Role string

const (
	RoleViewer Role = "viewer"
	RoleEditor Role = "editor"
)

// envAdminToken is an extra editor token from the environment (for
// containers and secrets managers); at least 16 characters.
const envAdminTokenVar = "FLUID_ADMIN_TOKEN"

func envAdminToken() string {
	if v := os.Getenv(envAdminTokenVar); len(v) >= 16 {
		return v
	}
	return ""
}

// AdminConfig lives in <dynamic_dir>/admin.json. It is never editable over
// the API, so a viewer token can never be turned into an editor token.
type AdminConfig struct {
	Schema string       `json:"$schema,omitempty"`
	Tokens []AdminToken `json:"tokens"`
}

type AdminToken struct {
	Name  string `json:"name"`
	Token string `json:"token"`
	Role  Role   `json:"role"`
}

func (a *AdminConfig) normalize() error {
	var errs []error
	seen := map[string]bool{}
	for i, t := range a.Tokens {
		if !idRe.MatchString(t.Name) {
			errs = append(errs, fmt.Errorf("tokens[%d].name must match %s", i, idRe))
		}
		if seen[t.Name] {
			errs = append(errs, fmt.Errorf("duplicate token name %q", t.Name))
		}
		seen[t.Name] = true
		if len(t.Token) < 16 {
			errs = append(errs, fmt.Errorf("token %q must be at least 16 characters", t.Name))
		}
		if t.Role != RoleViewer && t.Role != RoleEditor {
			errs = append(errs, fmt.Errorf("token %q: role must be \"viewer\" or \"editor\"", t.Name))
		}
	}
	return errors.Join(errs...)
}
