// SPDX-FileCopyrightText: 2026 LINUXexpert-org
// SPDX-License-Identifier: GPL-3.0-or-later

package validate

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// TestMain lets this test binary also act as a fake Stalwart binary,
// mirroring internal/recovery's own TestMain - see that package's doc
// comment for why (the standard os/exec "helper process" technique). Beyond
// plain reachability, it also speaks just enough JMAP to serve
// stalwartapi.Client.AccountSnapshot (x:Account/query, x:Account/get,
// session discovery, Mailbox/get) for a single fixed fake account
// "alice@example.com", so BootCheck's content-integrity comparison can be
// exercised against a real subprocess rather than mocked in-process. The
// mailbox message count it reports is configurable via
// STALWART_MIGRATOR_TEST_MAILBOX_COUNT (default 42), so tests can produce
// both a matching and a mismatching post-migration snapshot.
func TestMain(m *testing.M) {
	if os.Getenv("STALWART_MIGRATOR_TEST_HELPER") == "1" {
		runFakeStalwartServer()
		return
	}
	os.Exit(m.Run())
}

func runFakeStalwartServer() {
	port := os.Getenv("STALWART_MIGRATOR_TEST_PORT")
	messageCount := 42
	if v := os.Getenv("STALWART_MIGRATOR_TEST_MAILBOX_COUNT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			messageCount = n
		}
	}

	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake stalwart: listen:", err)
		os.Exit(1)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/principal" {
			w.WriteHeader(http.StatusNotFound) // v0.16 shape: no REST management API
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/.well-known/jmap":
			user, _, _ := r.BasicAuth()
			if strings.Contains(user, "%") {
				json.NewEncoder(w).Encode(map[string]any{
					"apiUrl":          "http://127.0.0.1:" + port + "/api",
					"primaryAccounts": map[string]string{"urn:ietf:params:jmap:mail": "mail-alice"},
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"apiUrl":       "http://127.0.0.1:" + port + "/api",
				"capabilities": map[string]any{"urn:ietf:params:jmap:core": map[string]any{}, "urn:stalwart:jmap": map[string]any{}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			methodCalls, _ := body["methodCalls"].([]any)
			if len(methodCalls) == 0 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			call := methodCalls[0].([]any)
			name := call[0].(string)
			switch name {
			case "x:Domain/query":
				json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{
					[]any{"x:Domain/query", map[string]any{"ids": []string{"d1"}}, "q"},
					[]any{"x:Domain/get", map[string]any{"list": []map[string]any{
						{"id": "d1", "name": "example.com"},
					}}, "g"},
				}})
			case "x:Account/query":
				json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{
					[]any{"x:Account/query", map[string]any{"ids": []string{"a1"}}, "q"},
				}})
			case "x:Account/get":
				json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{
					[]any{"x:Account/get", map[string]any{"list": []map[string]any{
						{"id": "a1", "name": "alice@example.com", "domainId": "example.com"},
					}}, "g"},
				}})
			case "Mailbox/get":
				json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{
					[]any{"Mailbox/get", map[string]any{"list": []map[string]any{
						{"name": "Inbox", "totalEmails": messageCount},
					}}, "m"},
				}})
			default:
				w.WriteHeader(http.StatusBadRequest)
			}
		default:
			w.WriteHeader(http.StatusOK)
		}
	})}
	go srv.Serve(ln)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM)
	<-sigCh
	os.Exit(0)
}
