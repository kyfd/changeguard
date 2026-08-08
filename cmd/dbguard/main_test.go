package main

import "testing"

func TestWorkerCountFromEnvironment(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    int
		wantErr bool
	}{
		{name: "default", want: 2},
		{name: "disabled", value: "0", want: 0},
		{name: "configured", value: "4", want: 4},
		{name: "negative", value: "-1", wantErr: true},
		{name: "too large", value: "65", wantErr: true},
		{name: "invalid", value: "two", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("DBGUARD_WORKERS", test.value)
			got, err := workerCountFromEnvironment()
			if test.wantErr {
				if err == nil {
					t.Fatal("expected validation error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("worker count = %d, want %d", got, test.want)
			}
		})
	}
}

func TestStartupMode(t *testing.T) {
	tests := []struct {
		name      string
		arguments []string
		checkOnly bool
		wantErr   bool
	}{
		{name: "serve"},
		{name: "check config", arguments: []string{"--check-config"}, checkOnly: true},
		{name: "unknown flag", arguments: []string{"--version"}, wantErr: true},
		{name: "extra argument", arguments: []string{"--check-config", "extra"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := startupMode(test.arguments)
			if test.wantErr {
				if err == nil {
					t.Fatal("expected command-line validation error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.checkOnly {
				t.Fatalf("checkOnly = %t, want %t", got, test.checkOnly)
			}
		})
	}
}
