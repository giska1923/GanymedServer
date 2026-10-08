// Command gscli is the backend's test client: a scriptable stand-in for the game, faster to
// iterate with than launching the engine.
//
//	gscli [-server URL] [-profile NAME] [-v] <command> [args]
//
//	login                       sign in with this profile's device ID (created on first use)
//	me                          GET /v1/me with the stored access token
//	refresh                     exchange the stored refresh token for a new session
//	profile                     show this player's profile
//	rename NAME                 change the display name
//	submit BOARD SCORE [KEY]    submit a score; KEY is the Idempotency-Key, a fresh UUID if
//	                            omitted. Pass the key a previous submit printed to retry it.
//	top BOARD [N]               the first N entries (default 10)
//	rank BOARD                  this player's rank and best
//	party                       this player's party (or null)
//	party-create                start a party, led by this player
//	invite ACCOUNT_ID           invite a player (leader only)
//	invites                     pending invites to this player
//	accept PARTY_ID             accept an invite
//	decline PARTY_ID            decline an invite
//	leave                       leave the party
//	kick ACCOUNT_ID             remove a member (leader only)
//	listen                      open the realtime socket and print every push until Ctrl+C or
//	                            the server closes it (the close code is printed)
//
// A profile is one simulated player: a device ID plus the tokens from its last login or refresh,
// kept in the user config directory. -profile mirrors the engine's --profile= flag.
//
// Unlike the engine, which keeps tokens in memory only, gscli stores them in the profile file:
// each invocation is a separate process, and a test client that had to log in before every call
// could never exercise refresh or expiry.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/coder/websocket"
)

type profile struct {
	DeviceID     string `json:"device_id"`
	AccountID    string `json:"account_id,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

type cli struct {
	server  string
	path    string
	verbose bool
	http    *http.Client
	prof    profile
}

func main() {
	server := flag.String("server", "http://localhost:8080", "backend base URL")
	name := flag.String("profile", "default", "simulated player")
	verbose := flag.Bool("v", false, "print each HTTP exchange (tokens redacted)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: gscli [-server URL] [-profile NAME] [-v] <command> [args]")
		fmt.Fprintln(os.Stderr, "commands: login | me | refresh | profile | rename NAME |")
		fmt.Fprintln(os.Stderr, "          submit BOARD SCORE [KEY] | top BOARD [N] | rank BOARD |")
		fmt.Fprintln(os.Stderr, "          party | party-create | invite ACCOUNT_ID | invites | accept PARTY_ID |")
		fmt.Fprintln(os.Stderr, "          decline PARTY_ID | leave | kick ACCOUNT_ID | listen")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}

	c, err := newCLI(*server, *name, *verbose)
	if err == nil {
		err = c.run(flag.Arg(0), flag.Args()[1:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gscli:", err)
		os.Exit(1)
	}
}

func newCLI(server, name string, verbose bool) (*cli, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	c := &cli{
		server:  strings.TrimRight(server, "/"),
		path:    filepath.Join(dir, "GanymedServer", "gscli", name+".json"),
		verbose: verbose,
		http:    &http.Client{Timeout: 10 * time.Second},
	}

	data, err := os.ReadFile(c.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		c.prof.DeviceID = newUUID()
		return c, c.save()
	case err != nil:
		return nil, err
	}
	return c, json.Unmarshal(data, &c.prof)
}

func (c *cli) run(cmd string, args []string) error {
	want := func(min, max int, usage string) error {
		if len(args) < min || len(args) > max {
			return fmt.Errorf("usage: gscli %s %s", cmd, usage)
		}
		return nil
	}

	switch cmd {
	case "login":
		return c.session("/v1/auth/device", map[string]string{"device_id": c.prof.DeviceID})
	case "refresh":
		if c.prof.RefreshToken == "" {
			return errors.New("no refresh token in this profile: run login first")
		}
		return c.session("/v1/auth/refresh", map[string]string{"refresh_token": c.prof.RefreshToken})
	case "me":
		return c.print("GET", "/v1/me", nil, nil)
	case "profile":
		return c.print("GET", "/v1/me/profile", nil, nil)
	case "rename":
		if err := want(1, 1, "NAME"); err != nil {
			return err
		}
		return c.print("PATCH", "/v1/me/profile", map[string]string{"display_name": args[0]}, nil)
	case "submit":
		if err := want(2, 3, "BOARD SCORE [KEY]"); err != nil {
			return err
		}
		// Sent as a JSON number exactly as typed, so the server's integer validation can be
		// exercised: "12.0" and "-1" are rejected there, not here.
		score := json.RawMessage(args[1])
		key := newUUID()
		if len(args) == 3 {
			key = args[2]
		}
		fmt.Println("Idempotency-Key:", key)
		return c.print("POST", "/v1/leaderboards/"+args[0]+"/scores", map[string]any{"score": score},
			map[string]string{"Idempotency-Key": key})
	case "top":
		if err := want(1, 2, "BOARD [N]"); err != nil {
			return err
		}
		route := "/v1/leaderboards/" + args[0]
		if len(args) == 2 {
			route += "?limit=" + args[1]
		}
		return c.print("GET", route, nil, nil)
	case "rank":
		if err := want(1, 1, "BOARD"); err != nil {
			return err
		}
		return c.print("GET", "/v1/leaderboards/"+args[0]+"/me", nil, nil)
	case "party":
		return c.print("GET", "/v1/party", nil, nil)
	case "party-create":
		return c.print("POST", "/v1/party", nil, nil)
	case "invite", "kick":
		if err := want(1, 1, "ACCOUNT_ID"); err != nil {
			return err
		}
		route := map[string]string{"invite": "/v1/party/invites", "kick": "/v1/party/kick"}[cmd]
		return c.print("POST", route, map[string]string{"account_id": args[0]}, nil)
	case "invites":
		return c.print("GET", "/v1/party/invites", nil, nil)
	case "accept", "decline":
		if err := want(1, 1, "PARTY_ID"); err != nil {
			return err
		}
		return c.print("POST", "/v1/party/invites/"+args[0]+"/"+cmd, nil, nil)
	case "leave":
		return c.print("POST", "/v1/party/leave", nil, nil)
	case "listen":
		return c.listen()
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// session posts to a route that returns a session and stores the tokens.
func (c *cli) session(route string, req any) error {
	status, _, body, err := c.do("POST", route, req, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("%d %s", status, body)
	}

	var resp struct {
		AccountID    string `json:"account_id"`
		AccessToken  string `json:"access_token"`
		ExpiresIn    int    `json:"expires_in"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("decode session: %w", err)
	}
	c.prof.AccountID, c.prof.AccessToken, c.prof.RefreshToken = resp.AccountID, resp.AccessToken, resp.RefreshToken
	if err := c.save(); err != nil {
		return err
	}
	fmt.Printf("signed in: account %s, access token valid %ds\n", resp.AccountID, resp.ExpiresIn)
	return nil
}

