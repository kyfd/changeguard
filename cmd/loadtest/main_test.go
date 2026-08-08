package main

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
)

func TestAuthenticateStoresSessionCookie(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "dbguard_session", Value: "test-session", Path: "/"})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{}"))
		case r.Method == http.MethodGet && r.URL.Path == "/api/dashboard":
			cookie, err := r.Cookie("dbguard_session")
			if err != nil || cookie.Value != "test-session" {
				http.Error(w, "missing session", http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	if err := authenticate(client, server.URL+"/api/auth/login", "owner@example.com", "owner123"); err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/dashboard", nil)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("authenticated client did not reuse session cookie: %d", response.StatusCode)
	}
}
