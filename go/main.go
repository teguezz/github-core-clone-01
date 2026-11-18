package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// The forge's lifecycle shell: a faithful port of the reference main.ts.
// The environment's origin arrives as MAKEFUN_ENV_URL; the server binds an
// ephemeral port, announces it with POST /register (200 or 409 finish; 400
// and 405 are fatal; anything else retries on a doubling pause), then polls
// GET /done until the environment dismisses it, and exits 0.

const envCallTimeout = 30 * time.Second

func retryDelay(failures int) time.Duration {
	exp := failures
	if exp > 8 {
		exp = 8
	}
	delay := 250 * (1 << exp)
	if delay > 2000 {
		delay = 2000
	}
	return time.Duration(delay) * time.Millisecond
}

func announcePort(client *http.Client, envOrigin string, port int) error {
	failures := 0
	for {
		done, err := func() (bool, error) {
			payload, _ := json.Marshal(map[string]int{"port": port})
			req, err := http.NewRequest("POST", envOrigin+"/register", bytes.NewReader(payload))
			if err != nil {
				return false, nil
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				return false, nil
			}
			defer resp.Body.Close()
			if resp.StatusCode == 200 || resp.StatusCode == 409 {
				return true, nil
			}
			if resp.StatusCode == 400 || resp.StatusCode == 405 {
				body, _ := io.ReadAll(resp.Body)
				return false, fmt.Errorf("register answered HTTP %d: %s", resp.StatusCode, string(body))
			}
			io.Copy(io.Discard, resp.Body)
			return false, nil
		}()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		time.Sleep(retryDelay(failures))
		failures++
	}
}

func awaitDismissal(client *http.Client, envOrigin string) {
	failures := 0
	for {
		done := func() bool {
			resp, err := client.Get(envOrigin + "/done")
			if err != nil {
				return false
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				io.Copy(io.Discard, resp.Body)
				return false
			}
			var payload struct {
				Done *bool `json:"done"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
				return false
			}
			return payload.Done != nil && *payload.Done
		}()
		if done {
			return
		}
		time.Sleep(retryDelay(failures))
		failures++
	}
}

func main() {
	envOrigin := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envOrigin == "" {
		fmt.Fprint(os.Stderr, "[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n")
		os.Exit(1)
	}
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")

	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
		os.Exit(1)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	server := &http.Server{Handler: newApp(token)}
	go func() {
		_ = server.Serve(listener)
	}()
	fmt.Printf("[entry] listening on port %d\n", port)

	client := &http.Client{Timeout: envCallTimeout}
	if err := announcePort(client, envOrigin, port); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
		os.Exit(1)
	}
	fmt.Print("[entry] registered; waiting for the env to say done\n")

	awaitDismissal(client, envOrigin)
	server.Close()
	fmt.Print("[entry] done\n")
}