// listen holds the realtime socket open and prints each push as it arrives. Ctrl+C closes it
// normally; a close from the server prints its code, which is how 1001 and 4001 are observed.
func (c *cli) listen() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	url := "ws" + strings.TrimPrefix(c.server, "http") + "/v1/realtime"
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, resp, err := websocket.Dial(dialCtx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + c.prof.AccessToken}},
	})
	cancel()
	if err != nil {
		if resp != nil {
			return fmt.Errorf("upgrade refused: %s", resp.Status)
		}
		return err
	}
	defer conn.CloseNow()
	fmt.Fprintf(os.Stderr, "connected to %s as %s; Ctrl+C to stop\n", url, c.prof.AccountID)

	for {
		_, msg, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				conn.Close(websocket.StatusNormalClosure, "")
				return nil
			}
			if code := websocket.CloseStatus(err); code != -1 {
				fmt.Printf("closed by server: %d %s\n", int(code), code)
				return nil
			}
			return fmt.Errorf("connection lost: %w", err)
		}
		fmt.Printf("%s %s\n", time.Now().Format("15:04:05.000"), msg)
	}
}

// print performs a request and prints the status, the replay header if present, and the body.
func (c *cli) print(method, route string, body any, headers map[string]string) error {
	status, header, respBody, err := c.do(method, route, body, headers)
	if err != nil {
		return err
	}
	if header.Get("Idempotent-Replayed") == "true" {
		fmt.Println("(replayed: the server had already processed this key)")
	}
	fmt.Println(status, string(respBody))
	return nil
}

func (c *cli) do(method, route string, body any, headers map[string]string) (int, http.Header, []byte, error) {
	var reqBody []byte
	if body != nil {
		var err error
		if reqBody, err = json.Marshal(body); err != nil {
			return 0, nil, nil, err
		}
	}
	req, err := http.NewRequest(method, c.server+route, bytes.NewReader(reqBody))
	if err != nil {
		return 0, nil, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Auth routes authenticate by their body; sending the access token there is noise at best.
	if c.prof.AccessToken != "" && !strings.HasPrefix(route, "/v1/auth/") {
		req.Header.Set("Authorization", "Bearer "+c.prof.AccessToken)
	}
	if c.verbose {
		fmt.Fprintf(os.Stderr, "> %s %s\n", method, req.URL)
		if req.Header.Get("Authorization") != "" {
			fmt.Fprintf(os.Stderr, "> Authorization: Bearer %s\n", redact(c.prof.AccessToken))
		}
		for k, v := range headers {
			fmt.Fprintf(os.Stderr, "> %s: %s\n", k, v)
		}
		if reqBody != nil {
			fmt.Fprintf(os.Stderr, "> %s\n", redactJSON(reqBody))
		}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, err
	}
	respBody = bytes.TrimSpace(respBody)
	if c.verbose {
		fmt.Fprintf(os.Stderr, "< %s\n", resp.Status)
		for _, h := range []string{"Content-Type", "Cache-Control", "Www-Authenticate", "Idempotent-Replayed", "X-Request-Id"} {
			if v := resp.Header.Get(h); v != "" {
				fmt.Fprintf(os.Stderr, "< %s: %s\n", h, v)
			}
		}
		fmt.Fprintf(os.Stderr, "< %s\n", redactJSON(respBody))
	}
	return resp.StatusCode, resp.Header, respBody, nil
}

func (c *cli) save() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c.prof, "", "  ")
	if err != nil {
		return err
	}
	// 0600: the file holds a device ID and a refresh token, both credentials.
	return os.WriteFile(c.path, data, 0o600)
}

// newUUID returns a random (version 4) UUID, as the engine will generate one.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 9562 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func redact(secret string) string {
	if len(secret) <= 8 {
		return "…"
	}
	return secret[:8] + "…"
}

var secretField = regexp.MustCompile(`"(device_id|access_token|refresh_token)":"([^"]*)"`)

// redactJSON shortens credential fields so -v output is safe to paste into an issue or a doc.
func redactJSON(b []byte) string {
	return secretField.ReplaceAllStringFunc(string(b), func(m string) string {
		parts := secretField.FindStringSubmatch(m)
		return fmt.Sprintf(`"%s":"%s"`, parts[1], redact(parts[2]))
	})
}
