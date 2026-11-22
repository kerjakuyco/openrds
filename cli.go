package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

const helpText = `OpenRDS database control plane

Run without arguments to start the server. On the VPS, from the installation
directory:

  docker compose exec openrds openrds reset-password <username>
      Print a one-time link that lets the account choose a new password,
      for example when the only administrator has forgotten theirs.

  docker compose exec openrds openrds api-key
      Print the API key that scripts send as "Authorization: Bearer <key>".
`

// command runs a maintenance command and returns the process exit code.
func command(args []string, out, errOut io.Writer) int {
	dir := env("OPENRDS_DATA_DIR", ".openrds")
	switch args[0] {
	case "reset-password":
		if len(args) != 2 {
			fmt.Fprintln(errOut, "Usage: openrds reset-password <username>")
			return 2
		}
		key, err := adminToken(dir)
		if err == nil {
			var link string
			if link, err = resetPassword(localURL(), key, args[1], publicURL()); err == nil {
				fmt.Fprintf(out, "Send this one-time link to %s. It works once and expires in 72 hours:\n\n  %s\n\n"+
					"If you are not on the VPS, open it through an SSH tunnel to the dashboard port.\n", args[1], link)
				return 0
			}
		}
		fmt.Fprintln(errOut, err)
		return 1
	case "api-key":
		key, err := adminToken(dir)
		if err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
		fmt.Fprintln(out, key)
		return 0
	case "gateway":
		return runGateway(out, errOut)
	case "help", "-h", "--help":
		fmt.Fprint(out, helpText)
		return 0
	default:
		fmt.Fprintf(errOut, "Unknown command %q.\n\n%s", args[0], helpText)
		return 2
	}
}

func listenPort() string {
	if _, port, err := net.SplitHostPort(env("OPENRDS_LISTEN", "127.0.0.1:8080")); err == nil && port != "" {
		return port
	}
	return "8080"
}

// The server inside the container, and the address people open in a browser.
func localURL() string { return "http://127.0.0.1:" + listenPort() }
func publicURL() string {
	return env("OPENRDS_PUBLIC_URL", "http://127.0.0.1:"+env("OPENRDS_PUBLIC_PORT", listenPort()))
}

// resetPassword asks the running server for a reset link, so its accounts stay
// the single source of truth while it runs.
func resetPassword(base, key, username, public string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	call := func(method, path string, dst any) error {
		req, err := http.NewRequest(method, base+"/api/"+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-OpenRDS-Client", "command-line")
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("cannot reach the OpenRDS server at %s; is it running? (%v)", base, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode >= 300 {
			var e struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(body, &e) != nil || e.Error == "" {
				e.Error = resp.Status
			}
			return errors.New(e.Error)
		}
		return json.Unmarshal(body, dst)
	}
	var users []userView
	if err := call("GET", "users", &users); err != nil {
		return "", err
	}
	if len(users) == 0 {
		return "", errors.New("there are no accounts yet; open the dashboard and create the administrator account")
	}
	wanted := strings.ToLower(strings.TrimSpace(username))
	var names []string
	for _, u := range users {
		if u.Username == wanted {
			var res struct {
				Secret string `json:"secret"`
			}
			if err := call("POST", "users/"+u.ID+"/reset", &res); err != nil {
				return "", err
			}
			return strings.TrimRight(public, "/") + "/#/invite/" + res.Secret, nil
		}
		names = append(names, u.Username)
	}
	sort.Strings(names)
	return "", fmt.Errorf("no account is called %q; the accounts are %s", username, strings.Join(names, ", "))
}
