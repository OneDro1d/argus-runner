package chain

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// WHERE A DB STEP CONNECTS: a declared PostgreSQL target, read from its JDBC URL.
//
// ⛔ WHY A PARSER OF ITS OWN. The three JDBC readers already in the tree answer other questions: onboard's
// jdbcDriver returns the driver token, doctor/toolcore's jdbcAddr returns host:port. None of them yields the
// database name or the URL parameters a connection needs, and none REFUSES what it does not understand - they
// were written to describe a target, not to open one. A connection that silently dropped `currentSchema` would
// read a table in another schema and say so with a straight face, so this one maps the few parameters that change
// WHERE or HOW the read happens and refuses every other, by name.

// DBConn is a db step's target as the executor will open it. Username and Password come from the target's own
// fields (they are resolved from the environment when the config loads), never from the URL.
type DBConn struct {
	Host     string
	Port     int
	Database string
	User     string
	Password string
	// SSLMode is a libpq/pgjdbc sslmode ("" = the driver's default, prefer).
	SSLMode string
	// Schema is currentSchema, applied as the session's search_path ("" = the server's default).
	Schema string
	// AppName is ApplicationName ("" = the driver's default).
	AppName string
}

// pgSSLModes are the sslmode values libpq and pgjdbc share.
var pgSSLModes = map[string]bool{
	"disable": true, "allow": true, "prefer": true, "require": true, "verify-ca": true, "verify-full": true,
}

// schemaListRe is a currentSchema value: identifiers separated by commas. Quoting is not offered; a schema that
// needs it is not one a check should depend on.
var schemaListRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*(,[A-Za-z_][A-Za-z0-9_$]*)*$`)

// DBStepDialectRefusal is the sentence for a target that is not PostgreSQL.
func DBStepDialectRefusal(driver string) string {
	return "a db step reads PostgreSQL targets only (jdbc:postgresql://host[:port]/db) and this target's jdbc_url names " +
		driver + "; `## VERIFY` on a plain scenario covers the JDBC dialects"
}

// ParsePostgresJDBC reads jdbc:postgresql://host[:port]/db[?params] plus the target's username and password. The
// error never contains the URL or the password: a URL can carry credentials in its query, so only parameter NAMES
// are ever quoted.
func ParsePostgresJDBC(jdbcURL, user, password string) (DBConn, error) {
	raw := strings.TrimSpace(jdbcURL)
	if raw == "" {
		return DBConn{}, fmt.Errorf("the target declares no jdbc_url")
	}
	rest, ok := strings.CutPrefix(raw, "jdbc:")
	if !ok {
		return DBConn{}, fmt.Errorf("the target's jdbc_url is not a jdbc: URL (a db step needs jdbc:postgresql://host[:port]/db)")
	}
	driver, _, _ := strings.Cut(rest, ":")
	if !strings.EqualFold(driver, "postgresql") {
		return DBConn{}, fmt.Errorf("%s", DBStepDialectRefusal("the "+strconv.Quote(strings.ToLower(driver))+" driver"))
	}
	u, err := url.Parse(rest)
	if err != nil || u.Scheme != "postgresql" || u.Host == "" {
		return DBConn{}, fmt.Errorf("the target's jdbc_url is not of the form jdbc:postgresql://host[:port]/db[?params]")
	}
	if u.User != nil {
		return DBConn{}, fmt.Errorf("the target's jdbc_url carries credentials in its authority - put them in the target's " +
			"username and password fields, which a db step reads")
	}
	c := DBConn{Host: u.Hostname(), Port: 5432, User: user, Password: password}
	if c.Host == "" {
		return DBConn{}, fmt.Errorf("the target's jdbc_url names no host")
	}
	if p := u.Port(); p != "" {
		n, perr := strconv.Atoi(p)
		if perr != nil || n < 1 || n > 65535 {
			return DBConn{}, fmt.Errorf("the target's jdbc_url names a port outside 1..65535")
		}
		c.Port = n
	}
	c.Database = strings.TrimPrefix(u.Path, "/")
	if c.Database == "" || strings.Contains(c.Database, "/") {
		return DBConn{}, fmt.Errorf("the target's jdbc_url names no single database (jdbc:postgresql://host[:port]/db)")
	}
	if strings.TrimSpace(user) == "" {
		return DBConn{}, fmt.Errorf("the target declares no username - a db step connects as that role and never as a default")
	}
	q, qerr := url.ParseQuery(u.RawQuery)
	if qerr != nil {
		return DBConn{}, fmt.Errorf("the target's jdbc_url has a query string that does not parse")
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ssl := ""
	for _, k := range keys {
		v := q.Get(k)
		switch k {
		case "sslmode":
			if !pgSSLModes[v] {
				return DBConn{}, fmt.Errorf("the target's jdbc_url sets sslmode to a value outside disable, allow, prefer, require, verify-ca, verify-full")
			}
			c.SSLMode = v
		case "ssl":
			switch v {
			case "true", "":
				ssl = "verify-full" // pgjdbc: ssl=true validates the server certificate and its host name
			case "false":
				ssl = "disable"
			default:
				return DBConn{}, fmt.Errorf("the target's jdbc_url sets ssl to something other than true or false")
			}
		case "currentSchema":
			if !schemaListRe.MatchString(v) {
				return DBConn{}, fmt.Errorf("the target's jdbc_url sets currentSchema to something other than a comma-separated list of plain schema names")
			}
			c.Schema = v
		case "ApplicationName":
			c.AppName = v
		case "user", "password":
			return DBConn{}, fmt.Errorf("the target's jdbc_url carries the parameter %q - put credentials in the target's "+
				"username and password fields, which a db step reads", k)
		default:
			return DBConn{}, fmt.Errorf("the target's jdbc_url carries the parameter %q, which a db step does not map "+
				"(it reads sslmode, ssl, currentSchema and ApplicationName) - a parameter it dropped could change what is read, "+
				"so it is refused rather than ignored", k)
		}
	}
	if ssl != "" {
		if c.SSLMode != "" && c.SSLMode != ssl {
			return DBConn{}, fmt.Errorf("the target's jdbc_url sets both ssl and sslmode, and they disagree")
		}
		c.SSLMode = ssl
	}
	return c, nil
}
