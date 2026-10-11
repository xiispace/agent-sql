package resolve

import (
	stderrors "errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/shhac/agent-sql/internal/errors"
)

// genericURL is the structured form of a host:port:database connection
// URL (postgres, mysql, mariadb, mssql, sqlserver). Userinfo is preserved
// because ad-hoc URL connections legitimately carry credentials -- only
// stored connections reject them. Options carries any query-string
// parameters for pass-through to the driver.
type genericURL struct {
	Host     string
	Port     string
	Database string
	Username string
	Password string
	Options  map[string]string
}

// parseGenericURL parses a host-style connection URL. Returns a
// FixableByHuman error if the URL is malformed -- the previous
// behavior of silently swallowing parse errors and falling back to
// localhost:default-port was a footgun.
func parseGenericURL(connStr string) (genericURL, error) {
	u, err := url.Parse(connStr)
	if err != nil {
		return genericURL{}, errors.New(
			fmt.Sprintf("Invalid connection URL %q: %s", redactURL(connStr), urlParseCause(connStr, err)),
			errors.FixableByHuman,
		)
	}
	out := genericURL{
		Host:     u.Hostname(),
		Port:     u.Port(),
		Database: strings.TrimPrefix(u.Path, "/"),
		Username: u.User.Username(),
	}
	if out.Host == "" {
		out.Host = "localhost"
	}
	out.Password, _ = u.User.Password()
	if q := u.Query(); len(q) > 0 {
		out.Options = make(map[string]string, len(q))
		for k, vs := range q {
			if len(vs) > 0 {
				out.Options[k] = vs[0]
			}
		}
	}
	return out, nil
}

// parsePort returns the integer form of s, or def if s is empty or
// cannot be parsed.
func parsePort(s string, def int) int {
	if s == "" {
		return def
	}
	var p int
	_, _ = fmt.Sscanf(s, "%d", &p)
	if p == 0 {
		return def
	}
	return p
}

// withURLDatabase returns connStr with its database path replaced by
// database. Userinfo and the query string are preserved untouched.
func withURLDatabase(connStr, database string) (string, error) {
	u, err := url.Parse(connStr)
	if err != nil {
		return "", errors.New(
			fmt.Sprintf("Invalid connection URL %q: %s", redactURL(connStr), urlParseCause(connStr, err)),
			errors.FixableByHuman,
		)
	}
	u.Path = "/" + database
	u.RawPath = ""
	return u.String(), nil
}

// urlUserinfo matches the userinfo of a URL that may not parse: everything
// between "://" and the last "@". It errs towards hiding too much, since a
// malformed password may hold any character.
var urlUserinfo = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*://).*@`)

// redactURL hides a connection string's userinfo for error messages: an
// ad-hoc -c URL may embed a password, and errors reach the LLM.
func redactURL(s string) string { return urlUserinfo.ReplaceAllString(s, "${1}***@") }

// urlParseCause gives the reason a URL did not parse, without *url.Error's
// verbatim copy of the input. When the URL has userinfo the reason is
// dropped too: the parser may quote a fragment of the password (a password
// holding "/" reads as an invalid port).
func urlParseCause(connStr string, err error) string {
	if urlUserinfo.MatchString(connStr) {
		return "malformed URL (details hidden: it carries credentials)"
	}
	var ue *url.Error
	if stderrors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}
